# Copyright 2026 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# 	http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""ViPE engine

Architecture
~~~~~~~~~~~~

Main process (asyncio event loop)
  │
  ├─ start(): spawns 1 worker process (worker-0)
  ├─ Per request:
  │   1. Downloads video (async httpx)
  │   2. Sends (input_path, req_json, profile?) to worker via Queue
  │   3. Worker runs pipeline.run(), returns output_path
  │   4. Uploads artifacts from output_path (async storage)
  ├─ First request: dispatched to worker-0 with profile=True.
  │  Worker-0 runs pipeline with GPU memory profiling, returns profiling
  │  data.  Main process computes worker count, spawns workers 1..N-1.
  ├─ Subsequent requests: round-robin dispatch to all N workers.
  └─ No pipeline, no CUDA in the main process.

Worker process (one per pipeline instance)
  ├─ Owns a dedicated CUDA context + full model weights
  ├─ Receives (input_path, req_json, profile?) from main process via Queue
  ├─ Runs pipeline.run() only (no download, no upload)
  └─ Sends (req_id, output_path | error, profile_data?) back via Queue
"""

from __future__ import annotations

import asyncio
import gc
import logging
import multiprocessing as mp
import os
import tempfile
import time
import uuid
from pathlib import Path
from typing import Any, AsyncIterator, Dict, List, Optional, Union, cast

from prometheus_client import (
    CollectorRegistry,
    Counter,
    Gauge,
    Histogram,
    generate_latest,
)

from aibrix.openai_frontend.engine.engine import LLMEngine
from aibrix.openai_frontend.schemas.openai import (
    ChatCompletionChoice,
    ChatCompletionFinishReason,
    ChatCompletionResponseMessage,
    ChatCompletionStreamingResponseChoice,
    ChatCompletionStreamResponseDelta,
    CompletionUsage,
    CreateChatCompletionRequest,
    CreateChatCompletionResponse,
    CreateChatCompletionStreamResponse,
    CreateCompletionRequest,
    CreateCompletionResponse,
    CreateEmbeddingRequest,
    CreateEmbeddingResponse,
    Model,
    ObjectType,
)
from aibrix.openai_frontend.schemas.vipe import (
    ViPEOutputResult,
    ViPERequest,
    ViPEResponse,
)
from aibrix.openai_frontend.utils.mem_utils import gpu_memory_total
from aibrix.openai_frontend.utils.utils import ClientError

MAX_VIDEO_DOWNLOAD_BYTES = 512 * 1024 * 1024
PER_PIPELINE_MEM_SAFETY_FACTOR = 1.3
WORKER_QUEUE_TIMEOUT = 0.5

logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Shared helpers
# ---------------------------------------------------------------------------


def _pid_exists(pid: int) -> bool:
    try:
        os.kill(pid, 0)
        return True
    except (ProcessLookupError, PermissionError):
        return False


def _configure_pipeline(pipeline: Any, req: ViPERequest, output_path: Path) -> None:
    visualize = req.parameters.visualize or req.parameters.save_viz
    pipeline.out_path = output_path
    pipeline.out_path.mkdir(parents=True, exist_ok=True)
    pipeline.out_cfg.save_artifacts = req.parameters.save_artifacts
    pipeline.out_cfg.save_viz = visualize


def _run_pipeline(pipeline: Any, input_path: Path) -> None:
    from vipe.streams.base import ProcessedVideoStream
    from vipe.streams.raw_mp4_stream import RawMp4Stream

    video_stream = ProcessedVideoStream(RawMp4Stream(input_path), []).cache(
        desc="Reading video stream"
    )
    pipeline.run(video_stream)


def _build_result_dict(
    subdir: str, upload_prefix: str, artifacts: List[str]
) -> Dict[str, Any]:
    from aibrix import envs as _envs

    tos_endpoint = _envs.STORAGE_TOS_BUCKET and _envs.STORAGE_TOS_ENDPOINT or ""
    tos_bucket = _envs.STORAGE_TOS_BUCKET or ""
    output_path_str = f"tos://{tos_endpoint}/{tos_bucket}/{upload_prefix}/"
    return {
        "subdir": subdir,
        "output_path": output_path_str,
        "artifacts": artifacts,
    }


def _result_to_response(result: Dict[str, Any]) -> ViPEResponse:
    return ViPEResponse(
        output=ViPEOutputResult(
            subdir=result["subdir"],
            output_path=result["output_path"],
            artifacts=result["artifacts"],
        )
    )


# ---------------------------------------------------------------------------
# Worker process
# ---------------------------------------------------------------------------


def _worker_main(
    worker_id: int,
    pipeline_type: str,
    request_queue: mp.Queue,
    result_queue: mp.Queue,
) -> None:
    """Entry point for each worker process."""
    proc_name = f"worker-{worker_id}"
    mp.current_process().name = proc_name
    logger.info("[%s] Starting worker process (pid=%d)", proc_name, os.getpid())

    try:
        _worker_main_inner(
            worker_id,
            proc_name,
            pipeline_type,
            request_queue,
            result_queue,
        )
    except KeyboardInterrupt:
        pass


def _worker_main_inner(
    worker_id: int,
    proc_name: str,
    pipeline_type: str,
    request_queue: mp.Queue,
    result_queue: mp.Queue,
) -> None:
    """Create pipeline, then loop on request_queue running pipeline.run only.

    Main process handles download (before dispatch) and upload (after result).
    Worker receives input_path and req_json, runs the pipeline, and returns
    the output_path so the main process can upload artifacts.
    """
    try:
        from vipe import make_pipeline
        from vipe.config import parse_typed_config

        args = parse_typed_config("default", hydra_args=[f"pipeline={pipeline_type}"])
        pipeline = make_pipeline(args.pipeline)
        logger.info("[%s] Pipeline created successfully", proc_name)
    except Exception as exc:
        logger.exception("[%s] Failed to create pipeline", proc_name)
        result_queue.put({"worker_id": worker_id, "init_error": str(exc)})
        return

    while True:
        try:
            item = request_queue.get(timeout=WORKER_QUEUE_TIMEOUT)
        except Exception:
            if not _pid_exists(os.getppid()):
                logger.info("[%s] Parent process gone, exiting", proc_name)
                return
            continue

        if item is None:
            logger.info("[%s] Received shutdown sentinel", proc_name)
            return

        req_id = item["req_id"]
        req_json = item["req_json"]
        input_path_str = item["input_path"]
        output_path_str = item["output_path"]
        profile = item.get("profile", False)

        try:
            req = ViPERequest.model_validate_json(req_json)
            input_path = Path(input_path_str)
            output_path = Path(output_path_str)

            _configure_pipeline(pipeline, req, output_path)

            profile_data: Optional[Dict[str, Any]] = None
            if profile:
                import torch

                from aibrix.openai_frontend.utils.mem_utils import (
                    MemorySnapshot,
                    memory_profiling,
                )

                gc.collect()
                torch.cuda.empty_cache()
                torch.cuda.reset_peak_memory_stats()
                baseline = MemorySnapshot()
                with memory_profiling(baseline, log_diff=True) as profile_result:
                    _run_pipeline(pipeline, input_path)
                profile_data = {
                    "peak_delta_bytes": profile_result.torch_peak_increase,
                }
                logger.info(
                    "[worker-%d] Profile: peak_delta=%.2f GiB",
                    worker_id,
                    profile_result.torch_peak_increase / (1 << 30),
                )
            else:
                _run_pipeline(pipeline, input_path)

            logger.info("[worker-%d] pipeline.run() complete", worker_id)

            result: Dict[str, Any] = {"req_id": req_id, "output_path": output_path_str}
            if profile_data is not None:
                result["profile"] = profile_data
            result_queue.put(result)
        except Exception as exc:
            logger.exception("[%s] Request %s failed", proc_name, req_id)
            result_queue.put(
                {"req_id": req_id, "error": f"{type(exc).__name__}: {exc}"}
            )


# ---------------------------------------------------------------------------
# Main-process engine — download, dispatch, upload
# ---------------------------------------------------------------------------


class ViPEEngine(LLMEngine):
    """Multiprocessing ViPE engine — main process handles I/O, workers run pipelines.

    Lifecycle
    ~~~~~~~~~
    1. ``start()`` — spawn worker-0 (first child process with pipeline).
    2. First ``chat()`` — download in main process, dispatch to worker-0 with
       profile=True.  Worker-0 runs pipeline, returns output_path + profiling
       data.  Main process uploads artifacts, then spawns workers 1..N-1.
    3. Subsequent ``chat()`` — download in main process, round-robin dispatch
       to any worker, upload artifacts in main process.
    """

    def __init__(
        self,
        pipeline_type: str = "default",
        gpu_memory_utilization: float = 0.9,
        max_seq_num: int = 32,
        default_model: str = "vipe",
        max_video_download_bytes: int = MAX_VIDEO_DOWNLOAD_BYTES,
    ):
        self.pipeline_type = pipeline_type
        self._gpu_memory_utilization = gpu_memory_utilization
        self._max_seq_num = max_seq_num
        self.default_model = default_model
        self._max_video_download_bytes = max_video_download_bytes

        self._loaded_models: Dict[str, int] = {default_model: int(time.time())}
        self._ready = False

        # Worker management
        self._workers: List[mp.process.BaseProcess] = []
        self._request_queues: List[mp.Queue] = []
        self._result_queue: mp.Queue = mp.get_context("spawn").Queue()
        self._num_workers: int = 0
        self._rr_index: int = 0
        self._profiling_done: asyncio.Event = asyncio.Event()
        self._profiling_started: bool = False

        # Pending request tracking: req_id → asyncio.Future
        self._pending: Dict[str, asyncio.Future] = {}
        self._result_listener_task: Optional[asyncio.Task] = None

        # Shared temp dirs for each active request (kept alive until upload finishes)
        self._active_tmpdirs: Dict[str, tempfile.TemporaryDirectory] = {}

        # Metrics
        self._metrics_registry = CollectorRegistry()
        self._requests_total = Counter(
            "openai_frontend_requests_total",
            "Total requests served",
            registry=self._metrics_registry,
        )
        self._active_workers = Gauge(
            "openai_frontend_vipe_active_workers",
            "Number of active worker processes",
            registry=self._metrics_registry,
        )
        self._pipeline_duration_seconds = Histogram(
            "openai_frontend_vipe_pipeline_duration_seconds",
            "End-to-end pipeline duration (download+run+upload) per worker",
            buckets=(1, 5, 10, 30, 60, 120, 300, 600),
            registry=self._metrics_registry,
        )

    # ------------------------------------------------------------------
    # Lifecycle
    # ------------------------------------------------------------------

    async def start(self) -> None:
        """Spawn worker-0 so it creates its pipeline while the server warms up."""
        await self._ensure_result_listener()
        self._spawn_worker(worker_id=0)
        self._num_workers = 1
        self._active_workers.set(1)
        self._ready = True
        logger.info("ViPE engine started with worker-0 (type=%s)", self.pipeline_type)

    def _spawn_worker(self, worker_id: int) -> mp.process.BaseProcess:
        """Spawn a single worker process using 'spawn' start method (required for CUDA)."""
        spawn_ctx = mp.get_context("spawn")
        q = spawn_ctx.Queue()
        self._request_queues.append(q)
        p = spawn_ctx.Process(
            target=_worker_main,
            args=(
                worker_id,
                self.pipeline_type,
                q,
                self._result_queue,
            ),
            name=f"vipe-worker-{worker_id}",
            daemon=True,
        )
        p.start()
        self._workers.append(p)
        logger.info("Spawned worker-%d (pid=%d)", worker_id, p.pid)
        return p

    def stop(self) -> None:
        """Send shutdown sentinels to all workers and join them."""
        for q in self._request_queues:
            try:
                q.put(None)
            except Exception:
                pass
        for w in self._workers:
            w.join(timeout=5)
            if w.is_alive():
                logger.warning("Worker %d (pid=%d) did not exit, terminating", w.pid)
                w.terminate()
        logger.info("All workers stopped")

    async def _ensure_result_listener(self) -> None:
        if self._result_listener_task is not None:
            return
        self._result_listener_task = asyncio.create_task(self._result_listener_loop())

    async def _result_listener_loop(self) -> None:
        """Poll the result queue and resolve pending futures."""
        loop = asyncio.get_event_loop()
        while True:
            try:
                item = self._result_queue.get(timeout=0.1)
            except Exception:
                await asyncio.sleep(0.05)
                continue

            if "init_error" in item:
                worker_id = item["worker_id"]
                logger.error(
                    "Worker-%d failed to initialize: %s",
                    worker_id,
                    item["init_error"],
                )
                continue

            req_id = item["req_id"]
            future = self._pending.pop(req_id, None)
            if future is None:
                logger.warning("No pending future for req_id=%s", req_id)
                continue

            if "error" in item:
                loop.call_soon_threadsafe(
                    future.set_exception, ClientError(item["error"])
                )
            else:
                loop.call_soon_threadsafe(future.set_result, item)

    # ------------------------------------------------------------------
    # LLMEngine protocol
    # ------------------------------------------------------------------

    def health(self) -> bool:
        return self._ready

    def metrics(self) -> str:
        return generate_latest(self._metrics_registry).decode("utf-8")

    def models(self) -> List[Model]:
        return [
            Model(
                id=model_name,
                created=created,
                object=ObjectType.model,
                owned_by="ViPE Engine",
            )
            for model_name, created in self._loaded_models.items()
        ]

    async def load_model(self, model_name: str) -> Model:
        if model_name in self._loaded_models:
            raise ClientError(f"Model '{model_name}' is already loaded")
        created = int(time.time())
        self._loaded_models[model_name] = created
        return Model(
            id=model_name,
            created=created,
            object=ObjectType.model,
            owned_by="ViPE Engine",
        )

    async def unload_model(self, model_name: str) -> None:
        if model_name not in self._loaded_models:
            raise ClientError(f"Unknown model: {model_name}")
        if model_name == self.default_model and len(self._loaded_models) == 1:
            raise ClientError("Cannot unload the last available model")
        del self._loaded_models[model_name]

    async def embedding(
        self, request: CreateEmbeddingRequest
    ) -> CreateEmbeddingResponse:
        self._requests_total.inc()
        raise NotImplementedError("Embeddings are not supported by ViPE engine")

    async def completion(
        self, request: CreateCompletionRequest
    ) -> Union[CreateCompletionResponse, AsyncIterator[str]]:
        self._requests_total.inc()
        raise NotImplementedError("Completions are not supported by ViPE engine")

    async def chat(
        self, request: CreateChatCompletionRequest
    ) -> Union[CreateChatCompletionResponse, AsyncIterator[str]]:
        self._requests_total.inc()
        model = str(request.model)
        self._assert_model_exists(model)
        if len(request.messages) == 0:
            raise ClientError("Messages is empty")
        if request.messages[0].content is None:
            raise ClientError("Message content is required")
        req = cast(ViPERequest, request.messages[0].content)
        resp = await self._run_completion(req)

        if request.stream:
            return self._stream_chat_response(model, resp)

        usage = self._build_usage(
            req.model_dump_json(exclude_unset=True),
            resp.model_dump_json(exclude_unset=True),
        )
        return CreateChatCompletionResponse(
            id=f"chatcmpl-{uuid.uuid4().hex}",
            choices=[
                ChatCompletionChoice(
                    finish_reason=ChatCompletionFinishReason.stop,
                    index=0,
                    message=ChatCompletionResponseMessage(
                        content=resp,
                        role="assistant",
                        tool_calls=None,
                        function_call=None,
                    ),
                    logprobs=None,
                )
            ],
            created=int(time.time()),
            model=model,
            system_fingerprint=None,
            object=ObjectType.chat_completion,
            usage=usage,
        )

    # ------------------------------------------------------------------
    # Core dispatch logic: download → dispatch → upload
    # ------------------------------------------------------------------

    async def _run_completion(self, req: ViPERequest) -> ViPEResponse:
        if not self._profiling_done.is_set():
            if not self._profiling_started:
                self._profiling_started = True
                return await self._run_first_request(req)
            # Another request is already doing profiling — wait for it to finish
            await self._profiling_done.wait()
        return await self._dispatch_to_worker(req)

    async def _download_video(self, video_url: str, input_path: Path) -> None:
        """Download video to input_path using async httpx."""
        import httpx

        downloaded = 0
        async with httpx.AsyncClient(timeout=60, follow_redirects=False) as client:
            async with client.stream("GET", video_url) as resp:
                resp.raise_for_status()
                with input_path.open("wb") as f:
                    async for chunk in resp.aiter_bytes():
                        if not chunk:
                            continue
                        downloaded += len(chunk)
                        if downloaded > self._max_video_download_bytes:
                            raise ClientError(
                                f"'video_url' file too large, max "
                                f"{self._max_video_download_bytes} bytes"
                            )
                        f.write(chunk)
        logger.info(
            "Download complete: %d bytes (%.2f MiB)", downloaded, downloaded / (1 << 20)
        )

    async def _upload_outputs(self, output_path: Path, upload_prefix: str) -> List[str]:
        """Upload pipeline outputs to storage using async storage client."""
        from aibrix.storage import create_storage_from_env

        storage = create_storage_from_env()
        keys: List[str] = []
        base_prefix = upload_prefix.strip("/")
        for file_path in output_path.rglob("*"):
            if not file_path.is_file():
                continue
            rel = file_path.relative_to(output_path).as_posix()
            key = f"{base_prefix}/{rel}"
            data = file_path.read_bytes()
            await storage.put_object(key, data)
            keys.append(key)
        return keys

    async def _prepare_request(self, req: ViPERequest) -> tuple[str, str, str]:
        """Download video and prepare temp dirs. Returns (req_id, input_path_str, output_path_str)."""
        req_id = uuid.uuid4().hex
        tmpdir = tempfile.TemporaryDirectory(prefix=f"aibrix_vipe_{req_id[:8]}_")
        tmp_path = Path(tmpdir.name)
        input_path = tmp_path / "input.mp4"
        output_path = tmp_path / "output"
        output_path.mkdir(parents=True, exist_ok=True)

        video_url = req.input.video_url
        logger.info("[req-%s] Downloading video from %s", req_id[:8], video_url)
        await self._download_video(video_url, input_path)

        # Keep tmpdir alive until upload finishes
        self._active_tmpdirs[req_id] = tmpdir
        return req_id, str(input_path), str(output_path)

    async def _finalize_request(
        self, req: ViPERequest, req_id: str, worker_result: Dict[str, Any]
    ) -> tuple[ViPEResponse, Dict[str, Any]]:
        """Upload outputs, clean up temp dir, and return response."""
        custom_id = req.custom_id
        subdir = req.output.subdir
        upload_prefix = f"{subdir}/{custom_id}"

        output_path = Path(worker_result["output_path"])
        uploaded = await self._upload_outputs(output_path, upload_prefix)
        logger.info("[req-%s] Uploaded %d artifacts", req_id[:8], len(uploaded))

        # Clean up temp dir
        self._active_tmpdirs.pop(req_id, None)

        result = _build_result_dict(subdir, upload_prefix, uploaded)
        if "profile" in worker_result:
            result["profile"] = worker_result["profile"]
        return _result_to_response(result), result

    async def _run_first_request(self, req: ViPERequest) -> ViPEResponse:
        """Download, dispatch to worker-0 with profiling, upload, then spawn more workers."""
        req_id, input_path_str, output_path_str = await self._prepare_request(req)
        logger.info("Dispatching first request to worker-0 with GPU profiling")

        req_json = req.model_dump_json(exclude_unset=True)
        queue = self._request_queues[0]
        loop = asyncio.get_event_loop()
        future: asyncio.Future = loop.create_future()
        self._pending[req_id] = future

        queue.put(
            {
                "req_id": req_id,
                "req_json": req_json,
                "input_path": input_path_str,
                "output_path": output_path_str,
                "profile": True,
            }
        )
        logger.info("Dispatched req_id=%s to worker-0 (profile=True)", req_id[:8])

        try:
            worker_result = await asyncio.wait_for(future, timeout=600.0)
        except asyncio.TimeoutError:
            self._pending.pop(req_id, None)
            self._active_tmpdirs.pop(req_id, None)
            raise ClientError(f"Request {req_id} timed out waiting for worker-0")

        if "error" in worker_result:
            self._active_tmpdirs.pop(req_id, None)
            raise ClientError(worker_result["error"])

        # Upload artifacts
        resp, result = await self._finalize_request(req, req_id, worker_result)

        # Extract profiling data to decide worker count
        profile = result.pop("profile", None)
        if profile is not None:
            peak_delta = profile.get("peak_delta_bytes", 0)
            per_pipeline_bytes = int(peak_delta * PER_PIPELINE_MEM_SAFETY_FACTOR)
            if per_pipeline_bytes <= 0:
                per_pipeline_bytes = max(peak_delta * 2, 12 * (1 << 30))
        else:
            per_pipeline_bytes = 12 * (1 << 30)  # fallback

        gpu_budget = int(gpu_memory_total() * self._gpu_memory_utilization)
        target_workers = max(
            1,
            min(self._max_seq_num, gpu_budget // per_pipeline_bytes),
        )
        logger.info(
            "GPU profiling: per_pipeline_avg=%.2f GiB, gpu_budget=%.2f GiB, "
            "target_workers=%d",
            per_pipeline_bytes / (1 << 30),
            gpu_budget / (1 << 30),
            target_workers,
        )

        # Spawn additional workers (worker-0 already exists)
        for i in range(1, target_workers):
            self._spawn_worker(worker_id=i)

        self._num_workers = target_workers
        self._active_workers.set(target_workers)
        self._profiling_done.set()

        return resp

    async def _dispatch_to_worker(self, req: ViPERequest) -> ViPEResponse:
        """Download, round-robin dispatch to a worker, upload."""
        req_id, input_path_str, output_path_str = await self._prepare_request(req)
        worker_idx = self._rr_index % self._num_workers
        self._rr_index += 1

        req_json = req.model_dump_json(exclude_unset=True)
        queue = self._request_queues[worker_idx]
        loop = asyncio.get_event_loop()
        future: asyncio.Future = loop.create_future()
        self._pending[req_id] = future

        self._validate_pipeline_type(req)
        queue.put(
            {
                "req_id": req_id,
                "req_json": req_json,
                "input_path": input_path_str,
                "output_path": output_path_str,
                "profile": False,
            }
        )
        logger.info("Dispatched req_id=%s to worker-%d", req_id[:8], worker_idx)

        try:
            worker_result = await asyncio.wait_for(future, timeout=600.0)
        except asyncio.TimeoutError:
            self._pending.pop(req_id, None)
            self._active_tmpdirs.pop(req_id, None)
            raise ClientError(
                f"Request {req_id} timed out waiting for worker-{worker_idx}"
            )

        if "error" in worker_result:
            self._active_tmpdirs.pop(req_id, None)
            raise ClientError(worker_result["error"])

        resp, _ = await self._finalize_request(req, req_id, worker_result)
        return resp

    def _validate_pipeline_type(self, req: ViPERequest) -> None:
        if req.parameters.pipeline != self.pipeline_type:
            raise ClientError(
                f"ViPE pipeline type '{req.parameters.pipeline}' is not supported, "
                f"only '{self.pipeline_type}' is supported"
            )

    # ------------------------------------------------------------------
    # Helpers
    # ------------------------------------------------------------------

    def _build_usage(self, prompt: str, completion: str) -> CompletionUsage:
        import re

        prompt_tokens = len(re.split(r"[^a-zA-Z0-9]+", prompt))
        completion_tokens = len(re.split(r"[^a-zA-Z0-9]+", completion))
        return CompletionUsage(
            prompt_tokens=prompt_tokens,
            completion_tokens=completion_tokens,
            total_tokens=prompt_tokens + completion_tokens,
        )

    def _assert_model_exists(self, model_name: str) -> None:
        if model_name not in self._loaded_models:
            raise ClientError(f"Unknown model: {model_name}")

    # ------------------------------------------------------------------
    # Streaming helpers
    # ------------------------------------------------------------------

    async def _chat_stream_generator(
        self, request_id: str, model: str, resp: ViPEResponse
    ) -> AsyncIterator[str]:
        created = int(time.time())
        first = CreateChatCompletionStreamResponse(
            id=request_id,
            choices=[
                ChatCompletionStreamingResponseChoice(
                    index=0,
                    delta=ChatCompletionStreamResponseDelta(
                        role="assistant", content="", function_call=None
                    ),
                    logprobs=None,
                    finish_reason=None,
                )
            ],
            created=created,
            model=model,
            system_fingerprint=None,
            object=ObjectType.chat_completion_chunk,
            usage=None,
        )
        yield f"data: {first.model_dump_json(exclude_unset=True)}\n\n"

        chunk = CreateChatCompletionStreamResponse(
            id=request_id,
            choices=[
                ChatCompletionStreamingResponseChoice(
                    index=0,
                    delta=ChatCompletionStreamResponseDelta(
                        role="assistant", content=resp, function_call=None
                    ),
                    logprobs=None,
                    finish_reason=ChatCompletionFinishReason.stop,
                )
            ],
            created=created,
            model=model,
            system_fingerprint=None,
            object=ObjectType.chat_completion_chunk,
            usage=None,
        )
        yield f"data: {chunk.model_dump_json(exclude_unset=True)}\n\n"
        yield "data: [DONE]\n\n"

    def _stream_chat_response(
        self, model: str, resp: ViPEResponse
    ) -> AsyncIterator[str]:
        return self._chat_stream_generator(
            request_id=f"chatcmpl-{uuid.uuid4().hex}",
            model=model,
            resp=resp,
        )

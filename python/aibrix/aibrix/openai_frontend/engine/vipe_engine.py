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
import sys
import tempfile
import time
import uuid
from pathlib import Path
from typing import Any, AsyncIterator, Dict, List, Optional, Union, cast

import httpx
from prometheus_client import (
    CollectorRegistry,
    Counter,
    Gauge,
    Histogram,
    generate_latest,
)
from tenacity import (
    retry,
    retry_if_exception_type,
    stop_after_attempt,
    wait_exponential,
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
from aibrix.openai_frontend.utils.utils import (
    ClientError,
    ServerError,
    make_prefix_formatter,
    prefix_line,
)

MAX_VIDEO_DOWNLOAD_BYTES = 512 * 1024 * 1024
PER_PIPELINE_MEM_SAFETY_FACTOR = 1.2
WORKER_QUEUE_TIMEOUT = 0.5
DEFAULT_QUEUE_PUT_TIMEOUT_SECONDS = 1.0

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
    pipeline.post_cfg.depth_align_model = req.parameters.depth_align_model


def _run_pipeline(
    pipeline: Any, input_path: Path, input_fps: float | None = None
) -> None:
    from vipe.streams.base import ProcessedVideoStream

    if input_fps is not None:
        from vipe.streams.frame_dir_stream import FrameDirStream

        stream = FrameDirStream(input_path)
        stream._fps = input_fps
        video_stream = ProcessedVideoStream(stream, []).cache(
            desc="Reading frame stream"
        )
    else:
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


class _PrefixedStream:
    """Wraps a stream so every write gets a [worker-N] prefix."""

    def __init__(self, original: Any, prefix: str) -> None:
        self._original = original
        self._prefix = prefix
        if hasattr(original, "buffer") and original.buffer is not None:
            self.buffer = _PrefixedBufferWriter(original.buffer, prefix)

    def write(self, text: str) -> int:
        return self._original.write(prefix_line(text, self._prefix))

    def flush(self) -> None:
        self._original.flush()

    def __getattr__(self, name: str) -> Any:
        return getattr(self._original, name)


class _PrefixedBufferWriter:
    """Wraps a binary buffer so every write gets a [worker-N] prefix."""

    def __init__(self, original: Any, prefix: str) -> None:
        self._original = original
        self._prefix = prefix

    def write(self, data: bytes) -> int:
        if not data:
            return 0
        text = (
            data.decode("utf-8", errors="replace")
            if isinstance(data, bytes)
            else str(data)
        )
        return self._original.write(prefix_line(text, self._prefix).encode("utf-8"))

    def flush(self) -> None:
        self._original.flush()

    def __getattr__(self, name: str) -> Any:
        return getattr(self._original, name)


def _worker_main(
    worker_id: int,
    pipeline_type: str,
    request_queue: mp.Queue,
    result_queue: mp.Queue,
    log_level: int = logging.INFO,
) -> None:
    """Entry point for each worker process."""
    proc_name = f"worker-{worker_id}"
    mp.current_process().name = proc_name

    prefix = f"[worker-{worker_id}]"

    # Throttle tqdm to ~1 update every 5 s in non-TTY mode so progress is
    # visible without flooding the log with hundreds of lines.
    os.environ["TQDM_MININTERVAL"] = "5"
    os.environ["PYTHONUNBUFFERED"] = "1"
    os.environ["TOKENIZERS_PARALLELISM"] = "false"

    # Configure root logger to match main process format and level.
    # Spawned child processes do NOT inherit the parent's logging handlers,
    # but a dependency's module-level logging.basicConfig() may have added one.
    root = logging.getLogger()
    formatter = make_prefix_formatter(prefix)
    if root.handlers:
        for h in root.handlers:
            h.setFormatter(formatter)
    else:
        handler = logging.StreamHandler(sys.stderr)
        handler.setFormatter(formatter)
        root.addHandler(handler)
    root.setLevel(log_level)

    # Add [worker-N] prefix to all stdout/stderr output.
    sys.stdout = _PrefixedStream(sys.stdout, prefix)  # type: ignore[assignment]
    sys.stderr = _PrefixedStream(sys.stderr, prefix)  # type: ignore[assignment]
    sys.__stdout__ = sys.stdout  # type: ignore[misc,assignment]
    sys.__stderr__ = sys.stderr  # type: ignore[misc,assignment]

    logger.info("Starting worker process (pid=%d)", os.getpid())

    try:
        _worker_main_inner(
            worker_id,
            pipeline_type,
            request_queue,
            result_queue,
        )
    except KeyboardInterrupt:
        pass


def _worker_main_inner(
    worker_id: int,
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
        logger.info("Pipeline created successfully")
    except Exception as exc:
        logger.exception("Failed to create pipeline")
        result_queue.put({"worker_id": worker_id, "init_error": str(exc)})
        return

    while True:
        try:
            item = request_queue.get(timeout=WORKER_QUEUE_TIMEOUT)
        except Exception:
            if not _pid_exists(os.getppid()):
                logger.info("Parent process gone, exiting")
                return
            continue

        if item is None:
            logger.info("Received shutdown sentinel")
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

            input_fps = req.parameters.input_fps
            if input_fps is not None:
                logger.info("Resampling to input_fps=%.1f", input_fps)

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
                    _run_pipeline(pipeline, input_path, input_fps)
                profile_data = {
                    "peak_delta_bytes": profile_result.torch_peak_increase,
                    "gpu_total_bytes": torch.cuda.mem_get_info()[1],
                }
                logger.info(
                    "Profile: peak_delta=%.2f GiB",
                    profile_result.torch_peak_increase / (1 << 30),
                )
            else:
                _run_pipeline(pipeline, input_path, input_fps)

            logger.info("pipeline.run() complete")

            result: Dict[str, Any] = {"req_id": req_id, "output_path": output_path_str}
            if profile_data is not None:
                result["profile"] = profile_data
            result_queue.put(result)
        except Exception as exc:
            logger.exception("Request %s failed", req_id)
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
        queue_put_timeout_seconds: float = DEFAULT_QUEUE_PUT_TIMEOUT_SECONDS,
    ):
        self.pipeline_type = pipeline_type
        self._gpu_memory_utilization = gpu_memory_utilization
        self._max_seq_num = max_seq_num
        self.default_model = default_model
        self._max_video_download_bytes = max_video_download_bytes
        self._queue_put_timeout_seconds = queue_put_timeout_seconds
        self._log_level = logging.getLogger().getEffectiveLevel()

        self._loaded_models: Dict[str, int] = {default_model: int(time.time())}
        self._ready = False

        # Worker management
        self._workers: List[mp.process.BaseProcess] = []
        self._request_queues: List[mp.Queue] = []
        self._result_queues: List[mp.Queue] = []
        self._dead_result_queue_indices: set[int] = set()
        self._num_workers: int = 0
        self._rr_index: int = 0
        self._profiling_done: asyncio.Event = asyncio.Event()
        self._profiling_started: bool = False
        self._profiling_success: bool = False

        # Worker health tracking
        self._respawn_counts: Dict[int, int] = {}
        # Per-worker backoff-until timestamp (monotonic); worker is excluded
        # from dispatch until all OTHER workers have completed >=1 request
        # after this worker was respawned, AND the backoff time has elapsed.
        self._worker_backoff_until: Dict[int, float] = {}
        # Count of successful completions per worker since its last respawn.
        # Reset to 0 on respawn, incremented on each successful result.
        self._worker_success_since_respawn: Dict[int, int] = {}

        # Waiting list: requests orphaned by dead workers, pending re-dispatch.
        # Each entry is (req_id, dispatch_item).
        self._waiting_list: List[tuple[str, Dict[str, Any]]] = []
        self._waiting_req_ids: set[str] = set()

        # Pending request tracking: req_id → asyncio.Future
        self._pending: Dict[str, asyncio.Future] = {}
        # Track dispatch timestamp for watchdog logging
        self._pending_since: Dict[str, float] = {}
        # Track which worker each request was dispatched to (for dead-worker cleanup)
        self._req_worker_map: Dict[str, int] = {}
        # Store the dispatch item per req_id so we can re-queue on worker death
        self._dispatched_items: Dict[str, Dict[str, Any]] = {}
        # OOM retry tracking: req_id → retry count
        self._oom_retry_counts: Dict[str, int] = {}
        self._result_listener_task: Optional[asyncio.Task] = None
        self._stopping: bool = False

        # Shared temp dirs for each active request (kept alive until upload finishes)
        self._active_tmpdirs: Dict[str, tempfile.TemporaryDirectory] = {}

        # Add [main] prefix to all logger output via formatter
        for h in logging.getLogger().handlers:
            h.setFormatter(make_prefix_formatter("[main]"))

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
        self._stopping = False
        await self._ensure_result_listener()
        self._spawn_worker(worker_id=0)
        self._num_workers = 1
        self._active_workers.set(1)
        self._ready = True
        logger.info("ViPE engine started with worker-0 (type=%s)", self.pipeline_type)

    def _spawn_worker(self, worker_id: int) -> mp.process.BaseProcess:
        """Spawn a single worker process using 'spawn' start method (required for CUDA)."""
        spawn_ctx = mp.get_context("spawn")
        req_q = spawn_ctx.Queue()
        res_q = spawn_ctx.Queue()
        self._request_queues.append(req_q)
        self._result_queues.append(res_q)
        p = spawn_ctx.Process(
            target=_worker_main,
            args=(
                worker_id,
                self.pipeline_type,
                req_q,
                res_q,
                self._log_level,
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
        self._stopping = True

        task = self._result_listener_task
        if task is not None:
            task.cancel()
            self._result_listener_task = None

        shutdown_exc = ClientError("ViPE engine is stopping")
        for req_id, future in list(self._pending.items()):
            if not future.done():
                future.set_exception(shutdown_exc)
            self._cleanup_request_tmpdir(req_id)

        self._pending.clear()
        self._pending_since.clear()
        self._req_worker_map.clear()
        self._dispatched_items.clear()
        self._waiting_list.clear()
        self._waiting_req_ids.clear()

        for req_id in list(self._active_tmpdirs.keys()):
            self._cleanup_request_tmpdir(req_id)

        for q in self._request_queues:
            try:
                put_nowait = getattr(q, "put_nowait", None)
                if callable(put_nowait):
                    put_nowait(None)
                else:
                    q.put(None, timeout=self._queue_put_timeout_seconds)
            except Exception:
                pass
        for w in self._workers:
            w.join(timeout=5)
            if w.is_alive():
                logger.warning(
                    "Worker (pid=%d) did not exit, terminating",
                    w.pid or -1,
                )
                w.terminate()
                w.join(timeout=5)
        logger.info("All workers stopped")

    async def _ensure_result_listener(self) -> None:
        if self._result_listener_task is not None:
            return
        logger.info("[listener] Creating _result_listener_loop task")
        self._result_listener_task = asyncio.create_task(
            self._result_listener_loop_guarded()
        )
        self._result_listener_task.add_done_callback(self._on_result_listener_done)

    def _on_result_listener_done(self, task: asyncio.Task) -> None:
        """Fires when _result_listener_loop exits — should never happen."""
        self._result_listener_task = None
        if self._stopping:
            logger.info("_result_listener_loop stopped during shutdown")
            return

        if task.cancelled():
            logger.warning("_result_listener_loop was cancelled unexpectedly")
            asyncio.ensure_future(self._restart_result_listener())
            return

        exc = task.exception()
        if exc is not None:
            logger.critical("_result_listener_loop crashed: %s", exc, exc_info=exc)
        else:
            logger.critical("_result_listener_loop exited unexpectedly")

        # Auto-restart the listener so pending futures get resolved.
        asyncio.ensure_future(self._restart_result_listener())

    async def _restart_result_listener(self) -> None:
        if self._stopping:
            return
        await asyncio.sleep(1.0)
        if self._stopping:
            return
        logger.warning("[listener] Restarting _result_listener_loop after crash")
        await self._ensure_result_listener()

    async def _check_worker_health(self) -> None:
        """Check if any workers have died and respawn them.

        Orphaned requests are moved to a waiting list instead of being
        immediately re-dispatched.  The respawned worker is put on
        minute-level backoff and excluded from dispatch until every other
        non-backoff worker has completed at least one request since the respawn.
        """
        if self._stopping:
            return

        for idx in range(len(self._workers)):
            worker = self._workers[idx]
            if worker.is_alive():
                continue
            self._worker_backoff_until.pop(idx, None)
            logger.warning(
                "Worker-%d (pid=%d) is dead, respawning",
                idx,
                worker.pid or -1,
            )
            # Mark the result queue as dead immediately so the listener
            # loop never tries to read from a potentially corrupted queue
            # (a SIGKILL'd worker can leave the queue in a state where
            # even get(block=False) hangs).
            if idx < len(self._result_queues):
                self._dead_result_queue_indices.add(idx)
            # Move orphaned requests to waiting list (or fail if
            # redispatch limit exceeded)
            MAX_REDISPATCH = 5
            dead_req_ids = [
                req_id for req_id, widx in self._req_worker_map.items() if widx == idx
            ]
            for req_id in dead_req_ids:
                item = self._dispatched_items.pop(req_id, None)
                if item is not None:
                    redispatch_count = item.get("_redispatch_count", 0) + 1
                    if redispatch_count > MAX_REDISPATCH:
                        self._req_worker_map.pop(req_id, None)
                        self._remove_from_waiting_list(req_id)
                        future = self._pending.pop(req_id, None)
                        self._pending_since.pop(req_id, None)
                        self._cleanup_request_tmpdir(req_id)
                        if future is not None and not future.done():
                            future.set_exception(
                                ServerError(
                                    f"Request {req_id[:8]} exceeded max "
                                    f"redispatch limit ({MAX_REDISPATCH}), "
                                    f"workers keep dying"
                                )
                            )
                        logger.warning(
                            "Failing req_id=%s: exceeded redispatch limit (%d)",
                            req_id[:8],
                            MAX_REDISPATCH,
                        )
                        continue
                    item["_redispatch_count"] = redispatch_count
                    self._waiting_list.append((req_id, item))
                    self._waiting_req_ids.add(req_id)
                    self._req_worker_map.pop(req_id, None)
                    logger.info(
                        "Moved req_id=%s to waiting list (worker-%d died, "
                        "redispatch #%d)",
                        req_id[:8],
                        idx,
                        redispatch_count,
                    )
                else:
                    self._req_worker_map.pop(req_id, None)
                    future = self._pending.pop(req_id, None)
                    self._pending_since.pop(req_id, None)
                    self._cleanup_request_tmpdir(req_id)
                    if future is not None and not future.done():
                        future.set_exception(
                            ServerError(
                                f"Worker-{idx} died and dispatch item lost "
                                f"for request {req_id[:8]}"
                            )
                        )

            # Fork in executor to avoid blocking event loop; assign state
            # back on the main thread to avoid race conditions.
            p, req_q, res_q = await asyncio.get_event_loop().run_in_executor(
                None, self._create_and_start_worker, idx
            )
            old = self._workers[idx]
            self._workers[idx] = p
            self._request_queues[idx] = req_q
            self._result_queues[idx] = res_q
            self._dead_result_queue_indices.discard(idx)
            logger.info(
                "Respawned worker-%d (new pid=%d, old pid=%d)",
                idx,
                p.pid,
                old.pid or -1,
            )
            # Reap old process without blocking
            if old.pid:
                asyncio.get_event_loop().run_in_executor(None, old.join, 0)
            # Minute-level exponential backoff: 1m, 2m, 4m, 8m, ... max 30m
            count = self._respawn_counts.get(idx, 0) + 1
            self._respawn_counts[idx] = count
            backoff_minutes = min(30, 2 ** (count - 1))
            self._worker_backoff_until[idx] = time.monotonic() + backoff_minutes * 60
            self._worker_success_since_respawn[idx] = 0
            logger.info(
                "Worker-%d backoff: %dm (respawn #%d)",
                idx,
                backoff_minutes,
                count,
            )

    def _is_worker_available(self, idx: int) -> bool:
        """Check if a worker is available for dispatch.

        A respawned worker is available only when:
        1. Its backoff time has elapsed, AND
        2. Every other non-backoff worker has completed >=1 request
           since this worker's last respawn.

        If ALL alive workers are in backoff, one is released immediately
        (the one with the earliest backoff-until) so the engine can
        serve requests even when backoff hasn't fully elapsed.
        """
        if not self._workers[idx].is_alive():
            return False
        backoff_until = self._worker_backoff_until.get(idx)
        if backoff_until is None:
            return True
        # All alive workers in backoff? Release the one with earliest backoff-until.
        all_in_backoff = all(
            i in self._worker_backoff_until
            for i in range(len(self._workers))
            if self._workers[i].is_alive()
        )
        if all_in_backoff:
            earliest_idx = min(
                (i for i in self._worker_backoff_until if self._workers[i].is_alive()),
                key=lambda i: self._worker_backoff_until[i],
            )
            if earliest_idx != idx:
                return False
            del self._worker_backoff_until[idx]
            logger.info("All workers in backoff, releasing worker-%d (earliest)", idx)
            return True
        if time.monotonic() < backoff_until:
            return False
        # Check that all OTHER non-backoff alive workers have completed >=1 request
        for other_idx in range(len(self._workers)):
            if other_idx == idx:
                continue
            if other_idx in self._worker_backoff_until:
                continue
            if not self._workers[other_idx].is_alive():
                continue
            if self._worker_success_since_respawn.get(other_idx, 0) < 1:
                return False
        # All conditions met — clear backoff
        del self._worker_backoff_until[idx]
        logger.info("Worker-%d backoff cleared, now available for dispatch", idx)
        return True

    def _drain_waiting_list(self) -> None:
        """Re-dispatch waiting-list requests to available workers."""
        if not self._waiting_list:
            return
        logger.info("[drain] %d requests in waiting list", len(self._waiting_list))
        still_waiting: List[tuple[str, Dict[str, Any]]] = []
        for req_id, item in self._waiting_list:
            dispatched = False
            for i in range(self._num_workers):
                idx = (self._rr_index + i) % self._num_workers
                if self._is_worker_available(idx):
                    queue = self._request_queues[idx]
                    queue.put(item)
                    self._req_worker_map[req_id] = idx
                    self._dispatched_items[req_id] = item
                    self._waiting_req_ids.discard(req_id)
                    self._rr_index = idx + 1
                    logger.info(
                        "Re-dispatched req_id=%s from waiting list to worker-%d",
                        req_id[:8],
                        idx,
                    )
                    dispatched = True
                    break
            if not dispatched:
                still_waiting.append((req_id, item))
        if still_waiting:
            logger.info(
                "[drain] %d requests still waiting (no available workers)",
                len(still_waiting),
            )
        self._waiting_list = still_waiting

    def _create_and_start_worker(self, idx: int):
        """Fork and start a new worker process."""
        spawn_ctx = mp.get_context("spawn")
        req_q = spawn_ctx.Queue()
        res_q = spawn_ctx.Queue()
        p = spawn_ctx.Process(
            target=_worker_main,
            args=(idx, self.pipeline_type, req_q, res_q, self._log_level),
            name=f"vipe-worker-{idx}",
            daemon=True,
        )
        p.start()
        return p, req_q, res_q

    def _respawn_worker(self, idx: int) -> None:
        """Replace a dead worker at the given index with a new process."""
        if self._stopping:
            return

        old = self._workers[idx]
        p, req_q, res_q = self._create_and_start_worker(idx)
        self._request_queues[idx] = req_q
        self._result_queues[idx] = res_q
        self._dead_result_queue_indices.discard(idx)
        self._workers[idx] = p
        logger.info(
            "Respawned worker-%d (new pid=%d, old pid=%d)",
            idx,
            p.pid,
            old.pid or -1,
        )
        try:
            old.join(timeout=0)
        except Exception:
            pass

    async def _result_listener_loop(self) -> None:
        """Poll all per-worker result queues and resolve pending futures.

        Each worker has its own result queue so that an OOM-killed worker
        can only poison its own queue — other workers' results are unaffected.
        """
        loop = asyncio.get_event_loop()
        cycle = 0
        while True:
            if self._stopping:
                return

            cycle += 1
            try:
                await self._check_worker_health()
            except Exception:
                logger.exception("Error in _check_worker_health; will retry next cycle")

            # Drain waiting list every cycle so requests are dispatched
            # as soon as workers become available (e.g. after backoff expires).
            if self._waiting_list:
                self._drain_waiting_list()

            # Periodic heartbeat: log state every 600 cycles (~60s) to
            # help diagnose hangs.  If these logs stop appearing, the
            # loop is stuck *between* two log points.
            if cycle % 600 == 0:
                alive_workers = sum(1 for w in self._workers if w.is_alive())
                logger.info(
                    "[listener-heartbeat] cycle=%d alive_workers=%d/%d "
                    "pending=%d waiting=%d dead_queues=%s",
                    cycle,
                    alive_workers,
                    len(self._workers),
                    len(self._pending),
                    len(self._waiting_list),
                    self._dead_result_queue_indices,
                )
                # Watchdog: log any future pending > 5 minutes
                now = time.monotonic()
                for rid, since in list(self._pending_since.items()):
                    elapsed_s = now - since
                    if elapsed_s > 300:
                        widx = self._req_worker_map.get(rid, "?")
                        logger.warning(
                            "[watchdog] req_id=%s has been pending %.0fs "
                            "(dispatched to worker-%s)",
                            rid[:8],
                            elapsed_s,
                            widx,
                        )

            got_result = False
            for q_idx in range(len(self._result_queues)):
                if q_idx in self._dead_result_queue_indices:
                    continue
                q = self._result_queues[q_idx]
                if cycle % 600 == 0:
                    logger.info("[listener] cycle %d: polling queue-%d", cycle, q_idx)
                try:
                    item = q.get(block=False)
                except OSError:
                    logger.critical(
                        "Result queue-%d is poisoned (OSError), marking as dead",
                        q_idx,
                    )
                    self._dead_result_queue_indices.add(q_idx)
                    continue
                except Exception:
                    # queue.Empty — normal, no result yet from this worker
                    continue
                logger.info(
                    "[listener] got item from queue-%d: %s",
                    q_idx,
                    list(item.keys()) if isinstance(item, dict) else type(item),
                )

                got_result = True
                try:
                    if "init_error" in item:
                        worker_id = item["worker_id"]
                        logger.error(
                            "Worker-%d failed to initialize: %s",
                            worker_id,
                            item["init_error"],
                        )
                        # Clear backoff so _check_worker_health will detect the dead
                        # worker on the next cycle and respawn it again.
                        self._worker_backoff_until.pop(worker_id, None)
                        continue

                    req_id = item["req_id"]
                    worker_idx = self._req_worker_map.pop(req_id, None)
                    future = self._pending.pop(req_id, None)
                    self._pending_since.pop(req_id, None)
                    if future is None:
                        logger.warning("No pending future for req_id=%s", req_id)
                        self._dispatched_items.pop(req_id, None)
                        continue

                    # Track successful completions per worker for backoff gate
                    if "error" not in item and worker_idx is not None:
                        self._respawn_counts[worker_idx] = 0
                        self._worker_success_since_respawn[worker_idx] = (
                            self._worker_success_since_respawn.get(worker_idx, 0) + 1
                        )
                        self._oom_retry_counts.pop(req_id, None)
                        self._dispatched_items.pop(req_id, None)
                        # Try to drain waiting list now that this worker is healthy
                        self._drain_waiting_list()

                    if "error" in item:
                        err_msg = item["error"]
                        is_oom = self._is_oom_error(err_msg)
                        if is_oom:
                            retry_count = self._oom_retry_counts.get(req_id, 0) + 1
                            self._oom_retry_counts[req_id] = retry_count
                            # Kill the OOM worker so it doesn't receive more
                            # requests with corrupted GPU memory.
                            # _check_worker_health will respawn it on the next
                            # cycle with a fresh GPU context.
                            if worker_idx is not None:
                                await loop.run_in_executor(
                                    None, self._kill_oom_worker, worker_idx
                                )
                            dispatch_item = self._dispatched_items.get(req_id)
                            if dispatch_item is not None:
                                self._pending[req_id] = future
                                self._pending_since[req_id] = time.monotonic()
                                self._waiting_list.append((req_id, dispatch_item))
                                self._waiting_req_ids.add(req_id)
                                logger.warning(
                                    "[oom-retry] req_id=%s hit CUDA OOM on worker-%s "
                                    "(attempt %d), worker killed, re-dispatching",
                                    req_id[:8],
                                    worker_idx,
                                    retry_count,
                                )
                                continue  # skip failure path
                            logger.error(
                                "[oom-retry] req_id=%s lost dispatch item, failing",
                                req_id[:8],
                            )
                        self._fail_result(req_id, future, err_msg, loop)
                    else:
                        loop.call_soon_threadsafe(future.set_result, item)

                    # If this request was also in the waiting list (dying worker
                    # completed before death was detected), remove it to avoid
                    # double dispatch.
                    if req_id in self._waiting_req_ids:
                        self._remove_from_waiting_list(req_id)
                        logger.info(
                            "Removed req_id=%s from waiting list (result already received)",
                            req_id[:8],
                        )
                except Exception:
                    logger.exception(
                        "Error processing result for req_id=%s; skipping",
                        item.get("req_id", "unknown")
                        if isinstance(item, dict)
                        else "unknown",
                    )

            if not got_result:
                await asyncio.sleep(0.1)
            else:
                await asyncio.sleep(0)

    async def _result_listener_loop_guarded(self) -> None:
        """Wrapper that catches BaseException so the loop never dies silently."""
        try:
            await self._result_listener_loop()
        except asyncio.CancelledError:
            if self._stopping:
                logger.info("_result_listener_loop cancelled during shutdown")
            else:
                logger.warning("_result_listener_loop cancelled unexpectedly")
            raise
        except BaseException:
            logger.critical(
                "_result_listener_loop exiting due to BaseException",
                exc_info=True,
            )
            raise

    # ------------------------------------------------------------------
    # LLMEngine protocol
    # ------------------------------------------------------------------

    async def health(self) -> bool:
        return self._ready

    async def metrics(self) -> str:
        return generate_latest(self._metrics_registry).decode("utf-8")

    async def models(self) -> List[Model]:
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
            raise ServerError(f"Model '{model_name}' is already loaded")
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
            raise ServerError(f"Unknown model: {model_name}")
        if model_name == self.default_model and len(self._loaded_models) == 1:
            raise ServerError("Cannot unload the last available model")
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
            raise ServerError("Messages is empty")
        if request.messages[0].content is None:
            raise ServerError("Message content is required")
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
        if self._stopping:
            logger.warning("Rejecting request: ViPE engine is stopping")
            raise ClientError("ViPE engine is stopping")

        if not self._profiling_success:
            if not self._profiling_started:
                self._profiling_started = True
                self._profiling_done.clear()
                try:
                    return await self._run_first_request(req)
                except asyncio.CancelledError:
                    self._profiling_started = False
                    self._profiling_success = False
                    self._profiling_done.set()
                    logger.warning("First request cancelled during profiling")
                    raise
                except Exception:
                    self._profiling_started = False
                    self._profiling_success = False
                    self._profiling_done.set()
                    logger.warning(
                        "First request failed during profiling", exc_info=True
                    )
                    raise
            # Another request is already doing profiling — start download
            # in parallel while waiting for profiling to finish
            download_task = asyncio.create_task(self._prepare_request(req))

            def _cancel_download_task() -> None:
                def _cleanup_prepared_tmpdir_from_done_task() -> None:
                    if download_task.cancelled():
                        return
                    try:
                        prepared_req_id, _, _ = download_task.result()
                    except Exception:
                        return
                    self._cleanup_request_tmpdir(prepared_req_id)

                if download_task.done():
                    _cleanup_prepared_tmpdir_from_done_task()
                    return

                download_task.cancel()

                async def _cleanup_cancelled_download_task() -> None:
                    try:
                        await download_task
                    except Exception:
                        return
                    _cleanup_prepared_tmpdir_from_done_task()

                asyncio.create_task(_cleanup_cancelled_download_task())

            try:
                await self._profiling_done.wait()
            except asyncio.CancelledError:
                _cancel_download_task()
                logger.warning("Request cancelled while waiting for profiling")
                raise
            except Exception:
                _cancel_download_task()
                logger.warning(
                    "Request failed while waiting for profiling", exc_info=True
                )
                raise
            if self._stopping:
                _cancel_download_task()
                logger.warning(
                    "Rejecting request after profiling wait: engine is stopping"
                )
                raise ClientError("ViPE engine is stopping")
            if not self._profiling_success:
                _cancel_download_task()
                return await self._dispatch_to_worker(req)
            try:
                req_id, input_path_str, output_path_str = await download_task
            except asyncio.CancelledError:
                _cancel_download_task()
                logger.warning("Download cancelled after profiling wait")
                raise
            return await self._dispatch_prepared_request(
                req, req_id, input_path_str, output_path_str
            )
        return await self._dispatch_to_worker(req)

    @retry(
        stop=stop_after_attempt(3),
        wait=wait_exponential(multiplier=1, min=1, max=10),
        retry=retry_if_exception_type(
            (
                httpx.RequestError,
                httpx.TimeoutException,
                httpx.ConnectError,
                httpx.HTTPStatusError,
            )
        ),
    )
    async def _download_video(self, video_url: str, input_path: Path) -> None:
        """Download video to input_path using async httpx."""
        import httpx

        started = time.monotonic()
        downloaded = 0
        async with httpx.AsyncClient(timeout=60, follow_redirects=True) as client:
            async with client.stream("GET", video_url) as resp:
                resp.raise_for_status()
                with input_path.open("wb") as f:
                    async for chunk in resp.aiter_bytes():
                        if not chunk:
                            continue
                        downloaded += len(chunk)
                        if downloaded > self._max_video_download_bytes:
                            raise ServerError(
                                f"'video_url' file too large, max "
                                f"{self._max_video_download_bytes} bytes"
                            )
                        f.write(chunk)
        elapsed = time.monotonic() - started
        logger.info(
            "Download complete: %d bytes (%.2f MiB), elapsed=%.3fs",
            downloaded,
            downloaded / (1 << 20),
            elapsed,
        )

    async def _upload_outputs(self, output_path: Path, upload_prefix: str) -> List[str]:
        """Upload pipeline outputs to storage using async storage client."""
        from aibrix.storage import create_storage_from_env

        started = time.monotonic()
        storage = create_storage_from_env()
        keys: List[str] = []
        base_prefix = upload_prefix.strip("/")
        loop = asyncio.get_event_loop()
        for fp in output_path.rglob("*"):
            if not fp.is_file():
                continue
            rel = fp.relative_to(output_path).as_posix()
            key = f"{base_prefix}/{rel}"
            data = await loop.run_in_executor(None, fp.read_bytes)
            await storage.put_object(key, data)
            keys.append(key)
        elapsed = time.monotonic() - started
        logger.info(
            "Upload complete: files=%d, elapsed=%.3fs",
            len(keys),
            elapsed,
        )
        return keys

    def _cleanup_request_tmpdir(self, req_id: str) -> None:
        tmpdir = self._active_tmpdirs.pop(req_id, None)
        if tmpdir is None:
            return
        try:
            tmpdir.cleanup()
        except Exception:
            logger.exception("[req-%s] Failed to cleanup tmpdir", req_id[:8])

    def _remove_from_waiting_list(self, req_id: str) -> None:
        if req_id not in self._waiting_req_ids:
            return
        self._waiting_list = [
            (rid, item) for rid, item in self._waiting_list if rid != req_id
        ]
        self._waiting_req_ids.discard(req_id)

    def _fail_request(
        self,
        req_id: str,
        error: Optional[Exception],
        *,
        remove_waiting: bool = False,
        cleanup_tmpdir: bool = False,
    ) -> None:
        future = self._pending.pop(req_id, None)
        self._pending_since.pop(req_id, None)
        self._req_worker_map.pop(req_id, None)
        self._dispatched_items.pop(req_id, None)
        self._oom_retry_counts.pop(req_id, None)
        if remove_waiting:
            self._remove_from_waiting_list(req_id)
        if cleanup_tmpdir:
            self._cleanup_request_tmpdir(req_id)
        if future is not None and not future.done():
            if error is not None:
                future.set_exception(error)
                future.exception()
            else:
                future.set_exception(ServerError("ViPE request failed"))
                future.exception()

    def _fail_result(
        self,
        req_id: str,
        future: asyncio.Future,
        err_msg: str,
        loop: asyncio.AbstractEventLoop,
    ) -> None:
        """Fail a request whose result came back with an error."""
        self._dispatched_items.pop(req_id, None)
        self._oom_retry_counts.pop(req_id, None)
        loop.call_soon_threadsafe(future.set_exception, ServerError(err_msg))

    async def _dispatch_and_await(
        self,
        req_id: str,
        req_json: str,
        input_path_str: str,
        output_path_str: str,
        worker_idx: int,
        profile: bool = False,
    ) -> Dict[str, Any]:
        """Put dispatch item on worker queue and await the result future."""
        queue = self._request_queues[worker_idx]
        loop = asyncio.get_event_loop()
        future: asyncio.Future = loop.create_future()
        self._pending[req_id] = future
        self._pending_since[req_id] = time.monotonic()
        self._req_worker_map[req_id] = worker_idx

        dispatch_item = {
            "req_id": req_id,
            "req_json": req_json,
            "input_path": input_path_str,
            "output_path": output_path_str,
            "profile": profile,
        }
        self._dispatched_items[req_id] = dispatch_item

        put_nowait = getattr(queue, "put_nowait", None)
        try:
            if callable(put_nowait):
                put_nowait(dispatch_item)
            else:
                queue.put(dispatch_item, timeout=self._queue_put_timeout_seconds)
        except Exception as exc:
            dispatch_error = ServerError(
                f"Failed to dispatch request to worker-{worker_idx}"
            )
            self._fail_request(req_id, dispatch_error, cleanup_tmpdir=True)
            raise dispatch_error from exc
        logger.info(
            "Dispatched req_id=%s to worker-%d (profile=%s)",
            req_id[:8],
            worker_idx,
            profile,
        )

        try:
            return await asyncio.shield(future)
        except asyncio.CancelledError:
            logger.warning(
                "Request %s cancelled while awaiting worker-%d result",
                req_id[:8],
                worker_idx,
            )
            self._fail_request(
                req_id,
                ServerError("ViPE request was cancelled"),
                remove_waiting=True,
                cleanup_tmpdir=True,
            )
            raise
        except Exception as exc:
            logger.warning(
                "Request %s failed on worker-%d: %s: %s",
                req_id[:8],
                worker_idx,
                type(exc).__name__,
                exc,
            )
            wrapped_error = ServerError(
                f"ViPE request failed on worker-{worker_idx}: {type(exc).__name__}: {exc}"
            )
            self._fail_request(req_id, wrapped_error, cleanup_tmpdir=True)
            raise wrapped_error from exc

    async def _prepare_request(self, req: ViPERequest) -> tuple[str, str, str]:
        """Download video, extract frames if input_fps set, and prepare temp dirs.

        Returns (req_id, input_path_str, output_path_str).
        When input_fps is specified, ffmpeg extracts frames and the input path
        points to the frame directory instead of the raw video.
        """
        req_id = uuid.uuid4().hex
        tmpdir = tempfile.TemporaryDirectory(prefix=f"aibrix_vipe_{req_id[:8]}_")
        tmp_path = Path(tmpdir.name)
        input_path = tmp_path / "input.mp4"
        output_path = tmp_path / "output"
        output_path.mkdir(parents=True, exist_ok=True)

        # Keep tmpdir alive until upload finishes
        self._active_tmpdirs[req_id] = tmpdir

        try:
            video_url = req.input.video_url
            logger.info("[req-%s] Downloading video from %s", req_id[:8], video_url)
            await self._download_video(video_url, input_path)

            input_fps = req.parameters.input_fps
            if input_fps is not None:
                frame_dir = tmp_path / "frames"
                frame_dir.mkdir(parents=True, exist_ok=True)
                logger.info(
                    "[req-%s] Extracting frames at %.1f fps", req_id[:8], input_fps
                )
                t0 = time.monotonic()
                proc = await asyncio.create_subprocess_exec(
                    "ffmpeg",
                    "-nostdin",
                    "-y",
                    "-hide_banner",
                    "-loglevel",
                    "error",
                    "-i",
                    str(input_path),
                    "-vf",
                    f"fps={input_fps:g}",
                    "-q:v",
                    "2",
                    str(frame_dir / "frame_%06d.jpg"),
                    stdout=asyncio.subprocess.DEVNULL,
                    stderr=asyncio.subprocess.PIPE,
                )
                _, stderr = await proc.communicate()
                elapsed = time.monotonic() - t0
                if proc.returncode != 0:
                    err_text = stderr.decode(errors="replace").strip()
                    raise RuntimeError(
                        f"ffmpeg failed (rc={proc.returncode}): {err_text}"
                    )
                logger.info(
                    "[req-%s] Frame extraction done in %.1fs", req_id[:8], elapsed
                )
                return req_id, str(frame_dir), str(output_path)

            return req_id, str(input_path), str(output_path)
        except BaseException:
            self._cleanup_request_tmpdir(req_id)
            raise

    async def _finalize_request(
        self, req: ViPERequest, req_id: str, worker_result: Dict[str, Any]
    ) -> tuple[ViPEResponse, Dict[str, Any]]:
        """Upload outputs, clean up temp dir, and return response."""
        custom_id = req.custom_id
        subdir = req.output.subdir
        upload_prefix = f"{subdir}/{custom_id}"

        try:
            output_path = Path(worker_result["output_path"])
            uploaded = await self._upload_outputs(output_path, upload_prefix)
            logger.info("[req-%s] Uploaded %d artifacts", req_id[:8], len(uploaded))

            result = _build_result_dict(subdir, upload_prefix, uploaded)
            if "profile" in worker_result:
                result["profile"] = worker_result["profile"]
            return _result_to_response(result), result
        finally:
            self._cleanup_request_tmpdir(req_id)

    async def _run_first_request(self, req: ViPERequest) -> ViPEResponse:
        """Download, dispatch to worker-0 with profiling, upload, then spawn more workers."""
        req_id, input_path_str, output_path_str = await self._prepare_request(req)
        if self._stopping:
            self._cleanup_request_tmpdir(req_id)
            logger.warning("First request rejected: engine is stopping")
            raise ClientError("ViPE engine is stopping")
        logger.info("Dispatching first request to worker-0 with GPU profiling")

        req_json = req.model_dump_json(exclude_unset=True)
        worker_result = await self._dispatch_and_await(
            req_id,
            req_json,
            input_path_str,
            output_path_str,
            worker_idx=0,
            profile=True,
        )

        if "error" in worker_result:
            self._cleanup_request_tmpdir(req_id)
            logger.warning("First request worker error: %s", worker_result["error"])
            raise ServerError(worker_result["error"])
        resp, result = await self._finalize_request(req, req_id, worker_result)

        # Extract profiling data to decide worker count
        profile = result.pop("profile", None)
        if profile is not None:
            peak_delta = profile.get("peak_delta_bytes", 0)
            per_pipeline_bytes = int(peak_delta * PER_PIPELINE_MEM_SAFETY_FACTOR)
            if per_pipeline_bytes <= 0:
                per_pipeline_bytes = max(peak_delta * 2, 12 * (1 << 30))
            gpu_total = profile.get("gpu_total_bytes", 0)
            gpu_budget = int(gpu_total * self._gpu_memory_utilization)
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
        else:
            target_workers = 1

        # Spawn additional workers (worker-0 already exists)
        for i in range(1, target_workers):
            self._spawn_worker(worker_id=i)

        self._num_workers = target_workers
        self._active_workers.set(target_workers)
        self._profiling_started = False
        self._profiling_success = True
        self._profiling_done.set()

        return resp

    async def _dispatch_to_worker(self, req: ViPERequest) -> ViPEResponse:
        """Download, round-robin dispatch to an available worker, upload."""
        req_id, input_path_str, output_path_str = await self._prepare_request(req)
        return await self._dispatch_prepared_request(
            req, req_id, input_path_str, output_path_str
        )

    async def _dispatch_prepared_request(
        self, req: ViPERequest, req_id: str, input_path_str: str, output_path_str: str
    ) -> ViPEResponse:
        """Dispatch a prepared request (video already downloaded) to an available worker."""
        if self._stopping:
            self._cleanup_request_tmpdir(req_id)
            logger.warning(
                "Rejecting prepared request %s: engine is stopping", req_id[:8]
            )
            raise ClientError("ViPE engine is stopping")

        # Find next available worker (skip workers in backoff)
        worker_idx: Optional[int] = None
        for i in range(self._num_workers):
            idx = (self._rr_index + i) % self._num_workers
            if self._is_worker_available(idx):
                worker_idx = idx
                self._rr_index = idx + 1
                break
        if worker_idx is None:
            self._cleanup_request_tmpdir(req_id)
            logger.warning(
                "No available workers for request %s (all in backoff or dead)",
                req_id[:8],
            )
            raise ServerError("No available workers (all in backoff or dead)")

        try:
            self._validate_pipeline_type(req)
        except Exception:
            self._cleanup_request_tmpdir(req_id)
            raise

        req_json = req.model_dump_json(exclude_unset=True)
        worker_result = await self._dispatch_and_await(
            req_id,
            req_json,
            input_path_str,
            output_path_str,
            worker_idx,
        )

        if "error" in worker_result:
            self._cleanup_request_tmpdir(req_id)
            logger.warning(
                "Worker-%d error for req %s: %s",
                worker_idx,
                req_id[:8],
                worker_result["error"],
            )
            raise ServerError(worker_result["error"])

        resp, _ = await self._finalize_request(req, req_id, worker_result)
        return resp

    @staticmethod
    def _is_oom_error(err_msg: str) -> bool:
        return "OutOfMemoryError" in err_msg or "CUDA out of memory" in err_msg

    def _kill_oom_worker(self, worker_idx: int) -> None:
        """Terminate a worker that hit CUDA OOM and mark it for respawn.

        After termination, _check_worker_health will detect the dead
        worker on the next cycle and respawn it with a fresh GPU context.
        """
        if worker_idx >= len(self._workers):
            return
        worker = self._workers[worker_idx]
        if not worker.is_alive():
            return

        logger.warning(
            "Killing worker-%d (pid=%d) after CUDA OOM to reset GPU state",
            worker_idx,
            worker.pid or -1,
        )

        worker.terminate()
        worker.join(timeout=5)
        if worker.is_alive():
            logger.warning("Worker-%d didn't terminate after OOM, killing", worker_idx)
            worker.kill()
            worker.join(timeout=5)

        if worker_idx < len(self._result_queues):
            self._dead_result_queue_indices.add(worker_idx)

    def _validate_pipeline_type(self, req: ViPERequest) -> None:
        if req.parameters.pipeline != self.pipeline_type:
            raise ServerError(
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
            raise ServerError(f"Unknown model: {model_name}")

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

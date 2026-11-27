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

from __future__ import annotations

import asyncio
import json
import logging
import math
import os
import stat
import tempfile
import time
import uuid
from collections.abc import AsyncIterable, Mapping
from copy import deepcopy
from dataclasses import dataclass, field
from enum import Enum
from pathlib import Path
from typing import (
    Any,
    AsyncIterator,
    Awaitable,
    Callable,
    Dict,
    Iterator,
    List,
    Literal,
    Optional,
    Tuple,
    Type,
    Union,
)
from urllib.parse import quote, unquote, urlencode, urlparse

import anyio
import httpx
from fastapi.requests import Request
from fastapi.responses import JSONResponse, Response
from prometheus_client import (
    CollectorRegistry,
    Counter,
    Gauge,
    Histogram,
    generate_latest,
)
from pydantic import BaseModel
from starlette.requests import ClientDisconnect
from starlette.websockets import WebSocket

from aibrix.openai_frontend.common.artifact_fetcher import (
    ArtifactFetcher,
    ArtifactFetchError,
    DefaultArtifactFetcher,
    FetchResult,
)
from aibrix.openai_frontend.engine.engine import LLMEngine
from aibrix.openai_frontend.frontend.fastapi.responses import ClosingStreamingResponse
from aibrix.openai_frontend.proxy.media.request_inspector import (
    MediaURLInspector,
    RequestInspector,
    SglangGenerateInspector,
    URLLocation,
)
from aibrix.openai_frontend.proxy.media.request_rewriter import (
    RequestRewriter,
    _set_at_path,
)
from aibrix.openai_frontend.proxy.output.output_writer import (
    OutputWriteError,
    OutputWriter,
    SourceIdentities,
)
from aibrix.openai_frontend.proxy.output.path_ref import (
    PathOwnership,
    PathRef,
    PathRefs,
)
from aibrix.openai_frontend.proxy.processors import (
    DEFAULT_PROCESSOR_TIMEOUT_SECONDS,
    ProcessorExecutor,
    ProcessorRegistry,
    ProcessorState,
)
from aibrix.openai_frontend.proxy.runtime.engine_spec import EngineSpec, ForwardKind
from aibrix.openai_frontend.proxy.runtime.subprocess_engine import (
    DEFAULT_SHUTDOWN_GRACE_SECONDS,
    DEFAULT_STARTUP_TIMEOUT_SECONDS,
    SubprocessEngine,
)
from aibrix.openai_frontend.proxy.runtime.video_job import (
    VideoJobContext,
    VideoOutputJob,
)
from aibrix.openai_frontend.proxy.runtime.video_sync import VideoSyncJob
from aibrix.openai_frontend.proxy.runtime.ws_bridge import (
    bridge_websocket,
    deny_websocket,
    forwardable_headers,
    offered_subprotocols,
)
from aibrix.openai_frontend.schemas.engine_common import (
    NativeRequestOptions,
    split_native_result,
)
from aibrix.openai_frontend.schemas.generation import (
    VIDEO_TERMINAL_STATUSES,
    ImageGenerationRequest,
    ImageGenerationResponse,
    VideoGenerationRequest,
    VideoResponse,
    VideoStatus,
    output_media_paths,
)
from aibrix.openai_frontend.schemas.openai import (
    CreateChatCompletionRequest,
    CreateChatCompletionResponse,
    CreateChatCompletionStreamResponse,
    CreateCompletionRequest,
    CreateCompletionResponse,
    CreateEmbeddingRequest,
    CreateEmbeddingResponse,
    ListModelsResponse,
    Model,
)
from aibrix.openai_frontend.schemas.output import OutputRequest, OutputResponse
from aibrix.openai_frontend.schemas.processors import ProcessorPipeline
from aibrix.openai_frontend.schemas.responses import (
    RESPONSE_STREAM_TRUE_VALUES,
    CompactedResponse,
    InputTokenCountResponse,
    ResponseDeleted,
    ResponseInputItemList,
    ResponseStreamEvent,
)
from aibrix.openai_frontend.schemas.responses import (
    Response as ResponsesResponse,
)
from aibrix.openai_frontend.utils.utils import (
    ClientError,
    EngineUnavailableError,
    ServerError,
    make_prefix_formatter,
)
from aibrix.runtime.downloaders import await_thread_completion


@dataclass(frozen=True)
class _ResponseOperation:
    """One Responses route: how to reach it and what its body parses to."""

    method: str
    path: str
    response_model: Optional[Type[Any]]
    chunk_model: Optional[Type[Any]]
    needs_id: bool = False


_RESPONSE_OPERATIONS = {
    "create": _ResponseOperation(
        "POST", "/v1/responses", ResponsesResponse, ResponseStreamEvent
    ),
    "retrieve": _ResponseOperation(
        "GET",
        "/v1/responses/{response_id}",
        ResponsesResponse,
        ResponseStreamEvent,
        needs_id=True,
    ),
    "delete": _ResponseOperation(
        "DELETE", "/v1/responses/{response_id}", ResponseDeleted, None, needs_id=True
    ),
    "cancel": _ResponseOperation(
        "POST",
        "/v1/responses/{response_id}/cancel",
        ResponsesResponse,
        None,
        needs_id=True,
    ),
    "input_items": _ResponseOperation(
        "GET",
        "/v1/responses/{response_id}/input_items",
        ResponseInputItemList,
        None,
        needs_id=True,
    ),
    "compact": _ResponseOperation(
        "POST", "/v1/responses/compact", CompactedResponse, None
    ),
    "input_tokens": _ResponseOperation(
        "POST", "/v1/responses/input_tokens", InputTokenCountResponse, None
    ),
}
# Result arrays persisted verbatim, then emptied in the HTTP response.
_OPENAI_RESULT_KEYS = ("choices", "data")


HEALTH_CHECK_INTERVAL_SECONDS = 10.0
WATCHDOG_STUCK_THRESHOLD_SECONDS = 300.0
HEARTBEAT_INTERVAL_CYCLES = 6  # ~60s at 10s interval
# Retries for connection-level failures on the forward path (the request
# never reached the engine, so retrying is safe even for non-idempotent POSTs).
FORWARD_RETRY_ATTEMPTS = 3
FORWARD_RETRY_BACKOFF_SECONDS = 0.5
DEFAULT_VIDEO_OUTPUT_TIMEOUT_SECONDS = 30 * 60.0
VIDEO_CONTENT_RESPONSE_HEADERS = frozenset(
    {
        "content-type",
        "content-encoding",
        "content-range",
        "accept-ranges",
        "content-disposition",
        "etag",
        "last-modified",
        "cache-control",
    }
)


logger = logging.getLogger(__name__)


class EngineState(str, Enum):
    """Lifecycle of the inner engine as seen by the proxy."""

    STOPPED = "stopped"
    STARTING = "starting"
    READY = "ready"
    # The inner engine exited: the proxy does not respawn it. It stays up
    # serving failures so the orchestrator sees /health fail and replaces
    # the pod.
    FAILED = "failed"
    STOPPING = "stopping"


HOP_BY_HOP_HEADERS = frozenset(
    {
        "connection",
        "keep-alive",
        "proxy-authenticate",
        "proxy-authorization",
        "te",
        "trailer",
        "transfer-encoding",
        "upgrade",
    }
)

_STATE_CODES = {state: i for i, state in enumerate(EngineState)}


class _ClosingStream(AsyncIterator[Any]):
    """Own the upstream even if iteration never starts or stops at a yield."""

    def __init__(
        self, iterator: AsyncIterator[Any], close: Callable[[], Awaitable[None]]
    ) -> None:
        self._iterator = iterator
        self._close = close

    def __aiter__(self) -> AsyncIterator[Any]:
        return self

    async def __anext__(self) -> Any:
        return await anext(self._iterator)

    async def aclose(self) -> None:
        try:
            close_iterator = getattr(self._iterator, "aclose", None)
            if close_iterator is not None:
                await close_iterator()
        finally:
            await self._close()


@dataclass(frozen=True)
class _RetainedVideoInputs:
    video_id: Optional[str]
    results: List[FetchResult]
    processor_dir: Optional[tempfile.TemporaryDirectory] = None


@dataclass(frozen=True)
class _RetainedVideoSources:
    output: OutputRequest
    identities: SourceIdentities


class _RetainedMedia:
    """Media the proxy still owns after the request that fetched it has ended.

    Each population answers a different question about when it may be released:

    * ``video_inputs``: inputs of a submitted video POST. A terminal status GET
      proves the job is done; without one they live until engine stop.
    * ``video_sources``: declared output sources of a job the engine never gave
      an ID for, so no terminal GET can exist. They live until engine stop.
    * ``response_inputs``: inputs of a stored Responses turn that a later
      ``previous_response_id`` may read back. They live until engine stop.

    ``detach_*`` hands one entry over at a time, so a release cancelled midway
    leaves the rest retained for the next trigger. Do not replace it with a
    bulk clear: the unconsumed entries would never be released.
    """

    def __init__(self) -> None:
        self.video_inputs: Dict[str, _RetainedVideoInputs] = {}
        self.video_sources: Dict[str, _RetainedVideoSources] = {}
        self.response_inputs: Dict[str, List[FetchResult]] = {}
        self.processor_dirs: Dict[str, tempfile.TemporaryDirectory] = {}

    def pending(self) -> bool:
        """Whether any retained media is still waiting for a release trigger."""
        return bool(
            self.video_inputs
            or self.video_sources
            or self.response_inputs
            or self.processor_dirs
        )

    def retain_video_inputs(self, req_id: str, entry: _RetainedVideoInputs) -> None:
        self.video_inputs[req_id] = entry

    def retain_video_sources(self, req_id: str, entry: _RetainedVideoSources) -> None:
        self.video_sources[req_id] = entry

    def retain_response_inputs(self, req_id: str, results: List[FetchResult]) -> None:
        self.response_inputs[req_id] = results

    def retain_processor_dir(
        self, req_id: str, directory: tempfile.TemporaryDirectory
    ) -> None:
        self.processor_dirs[req_id] = directory

    def detach_processor_dirs(
        self,
    ) -> Iterator[Tuple[str, tempfile.TemporaryDirectory]]:
        for req_id in list(self.processor_dirs):
            directory = self.processor_dirs.pop(req_id, None)
            if directory is not None:
                yield req_id, directory

    def detach_video_inputs(
        self, video_id: Optional[str]
    ) -> Iterator[Tuple[str, _RetainedVideoInputs]]:
        """Hand each matching input to the caller; ``None`` matches all of them."""
        for req_id, entry in list(self.video_inputs.items()):
            if video_id is not None and entry.video_id != video_id:
                continue
            retained = self.video_inputs.pop(req_id, None)
            if retained is not None:
                yield req_id, retained

    def detach_video_sources(self) -> Iterator[Tuple[str, _RetainedVideoSources]]:
        for req_id in list(self.video_sources):
            entry = self.video_sources.pop(req_id, None)
            if entry is not None:
                yield req_id, entry

    def detach_response_inputs(self) -> Iterator[Tuple[str, List[FetchResult]]]:
        for req_id in list(self.response_inputs):
            results = self.response_inputs.pop(req_id, None)
            if results is not None:
                yield req_id, results


@dataclass
class _ProxyRequestContext:
    req_id: str
    started_at: float
    output: Optional[OutputRequest]
    inputs: List[FetchResult] = field(default_factory=list)
    sources: PathRefs = field(default_factory=PathRefs)
    processors: Optional[ProcessorPipeline] = None
    processor_dir: Optional[tempfile.TemporaryDirectory] = None
    submitted: bool = False
    response_submitted: bool = False
    video: Optional[VideoResponse] = None
    owner: Literal["request", "stream", "video"] = "request"

    def submission_state(self, submitted: bool) -> None:
        self.submitted = submitted

    def response_submission_state(self, submitted: bool) -> None:
        self.response_submitted = submitted
        self.sources.retain_until_stop(submitted)

    def transfer_video(self) -> VideoJobContext:
        assert self.output is not None
        context = VideoJobContext(
            self.req_id,
            self.started_at,
            self.output,
            self.inputs,
            self.sources,
            processors=self.processors,
            processor_dir=self.processor_dir,
        )
        self.inputs, self.sources = [], PathRefs()
        self.processor_dir = None
        self.owner = "video"
        return context


class _VideoJobAdapter:
    def __init__(self, engine: ProxyEngine, context: VideoJobContext) -> None:
        self.engine, self.context = engine, context
        self.output_timeout = engine._video_output_timeout
        self.writers_stopped = engine._video_writers_stopped

    def available(self) -> bool:
        return self.engine._ready and not self.engine._stopping

    def check_available(self) -> None:
        self.engine._check_available()

    def engine_lost_error(self) -> Exception:
        return self.engine._engine_lost_error(self.context.req_id, "video")

    async def poll(self, video_id: str, *, cleanup: bool = False) -> httpx.Response:
        endpoint = f"/v1/videos/{quote(video_id, safe='')}"
        if cleanup:
            return await self.engine._forward_with_retry(endpoint, False, method="GET")
        return await self.engine._forward(
            endpoint,
            stream=False,
            payload={},
            req_id=self.context.req_id,
            kind="video",
            method="GET",
        )

    def parse_response(
        self, raw: httpx.Response
    ) -> tuple[VideoResponse, Dict[str, Any]]:
        self.engine._raise_engine_status(raw)
        envelope = self.engine._engine_json(raw)
        return self.engine._parse_engine_envelope(
            VideoResponse, envelope, "video"
        ), envelope

    def observe(self, raw: httpx.Response, video_id: str) -> Optional[VideoResponse]:
        envelope = self.engine._engine_json(raw)
        observed = VideoResponse.status_only(envelope, video_id)
        if observed is not None:
            self.engine._record_output_sources(
                envelope, VideoResponse, self.context.sources
            )
        return observed

    def cache(self, video_id: str, response: Optional[VideoResponse]) -> None:
        self.engine._uploaded_videos[video_id] = response

    async def persist(
        self, raw: httpx.Response, response: VideoResponse, envelope: Dict[str, Any]
    ) -> Any:
        return await self.engine._persist_output(
            self.context.req_id,
            self.context.output,
            raw,
            response,
            self.context.sources,
            envelope=envelope,
            processors=self.context.processors,
            processor_dir=self.context.processor_dir,
        )

    def register_task(
        self, video_id: str, task: asyncio.Task[Any], *, cleanup_only: bool = False
    ) -> None:
        engine, req_id = self.engine, self.context.req_id
        engine._video_tasks[video_id] = task
        if cleanup_only:
            engine._in_flight_tasks.pop(req_id, None)
        else:
            engine._in_flight_tasks[req_id] = task

    def is_registered(self, video_id: str, task: asyncio.Task[Any]) -> bool:
        return self.engine._video_tasks.get(video_id) is task

    async def finish(self, video_id: str, task: Optional[asyncio.Task[Any]]) -> None:
        engine, context = self.engine, self.context
        try:
            cached = engine._uploaded_videos.get(video_id)
            artifacts = (
                cached.output.artifacts
                if cached is not None and cached.output is not None
                else []
            )
            try:
                await engine._output_writer.cleanup_completed_sources(
                    context.sources, context.output, artifacts
                )
            except Exception:
                logger.exception("Video job source cleanup failed")
        finally:
            try:
                await engine._release(context.req_id, context.inputs)
            finally:
                if context.processor_dir is not None:
                    engine._cleanup_processor_dir(context.processor_dir)
                if engine._video_tasks.get(video_id) is task:
                    engine._video_tasks.pop(video_id, None)
                engine._requests_end(context.req_id, context.started_at)


class ProxyEngine(LLMEngine):
    """LLMEngine that supervises one OpenAI-compatible server subprocess.

    The proxy deliberately does **not** restart a crashed engine; an
    orchestrator (e.g. Kubernetes) is expected to own that decision.  When
    the engine exits the proxy:

    * records a forensic exit description and latches ``FAILED``;
    * fails in-flight requests immediately with 503 instead of letting them
      block on a socket that will never answer;
    * tears the whole process tree down SIGTERM -> grace -> SIGKILL,
      tracked by (pid, create-time) identity, so no GPU-holding worker the
      engine forked survives it;
    * keeps ``/health`` unhealthy, so the pod is replaced.
    """

    def __init__(
        self,
        engine_cmd: List[str],
        engine_port: int = 8001,
        engine_host: str = "127.0.0.1",
        engine_env: Optional[Dict[str, str]] = None,
        spec: Optional[EngineSpec] = None,
        artifact_fetcher: Optional[ArtifactFetcher] = None,
        credentials_map: Optional[Dict[str, Dict]] = None,
        chat_inspector: Optional[RequestInspector] = None,
        completion_inspector: Optional[RequestInspector] = None,
        embedding_inspector: Optional[RequestInspector] = None,
        media_dir: Optional[str] = None,
        fetch_media: Optional[bool] = None,
        materialize_inline_media: bool = False,
        output_writer: Optional[OutputWriter] = None,
        startup_timeout: Optional[float] = DEFAULT_STARTUP_TIMEOUT_SECONDS,
        shutdown_grace_seconds: float = DEFAULT_SHUTDOWN_GRACE_SECONDS,
        health_check_interval: float = HEALTH_CHECK_INTERVAL_SECONDS,
        video_output_timeout: float = DEFAULT_VIDEO_OUTPUT_TIMEOUT_SECONDS,
        processor_registry: Optional[ProcessorRegistry] = None,
        enable_processors: bool = False,
        processor_timeout_seconds: float = DEFAULT_PROCESSOR_TIMEOUT_SECONDS,
    ):
        if not math.isfinite(video_output_timeout) or video_output_timeout <= 0:
            raise ValueError("video_output_timeout must be a finite positive number")
        self._spec = spec or EngineSpec(name="default")
        self._processor_registry = processor_registry or ProcessorRegistry()
        self._enable_processors = enable_processors
        self._processor_executor = ProcessorExecutor(
            self._processor_registry, processor_timeout_seconds
        )

        # Remote media is fetched by the proxy only when the engine can read
        # local files; otherwise URLs pass through and the engine fetches them.
        self._fetch_media = (
            self._spec.supports_local_media if fetch_media is None else fetch_media
        )
        # Inline data:;base64 media is only useful when the engine consumes
        # local files, so it implies fetching.
        if materialize_inline_media:
            self._fetch_media = True
        self._owned_media_dir: Optional[tempfile.TemporaryDirectory] = None
        if (self._fetch_media or enable_processors) and media_dir is None:
            # Pre requests may localize media even when ordinary fetching is off.
            self._owned_media_dir = tempfile.TemporaryDirectory(prefix="aibrix_media_")
            media_dir = self._owned_media_dir.name
        self._media_dir = os.path.realpath(media_dir) if media_dir else None

        self._subprocess = SubprocessEngine(
            cmd=[*engine_cmd, *self._spec.spawn_args(self._media_dir)],
            port=engine_port,
            host=engine_host,
            env=engine_env,
            spec=self._spec,
            shutdown_grace_seconds=shutdown_grace_seconds,
        )

        if artifact_fetcher is None:
            artifact_fetcher = DefaultArtifactFetcher(base_temp_dir=self._media_dir)
        self._fetcher = artifact_fetcher
        media_inspector = MediaURLInspector(
            materialize_inline=materialize_inline_media,
        )
        self._media_inspector = media_inspector
        self._chat_inspector = chat_inspector or media_inspector
        self._completion_inspector = completion_inspector or media_inspector
        self._embedding_inspector = embedding_inspector or media_inspector
        self._credentials_map = credentials_map or {}
        self._generate_inspector = SglangGenerateInspector(
            materialize_inline=materialize_inline_media,
        )
        self._output_writer = output_writer or OutputWriter()
        # None marks an output-managed job until its first poll response.
        self._uploaded_videos: Dict[str, Optional[VideoResponse]] = {}
        self._video_tasks: Dict[str, asyncio.Task[Any]] = {}
        self._retained = _RetainedMedia()
        self._video_writers_stopped = asyncio.Event()

        self._startup_timeout = startup_timeout
        self._video_output_timeout = video_output_timeout
        self._health_check_interval = health_check_interval

        self._state = EngineState.STOPPED
        self._stopping: bool = False
        self._close_task: Optional[asyncio.Task[None]] = None
        self._last_exit: Optional[str] = None
        self._failure_reason: Optional[str] = None

        # Add [proxy] prefix to all logger output via formatter
        for h in logging.getLogger().handlers:
            h.setFormatter(make_prefix_formatter("[proxy]"))

        # Request tracking: req_id -> start timestamp
        self._in_flight: Dict[str, float] = {}
        # req_id -> task serving it, so a dead engine fails them immediately
        # instead of leaving them blocked on a socket that will never answer.
        self._in_flight_tasks: Dict[str, asyncio.Task] = {}
        # Wait for request resource ownership, not the entire caller/ASGI task.
        self._request_done: Dict[str, asyncio.Future[None]] = {}
        self._stream_closers: Dict[str, Callable[[], Awaitable[None]]] = {}

        # Background supervisor that detects inner-engine exits.
        self._health_monitor_task: Optional[asyncio.Task] = None

        # Metrics
        self._metrics_registry = CollectorRegistry()
        self._fetch_total = Counter(
            "proxy_fetch_total",
            "Total artifact fetch attempts",
            registry=self._metrics_registry,
        )
        self._fetch_bytes = Counter(
            "proxy_fetch_bytes_total",
            "Cumulative successfully fetched payload bytes (downloads and decoded inline media)",
            registry=self._metrics_registry,
        )
        self._fetch_errors = Counter(
            "proxy_fetch_errors",
            "Total artifact fetch errors",
            registry=self._metrics_registry,
        )
        self._fetch_seconds = Counter(
            "proxy_fetch_seconds",
            "Cumulative seconds spent fetching artifacts",
            registry=self._metrics_registry,
        )
        self._outputs_total = Counter(
            "proxy_output_writes",
            "Output write attempts by destination scheme and result",
            ["scheme", "result"],
            registry=self._metrics_registry,
        )
        self._output_bytes = Counter(
            "proxy_output_bytes_total",
            "Cumulative successfully persisted response payload bytes",
            ["scheme"],
            registry=self._metrics_registry,
        )
        self._output_seconds = Counter(
            "proxy_output_seconds",
            "Cumulative seconds spent writing outputs, including failures and cancellation",
            ["scheme", "result"],
            registry=self._metrics_registry,
        )
        self._requests_total = Counter(
            "proxy_requests_total",
            "Total requests served",
            registry=self._metrics_registry,
        )
        self._request_duration_seconds = Histogram(
            "proxy_request_duration_seconds",
            "Request duration in seconds",
            buckets=(1, 5, 10, 30, 60, 120, 300, 600),
            registry=self._metrics_registry,
        )
        self._in_flight_gauge = Gauge(
            "proxy_in_flight_requests",
            "Number of in-flight requests",
            registry=self._metrics_registry,
        )
        self._engine_state_gauge = Gauge(
            "proxy_engine_state",
            "Inner engine state: "
            + ", ".join(
                f"{code}={state.value}" for state, code in _STATE_CODES.items()
            ),
            registry=self._metrics_registry,
        )
        self._engine_state_gauge.set(_STATE_CODES[self._state])

    # ------------------------------------------------------------------
    # State
    # ------------------------------------------------------------------

    @property
    def state(self) -> EngineState:
        return self._state

    @property
    def _ready(self) -> bool:
        return self._state == EngineState.READY

    def _set_state(self, state: EngineState) -> None:
        if state != self._state:
            logger.info("Inner engine state %s -> %s", self._state.value, state.value)
        self._state = state
        self._engine_state_gauge.set(_STATE_CODES[state])

    def _unavailable_error(self) -> Exception:
        if self._stopping:
            return ClientError("Proxy engine is stopping")
        if self._state == EngineState.FAILED:
            return EngineUnavailableError(
                f"Inner engine failed permanently: {self._failure_reason}"
            )
        return EngineUnavailableError(
            f"Inner engine is {self._state.value}"
            + (f" (last exit: {self._last_exit})" if self._last_exit else "")
        )

    def _check_available(self) -> None:
        if self._stopping:
            raise ClientError("Proxy engine is stopping")
        if self._state != EngineState.READY:
            raise self._unavailable_error()

    # ------------------------------------------------------------------
    # Lifecycle
    # ------------------------------------------------------------------

    async def start(self) -> None:
        if self._close_task is not None:
            raise RuntimeError("Proxy engine is closed")
        self._stopping = False
        self._failure_reason = None
        self._set_state(EngineState.STARTING)
        try:
            await self._bring_up()
        except BaseException:
            # Never leave a half-started tree behind a failed start.
            self._fail_permanently("engine startup failed")
            await asyncio.to_thread(self._subprocess.shutdown, 5.0)
            await self._subprocess.close_client()
            raise
        self._set_state(EngineState.READY)
        await self._ensure_health_monitor()
        logger.info("ProxyEngine started; inner engine is ready")

    async def _bring_up(self) -> None:
        # spawn() can block briefly on the port check; keep it off the loop.
        await asyncio.to_thread(self._subprocess.spawn)
        await self._subprocess.wait_ready(timeout=self._startup_timeout)
        await self._subprocess.start_client()

    async def aclose(self) -> None:
        """Stop the engine once and wait for process and HTTP pool cleanup."""
        if self._close_task is None:
            self._close_task = asyncio.create_task(self._close_resources())
        await asyncio.shield(self._close_task)

    async def _close_resources(self) -> None:
        self._stopping = True
        self._set_state(EngineState.STOPPING)

        monitor, self._health_monitor_task = self._health_monitor_task, None
        if monitor is not None and not monitor.done():
            monitor.cancel()
        if monitor is not None:
            await asyncio.gather(monitor, return_exceptions=True)

        for req_id, start in list(self._in_flight.items()):
            logger.warning(
                "Failing in-flight req_id=%s during shutdown (pending %.1fs)",
                req_id,
                time.monotonic() - start,
            )
        pending = list(self._request_done.values())
        closers = list(self._stream_closers.values())
        self._cancel_in_flight("proxy engine stopping")
        try:
            if self._video_tasks or self._retained.pending():
                # Stop generation before releasing retained input media or
                # taking the final source snapshot. Uploads still own their readers.
                await asyncio.to_thread(self._subprocess.shutdown)
                self._video_writers_stopped.set()
            # Close even streams handed off before iteration; do not wait for
            # entire serving tasks, which may themselves be awaiting aclose().
            await asyncio.gather(
                *(close() for close in closers), return_exceptions=True
            )
            await asyncio.gather(*pending)
            if self._retained.pending() and not self._video_writers_stopped.is_set():
                # Initial POST cancellation may retain inputs while pending
                # requests drain, after the first shutdown snapshot above.
                await asyncio.to_thread(self._subprocess.shutdown)
                self._video_writers_stopped.set()
            await self._release_retained_resources()
        finally:
            # Writers and request cleanup have settled before deleting paths.
            try:
                cleanup_all = getattr(self._fetcher, "cleanup_all", None)
                if callable(cleanup_all):
                    cleanup_all()
            except Exception:
                logger.warning("artifact fetcher cleanup_all failed", exc_info=True)
            if self._owned_media_dir is not None:
                try:
                    self._owned_media_dir.cleanup()
                except Exception:
                    logger.warning("owned media dir cleanup failed", exc_info=True)
                self._owned_media_dir = None
            try:
                if not self._video_writers_stopped.is_set():
                    await asyncio.to_thread(self._subprocess.shutdown)
                    self._video_writers_stopped.set()
            finally:
                await self._subprocess.close_client()
        self._set_state(EngineState.STOPPED)
        logger.info("ProxyEngine stopped")

    # ------------------------------------------------------------------
    # Supervisor
    # ------------------------------------------------------------------

    async def _ensure_health_monitor(self) -> None:
        if self._health_monitor_task is not None:
            return
        self._health_monitor_task = asyncio.create_task(
            self._health_monitor_loop_guarded()
        )
        self._health_monitor_task.add_done_callback(self._on_health_monitor_done)

    def _on_health_monitor_done(self, task: asyncio.Task) -> None:
        # If the supervisor ever dies on its own, do not resurrect it: an exit
        # has been missed and silently continuing traffic is worse than a
        # failed /health that gets the pod replaced.
        if self._health_monitor_task is task:
            self._health_monitor_task = None
        if self._stopping:
            return
        if task.cancelled():
            logger.warning("health monitor cancelled unexpectedly")
            return
        exc = task.exception()
        self._failure_reason = (
            f"health monitor crashed: {exc}"
            if exc is not None
            else "health monitor exited"
        )
        self._set_state(EngineState.FAILED)
        logger.critical("health monitor is gone; proxy latched FAILED", exc_info=exc)

    async def _health_monitor_loop(self) -> None:
        cycle = 0
        while not self._stopping:
            cycle += 1
            await self._supervise_once()
            if self._stopping:
                return

            # Watchdog: log in-flight requests stuck > threshold
            now = time.monotonic()
            for req_id, start in list(self._in_flight.items()):
                elapsed = now - start
                if elapsed > WATCHDOG_STUCK_THRESHOLD_SECONDS:
                    logger.warning(
                        "[watchdog] req_id=%s has been pending %.0fs",
                        req_id,
                        elapsed,
                    )

            # Heartbeat logging
            if cycle % HEARTBEAT_INTERVAL_CYCLES == 0:
                snap = self._subprocess.snapshot
                logger.info(
                    "[heartbeat] state=%s subprocess_pid=%s alive=%s tree=%s "
                    "rss=%s in_flight=%d",
                    self._state.value,
                    self._subprocess.pid,
                    self._subprocess.is_alive(),
                    snap.pids if snap else None,
                    f"{snap.rss_bytes / 2**20:.0f}MiB"
                    if snap and snap.rss_bytes
                    else None,
                    len(self._in_flight),
                )

            await asyncio.sleep(self._health_check_interval)

    async def _supervise_once(self) -> None:
        """One supervision step: detect an inner-engine exit and fail over.

        Health of a live engine is not probed here: :meth:`health` forwards
        its own /health for the orchestrator's readiness/liveness checks.
        """
        if self._state != EngineState.READY:
            return
        if self._subprocess.is_alive():
            # Keep the identity snapshot fresh while the root is alive, so
            # workers orphaned by a later crash can still be found.
            await asyncio.to_thread(self._subprocess.refresh_snapshot)
            return

        await self._handle_engine_exit()

    async def _handle_engine_exit(self) -> None:
        """Latch FAILED and tear everything down after the engine died.

        The proxy never respawns the engine; the orchestrator owns restarts.
        """
        self._last_exit = self._subprocess.describe_exit()
        logger.error("Inner engine exited unexpectedly (%s)", self._last_exit)
        self._fail_permanently(f"engine exited: {self._last_exit}")
        self._cancel_in_flight(f"inner engine exited: {self._last_exit}")
        # The root is dead, but workers it forked into their own sessions
        # (vLLM engine cores) may still hold the GPU. Kill them now; a later
        # aclose() has nothing left to reap.
        survivors = await asyncio.to_thread(self._subprocess.kill_leftovers, 5.0)
        if survivors:
            logger.error(
                "Leftover inner engine processes survived teardown: %s", survivors
            )
        self._video_writers_stopped.set()
        await self._release_retained_resources()
        await self._subprocess.close_client()

    def _fail_permanently(self, reason: str) -> None:
        self._failure_reason = reason
        self._set_state(EngineState.FAILED)
        self._cancel_in_flight(f"inner engine failed: {reason}")
        logger.critical(
            "Inner engine marked FAILED: %s. /health stays unhealthy until the "
            "pod is replaced; the proxy does not restart the engine.",
            reason,
        )

    def _cancel_in_flight(self, reason: str) -> None:
        current = asyncio.current_task() if self._has_running_loop() else None
        for req_id, task in list(self._in_flight_tasks.items()):
            if task is current or task.done():
                continue
            logger.warning("[req-%s] failing in-flight request: %s", req_id, reason)
            task.cancel(msg=reason)

    @staticmethod
    def _has_running_loop() -> bool:
        try:
            asyncio.get_running_loop()
            return True
        except RuntimeError:
            return False

    async def _health_monitor_loop_guarded(self) -> None:
        try:
            await self._health_monitor_loop()
        except asyncio.CancelledError:
            if not self._stopping:
                logger.warning("_health_monitor_loop cancelled unexpectedly")
            raise
        except BaseException:
            logger.critical(
                "_health_monitor_loop exiting due to BaseException",
                exc_info=True,
            )
            raise

    # ------------------------------------------------------------------
    # Request tracking
    # ------------------------------------------------------------------

    def _track_request(self) -> str:
        req_id = uuid.uuid4().hex[:8]
        self._in_flight[req_id] = time.monotonic()
        self._request_done[req_id] = asyncio.get_running_loop().create_future()
        task = asyncio.current_task()
        if task is not None:
            self._in_flight_tasks[req_id] = task
        self._in_flight_gauge.set(len(self._in_flight))
        return req_id

    def _untrack_request(self, req_id: str) -> None:
        self._in_flight.pop(req_id, None)
        self._in_flight_tasks.pop(req_id, None)
        self._stream_closers.pop(req_id, None)
        done = self._request_done.pop(req_id, None)
        if done is not None and not done.done():
            done.set_result(None)
        self._in_flight_gauge.set(len(self._in_flight))

    def _engine_lost_error(self, req_id: str, kind: str) -> Exception:
        """Map a cancellation caused by an engine failure to an HTTP error."""
        logger.warning(
            "[req-%s] %s request aborted: inner engine went away", req_id, kind
        )
        return self._unavailable_error()

    @property
    def endpoint_kinds(self) -> Mapping[str, ForwardKind]:
        return self._spec.endpoints.kinds()

    def endpoint_kind(self, path: str) -> Optional[ForwardKind]:
        return self._spec.kind(path)

    def endpoint_methods(self, path: str) -> tuple[str, ...]:
        return self._spec.methods(path)

    async def forward_json(
        self, path: str, body: Dict[str, Any], *, query: str = "", method: str = "POST"
    ) -> Response:
        """Forward declared native JSON/SSE using the shared ownership pipeline."""
        if self._spec.require(path) in (ForwardKind.MULTIPART, ForwardKind.WEBSOCKET):
            raise ClientError(f"Endpoint '{path}' is not a JSON endpoint")
        if method not in self.endpoint_methods(path):
            raise ClientError(f"Endpoint '{path}' does not allow {method}")
        if not isinstance(body, dict):
            raise ClientError("Request body must be a JSON object")
        try:
            stream = bool(NativeRequestOptions.model_validate(body).stream)
        except ValueError as exc:
            raise ClientError(f"Invalid 'stream': {exc}") from exc
        inspector = (
            self._generate_inspector if path == "/generate" else self._media_inspector
        )
        return await self._proxy_request(
            self._kind_label(path),
            f"{path}?{query}" if query else path,
            body,
            inspector,
            method=method,
            stream_override=stream,
            result_schema=self._spec.native_response_schema(path),
        )

    async def forward_bodiless(
        self, method: str, path: str, raw_request: "Request"
    ) -> Response:
        """Forward a declared metadata GET, preserving its original query string."""
        self._spec.require(path)
        endpoint = f"{path}?{raw_request.url.query}" if raw_request.url.query else path
        return await self._proxy_request(
            self._kind_label(path),
            endpoint,
            None,
            self._media_inspector,
            method=method,
        )

    async def forward_multipart(self, path: str, request: "Request") -> Response:
        """Proxy a multipart/form upload (audio transcription or video generation).

        Both directions are streamed: the client's body is relayed to the
        engine chunk by chunk as it arrives (never buffered or re-encoded, so
        every form field and the query string survive byte-for-byte), and the
        engine's response -- JSON, SSE, or binary media -- is relayed back the
        same way. vLLM-Omni sync video uses an internal async job so disconnect
        can abort generation. Peak memory per upload is one transport chunk.
        """
        kind = self._spec.require(path)
        if kind != ForwardKind.MULTIPART:
            raise ClientError(f"Endpoint '{path}' is not a multipart endpoint")
        content_type = request.headers.get("content-type", "")
        if not content_type.lower().startswith("multipart/"):
            raise ClientError(
                f"Endpoint '{path}' requires multipart/form-data, got "
                f"{content_type or 'no content-type'}"
            )
        self._check_available()

        label = self._kind_label(path)
        req_id, t0 = self._requests_begin()
        streamed_body = _StreamedBody(request)
        video_job = (
            VideoSyncJob(self._forward_with_retry, streamed_body.upload_done)
            if self._spec.name == "vllm-omni" and path == "/v1/videos/sync"
            else None
        )

        headers: Dict[str, str] = {}
        if (length := request.headers.get("content-length")) is not None:
            # Keep the client's framing: no chunked re-encoding when the
            # length is known up front.
            headers["Content-Length"] = length
        url = path
        if request.url.query:
            url = f"{path}?{request.url.query}"
        raw_resp: Optional[httpx.Response] = None
        scope = anyio.CancelScope()

        async def cancel_on_disconnect() -> None:
            # The upload must finish before a second coroutine reads receive().
            await streamed_body.upload_done.wait()
            while True:
                message = await request.receive()
                if message["type"] == "http.disconnect":
                    scope.cancel()
                    return

        disconnect_task = asyncio.create_task(cancel_on_disconnect())
        try:
            try:
                with scope:
                    kwargs: Dict[str, Any] = dict(
                        method="POST",
                        content=streamed_body,
                        content_type=content_type,
                        headers=headers,
                        replayable=lambda: not streamed_body.started,
                    )
                    raw_resp = (
                        await video_job.run(
                            query=f"?{request.url.query}" if request.url.query else "",
                            **kwargs,
                        )
                        if video_job is not None
                        else await self._forward_with_retry(url, True, **kwargs)
                    )
                if scope.cancel_called or raw_resp is None:
                    raise ClientDisconnect()
            except ClientDisconnect:
                raise ClientError(
                    f"Client disconnected after {streamed_body.bytes_sent} bytes "
                    "of upload"
                ) from None
            except asyncio.CancelledError:
                if self._stopping or self._state != EngineState.READY:
                    raise self._engine_lost_error(req_id, label) from None
                raise
            except (httpx.ConnectError, httpx.TimeoutException) as exc:
                raise ServerError(
                    f"Inner engine unreachable: {type(exc).__name__}"
                ) from exc
            except (httpx.RemoteProtocolError, httpx.WriteError) as exc:
                # The engine dropped the connection mid-upload.
                raise ServerError(
                    "Inner engine closed the connection during upload: "
                    f"{type(exc).__name__}"
                ) from exc
            finally:
                disconnect_task.cancel()
                await await_thread_completion(
                    asyncio.gather(disconnect_task, return_exceptions=True)
                )

            logger.info(
                "[req-%s] %s upload relayed (%d bytes, %d chunks)",
                req_id,
                label,
                streamed_body.bytes_sent,
                streamed_body.chunks,
            )
            self._raise_engine_status(raw_resp)
            # The response is handed to the ASGI server: the in-flight
            # accounting now belongs to the relayed stream, not this call.
            return ClosingStreamingResponse(
                self._hand_off_stream(
                    _ProxyRequestContext(req_id, t0, None),
                    raw_resp,
                    None,
                    label,
                    on_close=video_job.aclose if video_job is not None else None,
                ),
                status_code=raw_resp.status_code,
                media_type=raw_resp.headers.get("content-type", "application/json"),
                headers=self._forward_headers(raw_resp, encoded=True),
            )
        except BaseException:
            try:
                if raw_resp is not None:
                    await await_thread_completion(raw_resp.aclose())
            finally:
                try:
                    if video_job is not None:
                        await await_thread_completion(video_job.aclose())
                finally:
                    self._requests_end(req_id, t0)
            raise

    async def forward_websocket(self, path: str, websocket: "WebSocket") -> None:
        """Bridge a client WebSocket to the engine's WebSocket (realtime).

        The session counts as an in-flight request, so proxy shutdown cancels
        it with 1012 instead of leaving it attached to a torn-down upstream.
        An engine-side exit is relayed by the bridge with the engine's own
        close code (1011 when it died abruptly).
        """
        if self._spec.kind(path) != ForwardKind.WEBSOCKET:
            await deny_websocket(
                websocket, 404, b'{"detail":"Not a WebSocket endpoint"}'
            )
            return
        if not self._ready:
            await deny_websocket(
                websocket,
                503,
                json.dumps({"detail": str(self._unavailable_error())}).encode(),
            )
            return

        url = httpx.URL(self._subprocess.base_url).copy_with(
            scheme="ws", path=path, query=websocket.url.query.encode() or None
        )
        req_id, t0 = self._requests_begin()
        try:
            await bridge_websocket(
                websocket,
                str(url),
                headers=forwardable_headers(websocket),
                subprotocols=offered_subprotocols(websocket),
            )
        except asyncio.CancelledError:
            logger.warning("[req-%s] realtime session aborted", req_id)
            raise
        finally:
            self._requests_end(req_id, t0)

    @staticmethod
    def _kind_label(path: str) -> str:
        return (
            path.strip("/").replace("/", "_").replace("{", "").replace("}", "") or "api"
        )

    @staticmethod
    def _forward_headers(
        resp: httpx.Response, *, encoded: bool = False
    ) -> Dict[str, str]:
        skip = HOP_BY_HOP_HEADERS | {"content-length"}
        skip |= {
            part.strip().lower()
            for part in resp.headers.get("connection", "").split(",")
        }
        if not encoded or resp.is_stream_consumed:
            skip |= {"content-encoding"}
        return {k: v for k, v in resp.headers.items() if k.lower() not in skip}

    # ------------------------------------------------------------------
    # LLMEngine protocol
    # ------------------------------------------------------------------

    async def health(self) -> bool:
        if not self._ready:
            return False
        try:
            return await self._subprocess.forward_health()
        except Exception:
            return False

    async def metrics(self) -> str:
        try:
            inner_metrics = await self._subprocess.forward_metrics()
            inner_metrics += "\n"
        except Exception:
            inner_metrics = ""
        return inner_metrics + generate_latest(self._metrics_registry).decode("utf-8")

    async def models(self) -> List[Model]:
        self._check_available()
        try:
            raw_resp = await self._subprocess.forward_models()
        except (httpx.ConnectError, httpx.TimeoutException) as exc:
            raise ServerError(
                f"Inner engine unreachable: {type(exc).__name__}"
            ) from exc
        return ListModelsResponse.model_validate_json(raw_resp.content).data

    async def chat(
        self, request: CreateChatCompletionRequest
    ) -> Union[CreateChatCompletionResponse, AsyncIterator[str], Response]:
        self._spec.require_chat()
        return await self._proxy_request(
            "chat",
            self._spec.chat_endpoint,
            request,
            self._chat_inspector,
            response_model=CreateChatCompletionResponse,
            chunk_model=CreateChatCompletionStreamResponse,
        )

    async def completion(
        self, request: CreateCompletionRequest
    ) -> Union[CreateCompletionResponse, AsyncIterator[str], Response]:
        self._spec.require_completion()
        return await self._proxy_request(
            "completion",
            self._spec.completion_endpoint,
            request,
            self._completion_inspector,
            response_model=CreateCompletionResponse,
            chunk_model=CreateCompletionResponse,
        )

    async def embedding(
        self, request: CreateEmbeddingRequest
    ) -> Union[CreateEmbeddingResponse, Response]:
        self._spec.require_embedding()
        return await self._proxy_request(
            "embedding",
            self._spec.embedding_endpoint,
            request,
            self._embedding_inspector,
            response_model=CreateEmbeddingResponse,
        )

    async def image(self, request: ImageGenerationRequest) -> Response:
        return await self._proxy_request(
            "image",
            "/v1/images/generations",
            request,
            self._media_inspector,
            response_model=ImageGenerationResponse,
        )

    async def video(
        self, request: VideoGenerationRequest
    ) -> Union[VideoResponse, Response]:
        return await self._proxy_request(
            "video",
            "/v1/videos",
            request,
            self._media_inspector,
            response_model=VideoResponse,
        )

    async def retrieve_video(self, video_id: str) -> Union[VideoResponse, Response]:
        if video_id in self._uploaded_videos:
            cached = self._uploaded_videos[video_id]
            return (
                cached.model_copy(deep=True)
                if cached is not None
                else VideoResponse.pending(video_id)
            )
        self._check_available()
        req_id, t0 = self._requests_begin()
        try:
            raw = await self._forward(
                f"/v1/videos/{quote(video_id, safe='')}",
                stream=False,
                payload={},
                req_id=req_id,
                kind="video",
                method="GET",
            )
            status = VideoResponse.status_only(self._engine_json(raw), video_id)
            if status is not None and status.status in VIDEO_TERMINAL_STATUSES:
                await self._release_retained_resources(video_id)
            self._raise_engine_status(raw)
            return Response(
                raw.content,
                status_code=raw.status_code,
                media_type=raw.headers.get("content-type"),
            )
        finally:
            self._requests_end(req_id, t0)

    async def _release_video_inputs(self, video_id: Optional[str] = None) -> None:
        """Release input-only ownership after terminal GET, or all inputs at stop."""
        for req_id, entry in self._retained.detach_video_inputs(video_id):
            try:
                await self._release(req_id, entry.results)
            finally:
                if entry.processor_dir is not None:
                    self._cleanup_processor_dir(entry.processor_dir)

    async def _release_unknown_video_sources(self) -> None:
        """Unknown job IDs have no terminal GET; only engine stop permits cleanup."""
        for req_id, entry in self._retained.detach_video_sources():
            try:
                await self._output_writer.cleanup_completed_sources(
                    entry.identities, entry.output
                )
            except Exception:
                logger.exception(
                    "[req-%s] unknown video job source cleanup failed", req_id
                )

    async def _release_retained_resources(self, video_id: Optional[str] = None) -> None:
        """Coordinate retained resources with visible completion, not caller tasks.

        Register before any ownership is detached or cleanup awaits, so concurrent
        shutdown can wait even when nothing is retained any more.
        A specific ID releases only inputs; output sources require engine stop.
        """

        async def release() -> None:
            if video_id is None:
                await self._release_unknown_video_sources()
                for req_id, inputs in self._retained.detach_response_inputs():
                    await self._release(req_id, inputs)
                for _, directory in self._retained.detach_processor_dirs():
                    self._cleanup_processor_dir(directory)
            await self._release_video_inputs(video_id)

        cleanup_id = uuid.uuid4().hex
        done = asyncio.get_running_loop().create_future()
        self._request_done[cleanup_id] = done
        try:
            await await_thread_completion(release())
        finally:
            self._request_done.pop(cleanup_id, None)
            done.set_result(None)

    def _record_output_sources(
        self,
        body: Any,
        model: Type[Any],
        sources: SourceIdentities,
    ) -> None:
        """Record declared paths independently of response metadata validation."""
        try:
            self._output_writer.remember_sources(
                output_media_paths(body, model), sources
            )
        except Exception:
            logger.warning("Could not inspect all output sources", exc_info=True)

    async def video_content(
        self, video_id: str, query: str = "", headers: Optional[Dict[str, str]] = None
    ) -> Response:
        if video_id in self._uploaded_videos:
            raise NotImplementedError(
                "Downloading output-managed videos is unsupported; use the uploaded output location"
            )
        self._check_available()
        endpoint = f"/v1/videos/{quote(video_id, safe='')}/content"
        if query:
            endpoint += f"?{query}"
        req_id, t0 = self._requests_begin()
        try:
            # Read transfer-decoded but content-encoded bytes. Range coordinates
            # and Content-Encoding describe exactly this representation.
            raw = await self._forward_with_retry(
                endpoint, False, method="GET", headers=headers, raw_body=True
            )
            return Response(
                raw.content,
                status_code=raw.status_code,
                headers={
                    key: value
                    for key, value in raw.headers.items()
                    if key in VIDEO_CONTENT_RESPONSE_HEADERS
                },
            )
        except asyncio.CancelledError:
            if self._stopping or self._state != EngineState.READY:
                raise self._engine_lost_error(req_id, "video content") from None
            raise
        finally:
            self._requests_end(req_id, t0)

    async def responses(
        self,
        operation: str,
        request: BaseModel | None = None,
        *,
        response_id: str | None = None,
        query: list[tuple[str, str]] | None = None,
    ) -> BaseModel | AsyncIterator[str] | None:
        op = _RESPONSE_OPERATIONS.get(operation)
        if op is None:
            raise ValueError(f"Unknown Responses operation: {operation}")
        endpoint = op.path
        if op.needs_id:
            if not response_id:
                raise ClientError("response_id must not be empty")
            endpoint = endpoint.replace("{response_id}", quote(response_id, safe=""))
        if query:
            endpoint += "?" + urlencode(query)
        stream = (
            operation == "retrieve"
            and dict(query or []).get("stream", "").lower()
            in RESPONSE_STREAM_TRUE_VALUES
        )
        return await self._proxy_request(
            "responses",
            endpoint,
            request,
            self._chat_inspector,
            response_model=op.response_model,
            chunk_model=op.chunk_model,
            method=op.method,
            stream_override=stream if operation == "retrieve" else None,
        )

    # ------------------------------------------------------------------
    # Shared forward pipeline
    # ------------------------------------------------------------------

    async def _proxy_request(
        self,
        kind: str,
        endpoint: str,
        request: Any,
        inspector: RequestInspector,
        *,
        response_model: Optional[Type[Any]] = None,
        chunk_model: Optional[Type[Any]] = None,
        method: str = "POST",
        stream_override: Optional[bool] = None,
        result_schema: Any = None,
    ) -> Any:
        """Forward, then transfer ownership to a stream/job or finish locally."""
        self._check_available()
        payload = (
            dict(request)
            if isinstance(request, dict)
            else request.model_dump(exclude_unset=True, mode="json", by_alias=True)
            if request is not None
            else {}
        )
        raw_output = payload.pop("output", None)
        output = None
        if raw_output is not None:
            try:
                output = OutputRequest.model_validate(raw_output)
            except ValueError as exc:
                raise ClientError(f"Invalid 'output': {exc}") from exc
        requested_stream = (
            bool(payload.get("stream")) if stream_override is None else stream_override
        )
        stream = requested_stream and (
            chunk_model is not None or response_model is None
        )
        if output is not None and requested_stream:
            raise ClientError("'output' cannot be combined with 'stream': true")
        if (
            output is not None
            and response_model in (ResponsesResponse, CompactedResponse)
            and payload.get("background")
        ):
            raise ClientError("'output' cannot be combined with 'background': true")
        processors = self._parse_processors(
            payload.pop("processors", None),
            stream=requested_stream,
            has_output=output is not None,
            background=bool(payload.get("background")),
            response_model=response_model,
        )
        req_id, t0 = self._requests_begin()
        context = _ProxyRequestContext(
            req_id,
            t0,
            output,
            processors=processors,
        )
        try:
            context.processor_dir = self._new_processor_dir(req_id, processors)
            raw, envelope = await self._forward_request(
                context,
                kind,
                endpoint,
                request,
                response_model,
                inspector,
                payload,
                stream=stream,
                method=method,
            )
            if method == "DELETE" and raw.status_code == 204:
                return None
            if stream:
                iterator = self._hand_off_stream(context, raw, chunk_model, kind)
                if response_model is None:
                    return ClosingStreamingResponse(
                        iterator,
                        status_code=raw.status_code,
                        headers=self._forward_headers(raw, encoded=True),
                    )
                return iterator
            if context.output is not None:
                return await self._persist_response(
                    context,
                    raw,
                    envelope,
                    response_model,
                    kind,
                    result_schema=result_schema,
                )
            if response_model is None:
                return Response(
                    raw.content,
                    status_code=raw.status_code,
                    media_type=raw.headers.get("content-type", "application/json")
                    if method == "GET"
                    else None,
                    headers=self._forward_headers(raw),
                )
            if response_model in (VideoResponse, ImageGenerationResponse):
                return Response(
                    raw.content,
                    status_code=raw.status_code,
                    media_type=raw.headers.get("content-type"),
                )
            return self._parse_engine_body(response_model, raw.content, kind)
        except BaseException as exc:
            if (
                context.owner == "request"
                and context.output is not None
                and context.video is not None
                and context.video.status not in VIDEO_TERMINAL_STATUSES
            ):
                video = context.video
                self._uploaded_videos[video.id] = video.cache_view(
                    VideoStatus.FAILED.value,
                    {"message": str(exc) or "Video request cancelled"},
                )
                job = self._new_video_job(context, video)
                job.schedule_cleanup(video, recover_before_start=True)
            raise
        finally:
            await self._finish_request(context)

    @staticmethod
    def _retains_response_inputs(
        request: Any,
        response_model: Optional[Type[Any]],
        endpoint: str,
        payload: Dict[str, Any],
    ) -> bool:
        """Whether the engine keeps the inputs: saved history may read them back."""
        return (
            request is not None
            and response_model is ResponsesResponse
            and endpoint.split("?", 1)[0] == "/v1/responses"
            and (payload.get("store") is not False or bool(payload.get("background")))
        )

    @staticmethod
    def _submission_tracker(
        context: _ProxyRequestContext, *, video: bool, retained: bool
    ) -> Optional[Callable[[bool], None]]:
        """Report whether the engine accepted the request, for retention decisions."""
        if video:
            return context.submission_state
        if retained:
            return context.response_submission_state
        return None

    async def _forward_request(
        self,
        context: _ProxyRequestContext,
        kind: str,
        endpoint: str,
        request: Any,
        response_model: Optional[Type[Any]],
        inspector: RequestInspector,
        payload: Dict[str, Any],
        *,
        stream: bool,
        method: str,
    ) -> tuple[httpx.Response, Any]:
        has_pre = context.processors is not None and bool(context.processors.pre)
        locations = None
        if has_pre:
            if isinstance(inspector, MediaURLInspector):
                inspector = inspector.with_inline_media().with_local_media()
            locations = inspector.extract_urls(payload)
        elif context.output is not None:
            locations = inspector.extract_urls(payload)
        if context.output is not None:
            local_inspector = (
                SglangGenerateInspector()
                if isinstance(inspector, SglangGenerateInspector)
                else MediaURLInspector()
            ).with_local_media()
            self._record_external_inputs(
                context, [*(locations or []), *local_inspector.extract_urls(payload)]
            )
        if (self._fetch_media or has_pre) and request is not None:
            if locations is None:
                payload, context.inputs = await self._fetch_media_for(
                    context.req_id, payload, inspector
                )
            else:
                payload, context.inputs = await self._fetch_media_for(
                    context.req_id,
                    payload,
                    inspector,
                    locations=locations,
                    skip_local_media=has_pre,
                )
        if has_pre:
            payload = await self._apply_pre_processors(context, payload, locations)
        is_video = isinstance(request, VideoGenerationRequest)
        retains_response = self._retains_response_inputs(
            request, response_model, endpoint, payload
        )
        video_timeout = None
        if context.output is not None and is_video:
            video_timeout = (
                context.started_at + self._video_output_timeout - time.monotonic()
            )
            if video_timeout <= 0:
                raise ServerError("Video output generation timed out")
        forward = self._forward(
            endpoint,
            stream=stream,
            payload=payload,
            req_id=context.req_id,
            kind=kind,
            method=method,
            on_submission=self._submission_tracker(
                context, video=is_video, retained=retains_response
            ),
        )
        if video_timeout is not None:
            forward_task = asyncio.create_task(forward)
            try:
                raw = await asyncio.wait_for(
                    asyncio.shield(forward_task), video_timeout
                )
            except asyncio.TimeoutError as exc:
                raise ServerError("Video output generation timed out") from exc
            finally:
                # The POST may still be closing after timeout or repeated cancel.
                if not forward_task.done():
                    forward_task.cancel()
                await await_thread_completion(
                    asyncio.gather(forward_task, return_exceptions=True)
                )
        else:
            raw = await forward
        if context.response_submitted and 400 <= raw.status_code < 500:
            # A rejected create never owned the inputs; unknown transport/server
            # outcomes may have started a background job and must retain them.
            context.response_submission_state(False)
        envelope = (
            self._engine_json(raw)
            if context.output is not None or context.submitted
            else None
        )
        if context.submitted:
            context.video = VideoResponse.status_only(envelope)
            # No-ID client rejection proves no job; server errors do not.
            if 400 <= raw.status_code < 500 and context.video is None:
                context.submitted = False
        if context.output is not None and response_model in (
            VideoResponse,
            ImageGenerationResponse,
        ):
            self._record_output_sources(envelope, response_model, context.sources)
        self._raise_engine_status(raw)
        return raw, envelope

    def _hand_off_stream(
        self,
        context: _ProxyRequestContext,
        raw: httpx.Response,
        chunk_model: Optional[Type[Any]],
        kind: str,
        *,
        on_close: Optional[Callable[[], Awaitable[None]]] = None,
    ) -> AsyncIterator[Any]:
        close_task: Optional[asyncio.Task[None]] = None
        read_task: Optional[asyncio.Task[bytes]] = None
        read_scope: Optional[anyio.CancelScope] = None
        self._retain_response_inputs(context)
        inputs, context.inputs = context.inputs, []

        async def close_response() -> None:
            try:
                if read_task is not None:
                    if not read_task.done():
                        assert read_scope is not None
                        # Cancel through AnyIO so httpcore's shielded error
                        # cleanup finishes; direct task.cancel() bypasses it.
                        read_scope.cancel()
                    await asyncio.gather(read_task, return_exceptions=True)
                await raw.aclose()
            finally:
                try:
                    if on_close is not None:
                        await on_close()
                finally:
                    try:
                        await self._release(context.req_id, inputs)
                    finally:
                        self._requests_end(context.req_id, context.started_at)

        async def close_stream() -> None:
            nonlocal close_task
            if close_task is None:
                close_task = asyncio.create_task(close_response())
            await asyncio.shield(close_task)

        async def raw_events() -> AsyncIterator[bytes]:
            nonlocal read_task, read_scope
            task = asyncio.current_task()
            if task is not None:
                self._in_flight_tasks[context.req_id] = task
            try:
                # Buffered HTTPX responses are already content-decoded.
                if raw.is_stream_consumed:
                    yield raw.content
                else:
                    # Keep transport auto-close on read errors inside a shielded
                    # task. Read only on demand, preserving downstream backpressure.
                    assert isinstance(raw.stream, httpx.AsyncByteStream)
                    stream = raw.stream.__aiter__()

                    async def read_chunk() -> bytes:
                        assert read_scope is not None
                        with read_scope:
                            return await anext(stream)
                        raise asyncio.CancelledError()

                    while True:
                        read_scope = anyio.CancelScope()
                        read_task = asyncio.create_task(read_chunk())
                        try:
                            data = await asyncio.shield(read_task)
                        except StopAsyncIteration:
                            break
                        yield data
            finally:
                await close_stream()

        if chunk_model is None:
            iterator: AsyncIterator[Any] = raw_events()
        else:

            class DecodingStream(httpx.AsyncByteStream):
                def __aiter__(self) -> AsyncIterator[bytes]:
                    return raw_events()

            # HTTPX decodes content/text/lines above the protected byte stream.
            # Its EOF close is a no-op; close_stream owns the real response.
            decoded = httpx.Response(
                raw.status_code,
                headers=self._forward_headers(raw, encoded=True),
                stream=DecodingStream(),
            )
            decoded.encoding = raw.encoding
            iterator = self._typed_stream_events(
                decoded, chunk_model, context.req_id, kind, close_stream
            )
        self._stream_closers[context.req_id] = close_stream
        self._in_flight_tasks.pop(context.req_id, None)
        context.owner = "stream"
        return _ClosingStream(iterator, close_stream)

    def _new_video_job(
        self, context: _ProxyRequestContext, response: VideoResponse
    ) -> VideoOutputJob:
        resources = context.transfer_video()
        return VideoOutputJob(
            _VideoJobAdapter(self, resources),
            resources,
            response,
            resources_done=self._request_done[context.req_id],
        )

    async def _persist_response(
        self,
        context: _ProxyRequestContext,
        raw: httpx.Response,
        envelope: Any,
        response_model: Optional[Type[Any]],
        kind: str,
        *,
        result_schema: Any = None,
    ) -> Any:
        assert context.output is not None
        parsed = (
            self._parse_engine_envelope(response_model, envelope, kind)
            if response_model is not None
            else None
        )
        if isinstance(parsed, VideoResponse):
            return await self._new_video_job(context, parsed).run(raw, envelope)
        return await self._persist_output(
            context.req_id,
            context.output,
            raw,
            parsed,
            context.sources,
            envelope=envelope,
            result_keys=("output",)
            if response_model in (ResponsesResponse, CompactedResponse)
            else _OPENAI_RESULT_KEYS,
            result_schema=result_schema,
            processors=context.processors,
            processor_dir=context.processor_dir,
        )

    def _retain_response_inputs(self, context: _ProxyRequestContext) -> None:
        if context.response_submitted and context.inputs:
            self._retained.retain_response_inputs(context.req_id, context.inputs)
            context.inputs = []

    async def _finish_request(self, context: _ProxyRequestContext) -> None:
        try:
            await self._finish_request_body(context)
        finally:
            self._close_processor_dir(context)

    async def _finish_request_body(self, context: _ProxyRequestContext) -> None:
        self._retain_response_inputs(context)
        if context.owner != "request":
            return
        if context.submitted and (
            context.video is None or context.video.status not in VIDEO_TERMINAL_STATUSES
        ):
            # Non-output jobs await terminal metadata GET. Unknown IDs await stop.
            if context.inputs or context.processor_dir is not None:
                self._retained.retain_video_inputs(
                    context.req_id,
                    _RetainedVideoInputs(
                        context.video.id
                        if context.video is not None and context.output is None
                        else None,
                        context.inputs,
                        context.processor_dir,
                    ),
                )
                context.inputs = []
                context.processor_dir = None
            if context.sources and context.output is not None:
                self._retained.retain_video_sources(
                    context.req_id,
                    _RetainedVideoSources(context.output, context.sources),
                )
                context.sources = PathRefs()
        try:
            if (
                context.submitted or context.response_submitted
            ) and self._video_writers_stopped.is_set():
                # Cancelled creates can retain inputs after shutdown's snapshot.
                await self._release_retained_resources()
        finally:
            await self._finalize_request(
                context.req_id,
                context.started_at,
                context.output,
                context.sources,
                context.inputs,
            )

    async def _finalize_request(
        self,
        req_id: str,
        t0: float,
        spec: Optional[OutputRequest],
        sources: SourceIdentities,
        fetch_results: List[FetchResult],
    ) -> None:
        if not sources and not fetch_results:
            self._requests_end(req_id, t0)
            return

        async def finish() -> None:
            try:
                if spec is not None and sources:
                    try:
                        await self._output_writer.cleanup_sources(sources, spec)
                    except Exception:
                        logger.exception(
                            "Source cleanup failed while relaying engine error"
                        )
            finally:
                try:
                    await self._release(req_id, fetch_results)
                finally:
                    self._requests_end(req_id, t0)

        await await_thread_completion(finish())

    async def _forward(
        self,
        endpoint: str,
        *,
        stream: bool,
        payload: Dict[str, Any],
        req_id: str,
        kind: str,
        method: str = "POST",
        on_submission: Optional[Callable[[bool], None]] = None,
    ) -> httpx.Response:
        try:
            return await self._forward_with_retry(
                endpoint,
                stream,
                method=method,
                payload=payload if method not in ("GET", "DELETE") else None,
                on_submission=on_submission,
            )
        except asyncio.CancelledError:
            if self._stopping or self._state != EngineState.READY:
                raise self._engine_lost_error(req_id, kind) from None
            logger.warning("[req-%s] %s request cancelled", req_id, kind)
            raise
        except (httpx.ConnectError, httpx.TimeoutException) as exc:
            logger.warning(
                "[req-%s] Inner engine unreachable: %s", req_id, type(exc).__name__
            )
            raise ServerError(
                f"Inner engine unreachable: {type(exc).__name__}"
            ) from exc

    def _requests_begin(self) -> "tuple[str, float]":
        self._requests_total.inc()
        return self._track_request(), time.monotonic()

    def _requests_end(self, req_id: str, t0: float) -> None:
        self._untrack_request(req_id)
        elapsed = time.monotonic() - t0
        self._request_duration_seconds.observe(elapsed)
        logger.info("[req-%s] completed in %.3fs", req_id, elapsed)

    async def _fetch_media_for(
        self,
        req_id: str,
        request_dict: Dict[str, Any],
        inspector: RequestInspector,
        *,
        locations: Optional[List[URLLocation]] = None,
        skip_local_media: bool = True,
    ) -> "tuple[Dict[str, Any], List[FetchResult]]":
        rewriter = RequestRewriter(inspector, self._fetcher, self._credentials_map)
        fetch_t0 = time.monotonic()
        try:
            if locations is None:
                rewritten, results = await rewriter.rewrite(request_dict)
            else:
                rewritten, results = await rewriter.rewrite(
                    request_dict, locations=locations, skip_local_media=skip_local_media
                )
        except ArtifactFetchError as exc:
            self._fetch_errors.inc()
            logger.warning("[req-%s] %s", req_id, exc)
            # A bad/unreachable media URL is the client's problem, not ours.
            raise ClientError(str(exc)) from exc
        if results:
            self._fetch_total.inc(len(results))
            self._fetch_bytes.inc(sum(result.size_bytes for result in results))
            self._fetch_seconds.inc(time.monotonic() - fetch_t0)
            logger.info("[req-%s] fetched %d media file(s)", req_id, len(results))
        return rewritten, results

    async def _release(self, req_id: str, fetch_results: List[FetchResult]) -> None:
        try:
            if fetch_results:
                await self._fetcher.cleanup(fetch_results)
        except Exception:
            logger.exception("[req-%s] failed to clean up fetched media", req_id)

    # ------------------------------------------------------------------
    # Processor stages
    # ------------------------------------------------------------------

    def _parse_processors(
        self,
        raw: Any,
        *,
        stream: bool,
        has_output: bool,
        background: bool,
        response_model: Optional[Type[Any]],
    ) -> Optional[ProcessorPipeline]:
        """Validate the control-plane extension before any forwarding."""
        if raw is None:
            return None
        if not self._enable_processors:
            raise ClientError("'processors' is not enabled on this server")
        try:
            pipeline = ProcessorPipeline.model_validate(raw)
        except ValueError as exc:
            raise ClientError(f"Invalid 'processors': {exc}") from exc
        if stream:
            raise ClientError("'processors' cannot be combined with 'stream': true")
        if background:
            raise ClientError("'processors' cannot be combined with 'background': true")
        if pipeline.post and not has_output:
            raise ClientError("'processors.post' requires 'output'")
        if response_model is None:
            raise ClientError(
                "'processors' is not supported on engine-native endpoints"
            )
        try:
            self._processor_registry.validate(pipeline)
        except ValueError as exc:
            raise ClientError(f"Invalid 'processors': {exc}") from exc
        return pipeline

    def _new_processor_dir(
        self, req_id: str, pipeline: Optional[ProcessorPipeline]
    ) -> Optional[tempfile.TemporaryDirectory]:
        if pipeline is None:
            return None
        return tempfile.TemporaryDirectory(
            prefix=f"aibrix-proc-{req_id}-", dir=self._media_dir
        )

    def _close_processor_dir(self, context: "_ProxyRequestContext") -> None:
        directory, context.processor_dir = context.processor_dir, None
        if directory is None:
            return
        if context.response_submitted and not self._video_writers_stopped.is_set():
            # Stored Responses inputs live until this engine generation stops.
            self._retained.retain_processor_dir(context.req_id, directory)
            return
        self._cleanup_processor_dir(directory)

    @staticmethod
    def _cleanup_processor_dir(directory: tempfile.TemporaryDirectory) -> None:
        try:
            directory.cleanup()
        except Exception:
            logger.warning("failed to clean up processor workdir", exc_info=True)

    @staticmethod
    def _local_processor_path(value: str) -> Optional[Path]:
        if value.startswith("file://"):
            return Path(unquote(urlparse(value).path))
        if value.startswith("/"):
            return Path(value)
        return None

    def _record_external_inputs(
        self, context: _ProxyRequestContext, locations: List[URLLocation]
    ) -> None:
        """Record protection, not read authorization, without changing fetch policy."""
        for value in dict.fromkeys(location.url for location in locations):
            try:
                path = self._local_processor_path(value)
                if (
                    path is None
                    or "\x00" in str(path)
                    or (
                        value.startswith("file://")
                        and urlparse(value).netloc not in ("", "localhost")
                    )
                ):
                    continue
                try:
                    resolved = path.resolve()
                except (OSError, RuntimeError):
                    resolved = Path(os.path.abspath(path))
                path = Path(os.path.abspath(path))
            except ValueError:
                continue  # Unparseable references remain the fetcher/engine's concern.
            for source in dict.fromkeys((path, resolved)):
                try:
                    identity = source.stat()
                except OSError:
                    identity = None
                try:
                    ref = PathRef.from_path(source, identity, PathOwnership.EXTERNAL)
                except OSError:
                    ref = PathRef(source, identity, PathOwnership.EXTERNAL)
                context.sources.add(ref)

    async def _apply_pre_processors(
        self,
        context: "_ProxyRequestContext",
        payload: Dict[str, Any],
        locations: Optional[List[URLLocation]] = None,
    ) -> Dict[str, Any]:
        """Use first-pass loci and fetch results; authorize external files before reads."""
        pipeline = context.processors
        if pipeline is None or not pipeline.pre:
            return payload
        if context.processor_dir is None:
            raise ServerError("Processor work directory is missing")
        fetched_urls = {
            result.original_url: result.local_path for result in context.inputs
        }
        fetched_paths = {
            result.local_path: path
            for result in context.inputs
            if (path := self._local_processor_path(result.local_path)) is not None
        }
        originals: Dict[str, Path] = {}
        input_roots: Dict[Path, Path] = {}
        identities: SourceIdentities = {}
        for location in locations or []:
            value = fetched_urls.get(location.url, location.url)
            if value in originals:
                continue
            if value in fetched_paths:
                originals[value] = fetched_paths[value]
                continue
            try:
                path = self._local_processor_path(value)
                if path is None:
                    continue
                root = Path(self._media_dir) if self._media_dir else None
                if (
                    root is None
                    or not path.is_absolute()
                    or ".." in path.parts
                    or (
                        value.startswith("file://")
                        and urlparse(value).netloc not in ("", "localhost")
                    )
                ):
                    raise ClientError(
                        "Local input must be inside the allowed media directory"
                    )
                path = path.resolve(strict=True)
                if not path.is_relative_to(root):
                    raise ClientError(
                        "Local input must be inside the allowed media directory"
                    )
                identity = path.lstat()
                if not stat.S_ISREG(identity.st_mode):
                    raise ClientError("Local input must be a regular file")
                ref = PathRef.external(path, identity)
            except (OSError, RuntimeError, ValueError) as exc:
                raise ClientError("Local media input is unavailable") from exc
            originals[value] = path
            input_roots[path] = root
            identities[path] = identity
            context.sources.add(ref)
        for value, path in fetched_paths.items():
            originals.setdefault(value, path)
        state = await self._processor_executor.execute(
            context.req_id,
            Path(context.processor_dir.name),
            pipeline,
            "pre",
            ProcessorState(paths=tuple(dict.fromkeys(originals.values()))),
            source_identities=identities,
            input_roots=input_roots,
        )
        context.sources.remember(state.source_identities)
        mapping = {
            uri: final_uri
            for uri, path in originals.items()
            if (final_uri := state.replacements.get(path, path).as_uri()) != uri
        }
        if not mapping:
            return payload
        if locations is None:
            return self._rewrite_processor_paths(payload, mapping)
        # Rewrite the original media loci, not text fields repeating an input URI.
        rewritten = deepcopy(payload)
        for location in locations:
            value = fetched_urls.get(location.url, location.url)
            if value in mapping:
                _set_at_path(rewritten, location.path, mapping[value])
        return rewritten

    @classmethod
    def _rewrite_processor_paths(cls, node: Any, mapping: Dict[str, str]) -> Any:
        if isinstance(node, str):
            return mapping.get(node, node)
        if isinstance(node, dict):
            return {
                key: cls._rewrite_processor_paths(value, mapping)
                for key, value in node.items()
            }
        if isinstance(node, list):
            return [cls._rewrite_processor_paths(value, mapping) for value in node]
        return node

    async def _apply_post_processors(
        self,
        req_id: str,
        directory: Optional[tempfile.TemporaryDirectory],
        pipeline: Optional[ProcessorPipeline],
        content: Optional[bytes],
        media_paths: List[str],
        *,
        sources: Optional[SourceIdentities] = None,
    ) -> Tuple[Optional[bytes], List[str]]:
        """Adapt extracted results to the executor before any output write."""
        if pipeline is None or not pipeline.post:
            return content, media_paths
        if directory is None:
            raise ServerError("Processor work directory is missing")
        paths = tuple(
            path
            for value in media_paths
            if (path := self._local_processor_path(value)) is not None
        )
        state = await self._processor_executor.execute(
            req_id,
            Path(directory.name),
            pipeline,
            "post",
            ProcessorState(paths=paths, payload=content),
            source_identities=sources,
        )
        if isinstance(sources, PathRefs):
            sources.remember(state.source_identities)
        elif sources is not None:
            for path, identity in state.source_identities.items():
                sources.setdefault(path, identity)  # Never re-claim a replaced file.
        # Payload-only calls must preserve the original media URI spelling.
        result_paths = (
            media_paths if state.paths == paths else [str(p) for p in state.paths]
        )
        return state.payload, result_paths

    async def _persist_output(
        self,
        req_id: str,
        spec: OutputRequest,
        raw_resp: httpx.Response,
        parsed: Any,
        sources: Optional[SourceIdentities] = None,
        *,
        envelope: Any = None,
        result_keys: tuple[str, ...] = _OPENAI_RESULT_KEYS,
        result_schema: Any = None,
        processors: Optional[ProcessorPipeline] = None,
        processor_dir: Optional[tempfile.TemporaryDirectory] = None,
    ) -> Any:
        """Persist declared results; preserve every other response field."""
        if envelope is None:
            envelope = raw_resp.json()
        payload, envelope, content = self._output_payload(
            envelope, result_keys, result_schema
        )
        # Upload only terminal-declared paths; historical sources are cleanup-only.
        media_paths = output_media_paths(parsed) if parsed is not None else []
        original_media_paths = media_paths
        if processors is not None and processors.post and sources is None:
            sources = {}
        content, media_paths = await self._apply_post_processors(
            req_id, processor_dir, processors, content, media_paths, sources=sources
        )
        if isinstance(parsed, VideoResponse) and content is None and not media_paths:
            # The video job's finally still owns historical source cleanup.
            return self._video_receipt(
                raw_resp,
                envelope,
                parsed,
                spec,
                OutputResponse(url=spec.url),
                has_payload=False,
                has_media=False,
                media_removed=bool(original_media_paths),
            )
        response_id = envelope.get("id") if isinstance(envelope, dict) else None
        name = f"{_safe_name(response_id) or req_id}.json"
        result = await self._write_output(
            req_id,
            spec,
            name,
            content,
            media_paths,
            sources,
            **(
                {"processor_workdir": Path(processor_dir.name)}
                if processor_dir is not None
                else {}
            ),
        )
        if result_schema is None:
            for key in payload:
                envelope[key] = []
        if isinstance(envelope, list):
            # A native batch has no outer metadata envelope for the receipt.
            envelope = {"data": envelope}
        envelope["output"] = result.model_dump()
        if isinstance(parsed, VideoResponse):
            return self._video_receipt(
                raw_resp,
                envelope,
                parsed,
                spec,
                result,
                has_payload=content is not None,
                has_media=bool(media_paths),
            )
        return JSONResponse(envelope, status_code=raw_resp.status_code)

    @staticmethod
    def _output_payload(
        envelope: Any, result_keys: tuple[str, ...], result_schema: Any
    ) -> Tuple[Any, Any, Optional[bytes]]:
        """Split what gets written from the metadata kept in the HTTP body."""
        if result_schema is not None:
            try:
                payload, envelope = split_native_result(envelope, result_schema)
            except ValueError as exc:
                raise ServerError(f"Invalid native output response: {exc}") from exc
        else:
            envelope = dict(envelope)
            payload = {key: envelope[key] for key in result_keys if key in envelope}
        content = (
            json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode(
                "utf-8"
            )
            if payload or result_schema is not None
            else None
        )
        return payload, envelope, content

    async def _write_output(
        self,
        req_id: str,
        spec: OutputRequest,
        name: str,
        content: Optional[bytes],
        media_paths: List[str],
        sources: Optional[SourceIdentities],
        *,
        processor_workdir: Optional[Path] = None,
    ) -> OutputResponse:
        """Write the payload and its declared media, owning cleanup either way."""
        scheme = urlparse(spec.url).scheme.lower() or "file"
        write_t0 = time.monotonic()
        outcome = "error"
        try:
            if media_paths or sources:
                try:
                    result, output_bytes = await self._output_writer.write_bundle(
                        spec,
                        content,
                        name,
                        media_paths,
                        sources,
                        **(
                            {"processor_workdir": processor_workdir}
                            if processor_workdir is not None
                            else {}
                        ),
                    )
                finally:
                    if sources is not None:
                        sources.clear()  # Ownership entered the writer's finally.
            elif content is not None:
                result = await self._output_writer.write(spec, content, name=name)
                output_bytes = len(content)
            else:
                raise OutputWriteError("Engine response has no payload or local media")
        except asyncio.CancelledError:
            outcome = "cancelled"
            raise
        except OutputWriteError as exc:
            logger.error("[req-%s] %s", req_id, exc)
            raise ServerError(str(exc)) from exc
        else:
            outcome = "ok"
            self._output_bytes.labels(scheme=scheme).inc(output_bytes)
            return result
        finally:
            self._outputs_total.labels(scheme=scheme, result=outcome).inc()
            self._output_seconds.labels(scheme=scheme, result=outcome).inc(
                time.monotonic() - write_t0
            )

    def _video_receipt(
        self,
        raw: httpx.Response,
        envelope: Dict[str, Any],
        video: VideoResponse,
        spec: OutputRequest,
        result: OutputResponse,
        *,
        has_payload: bool,
        has_media: bool,
        media_removed: bool = False,
    ) -> JSONResponse:
        receipt = VideoResponse.output_receipt(
            envelope,
            spec,
            result,
            has_payload=has_payload,
            has_media=has_media,
            media_removed=media_removed,
        )
        self._uploaded_videos[video.id] = receipt
        return JSONResponse(
            receipt.model_dump(mode="json", exclude_unset=True),
            status_code=raw.status_code,
        )

    @staticmethod
    def _engine_json(raw: httpx.Response) -> Any:
        try:
            return raw.json()
        except ValueError:
            return None

    @staticmethod
    def _parse_engine_envelope(model: Type[Any], envelope: Any, kind: str) -> Any:
        try:
            return model.model_validate(envelope)
        except Exception as exc:
            raise ServerError(
                f"Inner engine returned a body that does not match the {kind} schema: {exc}"
            ) from exc

    @staticmethod
    def _raise_engine_status(raw_resp: httpx.Response) -> None:
        if raw_resp.status_code >= 300:
            raise httpx.HTTPStatusError(
                f"Inner engine returned {raw_resp.status_code}",
                request=raw_resp.request,
                response=raw_resp,
            )

    @staticmethod
    def _parse_engine_body(model: Type[Any], content: bytes, kind: str) -> Any:
        try:
            return model.model_validate_json(content)
        except Exception as exc:
            raise ServerError(
                f"Inner engine returned a body that does not match the "
                f"{kind} schema: {exc}"
            ) from exc

    async def _typed_stream_events(
        self,
        raw_resp: httpx.Response,
        chunk_model: Type[Any],
        req_id: str,
        kind: str,
        close_stream: Optional[Callable[[], Awaitable[None]]] = None,
    ) -> AsyncIterator[str]:
        """Parse each SSE ``data`` event into a validated typed chunk.

        Events that are not JSON objects (``[DONE]``, comments, keepalives)
        and lines that are not ``data`` events pass through unchanged. A chunk
        that does not match the schema is also passed through rather than
        breaking a stream whose headers are already sent.
        """
        t0 = time.monotonic()
        task = asyncio.current_task()
        if task is not None:
            self._in_flight_tasks[req_id] = task
        completed = False

        def frame(lines: List[str]) -> str:
            data = [
                line[5:].removeprefix(" ") for line in lines if line.startswith("data:")
            ]
            if data:
                payload = "\n".join(data)
                if payload != "[DONE]":
                    try:
                        parsed = chunk_model.model_validate_json(payload)
                    except Exception:
                        logger.warning(
                            "[req-%s] %s chunk does not match the schema; relaying "
                            "it unchanged",
                            req_id,
                            kind,
                        )
                    else:
                        # Preserve event/id/retry fields while normalizing typed data.
                        first_data = next(
                            i
                            for i, line in enumerate(lines)
                            if line.startswith("data:")
                        )
                        lines = (
                            lines[:first_data]
                            + [
                                f"data: {parsed.model_dump_json(exclude_unset=True, by_alias=True)}"
                            ]
                            + [
                                line
                                for line in lines[first_data + 1 :]
                                if not line.startswith("data:")
                            ]
                        )
            return "\n".join(lines) + "\n\n"

        try:
            pending: List[str] = []
            async for line in raw_resp.aiter_lines():
                if line:
                    pending.append(line)
                elif pending:
                    # An SSE event ends at the blank line, not at the next data event.
                    yield frame(pending)
                    pending = []
            if pending:
                yield frame(pending)
            completed = True
        except asyncio.CancelledError:
            if not completed:
                logger.warning("[req-%s] %s stream aborted", req_id, kind)
            raise
        finally:
            # Media temp release + _requests_end live in close_response(); the
            # shared close_task ensures exactly-once even if we get here from
            # normal completion, cancellation, or pre-iteration aclose().
            if close_stream is not None:
                await close_stream()
            else:
                # Direct generator use (tests / internal callers without the
                # shared close_response wrapper): close upstream and untrack.
                try:
                    await raw_resp.aclose()
                except Exception:
                    pass
                self._requests_end(req_id, t0)
            logger.info(
                "[req-%s] %s stream finished in %.3fs",
                req_id,
                kind,
                time.monotonic() - t0,
            )

    async def _forward_with_retry(
        self,
        endpoint: str,
        stream: bool,
        *,
        method: str = "POST",
        payload: Optional[Dict[str, Any]] = None,
        content: Optional[AsyncIterable[bytes]] = None,
        content_type: Optional[str] = None,
        headers: Optional[Dict[str, str]] = None,
        raw_body: bool = False,
        on_submission: Optional[Callable[[bool], None]] = None,
        replayable: Callable[[], bool] = lambda: True,
    ) -> httpx.Response:
        """Forward with a bounded retry on connection-level failures.

        ``replayable`` guards one-shot (streamed) bodies: once it returns
        False, part of the body has been consumed and the request is not
        retried.
        """
        last_exc: Optional[BaseException] = None
        for attempt in range(FORWARD_RETRY_ATTEMPTS):
            if attempt:
                await asyncio.sleep(
                    FORWARD_RETRY_BACKOFF_SECONDS * (2 ** (attempt - 1))
                )
                # Don't hammer a dead or failed engine.
                if self._state != EngineState.READY:
                    break
            try:
                kwargs: Dict[str, Any] = {"json": payload, "stream": stream}
                if content is not None:
                    kwargs["content"] = content
                if content_type is not None:
                    kwargs["content_type"] = content_type
                if raw_body:
                    kwargs["raw_body"] = True
                if headers is not None:
                    kwargs["headers"] = headers
                if on_submission is not None:
                    on_submission(True)
                return await self._subprocess.forward(method, endpoint, **kwargs)
            except (httpx.ConnectError, httpx.ConnectTimeout, httpx.PoolTimeout) as exc:
                if on_submission is not None:
                    on_submission(False)
                if not replayable():
                    raise
                last_exc = exc
        if last_exc is None:
            raise self._unavailable_error()
        raise last_exc


def _safe_name(value: Any) -> Optional[str]:
    if not isinstance(value, str) or not value:
        return None
    cleaned = "".join(c if c.isalnum() or c in "-_." else "_" for c in value)
    return cleaned.strip(".") or None


class _StreamedBody:
    """One-shot async iterator over the client's request body.

    Handed to httpx as ``content`` so the upload is relayed chunk by chunk.
    ``started`` tells the retry loop whether bytes were already consumed (a
    streamed body cannot be replayed).
    """

    def __init__(self, request: Request) -> None:
        self._request = request
        self.upload_done = asyncio.Event()
        self.started = False
        self.bytes_sent = 0
        self.chunks = 0

    def __aiter__(self) -> AsyncIterator[bytes]:
        return self._iterate()

    async def _iterate(self) -> AsyncIterator[bytes]:
        self.started = True
        async for chunk in self._request.stream():
            if not chunk:
                continue
            self.bytes_sent += len(chunk)
            self.chunks += 1
            yield chunk
        self.upload_done.set()

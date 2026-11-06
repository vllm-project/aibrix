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
import tempfile
import time
import uuid
from dataclasses import dataclass, field
from enum import Enum
from typing import (
    Any,
    AsyncIterator,
    Awaitable,
    Callable,
    Dict,
    List,
    Literal,
    Optional,
    Type,
    Union,
)
from urllib.parse import quote, urlparse

import httpx
from fastapi.responses import JSONResponse, Response
from prometheus_client import (
    CollectorRegistry,
    Counter,
    Gauge,
    Histogram,
    generate_latest,
)

from aibrix.openai_frontend.common.artifact_fetcher import (
    ArtifactFetcher,
    ArtifactFetchError,
    DefaultArtifactFetcher,
    FetchResult,
)
from aibrix.openai_frontend.engine.engine import LLMEngine
from aibrix.openai_frontend.proxy.engine_spec import EngineSpec
from aibrix.openai_frontend.proxy.output_writer import (
    OutputWriteError,
    OutputWriter,
    SourceIdentities,
)
from aibrix.openai_frontend.proxy.request_inspector import (
    MediaURLInspector,
    RequestInspector,
)
from aibrix.openai_frontend.proxy.request_rewriter import RequestRewriter
from aibrix.openai_frontend.proxy.subprocess_engine import (
    DEFAULT_SHUTDOWN_GRACE_SECONDS,
    DEFAULT_STARTUP_TIMEOUT_SECONDS,
    SubprocessEngine,
)
from aibrix.openai_frontend.proxy.video_job import VideoJobContext, VideoOutputJob
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
from aibrix.openai_frontend.schemas.output import (
    OutputRequest,
    OutputResponse,
)
from aibrix.openai_frontend.utils.utils import (
    ClientError,
    EngineUnavailableError,
    ServerError,
    make_prefix_formatter,
)
from aibrix.runtime.downloaders import await_thread_completion

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


_STATE_CODES = {state: i for i, state in enumerate(EngineState)}


class _ClosingStream(AsyncIterator[str]):
    """Own the upstream even if iteration never starts or stops at a yield."""

    def __init__(
        self, iterator: AsyncIterator[str], close: Callable[[], Awaitable[None]]
    ) -> None:
        self._iterator = iterator
        self._close = close

    def __aiter__(self) -> AsyncIterator[str]:
        return self

    async def __anext__(self) -> str:
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


@dataclass(frozen=True)
class _RetainedVideoSources:
    output: OutputRequest
    identities: SourceIdentities


@dataclass
class _ProxyRequestContext:
    req_id: str
    started_at: float
    output: Optional[OutputRequest]
    inputs: List[FetchResult] = field(default_factory=list)
    sources: SourceIdentities = field(default_factory=dict)
    submitted: bool = False
    video: Optional[VideoResponse] = None
    owner: Literal["request", "stream", "video"] = "request"

    def submission_state(self, submitted: bool) -> None:
        self.submitted = submitted

    def transfer_video(self) -> VideoJobContext:
        assert self.output is not None
        context = VideoJobContext(
            self.req_id, self.started_at, self.output, self.inputs, self.sources
        )
        self.inputs, self.sources = [], {}
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
    ):
        if not math.isfinite(video_output_timeout) or video_output_timeout <= 0:
            raise ValueError("video_output_timeout must be a finite positive number")
        self._spec = spec or EngineSpec(name="default")

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
        if self._fetch_media and media_dir is None:
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
        self._output_writer = output_writer or OutputWriter()
        # None marks an output-managed job until its first poll response.
        self._uploaded_videos: Dict[str, Optional[VideoResponse]] = {}
        self._video_tasks: Dict[str, asyncio.Task[Any]] = {}
        # Input-only ownership is independent of output receipts and HTTP requests.
        # None means a submitted POST has no confirmed job ID; retain until stop.
        self._video_inputs: Dict[str, _RetainedVideoInputs] = {}
        # Unknown output job identities cannot be polled; retain declared sources
        # separately from input-only ownership until the engine stops.
        self._video_sources: Dict[str, _RetainedVideoSources] = {}
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
            if self._video_tasks or self._video_inputs or self._video_sources:
                # Stop generation before releasing retained video input media or
                # taking the final source snapshot. Uploads still own their readers.
                await asyncio.to_thread(self._subprocess.shutdown)
                self._video_writers_stopped.set()
            # Close even streams handed off before iteration; do not wait for
            # entire serving tasks, which may themselves be awaiting aclose().
            await asyncio.gather(
                *(close() for close in closers), return_exceptions=True
            )
            await asyncio.gather(*pending)
            if (
                self._video_inputs or self._video_sources
            ) and not self._video_writers_stopped.is_set():
                # Initial POST cancellation may retain inputs while pending
                # requests drain, after the first shutdown snapshot above.
                await asyncio.to_thread(self._subprocess.shutdown)
                self._video_writers_stopped.set()
            await self._release_retained_videos()
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
        await self._release_retained_videos()
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
        return await self._typed_proxy(
            "chat",
            self._spec.chat_endpoint,
            request,
            CreateChatCompletionResponse,
            CreateChatCompletionStreamResponse,
            self._chat_inspector,
        )

    async def completion(
        self, request: CreateCompletionRequest
    ) -> Union[CreateCompletionResponse, AsyncIterator[str], Response]:
        self._spec.require_completion()
        return await self._typed_proxy(
            "completion",
            self._spec.completion_endpoint,
            request,
            CreateCompletionResponse,
            CreateCompletionResponse,
            self._completion_inspector,
        )

    async def embedding(
        self, request: CreateEmbeddingRequest
    ) -> Union[CreateEmbeddingResponse, Response]:
        self._spec.require_embedding()
        return await self._typed_proxy(
            "embedding",
            self._spec.embedding_endpoint,
            request,
            CreateEmbeddingResponse,
            None,
            self._embedding_inspector,
        )

    async def image(self, request: ImageGenerationRequest) -> Response:
        return await self._typed_proxy(
            "image",
            "/v1/images/generations",
            request,
            ImageGenerationResponse,
            None,
            self._media_inspector,
        )

    async def video(
        self, request: VideoGenerationRequest
    ) -> Union[VideoResponse, Response]:
        return await self._typed_proxy(
            "video",
            "/v1/videos",
            request,
            VideoResponse,
            None,
            self._media_inspector,
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
                await self._release_retained_videos(video_id)
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
        retained = [
            req_id
            for req_id, entry in self._video_inputs.items()
            if video_id is None or entry.video_id == video_id
        ]
        for req_id in retained:
            entry = self._video_inputs.pop(req_id, None)
            if entry is not None:
                await self._release(req_id, entry.results)

    async def _release_unknown_video_sources(self) -> None:
        """Unknown job IDs have no terminal GET; only engine stop permits cleanup."""
        for req_id in list(self._video_sources):
            entry = self._video_sources.pop(req_id, None)
            if entry is None:
                continue
            try:
                await self._output_writer.cleanup_completed_sources(
                    entry.identities, entry.output
                )
            except Exception:
                logger.exception("Unknown video job source cleanup failed")

    async def _release_retained_videos(self, video_id: Optional[str] = None) -> None:
        """Coordinate retained resources with visible completion, not caller tasks.

        Register before any ownership is popped or cleanup awaits, so concurrent
        shutdown can wait even when the input/source maps are already empty.
        A specific ID releases only inputs; output sources require engine stop.
        """

        async def release() -> None:
            if video_id is None:
                await self._release_unknown_video_sources()
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

    # ------------------------------------------------------------------
    # Shared forward pipeline
    # ------------------------------------------------------------------

    async def _typed_proxy(
        self,
        kind: str,
        endpoint: str,
        request: Any,
        response_model: Type[Any],
        chunk_model: Optional[Type[Any]],
        inspector: RequestInspector,
        *,
        method: str = "POST",
    ) -> Any:
        """Forward, then transfer ownership to a stream/job or finish locally."""
        self._check_available()
        payload = request.model_dump(exclude_unset=True, mode="json")
        payload.pop("output", None)
        stream = bool(payload.get("stream")) and chunk_model is not None
        req_id, t0 = self._requests_begin()
        context = _ProxyRequestContext(req_id, t0, getattr(request, "output", None))
        try:
            raw, envelope = await self._forward_typed(
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
            if stream and chunk_model is not None:
                return self._hand_off_stream(context, raw, chunk_model, kind)
            if context.output is not None:
                return await self._persist_typed(
                    context, raw, envelope, response_model, kind
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
            await self._finish_typed(context)

    async def _forward_typed(
        self,
        context: _ProxyRequestContext,
        kind: str,
        endpoint: str,
        request: Any,
        response_model: Type[Any],
        inspector: RequestInspector,
        payload: Dict[str, Any],
        *,
        stream: bool,
        method: str,
    ) -> tuple[httpx.Response, Any]:
        if self._fetch_media:
            payload, context.inputs = await self._fetch_media_for(
                context.req_id, payload, inspector
            )
        is_video = isinstance(request, VideoGenerationRequest)
        forward = self._forward(
            endpoint,
            stream=stream,
            payload=payload,
            req_id=context.req_id,
            kind=kind,
            method=method,
            on_submission=context.submission_state if is_video else None,
        )
        if context.output is not None and is_video:
            try:
                raw = await asyncio.wait_for(
                    forward,
                    max(
                        0.0,
                        context.started_at
                        + self._video_output_timeout
                        - time.monotonic(),
                    ),
                )
            except asyncio.TimeoutError as exc:
                raise ServerError("Video output generation timed out") from exc
        else:
            raw = await forward
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
        chunk_model: Type[Any],
        kind: str,
    ) -> AsyncIterator[str]:
        close_task: Optional[asyncio.Task[None]] = None
        inputs, context.inputs = context.inputs, []

        async def close_response() -> None:
            try:
                await raw.aclose()
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

        iterator = self._typed_stream_events(
            raw, chunk_model, context.req_id, kind, close_stream
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

    async def _persist_typed(
        self,
        context: _ProxyRequestContext,
        raw: httpx.Response,
        envelope: Any,
        response_model: Type[Any],
        kind: str,
    ) -> Any:
        assert context.output is not None
        parsed = self._parse_engine_envelope(response_model, envelope, kind)
        if isinstance(parsed, VideoResponse):
            return await self._new_video_job(context, parsed).run(raw, envelope)
        return await self._persist_output(
            context.req_id,
            context.output,
            raw,
            parsed,
            context.sources,
            envelope=envelope,
        )

    async def _finish_typed(self, context: _ProxyRequestContext) -> None:
        if context.owner != "request":
            return
        if context.submitted and (
            context.output is None
            or context.video is None
            or context.video.status not in VIDEO_TERMINAL_STATUSES
        ):
            # Non-output jobs await terminal metadata GET. Unknown IDs await stop.
            if context.inputs:
                self._video_inputs[context.req_id] = _RetainedVideoInputs(
                    context.video.id
                    if context.video is not None and context.output is None
                    else None,
                    context.inputs,
                )
                context.inputs = []
            if context.sources and context.output is not None:
                self._video_sources[context.req_id] = _RetainedVideoSources(
                    context.output, context.sources
                )
                context.sources = {}
        try:
            if context.submitted and self._video_writers_stopped.is_set():
                await self._release_retained_videos()
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
                payload=payload if method != "GET" else None,
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
    ) -> "tuple[Dict[str, Any], List[FetchResult]]":
        rewriter = RequestRewriter(inspector, self._fetcher, self._credentials_map)
        fetch_t0 = time.monotonic()
        try:
            rewritten, results = await rewriter.rewrite(request_dict)
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

    async def _persist_output(
        self,
        req_id: str,
        spec: OutputRequest,
        raw_resp: httpx.Response,
        parsed: Any,
        sources: Optional[SourceIdentities] = None,
        *,
        envelope: Optional[Dict[str, Any]] = None,
    ) -> Any:
        """Persist only result arrays; preserve every other engine response field."""
        envelope = dict(envelope if envelope is not None else raw_resp.json())
        payload = {key: envelope[key] for key in _EMPTY_RESULT_KEYS if key in envelope}
        content = (
            json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode(
                "utf-8"
            )
            if payload
            else None
        )
        # Upload only terminal-declared paths; historical sources are cleanup-only.
        media_paths = output_media_paths(parsed) if parsed is not None else []
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
            )
        scheme = urlparse(spec.url).scheme.lower() or "file"
        name = f"{_safe_name(envelope.get('id')) or req_id}.json"
        write_t0 = time.monotonic()
        outcome = "error"
        try:
            if media_paths or sources:
                try:
                    result, output_bytes = await self._output_writer.write_bundle(
                        spec, content, name, media_paths, sources
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
        finally:
            self._outputs_total.labels(scheme=scheme, result=outcome).inc()
            self._output_seconds.labels(scheme=scheme, result=outcome).inc(
                time.monotonic() - write_t0
            )

        for key in payload:
            envelope[key] = []
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
    ) -> JSONResponse:
        receipt = VideoResponse.output_receipt(
            envelope, spec, result, has_payload=has_payload, has_media=has_media
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
                            + [f"data: {parsed.model_dump_json(exclude_unset=True)}"]
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
        headers: Optional[Dict[str, str]] = None,
        raw_body: bool = False,
        on_submission: Optional[Callable[[bool], None]] = None,
    ) -> httpx.Response:
        """Forward with a bounded retry on connection-level failures."""
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
                if raw_body:
                    kwargs["raw_body"] = True
                if headers is not None:
                    kwargs["headers"] = headers
                if on_submission is not None:
                    # Once an attempt starts, cancellation/read/write failures
                    # may mean the engine accepted it. Connection/pool failures
                    # are safe to retry and prove this attempt was not submitted.
                    on_submission(True)
                return await self._subprocess.forward(method, endpoint, **kwargs)
            except (httpx.ConnectError, httpx.ConnectTimeout, httpx.PoolTimeout) as exc:
                if on_submission is not None:
                    on_submission(False)
                last_exc = exc
        if last_exc is None:
            raise self._unavailable_error()
        raise last_exc


# Result arrays persisted verbatim, then emptied in the HTTP response.
_EMPTY_RESULT_KEYS = ("choices", "data")


def _safe_name(value: Any) -> Optional[str]:
    if not isinstance(value, str) or not value:
        return None
    cleaned = "".join(c if c.isalnum() or c in "-_." else "_" for c in value)
    return cleaned.strip(".") or None

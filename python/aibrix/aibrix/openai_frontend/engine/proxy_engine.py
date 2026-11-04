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
import logging
import time
import uuid
from enum import Enum
from typing import (
    Any,
    AsyncIterator,
    Awaitable,
    Callable,
    Dict,
    List,
    Optional,
    Type,
    Union,
)

import httpx
from prometheus_client import (
    CollectorRegistry,
    Counter,
    Gauge,
    Histogram,
    generate_latest,
)

from aibrix.openai_frontend.engine.engine import LLMEngine
from aibrix.openai_frontend.proxy.engine_spec import EngineSpec
from aibrix.openai_frontend.proxy.subprocess_engine import (
    DEFAULT_SHUTDOWN_GRACE_SECONDS,
    DEFAULT_STARTUP_TIMEOUT_SECONDS,
    SubprocessEngine,
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
from aibrix.openai_frontend.utils.utils import (
    ClientError,
    EngineUnavailableError,
    ServerError,
    make_prefix_formatter,
)

HEALTH_CHECK_INTERVAL_SECONDS = 10.0
WATCHDOG_STUCK_THRESHOLD_SECONDS = 300.0
HEARTBEAT_INTERVAL_CYCLES = 6  # ~60s at 10s interval
# Retries for connection-level failures on the forward path (the request
# never reached the engine, so retrying is safe even for non-idempotent POSTs).
FORWARD_RETRY_ATTEMPTS = 3
FORWARD_RETRY_BACKOFF_SECONDS = 0.5


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
        startup_timeout: Optional[float] = DEFAULT_STARTUP_TIMEOUT_SECONDS,
        shutdown_grace_seconds: float = DEFAULT_SHUTDOWN_GRACE_SECONDS,
        health_check_interval: float = HEALTH_CHECK_INTERVAL_SECONDS,
    ):
        self._spec = spec or EngineSpec(name="default")

        self._subprocess = SubprocessEngine(
            cmd=list(engine_cmd),
            port=engine_port,
            host=engine_host,
            env=engine_env,
            spec=self._spec,
            shutdown_grace_seconds=shutdown_grace_seconds,
        )

        self._startup_timeout = startup_timeout
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

        # Background supervisor that detects inner-engine exits.
        self._health_monitor_task: Optional[asyncio.Task] = None

        # Metrics
        self._metrics_registry = CollectorRegistry()
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
        self._cancel_in_flight("proxy engine stopping")
        self._in_flight.clear()
        self._in_flight_gauge.set(0)

        try:
            # Process teardown can wait for the grace period; keep it off the loop.
            await asyncio.to_thread(self._subprocess.shutdown)
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
        task = asyncio.current_task()
        if task is not None:
            self._in_flight_tasks[req_id] = task
        self._in_flight_gauge.set(len(self._in_flight))
        return req_id

    def _untrack_request(self, req_id: str) -> None:
        self._in_flight.pop(req_id, None)
        self._in_flight_tasks.pop(req_id, None)
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
    ) -> Union[CreateChatCompletionResponse, AsyncIterator[str]]:
        self._spec.require_chat()
        return await self._typed_proxy(
            "chat",
            self._spec.chat_endpoint,
            request,
            CreateChatCompletionResponse,
            CreateChatCompletionStreamResponse,
        )

    async def completion(
        self, request: CreateCompletionRequest
    ) -> Union[CreateCompletionResponse, AsyncIterator[str]]:
        self._spec.require_completion()
        return await self._typed_proxy(
            "completion",
            self._spec.completion_endpoint,
            request,
            CreateCompletionResponse,
            CreateCompletionResponse,
        )

    async def embedding(
        self, request: CreateEmbeddingRequest
    ) -> CreateEmbeddingResponse:
        self._spec.require_embedding()
        return await self._typed_proxy(
            "embedding",
            self._spec.embedding_endpoint,
            request,
            CreateEmbeddingResponse,
            None,
        )

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
    ) -> Any:
        """Forward a typed request and return a validated typed response.

        Non-2xx engine responses raise ``httpx.HTTPStatusError`` so the app
        relays them verbatim; a success body that does not match the pinned
        schema is an internal failure.
        """
        self._check_available()
        payload = request.model_dump(exclude_unset=True, mode="json")
        stream = bool(payload.get("stream")) and chunk_model is not None
        req_id, t0 = self._requests_begin()
        handed_off = False
        try:
            raw_resp = await self._forward(
                endpoint, stream=stream, payload=payload, req_id=req_id, kind=kind
            )
            self._raise_engine_status(raw_resp)
            if stream and chunk_model is not None:
                close_task: Optional[asyncio.Task[None]] = None

                async def close_response() -> None:
                    try:
                        await raw_resp.aclose()
                    finally:
                        self._requests_end(req_id, t0)

                async def close_stream() -> None:
                    nonlocal close_task
                    if close_task is None:
                        close_task = asyncio.create_task(close_response())
                    await asyncio.shield(close_task)

                iterator = self._typed_stream_events(
                    raw_resp, chunk_model, req_id, kind, close_stream
                )
                handed_off = True
                return _ClosingStream(iterator, close_stream)
            return self._parse_engine_body(response_model, raw_resp.content, kind)
        finally:
            if not handed_off:
                self._requests_end(req_id, t0)

    async def _forward(
        self,
        endpoint: str,
        *,
        stream: bool,
        payload: Dict[str, Any],
        req_id: str,
        kind: str,
    ) -> httpx.Response:
        try:
            return await self._forward_with_retry(endpoint, stream, payload=payload)
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
            if close_stream is not None:
                await close_stream()
            else:
                try:
                    await raw_resp.aclose()
                finally:
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
                return await self._subprocess.forward(
                    method, endpoint, json=payload, stream=stream
                )
            except (httpx.ConnectError, httpx.ConnectTimeout, httpx.PoolTimeout) as exc:
                last_exc = exc
        if last_exc is None:
            raise self._unavailable_error()
        raise last_exc

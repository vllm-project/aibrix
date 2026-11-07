# Copyright 2024 The Aibrix Team.
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
import bisect
import contextlib
from collections import Counter
from dataclasses import dataclass, field
from itertools import count
from typing import Any, AsyncContextManager, Optional

import httpx

from aibrix import envs
from aibrix.logger import init_logger
from aibrix.metadata.core.metrics import Emitter, T, metrics_names

logger = init_logger(__name__)

CLIENT_ID_COUNTER = count(1)
_LATENCY_BUCKET_MAX_SECONDS = 600.0
_LATENCY_BUCKET_MIN_SECONDS = 0.001
_PURPOSE_TELEMETRY_STATES: dict[str, "_HTTPPurposeTelemetryState"] = {}


def _normalize_client_backend(client_backend: str) -> str:
    return client_backend.lower().replace("-", "_")


def _build_latency_bucket_bounds() -> tuple[float, ...]:
    bounds: list[float] = []
    current = _LATENCY_BUCKET_MIN_SECONDS
    while current < _LATENCY_BUCKET_MAX_SECONDS:
        bounds.append(round(current, 6))
        current *= 2
    bounds.append(_LATENCY_BUCKET_MAX_SECONDS)
    return tuple(bounds)


_LATENCY_BUCKET_UPPER_BOUNDS_SECONDS = _build_latency_bucket_bounds()


def _telemetry_interval_seconds() -> float:
    return envs.CORE_HTTPX_ASYNC_CLIENT_TELEMETRY_INTERVAL_SECONDS


def _telemetry_enabled() -> bool:
    return envs.CORE_HTTPX_ASYNC_CLIENT_TELEMETRY_ENABLED


@dataclass(slots=True)
class _HTTPTelemetrySnapshot:
    started: int
    completed: int
    failed: int
    inflight: int
    max_inflight: int
    window_started: int
    window_completed: int
    window_failed: int
    avg_latency_seconds: Optional[float]
    p90_latency_seconds: Optional[float]
    exception_types: dict[str, int]
    latency_histogram: tuple[int, ...]
    call_sites: tuple["_HTTPTelemetryCallSiteSnapshot", ...]


@dataclass(slots=True)
class _HTTPTelemetryCallSiteStats:
    started: int = 0
    completed: int = 0
    failed: int = 0
    inflight: int = 0
    exception_types: Counter[str] = field(default_factory=Counter)


@dataclass(slots=True)
class _HTTPTelemetryCallSiteSnapshot:
    call_site: str
    started: int
    completed: int
    failed: int
    inflight: int
    exception_types: dict[str, int]


@dataclass(slots=True)
class HTTPTelemetryStats:
    started: int = 0
    completed: int = 0
    failed: int = 0
    inflight: int = 0
    max_inflight: int = 0
    window_started: int = 0
    window_completed: int = 0
    window_failed: int = 0
    _window_latency_histogram: list[int] = field(
        default_factory=lambda: [0] * (len(_LATENCY_BUCKET_UPPER_BOUNDS_SECONDS) + 1)
    )
    _window_latency_sum_seconds: float = 0.0
    _window_exception_types: Counter[str] = field(default_factory=Counter)
    _call_sites: dict[str, _HTTPTelemetryCallSiteStats] = field(default_factory=dict)

    def record_start(self, *, call_site: Optional[str] = None) -> None:
        self.started += 1
        self.window_started += 1
        self.inflight += 1
        self.max_inflight = max(self.max_inflight, self.inflight)
        if call_site is not None:
            site_stats = self._call_sites.setdefault(
                call_site,
                _HTTPTelemetryCallSiteStats(),
            )
            site_stats.started += 1
            site_stats.inflight += 1

    def record_completion(
        self,
        *,
        failed: bool,
        latency_seconds: float,
        call_site: Optional[str] = None,
        exception_type: Optional[str] = None,
    ) -> None:
        self.completed += 1
        self.window_completed += 1
        if failed:
            self.failed += 1
            self.window_failed += 1
        self.inflight = max(self.inflight - 1, 0)
        normalized_latency = max(latency_seconds, 0.0)
        bucket_index = bisect.bisect_left(
            _LATENCY_BUCKET_UPPER_BOUNDS_SECONDS,
            normalized_latency,
        )
        self._window_latency_histogram[bucket_index] += 1
        self._window_latency_sum_seconds += normalized_latency
        if failed and exception_type is not None:
            self._window_exception_types[exception_type] += 1
        if call_site is not None:
            site_stats = self._call_sites.setdefault(
                call_site,
                _HTTPTelemetryCallSiteStats(),
            )
            site_stats.completed += 1
            if failed:
                site_stats.failed += 1
                if exception_type is not None:
                    site_stats.exception_types[exception_type] += 1
            site_stats.inflight = max(site_stats.inflight - 1, 0)

    def snapshot(self, *, reset_window: bool) -> _HTTPTelemetrySnapshot:
        snapshot = _HTTPTelemetrySnapshot(
            started=self.started,
            completed=self.completed,
            failed=self.failed,
            inflight=self.inflight,
            max_inflight=self.max_inflight,
            window_started=self.window_started,
            window_completed=self.window_completed,
            window_failed=self.window_failed,
            avg_latency_seconds=self._window_average_latency(),
            p90_latency_seconds=self._histogram_percentile(0.90),
            exception_types=dict(sorted(self._window_exception_types.items())),
            latency_histogram=tuple(self._window_latency_histogram),
            call_sites=tuple(self._call_site_snapshots()),
        )
        if reset_window:
            self.window_started = 0
            self.window_completed = 0
            self.window_failed = 0
            self._window_latency_histogram = [0] * (
                len(_LATENCY_BUCKET_UPPER_BOUNDS_SECONDS) + 1
            )
            self._window_latency_sum_seconds = 0.0
            self._window_exception_types = Counter()
            self._call_sites = {}
        return snapshot

    def _window_average_latency(self) -> Optional[float]:
        total = sum(self._window_latency_histogram)
        if total <= 0:
            return None
        return self._window_latency_sum_seconds / total

    def _histogram_percentile(self, percentile: float) -> Optional[float]:
        total = sum(self._window_latency_histogram)
        if total <= 0:
            return None
        target = max(1, int(total * percentile + 0.999999))
        cumulative = 0
        for bucket_index, bucket_count in enumerate(self._window_latency_histogram):
            cumulative += bucket_count
            if cumulative >= target:
                if bucket_index < len(_LATENCY_BUCKET_UPPER_BOUNDS_SECONDS):
                    return _LATENCY_BUCKET_UPPER_BOUNDS_SECONDS[bucket_index]
                return _LATENCY_BUCKET_MAX_SECONDS
        return _LATENCY_BUCKET_MAX_SECONDS

    def _call_site_snapshots(self) -> list[_HTTPTelemetryCallSiteSnapshot]:
        snapshots: list[_HTTPTelemetryCallSiteSnapshot] = []
        for call_site, stats in sorted(
            self._call_sites.items(),
            key=lambda item: (item[1].failed, item[1].completed, item[0]),
            reverse=True,
        ):
            snapshots.append(
                _HTTPTelemetryCallSiteSnapshot(
                    call_site=call_site,
                    started=stats.started,
                    completed=stats.completed,
                    failed=stats.failed,
                    inflight=stats.inflight,
                    exception_types=dict(sorted(stats.exception_types.items())),
                )
            )
        return snapshots


@dataclass
class _HTTPPurposeTelemetryState:
    stats: HTTPTelemetryStats = field(default_factory=HTTPTelemetryStats)
    active_clients: int = 0
    emitter_client_id: Optional[str] = None
    last_emitted_at: Optional[float] = None

    def register(self, *, client_id: str, now: Optional[float]) -> None:
        self.active_clients += 1
        if self.emitter_client_id is None:
            self.emitter_client_id = client_id
            self.last_emitted_at = now

    def release(self, *, client_id: str) -> None:
        self.active_clients = max(self.active_clients - 1, 0)
        if self.emitter_client_id == client_id:
            self.emitter_client_id = None

    def ensure_emitter(self, *, client_id: str, now: Optional[float]) -> bool:
        if self.emitter_client_id is None:
            self.emitter_client_id = client_id
            self.last_emitted_at = self.last_emitted_at or now
        return self.emitter_client_id == client_id


class _HTTPClientMeta(type):
    def __call__(cls, **client_kwargs: Any):
        if cls is HTTPClient:
            client_backend = _normalize_client_backend(envs.BATCH_RUNTIME_HTTP_CLIENT)
            if client_backend == "httpx_client":
                from aibrix.metadata.core.httpx_client import HTTPXClientWrapper

                return HTTPXClientWrapper(**client_kwargs)
            raise ValueError("AIBRIX_BATCH_RUNTIME_HTTP_CLIENT must be 'httpx_client'")
        return super().__call__(**client_kwargs)


class HTTPClient(metaclass=_HTTPClientMeta):
    client_label = "http-client"
    instantiated_log_message = "HTTP client instantiated."
    closed_log_message = "HTTP client closed"

    def __init__(
        self,
        *,
        client_id: Optional[str] = None,
        purpose: Optional[str] = None,
        telemetry_enabled: Optional[bool] = None,
        telemetry_interval_seconds: Optional[float] = None,
        **client_kwargs: Any,
    ) -> None:
        self.client_id = (
            client_id or f"{self.client_label}-{next(CLIENT_ID_COUNTER):04d}"
        )
        self.purpose = purpose
        self._client_kwargs = client_kwargs
        self._telemetry_enabled = (
            telemetry_enabled if telemetry_enabled is not None else _telemetry_enabled()
        )
        self._telemetry_interval_seconds = (
            max(telemetry_interval_seconds, 0.0)
            if telemetry_interval_seconds is not None
            else _telemetry_interval_seconds()
        )
        self._telemetry_task: Optional[asyncio.Task[None]] = None
        self._purpose_state = (
            _PURPOSE_TELEMETRY_STATES.setdefault(purpose, _HTTPPurposeTelemetryState())
            if purpose is not None
            else None
        )
        self._telemetry_stats = (
            self._purpose_state.stats
            if self._purpose_state is not None
            else HTTPTelemetryStats()
        )
        self._last_telemetry_emitted_at: Optional[float] = None
        self._stopped = False

    def start(self):
        """Instantiate the client. Call from the FastAPI startup hook."""
        if self._stopped:
            raise RuntimeError(
                f"{type(self).__name__} cannot be reused after stop(); "
                "create a new wrapper"
            )
        if self.async_client is None:
            self.async_client = self._create_async_client()
            try:
                now = asyncio.get_running_loop().time()
            except RuntimeError:
                now = None
            if self._purpose_state is None:
                self._last_telemetry_emitted_at = now
            else:
                self._purpose_state.register(client_id=self.client_id, now=now)
            logger.info(
                self.instantiated_log_message,
                client_id=self.client_id,
                purpose=self.purpose,
                id=id(self.async_client),
                telemetry_enabled=self._telemetry_enabled,
                telemetry_interval_seconds=self._telemetry_interval_seconds,
            )  # type: ignore[call-arg]
        self._ensure_telemetry_task()

    async def stop(self):
        """Gracefully shutdown. Call from FastAPI shutdown hook."""
        self._stopped = True
        if self._telemetry_task is not None:
            self._telemetry_task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await self._telemetry_task
            self._telemetry_task = None
        if self.async_client is None:
            return
        should_emit_final = True
        if self._purpose_state is not None:
            self._purpose_state.release(client_id=self.client_id)
            should_emit_final = self._purpose_state.active_clients == 0
        logger.debug(
            "http.client.status",
            is_closed=self.is_closed,
            id=id(self.async_client),
            client_id=self.client_id,
            purpose=self.purpose,
        )  # type: ignore[call-arg]
        if self._telemetry_enabled and should_emit_final:
            try:
                now = asyncio.get_running_loop().time()
                elapsed = max(
                    now - (self._last_telemetry_emitted_at_or(now) or now),
                    1e-9,
                )
            except RuntimeError:
                now = None
                elapsed = max(self._telemetry_interval_seconds, 1.0)
            self._emit_telemetry(final=True, elapsed=elapsed)
            self._set_last_telemetry_emitted_at(now)
        await self._close_async_client()
        self.async_client = None
        logger.info(self.closed_log_message, client_id=self.client_id)  # type: ignore[call-arg]

    def __call__(self):
        """Calling the instantiated wrapper returns the wrapped singleton."""
        assert self.async_client is not None
        return self.async_client

    def __getattr__(self, name: str):
        self.start()
        assert self.async_client is not None
        return getattr(self.async_client, name)

    async def aclose(self) -> None:
        await self.stop()

    async def __aenter__(self):
        self.start()
        return self

    async def __aexit__(self, *args) -> None:
        del args
        await self.stop()

    async def request(
        self,
        method: str,
        url: str,
        *,
        telemetry_call_site: Optional[str] = None,
        **kwargs: Any,
    ) -> httpx.Response:
        self.start()
        assert self.async_client is not None
        start_time = asyncio.get_running_loop().time()
        if self._telemetry_enabled:
            self._telemetry_stats.record_start(call_site=telemetry_call_site)
        failed = False
        exception_type: Optional[str] = None
        try:
            return await self._request(method, url, **kwargs)
        except Exception as exc:
            failed = True
            exception_type = type(exc).__name__
            if telemetry_call_site is not None:
                logger.debug(
                    "HTTP request failed",
                    client_id=self.client_id,
                    purpose=self.purpose,
                    call_site=telemetry_call_site,
                    method=method,
                    url=url,
                    exception_type=exception_type,
                )  # type: ignore[call-arg]
            raise
        finally:
            if self._telemetry_enabled:
                self._telemetry_stats.record_completion(
                    failed=failed,
                    latency_seconds=asyncio.get_running_loop().time() - start_time,
                    call_site=telemetry_call_site,
                    exception_type=exception_type,
                )

    @contextlib.asynccontextmanager
    async def stream(
        self,
        method: str,
        url: str,
        *,
        telemetry_call_site: Optional[str] = None,
        **kwargs: Any,
    ):
        self.start()
        assert self.async_client is not None
        start_time = asyncio.get_running_loop().time()
        if self._telemetry_enabled:
            self._telemetry_stats.record_start(call_site=telemetry_call_site)
        failed = False
        exception_type: Optional[str] = None
        try:
            async with self._stream(method, url, **kwargs) as response:
                yield response
        except Exception as exc:
            failed = True
            exception_type = type(exc).__name__
            if telemetry_call_site is not None:
                logger.debug(
                    "HTTP stream failed",
                    client_id=self.client_id,
                    purpose=self.purpose,
                    call_site=telemetry_call_site,
                    method=method,
                    url=url,
                    exception_type=exception_type,
                )  # type: ignore[call-arg]
            raise
        finally:
            if self._telemetry_enabled:
                self._telemetry_stats.record_completion(
                    failed=failed,
                    latency_seconds=asyncio.get_running_loop().time() - start_time,
                    call_site=telemetry_call_site,
                    exception_type=exception_type,
                )

    async def get(
        self, url: str, *, telemetry_call_site: Optional[str] = None, **kwargs: Any
    ) -> httpx.Response:
        return await self.request(
            "GET",
            url,
            telemetry_call_site=telemetry_call_site,
            **kwargs,
        )

    async def post(
        self, url: str, *, telemetry_call_site: Optional[str] = None, **kwargs: Any
    ) -> httpx.Response:
        return await self.request(
            "POST",
            url,
            telemetry_call_site=telemetry_call_site,
            **kwargs,
        )

    async def put(
        self, url: str, *, telemetry_call_site: Optional[str] = None, **kwargs: Any
    ) -> httpx.Response:
        return await self.request(
            "PUT",
            url,
            telemetry_call_site=telemetry_call_site,
            **kwargs,
        )

    async def delete(
        self, url: str, *, telemetry_call_site: Optional[str] = None, **kwargs: Any
    ) -> httpx.Response:
        return await self.request(
            "DELETE",
            url,
            telemetry_call_site=telemetry_call_site,
            **kwargs,
        )

    async def patch(
        self, url: str, *, telemetry_call_site: Optional[str] = None, **kwargs: Any
    ) -> httpx.Response:
        return await self.request(
            "PATCH",
            url,
            telemetry_call_site=telemetry_call_site,
            **kwargs,
        )

    async def head(
        self, url: str, *, telemetry_call_site: Optional[str] = None, **kwargs: Any
    ) -> httpx.Response:
        return await self.request(
            "HEAD",
            url,
            telemetry_call_site=telemetry_call_site,
            **kwargs,
        )

    async def options(
        self, url: str, *, telemetry_call_site: Optional[str] = None, **kwargs: Any
    ) -> httpx.Response:
        return await self.request(
            "OPTIONS",
            url,
            telemetry_call_site=telemetry_call_site,
            **kwargs,
        )

    def _ensure_telemetry_task(self) -> None:
        if (
            not self._telemetry_enabled
            or self._telemetry_interval_seconds <= 0
            or self._telemetry_task is not None
        ):
            return
        try:
            loop = asyncio.get_running_loop()
        except RuntimeError:
            return
        if self._purpose_state is not None and not self._purpose_state.ensure_emitter(
            client_id=self.client_id,
            now=loop.time(),
        ):
            return
        self._telemetry_task = asyncio.create_task(self._log_telemetry())

    async def _log_telemetry(self) -> None:
        while True:
            await asyncio.sleep(self._telemetry_interval_seconds)
            now = asyncio.get_running_loop().time()
            elapsed = max(now - (self._last_telemetry_emitted_at_or(now) or now), 1e-9)
            self._emit_telemetry(final=False, elapsed=elapsed)
            self._set_last_telemetry_emitted_at(now)

    def _emit_telemetry(self, *, final: bool, elapsed: float) -> None:
        if not self._telemetry_enabled:
            return
        snapshot = self._telemetry_stats.snapshot(reset_window=True)
        elapsed = max(elapsed, 1e-9)
        event = "HTTP client telemetry summary"
        base_tags = (
            T("event", event),
            T("client_id", self.client_id),
            T("purpose", self.purpose or ""),
            T("backend", self.client_label),
            T("final", str(final).lower()),
            T("is_closed", str(self.is_closed).lower()),
            T("log_context", "client_id,purpose,backend,final,is_closed"),
            T("log_context_bool", "final,is_closed"),
            T("log_context_null_if_empty", "purpose"),
        )
        numeric_fields = {
            "started_qps": round(snapshot.window_started / elapsed, 3),
            "completed_qps": round(snapshot.window_completed / elapsed, 3),
            "failed_qps": round(snapshot.window_failed / elapsed, 3),
            "inflight": snapshot.inflight,
            "max_inflight": snapshot.max_inflight,
            "started": snapshot.started,
            "completed": snapshot.completed,
            "failed": snapshot.failed,
            "window_started": snapshot.window_started,
            "window_completed": snapshot.window_completed,
            "window_failed": snapshot.window_failed,
            "registered_clients": self._registered_clients(),
        }
        if snapshot.avg_latency_seconds is not None:
            numeric_fields["avg_latency_seconds"] = round(
                snapshot.avg_latency_seconds,
                6,
            )
        if snapshot.p90_latency_seconds is not None:
            numeric_fields["p90_latency_seconds"] = round(
                snapshot.p90_latency_seconds,
                6,
            )
        for metric_field, value in numeric_fields.items():
            Emitter.gauge(
                f"{metrics_names.METRIC_METADATA_HTTP_CLIENT_TELEMETRY}.{metric_field}",
                float(value),
                *base_tags,
                T("log_field", metric_field),
            )
        for exception_type, exception_count in snapshot.exception_types.items():
            Emitter.gauge(
                metrics_names.METRIC_METADATA_HTTP_CLIENT_TELEMETRY_EXCEPTION_COUNT,
                float(exception_count),
                *base_tags,
                T("log_map", "exception_types"),
                T("log_key", exception_type),
                T("exception_type", exception_type),
            )
        for bucket_index, bucket_count in enumerate(snapshot.latency_histogram):
            Emitter.gauge(
                metrics_names.METRIC_METADATA_HTTP_CLIENT_TELEMETRY_LATENCY_BUCKET_COUNT,
                float(bucket_count),
                *base_tags,
                T("log_list", "latency_histogram"),
                T("log_index", str(bucket_index)),
                T("latency_histogram_bucket", str(bucket_index)),
            )
        Emitter.gauge(
            metrics_names.METRIC_METADATA_HTTP_CLIENT_TELEMETRY_SUMMARY,
            1.0,
            *base_tags,
            T("log_default_null", "avg_latency_seconds,p90_latency_seconds"),
            T("log_default_map", "exception_types"),
            T("flush", "true"),
        )
        for call_site_snapshot in snapshot.call_sites:
            logger.debug(
                "HTTP client telemetry by call site",
                client_id=self.client_id,
                purpose=self.purpose,
                backend=self.client_label,
                final=final,
                call_site=call_site_snapshot.call_site,
                started=call_site_snapshot.started,
                completed=call_site_snapshot.completed,
                failed=call_site_snapshot.failed,
                inflight=call_site_snapshot.inflight,
                exception_types=call_site_snapshot.exception_types,
            )  # type: ignore[call-arg]

    def _last_telemetry_emitted_at_or(self, default: float) -> Optional[float]:
        if self._purpose_state is not None:
            return self._purpose_state.last_emitted_at or default
        return self._last_telemetry_emitted_at or default

    def _set_last_telemetry_emitted_at(self, value: Optional[float]) -> None:
        if self._purpose_state is not None:
            self._purpose_state.last_emitted_at = value
        else:
            self._last_telemetry_emitted_at = value

    def _registered_clients(self) -> int:
        if self._purpose_state is not None:
            return self._purpose_state.active_clients
        return 0 if self.is_closed else 1

    def _create_async_client(self) -> Any:
        raise NotImplementedError

    async def _close_async_client(self) -> None:
        raise NotImplementedError

    async def _request(self, method: str, url: str, **kwargs: Any) -> httpx.Response:
        del method, url, kwargs
        raise NotImplementedError

    def _stream(self, method: str, url: str, **kwargs: Any) -> AsyncContextManager[Any]:
        del method, url, kwargs
        raise NotImplementedError

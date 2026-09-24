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
import asyncio
import bisect
import contextlib
from collections import Counter
from dataclasses import dataclass, field
from itertools import count
from typing import Optional

import httpx

from aibrix import envs
from aibrix.logger import init_logger

logger = init_logger(__name__)

_CLIENT_ID_COUNTER = count(1)
_LATENCY_BUCKET_MAX_SECONDS = 600.0
_LATENCY_BUCKET_MIN_SECONDS = 0.001


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


def _round_optional(value: Optional[float]) -> Optional[float]:
    if value is None:
        return None
    return round(value, 6)


@dataclass(slots=True)
class _HTTPXTelemetrySnapshot:
    started: int
    completed: int
    failed: int
    inflight: int
    max_inflight: int
    window_started: int
    window_completed: int
    window_failed: int
    p50_latency_seconds: Optional[float]
    p95_latency_seconds: Optional[float]
    exception_types: dict[str, int]
    latency_histogram: tuple[int, ...]
    call_sites: tuple["_HTTPXTelemetryCallSiteSnapshot", ...]


@dataclass(slots=True)
class _HTTPXTelemetryCallSiteStats:
    started: int = 0
    completed: int = 0
    failed: int = 0
    inflight: int = 0
    exception_types: Counter[str] = field(default_factory=Counter)


@dataclass(slots=True)
class _HTTPXTelemetryCallSiteSnapshot:
    call_site: str
    started: int
    completed: int
    failed: int
    inflight: int
    exception_types: dict[str, int]


@dataclass(slots=True)
class _HTTPXTelemetryStats:
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
    _window_exception_types: Counter[str] = field(default_factory=Counter)
    _call_sites: dict[str, _HTTPXTelemetryCallSiteStats] = field(default_factory=dict)

    def record_start(self, *, call_site: Optional[str] = None) -> None:
        self.started += 1
        self.window_started += 1
        self.inflight += 1
        self.max_inflight = max(self.max_inflight, self.inflight)
        if call_site is not None:
            site_stats = self._call_sites.setdefault(
                call_site,
                _HTTPXTelemetryCallSiteStats(),
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
        if failed and exception_type is not None:
            self._window_exception_types[exception_type] += 1
        if call_site is not None:
            site_stats = self._call_sites.setdefault(
                call_site,
                _HTTPXTelemetryCallSiteStats(),
            )
            site_stats.completed += 1
            if failed:
                site_stats.failed += 1
                if exception_type is not None:
                    site_stats.exception_types[exception_type] += 1
            site_stats.inflight = max(site_stats.inflight - 1, 0)

    def snapshot(self, *, reset_window: bool) -> _HTTPXTelemetrySnapshot:
        snapshot = _HTTPXTelemetrySnapshot(
            started=self.started,
            completed=self.completed,
            failed=self.failed,
            inflight=self.inflight,
            max_inflight=self.max_inflight,
            window_started=self.window_started,
            window_completed=self.window_completed,
            window_failed=self.window_failed,
            p50_latency_seconds=self._histogram_percentile(0.50),
            p95_latency_seconds=self._histogram_percentile(0.95),
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
            self._window_exception_types = Counter()
            self._call_sites = {}
        return snapshot

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

    def _call_site_snapshots(self) -> list[_HTTPXTelemetryCallSiteSnapshot]:
        snapshots: list[_HTTPXTelemetryCallSiteSnapshot] = []
        for call_site, stats in sorted(
            self._call_sites.items(),
            key=lambda item: (item[1].failed, item[1].completed, item[0]),
            reverse=True,
        ):
            snapshots.append(
                _HTTPXTelemetryCallSiteSnapshot(
                    call_site=call_site,
                    started=stats.started,
                    completed=stats.completed,
                    failed=stats.failed,
                    inflight=stats.inflight,
                    exception_types=dict(sorted(stats.exception_types.items())),
                )
            )
        return snapshots


class HTTPXClientWrapper:
    async_client: Optional[httpx.AsyncClient] = None

    def __init__(
        self,
        *,
        client_id: Optional[str] = None,
        telemetry_enabled: Optional[bool] = None,
        telemetry_interval_seconds: Optional[float] = None,
        **client_kwargs,
    ) -> None:
        self.client_id = client_id or f"httpx-client-{next(_CLIENT_ID_COUNTER):04d}"
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
        self._telemetry_stats = _HTTPXTelemetryStats()
        self._last_telemetry_emitted_at: Optional[float] = None
        self._stopped = False

    def start(self):
        """Instantiate the client. Call from the FastAPI startup hook."""
        if self._stopped:
            raise RuntimeError(
                "HTTPXClientWrapper cannot be reused after stop(); create a new wrapper"
            )
        if self.async_client is None:
            self.async_client = httpx.AsyncClient(**self._client_kwargs)
            self._telemetry_stats = _HTTPXTelemetryStats()
            try:
                self._last_telemetry_emitted_at = asyncio.get_running_loop().time()
            except RuntimeError:
                self._last_telemetry_emitted_at = None
            logger.info(
                "httpx.AsyncClient instantiated.",
                client_id=self.client_id,
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
        logger.debug(
            "httpx.async_client.status",
            is_closed=self.async_client.is_closed,
            id=id(self.async_client),
            client_id=self.client_id,
        )  # type: ignore[call-arg]
        if self._telemetry_enabled:
            try:
                now = asyncio.get_running_loop().time()
                elapsed = max(
                    now - (self._last_telemetry_emitted_at or now),
                    1e-9,
                )
            except RuntimeError:
                now = None
                elapsed = max(self._telemetry_interval_seconds, 1.0)
            self._emit_telemetry(
                final=True,
                elapsed=elapsed,
            )
            self._last_telemetry_emitted_at = now
        await self.async_client.aclose()
        self.async_client = None
        logger.info("httpx.AsyncClient closed", client_id=self.client_id)  # type: ignore[call-arg]

    def __call__(self):
        """Calling the instantiated HTTPXClientWrapper returns the wrapped singleton."""
        # Ensure we don't use it if not started / running
        assert self.async_client is not None
        return self.async_client

    def __getattr__(self, name: str):
        self.start()
        assert self.async_client is not None
        return getattr(self.async_client, name)

    @property
    def is_closed(self) -> bool:
        return self.async_client.is_closed if self.async_client is not None else True

    async def aclose(self) -> None:
        await self.stop()

    async def __aenter__(self) -> "HTTPXClientWrapper":
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
        **kwargs,
    ) -> httpx.Response:
        self.start()
        assert self.async_client is not None
        start_time = asyncio.get_running_loop().time()
        if self._telemetry_enabled:
            self._telemetry_stats.record_start(call_site=telemetry_call_site)
        failed = False
        exception_type: Optional[str] = None
        try:
            return await self.async_client.request(method, url, **kwargs)
        except Exception as exc:
            failed = True
            exception_type = type(exc).__name__
            if telemetry_call_site is not None:
                logger.debug(
                    "HTTPX request failed",
                    client_id=self.client_id,
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
        **kwargs,
    ):
        self.start()
        assert self.async_client is not None
        start_time = asyncio.get_running_loop().time()
        if self._telemetry_enabled:
            self._telemetry_stats.record_start(call_site=telemetry_call_site)
        failed = False
        exception_type: Optional[str] = None
        try:
            async with self.async_client.stream(method, url, **kwargs) as response:
                yield response
        except Exception as exc:
            failed = True
            exception_type = type(exc).__name__
            if telemetry_call_site is not None:
                logger.debug(
                    "HTTPX stream failed",
                    client_id=self.client_id,
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
        self, url: str, *, telemetry_call_site: Optional[str] = None, **kwargs
    ) -> httpx.Response:
        return await self.request(
            "GET",
            url,
            telemetry_call_site=telemetry_call_site,
            **kwargs,
        )

    async def post(
        self, url: str, *, telemetry_call_site: Optional[str] = None, **kwargs
    ) -> httpx.Response:
        return await self.request(
            "POST",
            url,
            telemetry_call_site=telemetry_call_site,
            **kwargs,
        )

    async def put(
        self, url: str, *, telemetry_call_site: Optional[str] = None, **kwargs
    ) -> httpx.Response:
        return await self.request(
            "PUT",
            url,
            telemetry_call_site=telemetry_call_site,
            **kwargs,
        )

    async def delete(
        self, url: str, *, telemetry_call_site: Optional[str] = None, **kwargs
    ) -> httpx.Response:
        return await self.request(
            "DELETE",
            url,
            telemetry_call_site=telemetry_call_site,
            **kwargs,
        )

    async def patch(
        self, url: str, *, telemetry_call_site: Optional[str] = None, **kwargs
    ) -> httpx.Response:
        return await self.request(
            "PATCH",
            url,
            telemetry_call_site=telemetry_call_site,
            **kwargs,
        )

    async def head(
        self, url: str, *, telemetry_call_site: Optional[str] = None, **kwargs
    ) -> httpx.Response:
        return await self.request(
            "HEAD",
            url,
            telemetry_call_site=telemetry_call_site,
            **kwargs,
        )

    async def options(
        self, url: str, *, telemetry_call_site: Optional[str] = None, **kwargs
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
            asyncio.get_running_loop()
        except RuntimeError:
            return
        self._telemetry_task = asyncio.create_task(self._log_telemetry())

    async def _log_telemetry(self) -> None:
        while True:
            await asyncio.sleep(self._telemetry_interval_seconds)
            now = asyncio.get_running_loop().time()
            elapsed = max(now - (self._last_telemetry_emitted_at or now), 1e-9)
            self._emit_telemetry(final=False, elapsed=elapsed)
            self._last_telemetry_emitted_at = now

    def _emit_telemetry(self, *, final: bool, elapsed: float) -> None:
        if not self._telemetry_enabled:
            return
        snapshot = self._telemetry_stats.snapshot(reset_window=True)
        elapsed = max(elapsed, 1e-9)
        logger.info(
            "HTTPX client telemetry summary",
            client_id=self.client_id,
            final=final,
            started_qps=round(snapshot.window_started / elapsed, 3),
            completed_qps=round(snapshot.window_completed / elapsed, 3),
            failed_qps=round(snapshot.window_failed / elapsed, 3),
            inflight=snapshot.inflight,
            max_inflight=snapshot.max_inflight,
            started=snapshot.started,
            completed=snapshot.completed,
            failed=snapshot.failed,
            window_started=snapshot.window_started,
            window_completed=snapshot.window_completed,
            window_failed=snapshot.window_failed,
            p50_latency_seconds=_round_optional(snapshot.p50_latency_seconds),
            p95_latency_seconds=_round_optional(snapshot.p95_latency_seconds),
            exception_types=snapshot.exception_types,
            latency_histogram=snapshot.latency_histogram,
            is_closed=self.is_closed,
        )  # type: ignore[call-arg]
        for call_site_snapshot in snapshot.call_sites:
            logger.debug(
                "HTTPX client telemetry by call site",
                client_id=self.client_id,
                final=final,
                call_site=call_site_snapshot.call_site,
                started=call_site_snapshot.started,
                completed=call_site_snapshot.completed,
                failed=call_site_snapshot.failed,
                inflight=call_site_snapshot.inflight,
                exception_types=call_site_snapshot.exception_types,
            )  # type: ignore[call-arg]

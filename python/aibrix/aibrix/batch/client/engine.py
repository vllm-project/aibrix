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
"""Dispatch engine: the policy-driven concurrent loop.

Owns "how requests get pushed through": ordering (Scheduler), endpoint choice
(Router), concurrency / QPS pacing (plain parameters), and failover across
channels. Knows nothing batch-specific -- the caller injects a request stream
and an ``on_result`` sink. The same engine backs both batch jobs and benchmarks.
"""

from __future__ import annotations

import asyncio
from dataclasses import dataclass
from math import ceil, isfinite
from threading import Lock
from typing import (
    Any,
    AsyncIterable,
    Awaitable,
    Callable,
    Optional,
    Set,
    Union,
)

from aibrix.batch.client.channel import InferenceRequest, Response
from aibrix.batch.client.concurrency import (
    ConcurrencyController,
    FixedConcurrencyController,
    LLMAdaptiveConcurrencyController,
    concurrency_outcome_from_result,
)
from aibrix.batch.client.errors import InferenceError, InferenceErrorCode
from aibrix.batch.client.policy import FIFO, RoundRobin, Router, Scheduler
from aibrix.batch.client.source import CapacitySignal, EndpointSource
from aibrix.logger import init_logger

logger = init_logger(__name__)

# Called once per request with (request, response, error). Exactly one of
# response / error is non-None. May be sync or async.
OnResult = Callable[
    [InferenceRequest, Optional[Response], Optional[InferenceError]],
    Union[Any, Awaitable[Any]],
]

# Concurrency is a parameter, not a layer: a constant now, a callable later for
# adaptive control. ``None`` means "derive from the source's capacity".
ConcurrencyLimit = Union[int, Callable[[], int]]


@dataclass(frozen=True, slots=True)
class RetryConfig:
    max_retries: int = 2
    base_delay_seconds: float = 0.0
    max_delay_seconds: float = 5.0
    no_endpoint_max_retries: Optional[int] = None

    def __post_init__(self) -> None:
        if self.max_retries < 0:
            raise ValueError("max_retries must be >= 0")
        if self.base_delay_seconds < 0:
            raise ValueError("base_delay_seconds must be >= 0")
        if self.max_delay_seconds < 0:
            raise ValueError("max_delay_seconds must be >= 0")
        if (
            self.no_endpoint_max_retries is not None
            and self.no_endpoint_max_retries < 0
        ):
            raise ValueError("no_endpoint_max_retries must be >= 0")

    def no_endpoint_retries(self) -> int:
        if self.no_endpoint_max_retries is None:
            return self.max_retries
        return self.no_endpoint_max_retries


@dataclass(frozen=True, slots=True)
class DispatchStatsSnapshot:
    started: int
    completed: int
    failed: int
    inflight: int
    limit: int
    max_inflight: int
    window_started: int
    window_completed: int
    window_failed: int
    avg_latency_seconds: Optional[float]
    p95_latency_seconds: Optional[float]


class DispatchStats:
    """Lightweight per-run dispatch counters for logs and tests."""

    def __init__(self) -> None:
        self._lock = Lock()
        self._started = 0
        self._completed = 0
        self._failed = 0
        self._inflight = 0
        self._limit = 0
        self._max_inflight = 0
        self._window_started = 0
        self._window_completed = 0
        self._window_failed = 0
        self._window_latencies: list[float] = []

    def record_start(self, *, limit: int, inflight: int) -> None:
        with self._lock:
            self._started += 1
            self._window_started += 1
            self._limit = max(int(limit), 1)
            self._inflight = max(int(inflight), 0)
            self._max_inflight = max(self._max_inflight, self._inflight)

    def record_complete(
        self,
        *,
        success: bool,
        latency_seconds: float,
        limit: int,
        inflight: int,
    ) -> None:
        with self._lock:
            self._completed += 1
            self._window_completed += 1
            if not success:
                self._failed += 1
                self._window_failed += 1
            self._limit = max(int(limit), 1)
            self._inflight = max(int(inflight), 0)
            self._window_latencies.append(max(float(latency_seconds), 0.0))

    def snapshot(self, *, reset_window: bool = False) -> DispatchStatsSnapshot:
        with self._lock:
            latencies = list(self._window_latencies)
            snapshot = DispatchStatsSnapshot(
                started=self._started,
                completed=self._completed,
                failed=self._failed,
                inflight=self._inflight,
                limit=self._limit,
                max_inflight=self._max_inflight,
                window_started=self._window_started,
                window_completed=self._window_completed,
                window_failed=self._window_failed,
                avg_latency_seconds=_average(latencies),
                p95_latency_seconds=_percentile(latencies, 0.95),
            )
            if reset_window:
                self._window_started = 0
                self._window_completed = 0
                self._window_failed = 0
                self._window_latencies = []
            return snapshot


class DispatchEngine:
    def __init__(
        self,
        source: EndpointSource,
        *,
        router: Optional[Router] = None,
        scheduler: Optional[Scheduler] = None,
        max_retries: int = 2,
        retry: Optional[RetryConfig] = None,
    ) -> None:
        self._source = source
        self._router: Router = router or RoundRobin()
        self._scheduler: Scheduler = scheduler or FIFO()
        self._retry = retry or RetryConfig(max_retries=max_retries)

    @property
    def source(self) -> EndpointSource:
        """The endpoint source this engine dispatches to. Lets a caller rebuild
        an equivalent engine (e.g. wrap a pre-built engine in a Runtime)."""
        return self._source

    async def send_one(self, request: InferenceRequest) -> Response:
        """Single shot: pick an endpoint, send, fail over on error."""
        return await self._send_with_failover(request)

    async def capacity(self) -> CapacitySignal:
        """Expose the source's advertised concurrency capacity to callers that
        need to choose between serial and concurrent orchestration."""
        return await self._source.capacity()

    async def run(
        self,
        requests: AsyncIterable[InferenceRequest],
        on_result: OnResult,
        *,
        max_concurrency: Optional[ConcurrencyLimit] = None,
        qps: Optional[float] = None,
        adaptive_concurrency: bool = False,
        adaptive_max_factor: float = 1.0,
        adaptive_max_concurrency: Optional[int] = None,
        concurrency_controller: Optional[ConcurrencyController] = None,
        stats: Optional[DispatchStats] = None,
    ) -> None:
        """Drive ``requests`` to completion under a concurrency cap.

        A per-request inference failure is reported through ``on_result`` and
        never raised. Anything else -- a feeder error, or an ``on_result`` that
        itself raises (e.g. a caller's stop condition) -- stops scheduling,
        drains in-flight work, and re-raises the first such error.
        """
        admission = _ConcurrencyAdmission(
            await self._resolve_concurrency_controller(
                max_concurrency=max_concurrency,
                adaptive_concurrency=adaptive_concurrency,
                adaptive_max_factor=adaptive_max_factor,
                adaptive_max_concurrency=adaptive_max_concurrency,
                concurrency_controller=concurrency_controller,
            )
        )
        gate = _QpsGate(qps) if qps else None
        inflight: Set[asyncio.Task[None]] = set()
        first_error: list[BaseException] = []

        def _on_done(task: asyncio.Task[None]) -> None:
            inflight.discard(task)
            if not task.cancelled():
                exc = task.exception()
                if exc is not None and not first_error:
                    first_error.append(exc)

        scheduled = self._scheduler.schedule(requests).__aiter__()
        try:
            while not first_error:
                await admission.acquire()
                if first_error:
                    await admission.release()
                    break
                try:
                    if gate is not None:
                        await gate.wait()
                    request = await scheduled.__anext__()
                except StopAsyncIteration:
                    await admission.release()
                    break
                except BaseException as exc:
                    await admission.release()
                    if not first_error:
                        first_error.append(exc)
                    break

                if stats is not None:
                    stats.record_start(
                        limit=admission.limit(),
                        inflight=admission.inflight(),
                    )
                task = asyncio.create_task(
                    self._process(request, on_result, admission, first_error, stats)
                )
                inflight.add(task)
                task.add_done_callback(_on_done)
        except BaseException as exc:  # feeder raised; drain then re-raise
            if not first_error:
                first_error.append(exc)

        if inflight:
            await asyncio.gather(*inflight, return_exceptions=True)
        if first_error:
            raise first_error[0]

    async def _process(
        self,
        request: InferenceRequest,
        on_result: OnResult,
        admission: "_ConcurrencyAdmission",
        first_error: list[BaseException],
        stats: Optional[DispatchStats],
    ) -> None:
        outcome = None
        started = asyncio.get_running_loop().time()
        try:
            try:
                response = await self._send_with_failover(request)
            except InferenceError as err:
                outcome = concurrency_outcome_from_result(
                    None,
                    err,
                    latency_seconds=asyncio.get_running_loop().time() - started,
                )
                await _maybe_await(on_result(request, None, err))
            else:
                outcome = concurrency_outcome_from_result(
                    response,
                    None,
                    latency_seconds=asyncio.get_running_loop().time() - started,
                )
                await _maybe_await(on_result(request, response, None))
        except BaseException as exc:
            if not first_error:
                first_error.append(exc)
            raise
        finally:
            latency = asyncio.get_running_loop().time() - started
            limit, inflight = await admission.release(outcome)
            if stats is not None:
                stats.record_complete(
                    success=outcome.success if outcome is not None else False,
                    latency_seconds=latency,
                    limit=limit,
                    inflight=inflight,
                )

    async def _resolve_concurrency_controller(
        self,
        *,
        max_concurrency: Optional[ConcurrencyLimit],
        adaptive_concurrency: bool,
        adaptive_max_factor: float,
        adaptive_max_concurrency: Optional[int],
        concurrency_controller: Optional[ConcurrencyController],
    ) -> ConcurrencyController:
        if concurrency_controller is not None:
            return concurrency_controller
        limit = await self._resolve_limit(max_concurrency)
        if adaptive_concurrency:
            max_limit = (
                max(int(adaptive_max_concurrency), 1)
                if adaptive_max_concurrency is not None
                else self._adaptive_max_limit(limit, adaptive_max_factor)
            )
            return LLMAdaptiveConcurrencyController(
                initial_limit=min(limit, max_limit),
                max_limit=max_limit,
            )
        return FixedConcurrencyController(limit)

    async def _send_with_failover(self, request: InferenceRequest) -> Response:
        causes: list[str] = []
        last_error: Optional[InferenceError] = None
        attempted_channel = False
        endpoint_attempt = 0
        send_attempt = 0
        while True:
            channels = await self._source.channels()
            if not channels:
                no_endpoint = InferenceError(
                    InferenceErrorCode.NO_ENDPOINT, "no reachable endpoint"
                )
                last_error = no_endpoint
                causes.append(str(no_endpoint))
                if endpoint_attempt < self._retry.no_endpoint_retries():
                    await self._refresh_source()
                    await self._sleep_before_retry(endpoint_attempt)
                    endpoint_attempt += 1
                    continue
                if not attempted_channel:
                    raise no_endpoint
                break
            channel = self._router.pick(request, channels)
            if channel is None:
                break
            attempted_channel = True
            try:
                return await channel.send(request)
            except InferenceError as ex:
                last_error = ex
                causes.append(str(ex))
                if not _should_retry(ex):
                    raise ex
                await self._report_channel_error(channel.id, ex)
                if send_attempt < self._retry.max_retries:
                    await self._sleep_before_retry(send_attempt)
                    send_attempt += 1
                    continue
                break
        raise InferenceError(
            InferenceErrorCode.ALL_ENDPOINTS_FAILED,
            "all endpoints failed",
            causes=causes,
            status_code=last_error.status_code if last_error else None,
            response_body=last_error.response_body if last_error else None,
            retryable=last_error.retryable if last_error else None,
        )

    async def _resolve_limit(self, max_concurrency: Optional[ConcurrencyLimit]) -> int:
        if max_concurrency is None:
            capacity = await self._source.capacity()
            return max(capacity.count, 1)
        if callable(max_concurrency):
            return max(int(max_concurrency()), 1)
        return max(int(max_concurrency), 1)

    async def _sleep_before_retry(self, attempt: int) -> None:
        if self._retry.base_delay_seconds <= 0:
            return
        delay = min(
            self._retry.base_delay_seconds * (2**attempt),
            self._retry.max_delay_seconds,
        )
        await asyncio.sleep(delay)

    async def _refresh_source(self) -> None:
        refresh = getattr(self._source, "refresh", None)
        if callable(refresh):
            await _maybe_await(refresh())

    async def _report_channel_error(
        self, channel_id: str, error: InferenceError
    ) -> None:
        report = getattr(self._source, "report_channel_error", None)
        if callable(report):
            await _maybe_await(report(channel_id, error))

    @staticmethod
    def _adaptive_max_limit(initial_limit: int, factor: float) -> int:
        try:
            safe_factor = float(factor)
        except (TypeError, ValueError):
            safe_factor = 1.0
        if not isfinite(safe_factor):
            safe_factor = 1.0
        safe_factor = max(safe_factor, 1.0)
        return max(int(initial_limit), ceil(int(initial_limit) * safe_factor))


class _ConcurrencyAdmission:
    def __init__(self, controller: ConcurrencyController) -> None:
        self._controller = controller
        self._inflight = 0
        self._condition = asyncio.Condition()

    async def acquire(self) -> None:
        while True:
            async with self._condition:
                await self._condition.wait_for(
                    lambda: self._inflight < max(int(self._controller.limit()), 1)
                )
                delay = _admission_delay_seconds(self._controller)
                if delay <= 0:
                    self._inflight += 1
                    return
                try:
                    await asyncio.wait_for(self._condition.wait(), timeout=delay)
                except asyncio.TimeoutError:
                    pass

    async def release(self, outcome: Any = None) -> tuple[int, int]:
        async with self._condition:
            self._inflight -= 1
            if outcome is not None:
                self._controller.on_complete(outcome)
            self._condition.notify_all()
            return self.limit(), self._inflight

    def limit(self) -> int:
        return max(int(self._controller.limit()), 1)

    def inflight(self) -> int:
        return self._inflight


async def _maybe_await(value: Any) -> Any:
    if asyncio.iscoroutine(value):
        return await value
    return value


def _should_retry(error: InferenceError) -> bool:
    # Preserve previous behavior for errors created before retryable was added:
    # client-layer transport failures from tests/custom channels remain retryable.
    return True if error.retryable is None else error.retryable


def _admission_delay_seconds(controller: ConcurrencyController) -> float:
    delay = getattr(controller, "admission_delay_seconds", lambda: 0.0)()
    return max(float(delay), 0.0)


def _average(values: list[float]) -> Optional[float]:
    if not values:
        return None
    return sum(values) / len(values)


def _percentile(values: list[float], percentile: float) -> Optional[float]:
    if not values:
        return None
    ordered = sorted(values)
    index = min(ceil(len(ordered) * percentile) - 1, len(ordered) - 1)
    return ordered[max(index, 0)]


class _QpsGate:
    """Minimal request-rate limiter (token spacing). Distinct from the
    concurrency cap: concurrency bounds in-flight count, qps bounds start rate."""

    def __init__(self, qps: float) -> None:
        self._interval = 1.0 / qps
        self._next = 0.0
        self._lock = asyncio.Lock()

    async def wait(self) -> None:
        async with self._lock:
            now = asyncio.get_running_loop().time()
            if self._next <= now:
                self._next = now + self._interval
                return
            delay = self._next - now
            self._next += self._interval
        await asyncio.sleep(delay)

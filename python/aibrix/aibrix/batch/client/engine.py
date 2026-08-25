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
from time import time
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
    DEFAULT_ADAPTIVE_ADDITIVE_INCREASE,
    DEFAULT_ADAPTIVE_HEALTHY_WINDOW,
    ConcurrencyController,
    ConcurrencyOutcome,
    FixedConcurrencyController,
    LLMAdaptiveConcurrencyController,
    LLMAdaptiveConcurrencySettings,
    concurrency_outcome_from_result,
)
from aibrix.batch.client.errors import InferenceError, InferenceErrorCode
from aibrix.batch.client.policy import FIFO, RoundRobin, Router, Scheduler
from aibrix.batch.client.source import CapacitySignal, EndpointSource
from aibrix.logger import init_logger

logger = init_logger(__name__)

_CAPACITY_WATCH_RETRY_SECONDS = 1.0
_NO_ENDPOINT_MIN_RETRY_DELAY_SECONDS = 1.0
_NO_ENDPOINT_LOG_INTERVAL = 120

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
    no_endpoint_deadline_epoch_seconds: Optional[float] = None

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
        if (
            self.no_endpoint_deadline_epoch_seconds is not None
            and self.no_endpoint_deadline_epoch_seconds <= 0
        ):
            raise ValueError("no_endpoint_deadline_epoch_seconds must be > 0")

    def no_endpoint_retries(self) -> Optional[int]:
        if self.no_endpoint_max_retries is not None:
            return self.no_endpoint_max_retries
        if self.no_endpoint_deadline_epoch_seconds is not None:
            return None
        return self.max_retries


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


class _CapacityScaledConcurrencyController:
    """Clamp a controller to the fraction of configured capacity available."""

    def __init__(
        self,
        controller: Union[
            FixedConcurrencyController,
            LLMAdaptiveConcurrencyController,
        ],
        *,
        configured_capacity: int,
        full_max_limit: int,
        capacity: CapacitySignal,
    ) -> None:
        self._controller = controller
        self._configured_capacity = max(int(configured_capacity), 1)
        self._full_max_limit = max(int(full_max_limit), 1)
        self._capacity = capacity
        self._capacity_limit = 0
        self._apply_capacity(capacity.count)

    @property
    def capacity(self) -> CapacitySignal:
        return self._capacity

    @property
    def configured_capacity(self) -> int:
        return self._configured_capacity

    @property
    def full_max_limit(self) -> int:
        return self._full_max_limit

    def limit(self) -> int:
        return min(self._controller.limit(), self._capacity_limit)

    def admission_delay_seconds(self) -> float:
        return _admission_delay_seconds(self._controller)

    def on_complete(self, outcome: ConcurrencyOutcome) -> None:
        self._controller.on_complete(outcome)

    def update_capacity(self, capacity: CapacitySignal) -> None:
        self._capacity = capacity
        self._apply_capacity(capacity.count)

    def _apply_capacity(self, current_capacity: int) -> None:
        available = min(
            max(int(current_capacity), 0),
            self._configured_capacity,
        )
        scaled_limit = max(
            1,
            (self._full_max_limit * available + self._configured_capacity - 1)
            // self._configured_capacity,
        )
        self._capacity_limit = scaled_limit
        self._controller.set_max_limit(scaled_limit)


class DispatchEngine:
    def __init__(
        self,
        source: EndpointSource,
        *,
        router: Optional[Router] = None,
        scheduler: Optional[Scheduler] = None,
        max_retries: int = 2,
        retry: Optional[RetryConfig] = None,
        job_id: Optional[str] = None,
        configured_capacity: Optional[int] = None,
    ) -> None:
        self._source = source
        self._router: Router = router or RoundRobin()
        self._scheduler: Scheduler = scheduler or FIFO()
        self._retry = retry or RetryConfig(max_retries=max_retries)
        # One engine serves one job. Carrying the id lets every line this layer
        # emits be filtered per job, which the request ref alone cannot do.
        self._job_id = job_id
        self._configured_capacity = (
            max(int(configured_capacity), 1)
            if configured_capacity is not None and configured_capacity > 0
            else None
        )

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
        adaptive_healthy_window: int = DEFAULT_ADAPTIVE_HEALTHY_WINDOW,
        adaptive_additive_increase: int = DEFAULT_ADAPTIVE_ADDITIVE_INCREASE,
        concurrency_controller: Optional[ConcurrencyController] = None,
        stats: Optional[DispatchStats] = None,
    ) -> None:
        """Drive ``requests`` to completion under a concurrency cap.

        When the engine has configured capacity, explicit and adaptive maximums
        apply at that full capacity and scale with the source's live capacity.
        A per-request inference failure is reported through ``on_result`` and
        never raised. Anything else -- a feeder error, or an ``on_result`` that
        itself raises (e.g. a caller's stop condition) -- stops scheduling,
        drains in-flight work, and re-raises the first such error.
        """
        controller = await self._resolve_concurrency_controller(
            max_concurrency=max_concurrency,
            adaptive_concurrency=adaptive_concurrency,
            adaptive_max_factor=adaptive_max_factor,
            adaptive_max_concurrency=adaptive_max_concurrency,
            adaptive_healthy_window=adaptive_healthy_window,
            adaptive_additive_increase=adaptive_additive_increase,
            concurrency_controller=concurrency_controller,
        )
        admission = _ConcurrencyAdmission(controller)
        capacity_task = (
            asyncio.create_task(self._watch_capacity(admission, controller))
            if isinstance(controller, _CapacityScaledConcurrencyController)
            else None
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

        try:
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
                        self._process(
                            request,
                            on_result,
                            admission,
                            first_error,
                            stats,
                        )
                    )
                    inflight.add(task)
                    task.add_done_callback(_on_done)
            except BaseException as exc:  # feeder raised; drain then re-raise
                if not first_error:
                    first_error.append(exc)

            if inflight:
                await asyncio.gather(*inflight, return_exceptions=True)
        finally:
            if capacity_task is not None:
                capacity_task.cancel()
                try:
                    await capacity_task
                except asyncio.CancelledError:
                    pass
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
        adaptive_healthy_window: int,
        adaptive_additive_increase: int,
        concurrency_controller: Optional[ConcurrencyController],
    ) -> ConcurrencyController:
        if concurrency_controller is not None:
            return concurrency_controller
        normalized_capacity = self._configured_capacity
        capacity = (
            await self._source.capacity() if normalized_capacity is not None else None
        )
        limit = (
            max(capacity.count, 1)
            if capacity is not None and max_concurrency is None
            else await self._resolve_limit(max_concurrency)
        )
        if adaptive_concurrency:
            max_limit = (
                max(int(adaptive_max_concurrency), 1)
                if adaptive_max_concurrency is not None
                else self._adaptive_max_limit(
                    (
                        normalized_capacity
                        if normalized_capacity is not None and max_concurrency is None
                        else limit
                    ),
                    adaptive_max_factor,
                )
            )
            controller: Union[
                FixedConcurrencyController,
                LLMAdaptiveConcurrencyController,
            ] = LLMAdaptiveConcurrencyController(
                initial_limit=min(limit, max_limit),
                max_limit=max_limit,
                settings=LLMAdaptiveConcurrencySettings(
                    healthy_window=adaptive_healthy_window,
                    additive_increase=adaptive_additive_increase,
                ),
            )
        else:
            max_limit = (
                normalized_capacity
                if normalized_capacity is not None and max_concurrency is None
                else limit
            )
            controller = FixedConcurrencyController(max_limit)

        if normalized_capacity is None or capacity is None:
            return controller
        scaled_controller = _CapacityScaledConcurrencyController(
            controller,
            configured_capacity=normalized_capacity,
            full_max_limit=max_limit,
            capacity=capacity,
        )
        logger.info(
            "Configured dispatch concurrency for endpoint capacity",
            job_id=self._job_id,
            current_capacity=capacity.count,
            configured_capacity=normalized_capacity,
            configured_max_concurrency=max_limit,
            current_limit=scaled_controller.limit(),
        )  # type: ignore[call-arg]
        return scaled_controller

    async def _watch_capacity(
        self,
        admission: "_ConcurrencyAdmission",
        controller: _CapacityScaledConcurrencyController,
    ) -> None:
        previous = controller.capacity
        while True:
            try:
                current = await self._source.wait_capacity_change(previous)
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                logger.warning(
                    "Failed to watch endpoint capacity; retrying",
                    job_id=self._job_id,
                    error=str(exc),
                )  # type: ignore[call-arg]
                await asyncio.sleep(_CAPACITY_WATCH_RETRY_SECONDS)
                continue

            previous = current
            previous_limit = admission.limit()
            await admission.update_capacity(current)
            logger.info(
                "Updated dispatch concurrency for endpoint capacity",
                job_id=self._job_id,
                current_capacity=current.count,
                configured_capacity=controller.configured_capacity,
                configured_max_concurrency=controller.full_max_limit,
                previous_limit=previous_limit,
                current_limit=admission.limit(),
            )  # type: ignore[call-arg]

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
                if endpoint_attempt == 0:
                    causes.append(str(no_endpoint))
                max_endpoint_retries = self._retry.no_endpoint_retries()
                deadline_reached = (
                    self._retry.no_endpoint_deadline_epoch_seconds is not None
                    and time() >= self._retry.no_endpoint_deadline_epoch_seconds
                )
                retry_available = (
                    max_endpoint_retries is None
                    or endpoint_attempt < max_endpoint_retries
                )
                if retry_available and not deadline_reached:
                    if (
                        endpoint_attempt == 0
                        or endpoint_attempt % _NO_ENDPOINT_LOG_INTERVAL == 0
                    ):
                        logger.warning(
                            "No reachable endpoint; waiting for discovery",
                            job_id=self._job_id,
                            ref=request.ref,
                            attempt=endpoint_attempt + 1,
                            max_retries=max_endpoint_retries,
                            deadline_epoch_seconds=(
                                self._retry.no_endpoint_deadline_epoch_seconds
                            ),
                        )  # type: ignore[call-arg]
                    await self._refresh_source()
                    await self._sleep_before_retry(
                        endpoint_attempt,
                        deadline_epoch_seconds=(
                            self._retry.no_endpoint_deadline_epoch_seconds
                        ),
                        minimum_delay_seconds=_NO_ENDPOINT_MIN_RETRY_DELAY_SECONDS,
                    )
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
                    # Retries happen entirely inside this call, so the dispatch
                    # counters stay flat while a request spins here. Without
                    # this line a retry storm is indistinguishable from a slow
                    # backend.
                    logger.warning(
                        "Retrying inference request",
                        job_id=self._job_id,
                        ref=request.ref,
                        attempt=send_attempt + 1,
                        max_retries=self._retry.max_retries,
                        channel_id=channel.id,
                        error_code=ex.code.value,
                        status_code=ex.status_code,
                        # TRANSPORT_ERROR covers both timeouts and connection
                        # failures; only the message tells them apart.
                        error=ex.message,
                    )  # type: ignore[call-arg]
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

    async def _sleep_before_retry(
        self,
        attempt: int,
        *,
        deadline_epoch_seconds: Optional[float] = None,
        minimum_delay_seconds: float = 0.0,
    ) -> None:
        base_delay = max(
            self._retry.base_delay_seconds,
            minimum_delay_seconds,
        )
        max_delay = max(
            self._retry.max_delay_seconds,
            minimum_delay_seconds,
        )
        if base_delay <= 0:
            return
        delay = min(
            base_delay,
            max_delay,
        )
        for _ in range(attempt):
            if delay >= max_delay:
                break
            delay = min(delay * 2, max_delay)
        if deadline_epoch_seconds is not None:
            delay = min(delay, max(deadline_epoch_seconds - time(), 0.0))
        if delay <= 0:
            return
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
                    lambda: (
                        int(self._controller.limit()) > 0
                        and self._inflight < int(self._controller.limit())
                    )
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

    async def update_capacity(self, capacity: CapacitySignal) -> None:
        async with self._condition:
            if isinstance(
                self._controller,
                _CapacityScaledConcurrencyController,
            ):
                self._controller.update_capacity(capacity)
            self._condition.notify_all()

    def limit(self) -> int:
        return max(int(self._controller.limit()), 0)

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

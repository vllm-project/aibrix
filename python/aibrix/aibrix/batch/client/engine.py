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
from aibrix.batch.client.errors import InferenceError, InferenceErrorCode
from aibrix.batch.client.policy import FIFO, RoundRobin, Router, Scheduler
from aibrix.batch.client.source import EndpointSource
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


class DispatchEngine:
    def __init__(
        self,
        source: EndpointSource,
        *,
        router: Optional[Router] = None,
        scheduler: Optional[Scheduler] = None,
        max_retries: int = 2,
    ) -> None:
        self._source = source
        self._router: Router = router or RoundRobin()
        self._scheduler: Scheduler = scheduler or FIFO()
        self._max_retries = max_retries

    @property
    def source(self) -> EndpointSource:
        """The endpoint source this engine dispatches to. Lets a caller rebuild
        an equivalent engine (e.g. wrap a pre-built engine in a Runtime)."""
        return self._source

    async def send_one(self, request: InferenceRequest) -> Response:
        """Single shot: pick an endpoint, send, fail over on error."""
        return await self._send_with_failover(request)

    async def run(
        self,
        requests: AsyncIterable[InferenceRequest],
        on_result: OnResult,
        *,
        max_concurrency: Optional[ConcurrencyLimit] = None,
        qps: Optional[float] = None,
    ) -> None:
        """Drive ``requests`` to completion under a concurrency cap.

        A per-request inference failure is reported through ``on_result`` and
        never raised. Anything else -- a feeder error, or an ``on_result`` that
        itself raises (e.g. a caller's stop condition) -- stops scheduling,
        drains in-flight work, and re-raises the first such error.
        """
        limit = await self._resolve_limit(max_concurrency)
        semaphore = asyncio.Semaphore(limit)
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
            async for request in self._scheduler.schedule(requests):
                if first_error:
                    break
                await semaphore.acquire()
                if first_error:
                    semaphore.release()
                    break
                if gate is not None:
                    await gate.wait()
                task = asyncio.create_task(self._process(request, on_result, semaphore))
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
        semaphore: asyncio.Semaphore,
    ) -> None:
        try:
            response = await self._send_with_failover(request)
            await _maybe_await(on_result(request, response, None))
        except InferenceError as err:
            await _maybe_await(on_result(request, None, err))
        finally:
            semaphore.release()

    async def _send_with_failover(self, request: InferenceRequest) -> Response:
        channels = await self._source.channels()
        if not channels:
            raise InferenceError(
                InferenceErrorCode.NO_ENDPOINT, "no reachable endpoint"
            )
        causes: list[str] = []
        for _ in range(self._max_retries + 1):
            channel = self._router.pick(request, channels)
            if channel is None:
                break
            try:
                return await channel.send(request)
            except InferenceError as ex:
                causes.append(str(ex))
        raise InferenceError(
            InferenceErrorCode.ALL_ENDPOINTS_FAILED,
            "all endpoints failed",
            causes=causes,
        )

    async def _resolve_limit(self, max_concurrency: Optional[ConcurrencyLimit]) -> int:
        if max_concurrency is None:
            capacity = await self._source.capacity()
            return max(capacity.count, 1)
        if callable(max_concurrency):
            return max(int(max_concurrency()), 1)
        return max(int(max_concurrency), 1)


async def _maybe_await(value: Any) -> Any:
    if asyncio.iscoroutine(value):
        return await value
    return value


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

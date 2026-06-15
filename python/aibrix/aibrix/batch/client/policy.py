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
"""Dispatch policies: ordering (Scheduler) and endpoint choice (Router).

These are the two seams that request orchestration plugs into. v1 ships the
trivial implementations (FIFO order, round-robin choice); prefix-aware
ordering and prefix-affinity routing are future strategies on the same
interfaces, so adding them is additive.

Concurrency / QPS are NOT modeled here -- they are plain engine parameters.
"""

from __future__ import annotations

from typing import (
    AsyncIterable,
    AsyncIterator,
    List,
    Optional,
    Protocol,
    runtime_checkable,
)

from aibrix.batch.client.channel import Channel, InferenceRequest


@runtime_checkable
class Scheduler(Protocol):
    """Decides what to send next / in what order."""

    def schedule(
        self, requests: AsyncIterable[InferenceRequest]
    ) -> AsyncIterator[InferenceRequest]: ...


class FIFO:
    """Pass requests through in arrival order."""

    async def schedule(
        self, requests: AsyncIterable[InferenceRequest]
    ) -> AsyncIterator[InferenceRequest]:
        async for request in requests:
            yield request


@runtime_checkable
class Router(Protocol):
    """Picks the channel for a request among the currently reachable set."""

    def pick(
        self, request: InferenceRequest, channels: List[Channel]
    ) -> Optional[Channel]: ...


class RoundRobin:
    """Rotate across channels. Tolerates the set growing/shrinking between calls
    (the cursor is taken modulo the live length), and degenerates to identity
    when a single channel is offered (gateway / ClusterIP / LB case)."""

    def __init__(self) -> None:
        self._cursor = 0

    def pick(
        self, request: InferenceRequest, channels: List[Channel]
    ) -> Optional[Channel]:
        if not channels:
            return None
        index = self._cursor % len(channels)
        self._cursor = (self._cursor + 1) % len(channels)
        return channels[index]

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
"""Single-channel endpoint sources (no extra dependencies).

All three offer exactly one channel, so the engine Router degenerates to
identity. Concurrency control still applies (you cap in-flight even against a
gateway).
"""

from __future__ import annotations

import asyncio
from typing import List

from aibrix.batch.client.channel import Channel, EchoChannel, HttpChannel
from aibrix.batch.client.source import CapacitySignal

# A gateway / LB fronts an unknown number of replicas. Its concurrency must NOT
# collapse to 1 (that would needlessly serialize a self-balancing address), so a
# single channel still advertises a modest parallel capacity. Override from the
# planner / job profile when the real backend width is known.
DEFAULT_GATEWAY_CAPACITY = 8


class _SingleChannelSource:
    """Base for sources that always offer exactly one channel.

    ``capacity`` is decoupled from the channel count on purpose: one address can
    front many replicas (gateway/LB), so the advertised concurrency is not
    ``len(channels)``.
    """

    def __init__(self, channel: Channel, *, capacity: int = 1) -> None:
        self._channel = channel
        self._capacity = max(capacity, 1)

    async def channels(self) -> List[Channel]:
        return [self._channel]

    async def capacity(self) -> CapacitySignal:
        return CapacitySignal(count=self._capacity, version=0)

    async def wait_capacity_change(self, previous: CapacitySignal) -> CapacitySignal:
        # A single fixed endpoint never changes capacity.
        await asyncio.Future()
        raise AssertionError("unreachable")

    async def aclose(self) -> None:
        await self._channel.aclose()


class StaticEndpointSource(_SingleChannelSource):
    """One fixed base_url (a single direct endpoint, width 1 by default)."""

    def __init__(
        self, base_url: str, *, capacity: int = 1, timeout: float = 30.0
    ) -> None:
        super().__init__(HttpChannel(base_url, timeout=timeout), capacity=capacity)


class GatewayEndpointSource(_SingleChannelSource):
    """One self-balancing address: external LB / AIBrix gateway / k8s ClusterIP.

    Same wire as Static, distinct intent: the address load-balances internally,
    so the engine never routes across replicas -- but it must still drive
    parallelism, hence a default capacity > 1.
    """

    def __init__(
        self,
        base_url: str,
        *,
        capacity: int = DEFAULT_GATEWAY_CAPACITY,
        timeout: float = 30.0,
    ) -> None:
        super().__init__(HttpChannel(base_url, timeout=timeout), capacity=capacity)


class NoopEndpointSource(_SingleChannelSource):
    """Dry-run: a single EchoChannel that echoes the payload back."""

    def __init__(self, *, delay: float = 0.0) -> None:
        super().__init__(EchoChannel(delay=delay), capacity=1)

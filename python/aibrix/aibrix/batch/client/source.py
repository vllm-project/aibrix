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
"""Endpoint source: where the reachable channels come from, and how many.

The source is the single seam for deployment topology. A gateway / ClusterIP /
external LB yields exactly one self-balancing channel (the engine Router then
degenerates to identity); direct-to-pod and discovery yield N channels. The
``capacity`` signal feeds the engine's concurrency sizing.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import List, Protocol, runtime_checkable

from aibrix.batch.client.channel import Channel


@dataclass(frozen=True, slots=True)
class CapacitySignal:
    """How much backend is available right now.

    ``count`` is the number of reachable endpoints (the natural concurrency
    target); ``version`` changes when the set changes, so a waiter can tell a
    real churn from a no-op refresh.
    """

    count: int
    version: int = 0


@runtime_checkable
class EndpointSource(Protocol):
    """Catalog of reachable channels + a capacity signal + a lifecycle.

    Lifecycle matters: a port-forward source owns a subprocess that must be torn
    down via ``aclose`` -- this is why reaching an endpoint is a source concern,
    not an off-contract ``close()`` bolted onto a client.
    """

    async def channels(self) -> List[Channel]: ...

    async def capacity(self) -> CapacitySignal: ...

    async def wait_capacity_change(
        self, previous: CapacitySignal
    ) -> CapacitySignal: ...

    async def aclose(self) -> None: ...

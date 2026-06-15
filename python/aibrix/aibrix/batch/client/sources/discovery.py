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
"""Discovery-backed endpoint source (consul / k8s / static behind a registry).

Replaces the discovery half of the old ``FederalInferenceEngineClient``. The
per-endpoint busy-set / claim-release is gone: round-robin (and future affinity)
lives in the engine Router, concurrency in the engine cap.

To keep this layer free of ``aibrix.batch`` / ``aibrix.context`` imports, the
discovery dependency is expressed as a structural protocol; any concrete
``ModelDiscovery`` satisfies it at runtime.
"""

from __future__ import annotations

import asyncio
from typing import Dict, List, Optional, Protocol, runtime_checkable

from aibrix.batch.client.channel import Channel, HttpChannel
from aibrix.batch.client.source import CapacitySignal


@runtime_checkable
class _Endpoint(Protocol):
    @property
    def base_url(self) -> str: ...


@runtime_checkable
class _Snapshot(Protocol):
    version: int
    endpoints: List[_Endpoint]


@runtime_checkable
class _Discovery(Protocol):
    async def discover_model_endpoints(
        self, served_model_name: str, service_id: Optional[str] = None
    ) -> _Snapshot: ...


class DiscoveryEndpointSource:
    """N direct endpoints from a discovery backend, refreshed on snapshot change."""

    def __init__(
        self,
        discovery: _Discovery,
        served_model_name: str,
        *,
        service_id: Optional[str] = None,
        timeout: float = 30.0,
    ) -> None:
        self._discovery = discovery
        self._model = served_model_name
        self._service_id = service_id
        self._timeout = timeout
        self._version: Optional[int] = None
        self._channels: List[Channel] = []
        self._by_url: Dict[str, Channel] = {}
        self._lock = asyncio.Lock()

    async def channels(self) -> List[Channel]:
        await self._refresh()
        return list(self._channels)

    async def capacity(self) -> CapacitySignal:
        await self._refresh()
        return CapacitySignal(count=len(self._channels), version=self._version or 0)

    async def wait_capacity_change(self, previous: CapacitySignal) -> CapacitySignal:
        while True:
            snapshot = await self._discovery.discover_model_endpoints(
                served_model_name=self._model, service_id=self._service_id
            )
            if (
                snapshot.version != previous.version
                or len(snapshot.endpoints) != previous.count
            ):
                await self._apply(snapshot)
                return CapacitySignal(
                    count=len(self._channels), version=self._version or 0
                )
            await asyncio.sleep(1.0)

    async def _refresh(self) -> None:
        snapshot = await self._discovery.discover_model_endpoints(
            served_model_name=self._model, service_id=self._service_id
        )
        await self._apply(snapshot)

    async def _apply(self, snapshot: _Snapshot) -> None:
        async with self._lock:
            if snapshot.version == self._version:
                return
            new_urls = [endpoint.base_url for endpoint in snapshot.endpoints]
            keep: Dict[str, Channel] = {}
            for url in new_urls:
                keep[url] = self._by_url.get(url) or HttpChannel(
                    url, timeout=self._timeout
                )
            for url, channel in self._by_url.items():
                if url not in keep:
                    await channel.aclose()
            self._by_url = keep
            self._channels = [keep[url] for url in new_urls]
            self._version = snapshot.version

    async def aclose(self) -> None:
        for channel in self._channels:
            await channel.aclose()
        self._channels = []
        self._by_url = {}

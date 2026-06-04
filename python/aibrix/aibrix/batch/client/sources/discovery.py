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

This is the smart-client adapter for direct client-side load balancing across
multiple discovered endpoints. Runtime backends do not select it by default
yet; Kubernetes Deployment, SSH, and worker-local paths currently construct
their topology-specific sources directly.

The per-endpoint busy-set / claim-release from the old client model is gone:
round-robin (and future affinity) lives in the engine Router, concurrency in
the engine cap.

To keep this layer free of ``aibrix.batch`` / ``aibrix.context`` imports, the
discovery dependency is expressed as a structural protocol; any concrete
``ModelDiscovery`` satisfies it at runtime.
"""

from __future__ import annotations

import asyncio
from time import monotonic
from typing import (
    Any,
    Callable,
    Dict,
    List,
    Optional,
    Protocol,
    Sequence,
    runtime_checkable,
)

from aibrix.batch.client.channel import Channel, HttpChannel
from aibrix.batch.client.source import CapacitySignal
from aibrix.logger import init_logger

logger = init_logger(__name__)


@runtime_checkable
class _Endpoint(Protocol):
    @property
    def base_url(self) -> str: ...


@runtime_checkable
class _Snapshot(Protocol):
    @property
    def version(self) -> int: ...

    @property
    def endpoints(self) -> Sequence[_Endpoint]: ...


@runtime_checkable
class _Discovery(Protocol):
    async def discover_model_endpoints(
        self, served_model_name: str, service_id: Optional[str] = None
    ) -> Any: ...


class DiscoveryEndpointSource:
    """N direct endpoints from a discovery backend, refreshed on snapshot change."""

    def __init__(
        self,
        discovery: _Discovery,
        served_model_name: str,
        *,
        service_id: Optional[str] = None,
        timeout: float = 30.0,
        refresh_interval_seconds: float = 20.0,
        clock: Callable[[], float] = monotonic,
    ) -> None:
        self._discovery = discovery
        self._model = served_model_name
        self._service_id = service_id
        self._timeout = timeout
        self._refresh_interval_seconds = max(float(refresh_interval_seconds), 0.0)
        self._clock = clock
        self._version: Optional[int] = None
        self._channels: List[Channel] = []
        self._by_url: Dict[str, Channel] = {}
        self._retired_channels: List[Channel] = []
        self._lock = asyncio.Lock()
        self._refresh_lock = asyncio.Lock()
        self._last_refresh_at: Optional[float] = None

    async def channels(self) -> List[Channel]:
        await self._refresh_if_needed()
        return list(self._channels)

    async def capacity(self) -> CapacitySignal:
        await self._refresh_if_needed()
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
        await self.refresh()

    async def refresh(self) -> None:
        await self._refresh_if_needed(force=True)

    async def report_channel_error(self, channel_id: str, error: Exception) -> None:
        await self._remove_channel(channel_id)
        try:
            await self.refresh()
        except Exception as exc:  # noqa: BLE001 - discovery is best-effort here.
            logger.warning(
                "Failed to refresh discovered endpoints after channel error",
                channel_id=channel_id,
                error=str(exc),
            )  # type: ignore[call-arg]
        await self._remove_channel(channel_id)

    async def _remove_channel(self, channel_id: str) -> None:
        async with self._lock:
            failed = self._by_url.pop(channel_id, None)
            if failed is not None:
                self._retired_channels.append(failed)
                self._channels = [
                    channel for channel in self._channels if channel.id != channel_id
                ]

    async def _refresh_if_needed(self, *, force: bool = False) -> None:
        if not force and not self._should_refresh():
            return
        async with self._refresh_lock:
            if not force and not self._should_refresh():
                return
            snapshot = await self._discover()
            await self._apply(snapshot)
            self._last_refresh_at = self._clock()

    async def _discover(self) -> _Snapshot:
        snapshot = await self._discovery.discover_model_endpoints(
            served_model_name=self._model, service_id=self._service_id
        )
        return snapshot

    def _should_refresh(self) -> bool:
        if self._last_refresh_at is None:
            return True
        return self._clock() - self._last_refresh_at >= self._refresh_interval_seconds

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
                    self._retired_channels.append(channel)
            self._by_url = keep
            self._channels = [keep[url] for url in new_urls]
            self._version = snapshot.version

    async def aclose(self) -> None:
        for channel in [*self._channels, *self._retired_channels]:
            await channel.aclose()
        self._channels = []
        self._by_url = {}
        self._retired_channels = []

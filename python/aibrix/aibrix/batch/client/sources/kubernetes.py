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
"""Kubernetes endpoint sources, selected by known topology (no runtime probing).

- ``InClusterEndpointSource``: caller runs in-cluster -> hit the Service
  ClusterIP (kube-proxy balances, count=replicas) or a fixed pod-url set
  (count=N).
- ``PortForwardEndpointSource``: caller runs out-of-cluster -> ``kubectl
  port-forward`` a Service and expose one local channel; ``aclose`` tears the
  forwarder down.

The old runtime apiserver-proxy -> gateway -> port-forward fallback chain is
intentionally gone: topology is a construction-time choice, not a probe.
"""

from __future__ import annotations

import asyncio
import hashlib
import re
import subprocess
import time
from dataclasses import dataclass
from typing import Any, List, Optional, Sequence, Union

from aibrix.batch.client.channel import Channel, HttpChannel
from aibrix.batch.client.errors import InferenceError, InferenceErrorCode
from aibrix.batch.client.source import CapacitySignal
from aibrix.logger import init_logger

logger = init_logger(__name__)


@dataclass(frozen=True, slots=True)
class K8sDiscoveredEndpoint:
    base_url: str


@dataclass(frozen=True, slots=True)
class K8sEndpointSliceSnapshot:
    version: int
    endpoints: List[K8sDiscoveredEndpoint]


class K8sEndpointSliceDiscovery:
    """Discover ready pod endpoints behind one Kubernetes Service."""

    def __init__(
        self,
        discovery_v1_api: Any,
        *,
        namespace: str,
        service_name: str,
        service_port: int,
        scheme: str = "http",
    ) -> None:
        self._api = discovery_v1_api
        self._namespace = namespace
        self._service_name = service_name
        self._service_port = service_port
        self._scheme = scheme

    async def discover_model_endpoints(
        self, served_model_name: str, service_id: Optional[str] = None
    ) -> K8sEndpointSliceSnapshot:
        del served_model_name, service_id
        return await asyncio.to_thread(self._discover)

    def _discover(self) -> K8sEndpointSliceSnapshot:
        label_selector = f"kubernetes.io/service-name={self._service_name}"
        result = self._api.list_namespaced_endpoint_slice(
            namespace=self._namespace,
            label_selector=label_selector,
        )
        slices = list(getattr(result, "items", []) or [])
        endpoints: List[K8sDiscoveredEndpoint] = []
        version_parts: List[str] = []

        for endpoint_slice in slices:
            metadata = getattr(endpoint_slice, "metadata", None)
            version_parts.append(
                f"{getattr(metadata, 'name', '')}:"
                f"{getattr(metadata, 'resource_version', '')}"
            )
            port = self._endpoint_port(endpoint_slice)
            for endpoint in getattr(endpoint_slice, "endpoints", []) or []:
                if self._explicitly_not_ready(endpoint):
                    continue
                for address in getattr(endpoint, "addresses", []) or []:
                    endpoints.append(
                        K8sDiscoveredEndpoint(
                            base_url=f"{self._scheme}://{self._url_host(address)}:{port}"
                        )
                    )

        endpoints = sorted(endpoints, key=lambda endpoint: endpoint.base_url)
        version_parts.extend(endpoint.base_url for endpoint in endpoints)
        version = int.from_bytes(
            hashlib.sha1("|".join(version_parts).encode("utf-8")).digest()[:8],
            byteorder="big",
        )
        return K8sEndpointSliceSnapshot(version=version, endpoints=endpoints)

    def _endpoint_port(self, endpoint_slice: Any) -> int:
        for port in getattr(endpoint_slice, "ports", []) or []:
            value = getattr(port, "port", None)
            if value is not None:
                return int(value)
        return self._service_port

    @staticmethod
    def _explicitly_not_ready(endpoint: Any) -> bool:
        conditions = getattr(endpoint, "conditions", None)
        return getattr(conditions, "ready", None) is False

    @staticmethod
    def _url_host(host: str) -> str:
        if ":" in host and not host.startswith("["):
            return f"[{host}]"
        return host


class InClusterEndpointSource:
    """Reach an in-cluster Service ClusterIP or pod URLs over plain HTTP.

    Service URLs may advertise a larger capacity than their channel count
    because kube-proxy balances connections behind the single DNS name.
    """

    def __init__(
        self,
        base_urls: Union[str, Sequence[str]],
        *,
        capacity: Optional[int] = None,
        timeout: float = 30.0,
    ) -> None:
        urls = [base_urls] if isinstance(base_urls, str) else list(base_urls)
        self._channels: List[Channel] = [
            HttpChannel(url, timeout=timeout) for url in urls
        ]
        self._capacity = max(1, int(capacity)) if capacity is not None else len(urls)

    async def channels(self) -> List[Channel]:
        return list(self._channels)

    async def capacity(self) -> CapacitySignal:
        return CapacitySignal(count=self._capacity)

    async def wait_capacity_change(self, previous: CapacitySignal) -> CapacitySignal:
        await asyncio.Future()
        raise AssertionError("unreachable")

    async def aclose(self) -> None:
        for channel in self._channels:
            await channel.aclose()


class PortForwardEndpointSource:
    """Out-of-cluster dev/control-plane access: ``kubectl port-forward`` a
    Service, expose one local channel. Explicit and opt-in; not a fallback."""

    _URL_RE = re.compile(r"(?:127\.0\.0\.1|localhost):(\d+)")

    def __init__(
        self,
        namespace: str,
        service_name: str,
        service_port: int,
        *,
        timeout: float = 30.0,
        ready_timeout: float = 20.0,
    ) -> None:
        self._namespace = namespace
        self._service_name = service_name
        self._service_port = service_port
        self._timeout = timeout
        self._ready_timeout = ready_timeout
        self._process: Optional[subprocess.Popen[str]] = None
        self._channel: Optional[Channel] = None
        self._lock = asyncio.Lock()

    async def channels(self) -> List[Channel]:
        async with self._lock:
            if self._channel is None or not self._port_forward_running():
                if self._channel is not None:
                    await self._channel.aclose()
                    self._channel = None
                if self._process is not None:
                    logger.warning(
                        "port-forward process exited; restarting",
                        target=f"service/{self._service_name}:{self._service_port}",
                        namespace=self._namespace,
                    )  # type: ignore[call-arg]
                    await asyncio.to_thread(self._terminate)
                self._channel = await asyncio.to_thread(self._start)
            return [self._channel]

    async def capacity(self) -> CapacitySignal:
        return CapacitySignal(count=1)

    async def wait_capacity_change(self, previous: CapacitySignal) -> CapacitySignal:
        await asyncio.Future()
        raise AssertionError("unreachable")

    def _start(self) -> Channel:
        process = subprocess.Popen(
            [
                "kubectl",
                "-n",
                self._namespace,
                "port-forward",
                f"service/{self._service_name}",
                f":{self._service_port}",
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
        )
        deadline = time.time() + self._ready_timeout
        while time.time() < deadline:
            if process.poll() is not None:
                output = process.stdout.read() if process.stdout is not None else ""
                raise InferenceError(
                    InferenceErrorCode.CONNECTION_SETUP,
                    f"port-forward exited early for {self._service_name}: {output}",
                )
            line = process.stdout.readline() if process.stdout is not None else ""
            match = self._URL_RE.search(line)
            if match:
                self._process = process
                local_url = f"http://127.0.0.1:{match.group(1)}"
                logger.info(
                    "port-forward established",
                    local_url=local_url,
                    target=f"service/{self._service_name}:{self._service_port}",
                    namespace=self._namespace,
                )  # type: ignore[call-arg]
                return HttpChannel(local_url, timeout=self._timeout)
        process.terminate()
        raise InferenceError(
            InferenceErrorCode.CONNECTION_SETUP,
            f"timed out waiting for port-forward for {self._service_name}",
        )

    async def aclose(self) -> None:
        async with self._lock:
            if self._channel is not None:
                await self._channel.aclose()
                self._channel = None
            if self._process is not None:
                await asyncio.to_thread(self._terminate)

    def _port_forward_running(self) -> bool:
        return self._process is not None and self._process.poll() is None

    def _terminate(self) -> None:
        process = self._process
        if process is None:
            return
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        self._process = None

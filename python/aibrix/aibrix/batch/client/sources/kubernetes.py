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
  ClusterIP (kube-proxy balances, count=1) or a fixed pod-url set (count=N).
- ``PortForwardEndpointSource``: caller runs out-of-cluster -> ``kubectl
  port-forward`` a Service and expose one local channel; ``aclose`` tears the
  forwarder down.

The old runtime apiserver-proxy -> gateway -> port-forward fallback chain is
intentionally gone: topology is a construction-time choice, not a probe.
"""

from __future__ import annotations

import asyncio
import re
import subprocess
import time
from typing import List, Optional, Sequence, Union

from aibrix.batch.client.channel import Channel, HttpChannel
from aibrix.batch.client.errors import InferenceError, InferenceErrorCode
from aibrix.batch.client.source import CapacitySignal
from aibrix.logger import init_logger

logger = init_logger(__name__)


class InClusterEndpointSource:
    """Reach an in-cluster Service ClusterIP (count=1) or pod URLs (count=N)
    over plain HTTP. Assumes the caller runs inside the cluster."""

    def __init__(
        self,
        base_urls: Union[str, Sequence[str]],
        *,
        timeout: float = 30.0,
    ) -> None:
        urls = [base_urls] if isinstance(base_urls, str) else list(base_urls)
        self._channels: List[Channel] = [
            HttpChannel(url, timeout=timeout) for url in urls
        ]

    async def channels(self) -> List[Channel]:
        return list(self._channels)

    async def capacity(self) -> CapacitySignal:
        return CapacitySignal(count=len(self._channels))

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

    async def channels(self) -> List[Channel]:
        if self._channel is None:
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
                return HttpChannel(
                    f"http://127.0.0.1:{match.group(1)}", timeout=self._timeout
                )
        process.terminate()
        raise InferenceError(
            InferenceErrorCode.CONNECTION_SETUP,
            f"timed out waiting for port-forward for {self._service_name}",
        )

    async def aclose(self) -> None:
        if self._channel is not None:
            await self._channel.aclose()
            self._channel = None
        if self._process is not None:
            await asyncio.to_thread(self._terminate)

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

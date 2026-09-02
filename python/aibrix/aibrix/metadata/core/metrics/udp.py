# Copyright 2026 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

from __future__ import annotations

import socket
import time
from typing import Iterable, Optional

from aibrix.logger import init_logger
from aibrix.metadata.core.metrics.sink import Sink, Tag, normalize_tags

logger = init_logger(__name__)
_UDP_ADDR_CACHE_TTL_SECONDS = 30.0


def _split_udp_addr(address: str) -> tuple[str, int]:
    address = address.strip()
    if not address:
        raise ValueError("UDP address must not be empty")
    if address.startswith("["):
        end = address.find("]")
        if end == -1 or end + 1 >= len(address) or address[end + 1] != ":":
            raise ValueError("UDP address must use [host]:port for IPv6 literals")
        host = address[1:end]
        port_text = address[end + 2 :]
    else:
        if ":" not in address:
            raise ValueError("UDP address must be in host:port format")
        host, port_text = address.rsplit(":", 1)
    if not host:
        raise ValueError("UDP address host must not be empty")
    try:
        port = int(port_text)
    except ValueError as exc:
        raise ValueError("UDP address port must be an integer") from exc
    return host, port


def _resolve_udp_addr(address: str) -> tuple[int, tuple]:
    host, port = _split_udp_addr(address)
    family, _, _, _, sockaddr = socket.getaddrinfo(host, port, type=socket.SOCK_DGRAM)[
        0
    ]
    return family, sockaddr


class _UDPSinkBase(Sink):
    def __init__(self, address: str, prefix: str = ""):
        self._address = address
        self._family: Optional[int] = None
        self._sockaddr: Optional[tuple] = None
        self._socket: Optional[socket.socket] = None
        self._resolved_at = 0.0
        self._prefix = prefix.strip(".")

    def _metric_name(self, name: str) -> str:
        if self._prefix:
            return f"{self._prefix}.{name}"
        return name

    def _destination(self) -> tuple[socket.socket, tuple] | None:
        now = time.monotonic()
        if (
            self._socket is not None
            and self._sockaddr is not None
            and self._family is not None
            and now - self._resolved_at < _UDP_ADDR_CACHE_TTL_SECONDS
        ):
            return self._socket, self._sockaddr
        try:
            family, sockaddr = _resolve_udp_addr(self._address)
        except Exception as exc:
            logger.warning(
                "Failed to resolve UDP metrics sink address",
                address=self._address,
                error=str(exc),
            )  # type: ignore[call-arg]
            return None
        if self._socket is None or self._family != family:
            if self._socket is not None:
                self._socket.close()
            self._socket = socket.socket(family, socket.SOCK_DGRAM)
        self._family = family
        self._sockaddr = sockaddr
        self._resolved_at = now
        return self._socket, self._sockaddr

    def _send(self, payload: str) -> None:
        destination = self._destination()
        if destination is None:
            return
        udp_socket, sockaddr = destination
        try:
            udp_socket.sendto(payload.encode("utf-8"), sockaddr)
        except Exception:
            logger.exception("Failed to emit metrics payload", payload=payload)

    def counter(self, name: str, value: float, *tags: Tag) -> None:
        self._send(
            self._format(self._metric_name(name), value, "c", normalize_tags(tags))
        )

    def gauge(self, name: str, value: float, *tags: Tag) -> None:
        self._send(
            self._format(self._metric_name(name), value, "g", normalize_tags(tags))
        )

    def timer(self, name: str, value: float, *tags: Tag) -> None:
        self._send(
            self._format(self._metric_name(name), value, "ms", normalize_tags(tags))
        )

    def store(self, name: str, value: float, *tags: Tag) -> None:
        self.gauge(name, value, *tags)

    def rate(self, name: str, value: float, *tags: Tag) -> None:
        self.counter(name, value, *tags)

    def close(self) -> None:
        if self._socket is not None:
            self._socket.close()
            self._socket = None

    def _format(
        self, name: str, value: float, metric_type: str, tags: tuple[Tag, ...]
    ) -> str:
        raise NotImplementedError


class StatsdSink(_UDPSinkBase):
    def _format(
        self, name: str, value: float, metric_type: str, tags: tuple[Tag, ...]
    ) -> str:
        return f"{name}:{value}|{metric_type}"


class StatsiteSink(StatsdSink):
    pass


class DogStatsdSink(_UDPSinkBase):
    def __init__(self, address: str, prefix: str = "", global_tags: Iterable[str] = ()):
        super().__init__(address=address, prefix=prefix)
        self._global_tags = tuple(global_tags)

    def _format(
        self, name: str, value: float, metric_type: str, tags: tuple[Tag, ...]
    ) -> str:
        payload = f"{name}:{value}|{metric_type}"
        all_tags = list(self._global_tags)
        all_tags.extend(f"{tag.name}:{tag.value}" for tag in tags)
        if all_tags:
            payload = f"{payload}|#{','.join(all_tags)}"
        return payload

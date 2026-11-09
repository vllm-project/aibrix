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

import queue
import re
import socket
import threading
import time
from typing import Iterable, Optional

from aibrix.logger import init_logger
from aibrix.metadata.core.metrics.sink import Sink, Tag, normalize_tags

logger = init_logger(__name__)

_ADDR_CACHE_TTL_SECONDS = 30.0
_DEFAULT_QUEUE_SIZE = 1024
_VALID_OPENTSDB_CHARS = re.compile(r"[^A-Za-z0-9_\-./]")


def _split_addr(address: str) -> tuple[str, int]:
    address = address.strip()
    if not address:
        raise ValueError("OpenTSDB address must not be empty")
    if address.startswith("["):
        end = address.find("]")
        if end == -1 or end + 1 >= len(address) or address[end + 1] != ":":
            raise ValueError("OpenTSDB address must use [host]:port for IPv6 literals")
        host = address[1:end]
        port_text = address[end + 2 :]
    else:
        if ":" not in address:
            raise ValueError("OpenTSDB address must be in host:port format")
        host, port_text = address.rsplit(":", 1)
    if not host:
        raise ValueError("OpenTSDB address host must not be empty")
    try:
        port = int(port_text)
    except ValueError as exc:
        raise ValueError("OpenTSDB address port must be an integer") from exc
    return host, port


def _resolve_addr(address: str) -> tuple[int, tuple]:
    host, port = _split_addr(address)
    family, _, _, _, sockaddr = socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)[
        0
    ]
    return family, sockaddr


def _sanitize(value: str) -> str:
    sanitized = _VALID_OPENTSDB_CHARS.sub("_", value.strip())
    return sanitized or "none"


class OpenTSDBSink(Sink):
    """Submit metrics through OpenTSDB's telnet protocol."""

    def __init__(
        self,
        address: str,
        prefix: str = "",
        timeout_seconds: float = 0.2,
        global_tags: Iterable[Tag] = (),
        queue_size: int = _DEFAULT_QUEUE_SIZE,
    ):
        self._address = address
        self._prefix = prefix.strip(".")
        self._timeout_seconds = max(timeout_seconds, 0.01)
        self._global_tags = tuple(global_tags)
        self._queue: queue.Queue[Optional[str]] = queue.Queue(
            maxsize=max(1, queue_size)
        )
        self._closed = False
        self._family: Optional[int] = None
        self._sockaddr: Optional[tuple] = None
        self._socket: Optional[socket.socket] = None
        self._resolved_at = 0.0
        self._worker = threading.Thread(
            target=self._worker_loop,
            name="aibrix-opentsdb-metrics-sink",
            daemon=True,
        )
        self._worker.start()

    def counter(self, name: str, value: float, *tags: Tag) -> None:
        self._send(name, value, tags)

    def gauge(self, name: str, value: float, *tags: Tag) -> None:
        self._send(name, value, tags)

    def timer(self, name: str, value: float, *tags: Tag) -> None:
        self._send(name, value, tags)

    def store(self, name: str, value: float, *tags: Tag) -> None:
        self._send(name, value, tags)

    def rate(self, name: str, value: float, *tags: Tag) -> None:
        self._send(name, value, tags)

    def close(self) -> None:
        self._closed = True
        try:
            self._queue.put_nowait(None)
        except queue.Full:
            try:
                self._queue.get_nowait()
            except queue.Empty:
                pass
            self._queue.put_nowait(None)
        if self._worker.is_alive():
            self._worker.join(timeout=max(self._timeout_seconds, 1.0))
        self._close_socket()

    def _metric_name(self, name: str) -> str:
        if self._prefix:
            name = f"{self._prefix}.{name}"
        return _sanitize(name)

    def _destination(self) -> socket.socket | None:
        now = time.monotonic()
        if (
            self._socket is not None
            and self._sockaddr is not None
            and self._family is not None
            and now - self._resolved_at < _ADDR_CACHE_TTL_SECONDS
        ):
            return self._socket

        try:
            family, sockaddr = _resolve_addr(self._address)
        except Exception as exc:
            logger.warning(
                "Failed to resolve OpenTSDB metrics sink address",
                address=self._address,
                error=str(exc),
            )  # type: ignore[call-arg]
            return None

        try:
            if self._socket is None or self._family != family:
                self._close_socket()
                self._socket = socket.socket(family, socket.SOCK_STREAM)
                self._socket.settimeout(self._timeout_seconds)
                self._socket.connect(sockaddr)
            self._family = family
            self._sockaddr = sockaddr
            self._resolved_at = now
            return self._socket
        except Exception as exc:
            logger.warning(
                "Failed to connect to OpenTSDB metrics sink",
                address=self._address,
                error=str(exc),
            )  # type: ignore[call-arg]
            self._close_socket()
            return None

    def _send(self, name: str, value: float, tags: tuple[Tag, ...]) -> None:
        if self._closed:
            return
        payload = self._format(name, value, tags)
        try:
            self._queue.put_nowait(payload)
        except queue.Full:
            logger.warning(
                "Dropped OpenTSDB metrics payload because the sink queue is full",
                address=self._address,
            )  # type: ignore[call-arg]

    def _worker_loop(self) -> None:
        while True:
            payload = self._queue.get()
            if payload is None:
                break
            self._send_payload(payload)
        self._close_socket()

    def _send_payload(self, payload: str) -> None:
        destination = self._destination()
        if destination is None:
            return

        try:
            destination.sendall(payload.encode("utf-8"))
        except Exception:
            logger.exception("Failed to emit OpenTSDB metrics payload", payload=payload)
            self._close_socket()

    def _format(self, name: str, value: float, tags: tuple[Tag, ...]) -> str:
        timestamp = int(time.time())
        all_tags = normalize_tags((*self._global_tags, *tags))
        tag_text = " ".join(
            f"{_sanitize(tag.name)}={_sanitize(tag.value)}" for tag in all_tags
        )
        return f"put {self._metric_name(name)} {timestamp} {value} {tag_text}\n"

    def _close_socket(self) -> None:
        if self._socket is not None:
            self._socket.close()
            self._socket = None

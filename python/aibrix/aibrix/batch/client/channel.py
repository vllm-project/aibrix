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
"""Transport: how a single request goes out over one reachable channel.

A ``Channel`` is the leaf I/O unit. It knows nothing about endpoint selection,
concurrency, retry, or batching -- those live in the dispatch engine. The only
variation here is the wire protocol (plain HTTP vs. echo/dry-run).
"""

from __future__ import annotations

import asyncio
from dataclasses import dataclass
from typing import Any, Dict, Optional, Protocol, runtime_checkable
from urllib.parse import urljoin

import httpx

from aibrix.batch.client.errors import InferenceError, InferenceErrorCode
from aibrix.logger import init_logger

logger = init_logger(__name__)

Response = Dict[str, Any]
_MAX_ERROR_BODY_CHARS = 4096


@dataclass(slots=True)
class InferenceRequest:
    """One inference call.

    ``path`` is the engine route (e.g. ``/v1/chat/completions``); ``payload`` is
    the request body. ``ref`` is an opaque caller handle (e.g. a batch request
    id) echoed back via ``on_result`` so the caller can correlate outcomes.
    """

    path: str
    payload: Dict[str, Any]
    ref: Any = None


@runtime_checkable
class Channel(Protocol):
    """One reachable destination able to serve a request."""

    @property
    def id(self) -> str: ...

    async def send(self, request: InferenceRequest) -> Response: ...

    async def aclose(self) -> None: ...


class HttpChannel:
    """POST over HTTP to a fixed base_url."""

    def __init__(
        self,
        base_url: str,
        *,
        timeout: float = 30.0,
        client: Optional[httpx.AsyncClient] = None,
    ) -> None:
        self._base_url = base_url
        self._timeout = timeout
        self._client = client
        self._owns_client = client is None

    @property
    def id(self) -> str:
        return self._base_url

    async def send(self, request: InferenceRequest) -> Response:
        client = self._ensure_client()
        url = urljoin(self._base_url, request.path)
        logger.debug("requesting inference", url=url, body=request.payload)  # type: ignore[call-arg]
        try:
            response = await client.post(
                url, json=request.payload, timeout=self._timeout
            )
        except httpx.TimeoutException as ex:
            raise InferenceError(
                InferenceErrorCode.TRANSPORT_ERROR,
                f"{self._base_url}: {ex}",
                retryable=True,
            ) from ex
        except httpx.TransportError as ex:
            raise InferenceError(
                InferenceErrorCode.TRANSPORT_ERROR,
                f"{self._base_url}: {ex}",
                retryable=True,
            ) from ex

        if response.status_code >= 400:
            body = _response_body(response)
            raise InferenceError(
                InferenceErrorCode.HTTP_ERROR,
                f"{self._base_url}: HTTP {response.status_code}",
                status_code=response.status_code,
                response_body=body,
                retryable=_is_retryable_status(response.status_code),
            )

        try:
            return response.json()
        except ValueError as ex:
            raise InferenceError(
                InferenceErrorCode.RESPONSE_ERROR,
                f"{self._base_url}: invalid JSON response",
                status_code=response.status_code,
                response_body=_truncate_text(response.text),
                retryable=False,
            ) from ex

    def _ensure_client(self) -> httpx.AsyncClient:
        if self._client is None:
            self._client = httpx.AsyncClient()
        return self._client

    async def aclose(self) -> None:
        if self._owns_client and self._client is not None:
            await self._client.aclose()
            self._client = None


class EchoChannel:
    """Returns the request payload verbatim. For --dry-run only; no real engine.

    Models the case where the actual send happens elsewhere (e.g. a sidecar) and
    this process must not hit a backend.
    """

    def __init__(self, *, delay: float = 0.0, id: str = "echo") -> None:
        self._delay = delay
        self._id = id

    @property
    def id(self) -> str:
        return self._id

    async def send(self, request: InferenceRequest) -> Response:
        if self._delay:
            await asyncio.sleep(self._delay)
        return request.payload

    async def aclose(self) -> None:
        return None


def _is_retryable_status(status_code: int) -> bool:
    return status_code in {408, 429, 500, 502, 503, 504}


def _response_body(response: httpx.Response) -> Any:
    text = _truncate_text(response.text)
    if text != response.text:
        return text
    try:
        return response.json()
    except ValueError:
        return text


def _truncate_text(text: str) -> str:
    if len(text) <= _MAX_ERROR_BODY_CHARS:
        return text
    return text[:_MAX_ERROR_BODY_CHARS] + "...<truncated>"

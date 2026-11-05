# Copyright 2024 The Aibrix Team.
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
import contextlib
from typing import Any, Optional

import httpx

from aibrix.metadata.core.http_client import HTTPClient


class HTTPXClientWrapper(HTTPClient):
    async_client: Optional[httpx.AsyncClient] = None
    client_label = "httpx-client"
    instantiated_log_message = "httpx.AsyncClient instantiated."
    closed_log_message = "httpx.AsyncClient closed"

    def __init__(self, **client_kwargs: Any) -> None:
        client_kwargs.pop("aiohttp_limit_per_host", None)
        super().__init__(**client_kwargs)

    @property
    def is_closed(self) -> bool:
        return self.async_client.is_closed if self.async_client is not None else True

    def _create_async_client(self) -> httpx.AsyncClient:
        return httpx.AsyncClient(**self._client_kwargs)

    async def _close_async_client(self) -> None:
        assert self.async_client is not None
        await self.async_client.aclose()

    async def _request(
        self,
        method: str,
        url: str,
        **kwargs: Any,
    ) -> httpx.Response:
        assert self.async_client is not None
        return await self.async_client.request(method, url, **kwargs)

    @contextlib.asynccontextmanager
    async def _stream(
        self,
        method: str,
        url: str,
        **kwargs: Any,
    ):
        assert self.async_client is not None
        async with self.async_client.stream(method, url, **kwargs) as response:
            yield response

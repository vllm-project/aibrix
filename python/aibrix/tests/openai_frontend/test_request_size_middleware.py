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

"""RequestSizeLimitMiddleware: buffered (JSON) vs streamed (multipart) bodies."""

from typing import Iterator

import pytest
from fastapi.testclient import TestClient
from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import JSONResponse
from starlette.routing import Route

from aibrix.openai_frontend.frontend.fastapi.middleware.request_size import (
    RequestSizeLimitMiddleware,
)

LIMIT = 1000
MULTIPART = {"content-type": "multipart/form-data; boundary=b"}


async def _stream(req: Request) -> JSONResponse:
    n = 0
    async for chunk in req.stream():
        n += len(chunk)
    return JSONResponse({"n": n})


async def _body(req: Request) -> JSONResponse:
    return JSONResponse({"n": len(await req.body())})


async def _ignore(req: Request) -> JSONResponse:
    return JSONResponse({"ignored": True})


@pytest.fixture(scope="module")
def client() -> TestClient:
    app = Starlette(
        routes=[
            Route("/stream", _stream, methods=["POST"]),
            Route("/body", _body, methods=["POST"]),
            Route("/ignore", _ignore, methods=["POST"]),
        ]
    )
    app.add_middleware(RequestSizeLimitMiddleware, http_max_input_size=LIMIT)
    return TestClient(app, raise_server_exceptions=False)


def _chunks(total: int, size: int = 100) -> Iterator[bytes]:
    for _ in range(total // size):
        yield b"x" * size


@pytest.mark.parametrize("headers", [{}, MULTIPART], ids=["json", "multipart"])
@pytest.mark.parametrize("route", ["/stream", "/body"])
@pytest.mark.parametrize("size", [900, 2 * LIMIT])
def test_chunked_body_limit(client, headers, route, size):
    # No Content-Length: enforce the running count in both body modes.
    r = client.post(route, content=_chunks(size), headers=headers)
    if size <= LIMIT:
        assert r.status_code == 200
        assert r.json() == {"n": size}
    else:
        assert r.status_code == 413
        assert r.json()["error"]["code"] == "content_too_large"


@pytest.mark.parametrize("headers", [{}, MULTIPART], ids=["json", "multipart"])
def test_declared_oversize_rejected(client, headers):
    r = client.post("/stream", content=b"x" * (LIMIT + 1), headers=headers)
    assert r.status_code == 413
    assert r.json()["error"]["code"] == "content_too_large"


def test_non_multipart_rejected_even_if_handler_ignores_body(client):
    # Buffered mode reads the body up front, so every endpoint is protected.
    r = client.post("/ignore", content=_chunks(2 * LIMIT))
    assert r.status_code == 413


def test_invalid_content_length(client):
    r = client.post("/stream", content=b"x", headers={"content-length": "abc"})
    assert r.status_code == 400
    assert r.json()["error"]["code"] == "invalid_content_length"

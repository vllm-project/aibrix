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

"""The routers raise; the app-level handlers own the HTTP mapping."""

from __future__ import annotations

import httpx
import pytest
from fastapi import FastAPI, HTTPException
from fastapi.testclient import TestClient

from aibrix.openai_frontend.frontend.fastapi.errors import add_exception_handlers
from aibrix.openai_frontend.frontend.fastapi.routers import chat
from aibrix.openai_frontend.utils.utils import (
    ClientError,
    EngineUnavailableError,
    ServerError,
)

_CHAT = {"model": "m", "messages": [{"role": "user", "content": "hi"}]}


class _RaisingEngine:
    def __init__(self, exc: BaseException) -> None:
        self.exc = exc

    async def chat(self, request):
        raise self.exc


def _client(engine) -> TestClient:
    app = FastAPI()
    add_exception_handlers(app)
    app.include_router(chat.router)
    app.engine = engine  # type: ignore[attr-defined]
    return TestClient(app, raise_server_exceptions=False)


def _upstream_error() -> httpx.HTTPStatusError:
    req = httpx.Request("POST", "http://engine/v1/chat/completions")
    resp = httpx.Response(
        429,
        request=req,
        json={"error": {"message": "slow down"}},
        headers={"content-type": "application/json"},
    )
    return httpx.HTTPStatusError("429", request=req, response=resp)


@pytest.mark.parametrize(
    "exc, status, body",
    [
        (ClientError("bad input"), 400, {"detail": "bad input"}),
        (NotImplementedError("no chat"), 404, {"detail": "no chat"}),
        (HTTPException(418, "teapot"), 418, {"detail": "teapot"}),
        (ServerError("boom"), 500, {"detail": "boom"}),
        # A subclass of ServerError must not be swallowed as a plain 500.
        (EngineUnavailableError("engine down"), 503, {"detail": "engine down"}),
        # Upstream engine errors are relayed verbatim, not wrapped in "detail".
        (_upstream_error(), 429, {"error": {"message": "slow down"}}),
        # Unknown errors are a generic 500 that does not leak internals.
        (RuntimeError("secret token"), 500, {"detail": "Internal server error"}),
    ],
)
def test_error_mapping(exc, status, body):
    r = _client(_RaisingEngine(exc)).post("/v1/chat/completions", json=_CHAT)
    assert r.status_code == status
    assert r.json() == body


def test_no_engine_attached_is_500():
    r = _client(None).post("/v1/chat/completions", json=_CHAT)
    assert r.status_code == 500
    assert r.json() == {"detail": "No attached inference engine"}

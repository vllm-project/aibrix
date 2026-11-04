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

"""Register only declared engine-native paths, never a catch-all route.

Typed OpenAI and Responses routes are registered first. Errors are handled by
shared application handlers, preserving upstream HTTP status/body/content type.
"""

from __future__ import annotations

from typing import Any

from fastapi import APIRouter, HTTPException, Request, WebSocket
from fastapi.responses import Response

from aibrix.openai_frontend.proxy.engine_spec import ForwardKind

_TYPED_PATHS = {
    "/v1/models",
    "/v1/chat/completions",
    "/v1/completions",
    "/v1/embeddings",
}


def create_router(engine: Any) -> APIRouter:
    router = APIRouter()

    async def websocket_endpoint(websocket: WebSocket) -> None:
        await engine.forward_websocket(websocket.url.path, websocket)

    async def http_endpoint(request: Request) -> Response:
        endpoint = request.url.path
        kind = engine.endpoint_kind(endpoint)
        if kind == ForwardKind.MULTIPART:
            return await engine.forward_multipart(endpoint, request)
        if request.method == "GET":
            return await engine.forward_bodiless("GET", endpoint, request)
        try:
            body = await request.json()
        except ValueError as exc:
            raise HTTPException(400, "Request body must be JSON") from exc
        if not isinstance(body, dict):
            raise HTTPException(400, "Request body must be a JSON object")
        return await engine.forward_json(
            endpoint, body, query=request.url.query, method=request.method
        )

    for path, kind in sorted(engine.endpoint_kinds.items()):
        if path in _TYPED_PATHS or path.startswith("/v1/responses"):
            continue
        if kind == ForwardKind.WEBSOCKET:
            router.add_api_websocket_route(
                path, websocket_endpoint, name="engine_websocket"
            )
        else:
            router.add_api_route(
                path,
                http_endpoint,
                methods=list(engine.endpoint_methods(path)),
                tags=["Engine"],
                response_model=None,
                name="engine_native",
            )
    return router

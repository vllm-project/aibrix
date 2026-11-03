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

"""Request-level error handling for the FastAPI frontend.

Routers stay free of ``try/except`` ladders: they raise, and the handlers
registered here turn the exception into an HTTP response.

* :class:`FrontendError` subclasses carry their own status code (``ClientError``
  400, ``ServerError`` 500, ``EngineUnavailableError`` 503).
* ``NotImplementedError`` means the engine does not offer the API (404).
* ``httpx.HTTPStatusError`` from an upstream engine is relayed as-is, keeping
  the engine's status code, body and content type.
* Anything else is a bug: it is logged with its traceback and answered with a
  generic 500 that does not leak internal details.

``HTTPException`` keeps FastAPI's own handler.
"""

from __future__ import annotations

import logging

import httpx
from fastapi import FastAPI, Request, Response
from fastapi.responses import JSONResponse

from aibrix.openai_frontend.engine.engine import LLMEngine
from aibrix.openai_frontend.utils.utils import (
    FrontendError,
    ServerError,
    StatusCode,
)

logger = logging.getLogger(__name__)


def get_engine(request: Request) -> LLMEngine:
    """FastAPI dependency returning the engine attached to the app."""
    engine = getattr(request.app, "engine", None)
    if engine is None:
        raise ServerError("No attached inference engine")
    return engine


def _detail(status_code: int, detail: str) -> JSONResponse:
    return JSONResponse(status_code=status_code, content={"detail": detail})


async def _frontend_error(request: Request, exc: Exception) -> Response:
    assert isinstance(exc, FrontendError)
    status_code = exc.status_code
    if status_code >= StatusCode.SERVER_ERROR:
        logger.error(
            "%s %s failed: %s", request.method, request.url.path, exc, exc_info=exc
        )
    return _detail(status_code, str(exc))


async def _not_implemented(_: Request, exc: Exception) -> Response:
    return _detail(StatusCode.NOT_FOUND, str(exc) or "Not implemented")


async def _engine_http_error(_: Request, exc: Exception) -> Response:
    assert isinstance(exc, httpx.HTTPStatusError)
    upstream = exc.response
    return Response(
        content=upstream.content,
        status_code=upstream.status_code,
        media_type=upstream.headers.get("content-type"),
    )


async def _unexpected(request: Request, exc: Exception) -> Response:
    logger.error(
        "Unhandled error on %s %s", request.method, request.url.path, exc_info=exc
    )
    return _detail(StatusCode.SERVER_ERROR, "Internal server error")


def add_exception_handlers(app: FastAPI) -> None:
    """Register the frontend's exception-to-response mapping on ``app``."""
    app.add_exception_handler(FrontendError, _frontend_error)
    app.add_exception_handler(NotImplementedError, _not_implemented)
    app.add_exception_handler(httpx.HTTPStatusError, _engine_http_error)
    app.add_exception_handler(Exception, _unexpected)

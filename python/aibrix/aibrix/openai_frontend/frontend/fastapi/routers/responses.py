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

"""Explicit Responses routes; no arbitrary-path passthrough."""

from __future__ import annotations

from typing import Annotated, Any, Literal, Optional

from fastapi import APIRouter, Depends, Query, Request
from fastapi.responses import Response as HTTPResponse
from pydantic import BaseModel

from aibrix.openai_frontend.engine.engine import LLMEngine
from aibrix.openai_frontend.frontend.fastapi.errors import get_engine
from aibrix.openai_frontend.frontend.fastapi.responses import ClosingStreamingResponse
from aibrix.openai_frontend.schemas.responses import (
    CompactedResponse,
    CreateResponseRequest,
    InputTokenCountParams,
    InputTokenCountResponse,
    Response,
    ResponseCompactParams,
    ResponseDeleted,
    ResponseIncludable,
    ResponseInputItemList,
)

router = APIRouter(prefix="/v1/responses", tags=["Responses"])


def _response(value: BaseModel | Any) -> Any:
    if isinstance(value, (BaseModel, HTTPResponse)):
        return value
    return ClosingStreamingResponse(value, media_type="text/event-stream")


@router.post("", response_model=Response, response_model_exclude_unset=True)
async def create_response(
    request: CreateResponseRequest, engine: LLMEngine = Depends(get_engine)
) -> Any:
    return _response(await engine.responses("create", request))


@router.post(
    "/compact", response_model=CompactedResponse, response_model_exclude_unset=True
)
async def compact_response(
    request: ResponseCompactParams, engine: LLMEngine = Depends(get_engine)
) -> Any:
    return await engine.responses("compact", request)


@router.post(
    "/input_tokens",
    response_model=InputTokenCountResponse,
    response_model_exclude_unset=True,
)
async def count_input_tokens(
    request: InputTokenCountParams, engine: LLMEngine = Depends(get_engine)
) -> Any:
    return await engine.responses("input_tokens", request)


@router.get(
    "/{response_id}", response_model=Response, response_model_exclude_unset=True
)
async def retrieve_response(
    response_id: str,
    raw_request: Request,
    stream: Optional[bool] = None,
    starting_after: Annotated[Optional[int], Query(ge=0)] = None,
    include: Annotated[Optional[list[ResponseIncludable]], Query()] = None,
    include_obfuscation: Optional[bool] = None,
    engine: LLMEngine = Depends(get_engine),
) -> Any:
    return _response(
        await engine.responses(
            "retrieve",
            response_id=response_id,
            query=list(raw_request.query_params.multi_items()),
        )
    )


@router.delete(
    "/{response_id}", response_model=ResponseDeleted, response_model_exclude_unset=True
)
async def delete_response(
    response_id: str, engine: LLMEngine = Depends(get_engine)
) -> Any:
    response = await engine.responses("delete", response_id=response_id)
    return HTTPResponse(status_code=204) if response is None else response


@router.post(
    "/{response_id}/cancel", response_model=Response, response_model_exclude_unset=True
)
async def cancel_response(
    response_id: str, engine: LLMEngine = Depends(get_engine)
) -> Any:
    return await engine.responses("cancel", response_id=response_id)


@router.get(
    "/{response_id}/input_items",
    response_model=ResponseInputItemList,
    response_model_exclude_unset=True,
)
async def list_input_items(
    response_id: str,
    raw_request: Request,
    after: Optional[str] = None,
    limit: Annotated[Optional[int], Query(ge=1, le=100)] = None,
    order: Optional[Literal["asc", "desc"]] = None,
    include: Annotated[Optional[list[ResponseIncludable]], Query()] = None,
    engine: LLMEngine = Depends(get_engine),
) -> Any:
    return await engine.responses(
        "input_items",
        response_id=response_id,
        query=list(raw_request.query_params.multi_items()),
    )

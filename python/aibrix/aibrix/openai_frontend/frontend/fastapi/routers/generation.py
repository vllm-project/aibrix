# Copyright 2026 The Aibrix Team.
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy at http://www.apache.org/licenses/LICENSE-2.0

"""Small proxy-only generation surface; no arbitrary-path forwarding."""

from fastapi import APIRouter, Depends, Request
from fastapi.responses import Response

from aibrix.openai_frontend.engine.engine import LLMEngine
from aibrix.openai_frontend.engine.proxy_engine import ProxyEngine
from aibrix.openai_frontend.frontend.fastapi.errors import get_engine
from aibrix.openai_frontend.schemas.generation import (
    ImageGenerationRequest,
    VideoGenerationRequest,
    VideoResponse,
)
from aibrix.openai_frontend.utils.utils import ClientError

VIDEO_CONTENT_REQUEST_HEADERS = frozenset({"range", "if-range", "accept-encoding"})

router = APIRouter()


def _proxy(engine: LLMEngine) -> ProxyEngine:
    if not isinstance(engine, ProxyEngine):
        raise NotImplementedError("Generation is only supported by the proxy engine")
    return engine


@router.post("/v1/images/generations", response_model=None, tags=["Images"])
async def create_image(
    request: ImageGenerationRequest, engine: LLMEngine = Depends(get_engine)
) -> Response:
    return await _proxy(engine).image(request)


@router.post(
    "/v1/videos",
    response_model=VideoResponse,
    response_model_exclude_unset=True,
    tags=["Videos"],
)
async def create_video(
    request: VideoGenerationRequest, engine: LLMEngine = Depends(get_engine)
) -> VideoResponse | Response:
    return await _proxy(engine).video(request)


@router.get("/v1/videos/{video_id}/content", response_model=None, tags=["Videos"])
async def video_content(
    video_id: str, request: Request, engine: LLMEngine = Depends(get_engine)
) -> Response:
    return await _proxy(engine).video_content(
        video_id,
        request.url.query,
        {
            key: value
            for key, value in request.headers.items()
            if key in VIDEO_CONTENT_REQUEST_HEADERS
        },
    )


@router.get(
    "/v1/videos/{video_id}",
    response_model=VideoResponse,
    response_model_exclude_unset=True,
    tags=["Videos"],
)
async def retrieve_video(
    video_id: str,
    request: Request,
    engine: LLMEngine = Depends(get_engine),
) -> VideoResponse | Response:
    if "output" in request.query_params:
        raise ClientError("output is only supported by POST /v1/videos")
    return await _proxy(engine).retrieve_video(video_id)

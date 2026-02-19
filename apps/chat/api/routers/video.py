"""Video generation endpoints — submit jobs and poll status."""

from __future__ import annotations

import logging

from fastapi import APIRouter, HTTPException
from fastapi.responses import Response

from models.schemas import VideoGenerateRequest, VideoJobResponse
from services.providers import get_video_provider

logger = logging.getLogger(__name__)

router = APIRouter(prefix="/api/video", tags=["video"])


@router.post("/generate", response_model=VideoJobResponse)
async def generate_video(req: VideoGenerateRequest):
    """Submit a video generation job via the configured provider."""
    try:
        provider = get_video_provider()
        result = await provider.generate(
            prompt=req.prompt,
            model=req.model,
            size=req.size,
            seconds=req.seconds,
        )
        return result
    except httpx.HTTPError as e:
        logger.exception("Video generation failed")
        raise HTTPException(status_code=502, detail=str(e))


@router.get("/status/{job_id}", response_model=VideoJobResponse)
async def video_status(job_id: str):
    """Poll the status of a video generation job."""
    try:
        provider = get_video_provider()
        result = await provider.get_status(job_id)
        return result
    except httpx.HTTPError as e:
        logger.exception("Video status check failed")
        raise HTTPException(status_code=502, detail=str(e))


@router.get("/content/{job_id}")
async def video_content(job_id: str):
    """Download the finished video file (MP4)."""
    try:
        provider = get_video_provider()
        data = await provider.get_content(job_id)
        return Response(
            content=data,
            media_type="video/mp4",
            headers={
                "Content-Disposition": f'attachment; filename="{job_id}.mp4"',
            },
        )
    except ValueError as e:
        raise HTTPException(status_code=404, detail=str(e))
    except httpx.HTTPError as e:
        logger.exception("Video content download failed")
        raise HTTPException(status_code=502, detail=str(e))

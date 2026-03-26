"""Image generation endpoint — proxies to the configured image provider."""

from __future__ import annotations

import logging

from fastapi import APIRouter, File, Form, HTTPException, UploadFile
import httpx

from config import settings
from models.schemas import ImageGenerateRequest, ImageGenerateResponse
from services.providers import get_image_provider

logger = logging.getLogger(__name__)

router = APIRouter(prefix="/api/image", tags=["image"])


@router.post("/generate", response_model=ImageGenerateResponse)
async def generate_image(req: ImageGenerateRequest):
    """Generate images via the configured provider (default: OpenAI)."""
    kwargs = {}
    if req.quality:
        kwargs["quality"] = req.quality
    if req.style:
        kwargs["style"] = req.style
    if req.response_format:
        kwargs["response_format"] = req.response_format

    try:
        provider = get_image_provider()
        result = await provider.generate(
            prompt=req.prompt,
            model=req.model,
            size=req.size,
            n=req.n,
            **kwargs,
        )
        return result
    except httpx.HTTPError as e:
        logger.exception("Image generation failed")
        raise HTTPException(status_code=502, detail=str(e))


@router.post("/edit", response_model=ImageGenerateResponse)
async def edit_image(
    image: UploadFile = File(...),
    prompt: str = Form(...),
    model: str = Form(settings.image_edit_model or "dall-e-2"),
    size: str = Form("1024x1024"),
    n: int = Form(1),
):
    """Edit an image with a text prompt via the configured provider."""
    try:
        provider = get_image_provider()
        image_bytes = await image.read()
        fname = image.filename or "image.png"
        ctype = image.content_type or "image/png"
        result = await provider.edit(
            image=image_bytes,
            filename=fname,
            content_type=ctype,
            prompt=prompt,
            model=model,
            size=size,
            n=n,
        )
        return result
    except httpx.HTTPError as e:
        logger.exception("Image edit failed")
        raise HTTPException(status_code=502, detail=str(e))

"""Audio endpoints — transcription (STT) and speech (TTS)."""

from __future__ import annotations

import logging

from fastapi import APIRouter, File, Form, HTTPException, UploadFile
from fastapi.responses import Response
import httpx

from models.schemas import AudioSpeechRequest, AudioTranscribeResponse
from services.providers import get_audio_provider

logger = logging.getLogger(__name__)

router = APIRouter(prefix="/api/audio", tags=["audio"])


@router.post("/transcribe", response_model=AudioTranscribeResponse)
async def transcribe(
    file: UploadFile = File(...),
    model: str = Form("whisper-1"),
    language: str | None = Form(None),
):
    """Transcribe an audio file via the configured audio provider."""
    try:
        provider = get_audio_provider()
        file_bytes = await file.read()
        result = await provider.transcribe(
            file_bytes=file_bytes,
            filename=file.filename or "audio.wav",
            model=model,
            language=language,
        )
        return result
    except httpx.HTTPError as e:
        logger.exception("Transcription failed")
        raise HTTPException(status_code=502, detail=str(e))


@router.post("/speech")
async def speech(req: AudioSpeechRequest):
    """Generate speech audio from text via the configured audio provider."""
    try:
        provider = get_audio_provider()
        audio_bytes = await provider.speech(
            text=req.input,
            model=req.model,
            voice=req.voice,
            response_format=req.response_format,
            speed=req.speed,
        )
        media_types = {
            "mp3": "audio/mpeg",
            "opus": "audio/opus",
            "aac": "audio/aac",
            "flac": "audio/flac",
            "wav": "audio/wav",
        }
        media_type = media_types.get(req.response_format, "audio/mpeg")
        return Response(content=audio_bytes, media_type=media_type)
    except httpx.HTTPError as e:
        logger.exception("Speech generation failed")
        raise HTTPException(status_code=502, detail=str(e))

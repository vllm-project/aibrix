"""AIBrix Chat BFF — FastAPI entry point."""

from contextlib import asynccontextmanager
from pathlib import Path

from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import FileResponse
from fastapi.staticfiles import StaticFiles

from config import settings
from routers import audio, auth, chat, conversations, health, images, models, projects, video
from services.providers import (
    get_audio_provider,
    get_chat_provider,
    get_image_provider,
    get_video_provider,
)


@asynccontextmanager
async def lifespan(app: FastAPI):
    """Startup/shutdown hook — initialise persistent HTTP connection pools."""
    providers = [
        get_chat_provider(),
        get_image_provider(),
        get_audio_provider(),
        get_video_provider(),
    ]
    for p in providers:
        await p.startup()
    yield
    for p in providers:
        await p.shutdown()


app = FastAPI(
    title="AIBrix Chat API",
    description=(
        "Backend-for-Frontend (BFF) for AIBrix Chat. "
        "Proxies to any OpenAI-compatible endpoint (AIBrix gateway, vLLM, OpenAI cloud) "
        "configured via the AIBRIX_GATEWAY_URL environment variable.\n\n"
        "**Swagger UI**: `/api/docs`  |  **ReDoc**: `/api/redoc`  |  **OpenAPI JSON**: `/api/openapi.json`"
    ),
    version=settings.app_version,
    docs_url="/api/docs",
    redoc_url="/api/redoc",
    openapi_url="/api/openapi.json",
    lifespan=lifespan,
)

# CORS
origins = [o.strip() for o in settings.cors_origins.split(",")]
app.add_middleware(
    CORSMiddleware,
    allow_origins=origins,
    allow_credentials=True,
    allow_methods=["GET", "POST", "PATCH", "DELETE"],
    allow_headers=["Authorization", "Content-Type"],
)

# Mount routers
app.include_router(auth.router)
app.include_router(health.router)
app.include_router(models.router)
app.include_router(conversations.router)
app.include_router(chat.router)
app.include_router(projects.router)
app.include_router(images.router)
app.include_router(audio.router)
app.include_router(video.router)

# Serve frontend static files (built by `npm run build` into ./static)
_static_dir = Path(__file__).resolve().parent / "static"
if _static_dir.is_dir():
    _assets_dir = _static_dir / "assets"
    if _assets_dir.is_dir():
        app.mount("/assets", StaticFiles(directory=_assets_dir), name="static-assets")

    @app.get("/{full_path:path}")
    async def serve_spa(full_path: str):
        """SPA fallback — serve index.html for any non-API route."""
        file = (_static_dir / full_path).resolve()
        if file.is_relative_to(_static_dir) and file.is_file():
            return FileResponse(file)
        return FileResponse(_static_dir / "index.html")

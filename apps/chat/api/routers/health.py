"""Health check endpoint."""

from fastapi import APIRouter

from config import settings
from models.schemas import HealthResponse
from services import gateway

router = APIRouter(prefix="/api", tags=["health"])


@router.get("/health", response_model=HealthResponse)
async def health():
    reachable = await gateway.check_health()
    return HealthResponse(
        status="ok",
        version=settings.app_version,
        gateway_reachable=reachable,
    )

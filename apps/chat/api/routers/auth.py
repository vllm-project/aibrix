"""Auth endpoints: login, logout, me."""

from __future__ import annotations

from fastapi import APIRouter, Depends, HTTPException, Request

from config import settings
from middleware.auth import get_current_user
from models.schemas import LoginRequest, LoginResponse, User
from services.auth import auth_store

router = APIRouter(prefix="/api/auth", tags=["auth"])


@router.get("/mode")
async def auth_mode():
    """Return the current auth mode so the frontend can adapt."""
    return {"auth_mode": settings.auth_mode}


@router.post("/login", response_model=LoginResponse)
async def login(req: LoginRequest):
    name = req.name.strip()
    if not name:
        raise HTTPException(status_code=400, detail="Name is required")
    user, token = auth_store.login(name)
    return LoginResponse(user=user, token=token)


@router.get("/me", response_model=User)
async def me(user: User = Depends(get_current_user)):
    return user


@router.post("/logout")
async def logout(request: Request):
    auth_header = request.headers.get("Authorization", "")
    if auth_header.startswith("Bearer "):
        token = auth_header[7:]
        auth_store.logout(token)
    return {"ok": True}

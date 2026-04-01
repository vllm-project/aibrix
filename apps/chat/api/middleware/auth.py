"""Auth dependency for FastAPI routes."""

from __future__ import annotations

from fastapi import Depends, HTTPException, Request

from config import settings
from models.schemas import User
from services.auth import auth_store

# Default user returned when auth is disabled.
# id="" so that list_all(user_id="") skips filtering (empty is falsy),
# matching pre-existing and seeded data which also have user_id="".
_DEFAULT_USER = User(id="", name="Default User")


async def get_current_user(request: Request) -> User:
    """FastAPI dependency that resolves the current user.

    - auth_mode == "none": returns a default user (no auth required)
    - auth_mode == "simple": extracts Bearer token, looks up user
    """
    if settings.auth_mode == "none":
        return _DEFAULT_USER

    auth_header = request.headers.get("Authorization", "")
    if not auth_header.startswith("Bearer "):
        raise HTTPException(status_code=401, detail="Not authenticated")

    token = auth_header[7:]
    user = auth_store.get_user_by_token(token)
    if user is None:
        raise HTTPException(status_code=401, detail="Invalid or expired token")

    return user

# Copyright 2024 The Aibrix Team.
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

import warnings
from typing import Optional

import redis.asyncio as redis
from fastapi import APIRouter, HTTPException, Request
from pydantic import BaseModel, Field, field_validator

from aibrix.logger import init_logger

logger = init_logger(__name__)
router = APIRouter()

# Legacy router for backward compatibility with old POST-based endpoints
legacy_router = APIRouter()


class User(BaseModel):
    """User model with rate limiting configuration.

    Matches Go implementation:
    - Name: Required, user identifier
    - RPM: Requests per minute limit
    - TPM: Tokens per minute limit
    """

    name: str = Field(..., description="User name (required)")
    rpm: int = Field(default=0, description="Requests per minute limit")
    tpm: int = Field(default=0, description="Tokens per minute limit")

    @field_validator("rpm", "tpm")
    @classmethod
    def validate_non_negative(cls, v: int) -> int:
        """Validate that rpm and tpm are non-negative."""
        if v < 0:
            raise ValueError("rpm and tpm must be non-negative")
        return v


class UserResponse(BaseModel):
    """Response model for user operations."""

    message: str
    user: Optional[User] = None


def _gen_key(name: str) -> str:
    """Generate Redis key for user.

    Args:
        name: User name

    Returns:
        Redis key in format: aibrix-users/{name}
    """
    return f"aibrix-users/{name}"


async def _get_redis_client(request: Request) -> redis.Redis:
    """Get Redis client from app state.

    Args:
        request: FastAPI request object

    Returns:
        Redis client instance
    """
    return request.app.state.redis_client


# ==================== RESTful API Endpoints ====================


@router.post("/users")
async def create_user(request: Request, user: User) -> UserResponse:
    """Create a new user with rate limits.

    If user already exists, returns existing user without modification.

    Args:
        request: FastAPI request
        user: User data to create

    Returns:
        UserResponse with creation status
    """
    redis_client = await _get_redis_client(request)
    key = _gen_key(user.name)

    # Check if user exists
    exists = await redis_client.exists(key)
    if exists:
        logger.info(f"User already exists: {user.name}")
        return UserResponse(message=f"User: {user.name} exists", user=user)

    # Store user as JSON
    await redis_client.set(key, user.model_dump_json())

    logger.info(f"Created user: {user.name}, rpm={user.rpm}, tpm={user.tpm}")
    return UserResponse(message=f"Created User: {user.name}", user=user)


@router.get("/users/{name}")
async def get_user(request: Request, name: str) -> UserResponse:
    """Read user information by name.

    Args:
        request: FastAPI request
        name: User name to look up

    Returns:
        UserResponse with user data

    Raises:
        HTTPException: 404 if user not found
    """
    redis_client = await _get_redis_client(request)
    key = _gen_key(name)

    data = await redis_client.get(key)
    if not data:
        logger.warning(f"User not found: {name}")
        raise HTTPException(status_code=404, detail="user does not exist")

    # Parse JSON data
    stored_user = User.model_validate_json(data)

    logger.info(f"Read user: {stored_user.name}")
    return UserResponse(message=f"User: {stored_user.name}", user=stored_user)


@router.put("/users/{name}")
async def update_user(request: Request, name: str, user: User) -> UserResponse:
    """Update user information.

    Args:
        request: FastAPI request
        name: User name in the URL path
        user: Updated user data

    Returns:
        UserResponse with update status

    Raises:
        HTTPException: 404 if user not found
    """
    redis_client = await _get_redis_client(request)
    key = _gen_key(name)

    # Check if user exists
    exists = await redis_client.exists(key)
    if not exists:
        logger.warning(f"Cannot update non-existent user: {name}")
        raise HTTPException(
            status_code=404, detail=f"User: {name} does not exist"
        )

    # Use the name from URL path, override body if different
    user.name = name
    await redis_client.set(key, user.model_dump_json())

    logger.info(f"Updated user: {user.name}, rpm={user.rpm}, tpm={user.tpm}")
    return UserResponse(message=f"Updated User: {user.name}", user=user)


@router.delete("/users/{name}")
async def delete_user(request: Request, name: str) -> UserResponse:
    """Delete a user.

    Args:
        request: FastAPI request
        name: User name to delete

    Returns:
        UserResponse with deletion status

    Raises:
        HTTPException: 404 if user not found
    """
    redis_client = await _get_redis_client(request)
    key = _gen_key(name)

    # Check if user exists
    exists = await redis_client.exists(key)
    if not exists:
        logger.warning(f"Cannot delete non-existent user: {name}")
        raise HTTPException(
            status_code=404, detail=f"User: {name} does not exist"
        )

    # Delete user
    await redis_client.delete(key)

    logger.info(f"Deleted user: {name}")
    return UserResponse(message=f"Deleted User: {name}", user=None)


# ==================== Legacy API Endpoints (Deprecated) ====================
# These endpoints are kept for backward compatibility.
# Use the RESTful endpoints above (/users, /users/{name}) instead.


@legacy_router.post("/CreateUser")
async def legacy_create_user(request: Request, user: User) -> UserResponse:
    """Create a new user. Deprecated: use POST /users instead."""
    warnings.warn(
        "POST /CreateUser is deprecated, use POST /users instead",
        DeprecationWarning,
        stacklevel=1,
    )
    return await create_user(request, user)


@legacy_router.post("/ReadUser")
async def legacy_read_user(request: Request, user: User) -> UserResponse:
    """Read user information. Deprecated: use GET /users/{name} instead."""
    warnings.warn(
        "POST /ReadUser is deprecated, use GET /users/{name} instead",
        DeprecationWarning,
        stacklevel=1,
    )
    return await get_user(request, user.name)


@legacy_router.post("/UpdateUser")
async def legacy_update_user(request: Request, user: User) -> UserResponse:
    """Update user information. Deprecated: use PUT /users/{name} instead."""
    warnings.warn(
        "POST /UpdateUser is deprecated, use PUT /users/{name} instead",
        DeprecationWarning,
        stacklevel=1,
    )
    return await update_user(request, user.name, user)


@legacy_router.post("/DeleteUser")
async def legacy_delete_user(request: Request, user: User) -> UserResponse:
    """Delete a user. Deprecated: use DELETE /users/{name} instead."""
    warnings.warn(
        "POST /DeleteUser is deprecated, use DELETE /users/{name} instead",
        DeprecationWarning,
        stacklevel=1,
    )
    return await delete_user(request, user.name)

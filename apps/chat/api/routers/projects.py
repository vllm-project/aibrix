"""Projects CRUD endpoints."""

from __future__ import annotations

from fastapi import APIRouter, Depends, HTTPException

from middleware.auth import get_current_user
from models.schemas import CreateProjectRequest, UpdateProjectRequest, User
from services.project import project_store

router = APIRouter(prefix="/api/projects", tags=["projects"])


@router.post("")
async def create_project(
    req: CreateProjectRequest,
    user: User = Depends(get_current_user),
):
    project = project_store.create(
        name=req.name, description=req.description, user_id=user.id,
    )
    return project.model_dump()


@router.get("")
async def list_projects(user: User = Depends(get_current_user)):
    return project_store.list_all(user_id=user.id)


@router.get("/{project_id}")
async def get_project(
    project_id: str,
    user: User = Depends(get_current_user),
):
    project = project_store.get(project_id)
    if project is None or (project.user_id and project.user_id != user.id):
        raise HTTPException(status_code=404, detail="Project not found")
    return project.model_dump()


@router.patch("/{project_id}")
async def update_project(
    project_id: str,
    req: UpdateProjectRequest,
    user: User = Depends(get_current_user),
):
    project = project_store.get(project_id)
    if project is None or (project.user_id and project.user_id != user.id):
        raise HTTPException(status_code=404, detail="Project not found")
    updated = project_store.update(
        project_id,
        name=req.name,
        description=req.description,
        instructions=req.instructions,
    )
    return updated.model_dump()


@router.delete("/{project_id}")
async def delete_project(
    project_id: str,
    user: User = Depends(get_current_user),
):
    project = project_store.get(project_id)
    if project is None or (project.user_id and project.user_id != user.id):
        raise HTTPException(status_code=404, detail="Project not found")
    project_store.delete(project_id)
    return {"ok": True}

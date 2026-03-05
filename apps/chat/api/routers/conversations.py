"""Conversation CRUD endpoints."""

from fastapi import APIRouter, Depends, HTTPException
from pydantic import BaseModel

from middleware.auth import get_current_user
from models.schemas import Conversation, ConversationSummary, User
from services.conversation import store

router = APIRouter(prefix="/api/conversations", tags=["conversations"])


class CreateConversationRequest(BaseModel):
    model: str | None = None
    title: str = "New Chat"
    project_id: str | None = None


class UpdateTitleRequest(BaseModel):
    title: str


@router.post("", response_model=Conversation, status_code=201)
async def create_conversation(
    req: CreateConversationRequest,
    user: User = Depends(get_current_user),
):
    return store.create(
        model=req.model, title=req.title, user_id=user.id,
        project_id=req.project_id,
    )


@router.get("", response_model=list[ConversationSummary])
async def list_conversations(user: User = Depends(get_current_user)):
    return store.list_all(user_id=user.id)


@router.get("/{conversation_id}", response_model=Conversation)
async def get_conversation(
    conversation_id: str,
    user: User = Depends(get_current_user),
):
    conv = store.get(conversation_id)
    if conv is None or (conv.user_id and conv.user_id != user.id):
        raise HTTPException(status_code=404, detail="Conversation not found")
    return conv


@router.patch("/{conversation_id}", response_model=Conversation)
async def update_conversation(
    conversation_id: str,
    req: UpdateTitleRequest,
    user: User = Depends(get_current_user),
):
    conv = store.get(conversation_id)
    if conv is None or (conv.user_id and conv.user_id != user.id):
        raise HTTPException(status_code=404, detail="Conversation not found")
    return store.update_title(conversation_id, req.title)


@router.delete("/{conversation_id}", status_code=204)
async def delete_conversation(
    conversation_id: str,
    user: User = Depends(get_current_user),
):
    conv = store.get(conversation_id)
    if conv is None or (conv.user_id and conv.user_id != user.id):
        raise HTTPException(status_code=404, detail="Conversation not found")
    store.delete(conversation_id)

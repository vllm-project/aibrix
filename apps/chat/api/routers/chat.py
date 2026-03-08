"""Chat completions endpoint — the core SSE streaming route."""

from __future__ import annotations

import json
import logging

from fastapi import APIRouter, Depends, HTTPException

logger = logging.getLogger(__name__)
from fastapi.responses import JSONResponse
from sse_starlette.sse import EventSourceResponse

from middleware.auth import get_current_user
from models.schemas import CompletionRequest, CompletionResponse, Message, User
from services.conversation import store
from services import gateway
import httpx

router = APIRouter(tags=["chat"])


def _auto_title(conversation_id: str) -> None:
    """Set conversation title from first user message if still default."""
    conv = store.get(conversation_id)
    if conv is None or conv.title != "New Chat":
        return
    for msg in conv.messages:
        if msg.role == "user":
            text = msg.content if isinstance(msg.content, str) else ""
            if text:
                title = text[:40].strip()
                if len(text) > 40:
                    title += "..."
                store.update_title(conversation_id, title)
            break


@router.post("/api/conversations/{conversation_id}/completions")
async def chat_completions(
    conversation_id: str,
    req: CompletionRequest,
    user: User = Depends(get_current_user),
):
    """Send a message and get a response. Streams via SSE when stream=True.

    The frontend sends only the latest user message.
    The server builds the full history from the conversation and
    forwards it to the AIBrix gateway.
    """
    conv = store.get(conversation_id)
    if conv is None or (conv.user_id and conv.user_id != user.id):
        raise HTTPException(status_code=404, detail="Conversation not found")

    # Update conversation model if changed
    if conv.model != req.model:
        conv.model = req.model

    # Store the user message
    user_msg = Message(role="user", content=req.message, attachments=req.attachments)
    store.add_message(conversation_id, user_msg)

    # Resolve system prompt: explicit request > project instructions > None
    system_prompt = req.system_prompt
    if not system_prompt and conv.project_id:
        from services.project import project_store
        project = project_store.get(conv.project_id)
        if project and project.instructions:
            system_prompt = project.instructions

    # Build full message history for the gateway
    messages = store.get_messages_for_gateway(
        conversation_id, system_prompt=system_prompt
    )
    
    if req.stream:
        return EventSourceResponse(_stream_response(
            conversation_id=conversation_id,
            messages=messages,
            model=req.model,
            temperature=req.temperature,
            max_tokens=req.max_tokens,
        ))
    else:
        return await _non_stream_response(
            conversation_id=conversation_id,
            messages=messages,
            model=req.model,
            temperature=req.temperature,
            max_tokens=req.max_tokens,
        )


async def _stream_response(
    conversation_id: str,
    messages: list[dict],
    model: str,
    temperature: float,
    max_tokens: int,
):
    """Generator that yields SSE events from the gateway stream."""
    collected_content = []

    try:
        async for event_data in gateway.chat_completion_stream(
            messages=messages,
            model=model,
            temperature=temperature,
            max_tokens=max_tokens,
        ):
            parsed = json.loads(event_data)
            if parsed.get("event") == "text_delta":
                collected_content.append(parsed["delta"])
            yield {"data": event_data}

    except httpx.HTTPError as e:
        logger.exception("Chat stream error for model=%s", model)
        error_event = json.dumps({"event": "error", "message": str(e)})
        yield {"data": error_event}

    finally:
        # Save whatever content was collected, even if client disconnected
        full_content = "".join(collected_content)
        if full_content:
            assistant_msg = Message(role="assistant", content=full_content, model=model)
            store.add_message(conversation_id, assistant_msg)
            _auto_title(conversation_id)


async def _non_stream_response(
    conversation_id: str,
    messages: list[dict],
    model: str,
    temperature: float,
    max_tokens: int,
) -> JSONResponse:
    """Non-streaming: call gateway and return full response."""
    try:
        data = await gateway.chat_completion(
            messages=messages,
            model=model,
            temperature=temperature,
            max_tokens=max_tokens,
        )

        content = data["choices"][0]["message"]["content"]
        usage = data.get("usage")

        # Store assistant message
        assistant_msg = Message(role="assistant", content=content, model=model)
        store.add_message(conversation_id, assistant_msg)
        _auto_title(conversation_id)

        response = CompletionResponse(
            id=data.get("id", ""),
            conversation_id=conversation_id,
            model=model,
            message=assistant_msg,
            usage=usage,
        )
        return JSONResponse(content=response.model_dump())

    except httpx.HTTPError as e:
        logger.exception("Chat completion error for model=%s", model)
        raise HTTPException(status_code=502, detail=f"Gateway error: {e}")

import json

from fastapi.testclient import TestClient
from sse_starlette.sse import AppStatus

from main import app
from models.schemas import Message
from routers import chat as chat_router
from services.conversation import store


def setup_function() -> None:
    store._conversations.clear()
    AppStatus.should_exit_event = None


def test_stream_completion_replaces_edited_user_message_and_truncates_history(monkeypatch) -> None:
    captured: dict[str, list[dict]] = {}

    async def fake_stream(messages: list[dict], **kwargs):
        captured["messages"] = messages
        yield json.dumps({"event": "text_delta", "delta": "edited response"})
        yield json.dumps({"event": "done"})

    monkeypatch.setattr(chat_router.gateway, "chat_completion_stream", fake_stream)

    conv = store.create(model="gpt-4.1")
    store.add_message(conv.id, Message(role="user", content="first"))
    store.add_message(conv.id, Message(role="assistant", content="first response"))
    edited = store.add_message(conv.id, Message(role="user", content="old question"))
    store.add_message(conv.id, Message(role="assistant", content="old response"))

    with TestClient(app) as client:
        response = client.post(
            f"/api/conversations/{conv.id}/completions",
            data={
                "message": "edited question",
                "model": "gpt-4.1",
                "replace_message_id": edited.id,
            },
        )

    assert response.status_code == 200
    assert [message.content for message in store.get(conv.id).messages] == [
        "first",
        "first response",
        "edited question",
        "edited response",
    ]
    assert captured["messages"] == [
        {"role": "user", "content": "first"},
        {"role": "assistant", "content": "first response"},
        {"role": "user", "content": "edited question"},
    ]


def test_stream_completion_retries_from_existing_user_message_without_duplicating_it(monkeypatch) -> None:
    captured: dict[str, list[dict]] = {}

    async def fake_stream(messages: list[dict], **kwargs):
        captured["messages"] = messages
        yield json.dumps({"event": "text_delta", "delta": "new response"})
        yield json.dumps({"event": "done"})

    monkeypatch.setattr(chat_router.gateway, "chat_completion_stream", fake_stream)

    conv = store.create(model="gpt-4.1")
    user = store.add_message(conv.id, Message(role="user", content="try again"))
    store.add_message(conv.id, Message(role="assistant", content="old response"))

    with TestClient(app) as client:
        response = client.post(
            f"/api/conversations/{conv.id}/completions",
            data={
                "message": "try again",
                "model": "gpt-4.1",
                "retry_from_message_id": user.id,
            },
        )

    assert response.status_code == 200
    assert [message.content for message in store.get(conv.id).messages] == [
        "try again",
        "new response",
    ]
    assert captured["messages"] == [{"role": "user", "content": "try again"}]

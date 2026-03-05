"""In-memory conversation store. Swap for SQLite/PostgreSQL later."""

from __future__ import annotations

from datetime import datetime, timezone

from models.schemas import Conversation, ConversationSummary, Message


class ConversationStore:
    """Thread-safe in-memory conversation storage."""

    def __init__(self) -> None:
        self._conversations: dict[str, Conversation] = {}

    def create(
        self,
        model: str | None = None,
        title: str = "New Chat",
        user_id: str = "",
        project_id: str | None = None,
    ) -> Conversation:
        conv = Conversation(
            model=model, title=title, user_id=user_id, project_id=project_id,
        )
        self._conversations[conv.id] = conv
        return conv

    def get(self, conversation_id: str) -> Conversation | None:
        return self._conversations.get(conversation_id)

    def list_all(self, user_id: str = "") -> list[ConversationSummary]:
        summaries = []
        for conv in self._conversations.values():
            if user_id and conv.user_id != user_id:
                continue
            summaries.append(
                ConversationSummary(
                    id=conv.id,
                    title=conv.title,
                    model=conv.model,
                    message_count=len(conv.messages),
                    created_at=conv.created_at,
                    updated_at=conv.updated_at,
                )
            )
        summaries.sort(key=lambda s: s.updated_at, reverse=True)
        return summaries

    def delete(self, conversation_id: str) -> bool:
        return self._conversations.pop(conversation_id, None) is not None

    def update_title(self, conversation_id: str, title: str) -> Conversation | None:
        conv = self._conversations.get(conversation_id)
        if conv is None:
            return None
        conv.title = title
        conv.updated_at = datetime.now(timezone.utc).isoformat()
        return conv

    def add_message(self, conversation_id: str, message: Message) -> Message | None:
        conv = self._conversations.get(conversation_id)
        if conv is None:
            return None
        # Set parent_id to the last message if not specified
        if message.parent_id is None and conv.messages:
            message.parent_id = conv.messages[-1].id
        conv.messages.append(message)
        conv.updated_at = datetime.now(timezone.utc).isoformat()
        return message

    def get_messages_for_gateway(
        self, conversation_id: str, system_prompt: str | None = None
    ) -> list[dict] | None:
        """Build the full message list in OpenAI format for the gateway."""
        conv = self._conversations.get(conversation_id)
        if conv is None:
            return None

        messages: list[dict] = []
        if system_prompt:
            messages.append({"role": "system", "content": system_prompt})

        for msg in conv.messages:
            messages.append({"role": msg.role, "content": msg.content})

        return messages


# Singleton store
store = ConversationStore()

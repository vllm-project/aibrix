"""Shared SSE parsing utilities for OpenAI-compatible streams."""

from __future__ import annotations

import json
import logging
from collections.abc import AsyncIterator

logger = logging.getLogger(__name__)


async def parse_openai_sse(lines: AsyncIterator[str]) -> AsyncIterator[str]:
    """Parse an OpenAI-compatible SSE stream into normalised event JSON strings.

    Yields JSON strings with:
    - ``{"event": "text_delta", "delta": "..."}`` for content chunks
    - ``{"event": "done", "finish_reason": "...", "usage": {...}}`` on completion
    """
    async for line in lines:
        if not line.startswith("data: "):
            continue
        data = line[6:]
        if data.strip() == "[DONE]":
            yield json.dumps({"event": "done"})
            return
        try:
            chunk = json.loads(data)
            choices = chunk.get("choices", [])
            if not choices:
                continue
            choice = choices[0]
            delta = choice.get("delta", {})
            content = delta.get("content")
            if content:
                yield json.dumps({"event": "text_delta", "delta": content})
            finish = choice.get("finish_reason")
            if finish:
                yield json.dumps({
                    "event": "done",
                    "finish_reason": finish,
                    "usage": chunk.get("usage"),
                })
        except json.JSONDecodeError:
            continue

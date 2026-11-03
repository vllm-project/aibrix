# Copyright 2026 The Aibrix Team.
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

"""Extension fields shared by vLLM and SGLang.

Fields both engines use are declared once here, grouped by the models they
apply to; fields only one engine knows live in ``vllm.py`` / ``sglang.py``.
The inheritance hierarchy follows applicability so a field is declared
exactly once.

Requests::

    CommonEngineExtensionFields (chat + completion + embedding)
      -> GenerationEngineExtensionFields (+ chat & completion)
        -> SharedChatCompletionFields / SharedCompletionFields
      -> SharedEmbeddingFields

Responses: ``SharedCompletionChoiceFields`` and ``SharedModelFields``.

References: vLLM tag ``v0.33.0rc1``, SGLang tag ``v0.5.22``.
"""

from __future__ import annotations

from typing import Any, List, Optional

from pydantic import BaseModel, conint


class CommonEngineExtensionFields(BaseModel):
    """Shared fields present on every request type."""

    priority: Optional[Any] = None


class GenerationEngineExtensionFields(CommonEngineExtensionFields):
    """Shared fields on chat and completion requests."""

    cache_salt: Optional[Any] = None
    ignore_eos: Optional[bool] = False
    min_p: Optional[Any] = None
    min_tokens: Optional[conint(ge=0)] = None  # type: ignore[valid-type]
    repetition_penalty: Optional[Any] = None
    return_token_ids: Optional[Any] = None
    session_id: Optional[Any] = None
    skip_special_tokens: Optional[Any] = None
    stop_token_ids: Optional[Any] = None
    top_k: Optional[Any] = None


class SharedChatCompletionFields(GenerationEngineExtensionFields):
    """Shared chat-only fields."""

    chat_template_kwargs: Optional[Any] = None
    continue_final_message: Optional[Any] = None


class SharedCompletionFields(GenerationEngineExtensionFields):
    """Shared completion-only fields."""

    response_format: Optional[Any] = None


class SharedEmbeddingFields(CommonEngineExtensionFields):
    """Shared embedding-only fields."""

    pass


# ---------------------------------------------------------------------------
# Responses
# ---------------------------------------------------------------------------


class SharedCompletionChoiceFields(BaseModel):
    """Shared fields on a /v1/completions choice (and stream choice)."""

    prompt_token_ids: Optional[List[int]] = None
    token_ids: Optional[List[int]] = None


class SharedModelFields(BaseModel):
    """Shared fields on a ``/v1/models`` entry."""

    root: Optional[str] = None
    parent: Optional[str] = None
    max_model_len: Optional[int] = None

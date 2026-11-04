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

from types import UnionType
from typing import Any, Final, List, Optional, get_args, get_origin

from pydantic import BaseModel, ConfigDict, Field, TypeAdapter, conint


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


# Schema annotation keys shared by result declarations and extraction.
OUTPUT_RESULT: Final = "output_result"
RETAIN_RESULT: Final = "retain_result"


class NativeRequestOptions(BaseModel):
    """Common forwarding controls; validate without rewriting the native body."""

    stream: bool | None = False


# Minimal native result contracts shared by the engine-specific bindings.
class NativeResponse(BaseModel):
    model_config = ConfigDict(extra="allow")


class ChoicesResponse(NativeResponse):
    choices: list[Any] = Field(json_schema_extra={OUTPUT_RESULT: True})


class DataResponse(NativeResponse):
    # Pooling plugins explicitly define a generic data payload, not just arrays.
    data: Any = Field(json_schema_extra={OUTPUT_RESULT: True})


class MessagesResponse(NativeResponse):
    content: list[dict[str, Any]] = Field(json_schema_extra={OUTPUT_RESULT: True})


class CountTokensResponse(NativeResponse):
    input_tokens: int = Field(json_schema_extra={OUTPUT_RESULT: True})


class SystemOneResponse(NativeResponse):
    answers: dict[str, Any] = Field(json_schema_extra={OUTPUT_RESULT: True})


def split_native_result(envelope: Any, schema: Any) -> tuple[Any, Any]:
    """Validate and split original JSON without normalizing vendor metadata.

    Only schema-marked result leaves are stored and cleared in the HTTP copy.
    Preserve the native object/list structure, absent fields and explicit nulls.
    Empty arrays and zero-valued results are valid, not missing payloads.
    """
    TypeAdapter(schema).validate_python(envelope, strict=True)

    def split(value: Any, contract: Any) -> tuple[Any, Any, bool]:
        if get_origin(contract) is UnionType:
            contract = next(
                member
                for member in get_args(contract)
                if (get_origin(member) is list) == isinstance(value, list)
                and member is not type(None)
            )
        if get_origin(contract) is list:
            parts = [split(item, get_args(contract)[0]) for item in value]
            if any(not part[2] for part in parts):
                raise ValueError("A native result item has no schema-declared result")
            return [p[0] for p in parts], [p[1] for p in parts], True
        if not isinstance(contract, type) or not issubclass(contract, BaseModel):
            return {}, value, False
        payload: dict[str, Any] = {}
        remaining = dict(value)
        found_result = False
        for name, field in contract.model_fields.items():
            key = field.alias or name
            if key not in value:
                continue
            metadata = field.json_schema_extra
            if isinstance(metadata, dict) and OUTPUT_RESULT in metadata:
                payload[key] = value[key]
                found_result |= value[key] is not None
                if metadata.get(RETAIN_RESULT):
                    continue
                # Keep nulls and choose the empty shape from the actual wire value.
                if isinstance(value[key], list):
                    remaining[key] = []
                elif isinstance(value[key], dict):
                    remaining[key] = {}
                elif isinstance(value[key], str):
                    remaining[key] = ""
                else:
                    remaining[key] = None
            elif value[key] is not None:
                nested, cleared, found = split(value[key], field.annotation)
                if found:
                    payload[key], remaining[key] = nested, cleared
                    found_result = True
        return payload, remaining, found_result

    payload, remaining, found = split(envelope, schema)
    if not found:
        raise ValueError("Engine response has no schema-declared native result")
    return payload, remaining

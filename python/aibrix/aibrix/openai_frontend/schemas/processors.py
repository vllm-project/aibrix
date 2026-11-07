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

"""Request extension declaring pre/post processors.

``processors`` sits beside ``output`` as a proxy control-plane field: it is
stripped before forwarding, so engines never see it. This module owns the
wire schema only; execution lives in ``proxy.processors``.
"""

from __future__ import annotations

from typing import Any, Dict, Final, List, Literal, Mapping, Optional, TypeAlias

from pydantic import BaseModel, ConfigDict, Field, field_validator, model_validator

# A JSON Schema document uses heterogeneous and nested keyword values. Mapping
# describes its Python representation, not an unconstrained argument contract.
JSONSchema: TypeAlias = Mapping[str, Any]
ProcessorStage: TypeAlias = Literal["pre", "post"]
ProcessorScope: TypeAlias = Literal["input_media", "output_media", "output_payload"]

PROCESSOR_STAGE_PRE: Final = "pre"
PROCESSOR_STAGE_POST: Final = "post"
PROCESSOR_SCOPE_INPUT_MEDIA: Final = "input_media"
PROCESSOR_SCOPE_OUTPUT_MEDIA: Final = "output_media"
PROCESSOR_SCOPE_OUTPUT_PAYLOAD: Final = "output_payload"
PROCESSOR_POST_SCOPES: Final = (
    PROCESSOR_SCOPE_OUTPUT_MEDIA,
    PROCESSOR_SCOPE_OUTPUT_PAYLOAD,
)

# Both arrays are length-capped so one request cannot queue unbounded work.
MAX_PROCESSORS_PER_STAGE: Final = 8
DEFAULT_PROCESSOR_TIMEOUT_SECONDS: Final = 300.0
# Names are registry keys; keep them stable, lower-case and tool-like.
PROCESSOR_NAME_PATTERN: Final = r"^[a-z][a-z0-9_.-]*$"
PROCESSOR_MEDIA_TYPES: Final = ("image", "audio", "video")
PROCESSOR_ARCHIVE_FORMATS: Final = ("zip", "tar", "tar.gz")


class ProcessorSpec(BaseModel):
    """One processor invocation: a registry name plus typed arguments."""

    model_config = ConfigDict(extra="forbid")

    name: str = Field(min_length=1, max_length=64, pattern=PROCESSOR_NAME_PATTERN)
    arguments: Dict[str, Any] = Field(default_factory=dict)
    timeout_seconds: Optional[float] = Field(
        default=None,
        gt=0,
        allow_inf_nan=False,
        strict=True,
        description="Timeout for this invocation in seconds; omitted or null uses "
        "the executor's default timeout.",
    )
    media_types: Optional[List[Literal["image", "audio", "video"]]] = None

    @field_validator("media_types")
    @classmethod
    def _unique_media_types(cls, value: Optional[List[str]]) -> Optional[List[str]]:
        if value is not None and len(set(value)) != len(value):
            raise ValueError("duplicate media type")
        return value


class ProcessorCall(BaseModel):
    """Tools-shaped envelope; ``type`` is reserved for future kinds."""

    model_config = ConfigDict(extra="forbid")

    type: Literal["processor"]
    processor: ProcessorSpec


class ProcessorPipeline(BaseModel):
    """Two ordered arrays; the engine call between them keeps stages apart."""

    model_config = ConfigDict(extra="forbid")

    pre: List[ProcessorCall] = Field(
        default_factory=list, max_length=MAX_PROCESSORS_PER_STAGE
    )
    post: List[ProcessorCall] = Field(
        default_factory=list, max_length=MAX_PROCESSORS_PER_STAGE
    )

    @model_validator(mode="after")
    def _require_declared_work(self) -> "ProcessorPipeline":
        if not self.pre and not self.post:
            raise ValueError("'processors' must declare at least one processor")
        return self

    def calls(self, stage: ProcessorStage) -> List[ProcessorCall]:
        return self.pre if stage == PROCESSOR_STAGE_PRE else self.post

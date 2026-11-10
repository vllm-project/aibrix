# Copyright 2026 The Aibrix Team.
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy at http://www.apache.org/licenses/LICENSE-2.0

"""Minimal image generation and async video job wire models.

Unknown engine fields pass through. Local output paths are declared by SGLang's
multimodal_gen response schema, not inferred from input fields or filenames.
"""

from enum import Enum
from typing import Any, Dict, List, Optional, get_args
from urllib.parse import urlparse

from pydantic import BaseModel, ConfigDict, model_validator

from aibrix.openai_frontend.schemas.media import OUTPUT_MEDIA
from aibrix.openai_frontend.schemas.output import (
    HTTP_SCHEMES,
    OutputRequest,
    OutputResponse,
)
from aibrix.openai_frontend.schemas.processors import ProcessorPipeline
from aibrix.openai_frontend.schemas.sglang import (
    SglangImageResponseDataFields,
    SglangVideoResponseFields,
)

VIDEO_FILE_PATH_FIELD = "file_path"
VIDEO_FILE_PATHS_FIELD = "file_paths"
VIDEO_URL_FIELD = "url"
VIDEO_URLS_FIELD = "urls"
VIDEO_JOB_UNAVAILABLE_STATUS_CODES = frozenset({404, 410})


class VideoStatus(str, Enum):
    """Known video job states; the wire model still accepts unknown states."""

    IN_PROGRESS = "in_progress"
    COMPLETED = "completed"
    FAILED = "failed"
    CANCELLED = "cancelled"
    CANCELED = "canceled"


VIDEO_TERMINAL_STATUSES = frozenset(
    {
        VideoStatus.COMPLETED.value,
        VideoStatus.FAILED.value,
        VideoStatus.CANCELLED.value,
        VideoStatus.CANCELED.value,
    }
)


class ImageGenerationRequest(BaseModel):
    model_config = ConfigDict(extra="allow")

    prompt: str
    model: Optional[str] = None
    output: Optional[OutputRequest] = None
    processors: Optional[ProcessorPipeline] = None

    @model_validator(mode="after")
    def _validate_output(self) -> "ImageGenerationRequest":
        if self.output is not None and (
            getattr(self, "stream", False)
            or getattr(self, "response_format", None) == "file"
        ):
            raise ValueError("output requires a non-streaming JSON image response")
        return self


class ImageResponseData(SglangImageResponseDataFields):
    model_config = ConfigDict(extra="allow")

    b64_json: Optional[str] = None
    url: Optional[str] = None


class ImageGenerationResponse(BaseModel):
    model_config = ConfigDict(extra="allow")

    created: int
    data: List[ImageResponseData]


class VideoGenerationRequest(BaseModel):
    model_config = ConfigDict(extra="allow")

    prompt: str
    model: Optional[str] = None
    output: Optional[OutputRequest] = None
    processors: Optional[ProcessorPipeline] = None

    @model_validator(mode="after")
    def _validate_output(self) -> "VideoGenerationRequest":
        if self.output is not None and getattr(self, "stream", False):
            raise ValueError("output requires a non-streaming JSON video response")
        return self


class VideoResponse(SglangVideoResponseFields):
    model_config = ConfigDict(extra="allow")

    id: str
    # Only job identity/status and proxy output are protocol control fields.
    # Engine metadata is preserved without coercing or rejecting vendor shapes.
    object: Any = "video"
    status: str
    model: Any = None
    created_at: Any = None
    completed_at: Any = None
    expires_at: Any = None
    progress: Any = None
    size: Any = None
    seconds: Any = None
    prompt: Any = None
    remixed_from_video_id: Any = None
    url: Any = None
    urls: Any = None
    output: Optional[OutputResponse] = None
    error: Optional[Any] = None

    @classmethod
    def status_only(
        cls, body: Any, video_id: Optional[str] = None
    ) -> Optional["VideoResponse"]:
        """Read identity/status without trusting vendor metadata or claiming paths."""
        if not isinstance(body, dict):
            return None
        # Only a response-provided identity can establish trusted termination.
        rid = body.get("id")
        if (
            not isinstance(rid, str)
            or not rid
            or (video_id is not None and rid != video_id)
        ):
            return None
        status = body.get("status")
        return cls.model_validate(
            {
                "id": rid,
                "status": status
                if isinstance(status, str)
                else VideoStatus.IN_PROGRESS.value,
            }
        )

    @classmethod
    def pending(cls, video_id: str) -> "VideoResponse":
        return cls(
            id=video_id,
            status=VideoStatus.IN_PROGRESS.value,
            file_path=None,
            file_paths=None,
            url=None,
            urls=None,
        )

    def cache_view(self, status: str, error: Any = None) -> "VideoResponse":
        """Pending/failed views never expose engine-owned paths or stale output."""
        body = self.model_dump(mode="json", exclude_unset=True)
        for key in ("choices", "data"):
            if key in body:
                body[key] = []
        body.update(status=status)
        body.pop("output", None)
        for key in (
            VIDEO_FILE_PATH_FIELD,
            VIDEO_FILE_PATHS_FIELD,
            VIDEO_URL_FIELD,
            VIDEO_URLS_FIELD,
        ):
            if key in body:
                body[key] = None
        if error is not None:
            body["error"] = error
        return type(self).model_validate(body)

    @classmethod
    def output_receipt(
        cls,
        envelope: Dict[str, Any],
        spec: OutputRequest,
        result: OutputResponse,
        *,
        has_payload: bool,
        has_media: bool,
    ) -> "VideoResponse":
        """Map only persisted media to output locations; retain remote-only metadata."""
        body = {**envelope, "output": result.model_dump()}
        if has_payload or has_media:
            is_http = urlparse(spec.url).scheme.lower() in HTTP_SCHEMES
            artifacts = (
                (
                    result.artifacts[1:]
                    if has_payload and not is_http
                    else result.artifacts
                )
                if has_media
                else []
            )
            locations = [
                artifact if is_http else f"{spec.url.rstrip('/')}/{artifact}"
                for artifact in artifacts
            ]
            location = locations[0] if locations else None
            for key, value in (
                (VIDEO_FILE_PATH_FIELD, location),
                (VIDEO_FILE_PATHS_FIELD, locations),
            ):
                if key in body:
                    body[key] = value
            for key, value in (
                (VIDEO_URL_FIELD, location),
                (VIDEO_URLS_FIELD, locations),
            ):
                if locations or key in body:
                    body[key] = value
        return cls.model_validate(body)


def output_media_paths(
    response: Any, model: Optional[type[BaseModel]] = None
) -> List[str]:
    """Read declared output fields, even when unrelated metadata is invalid."""
    paths: List[str] = []

    def walk(node: Any, schema: Optional[type[BaseModel]] = None) -> None:
        if isinstance(node, BaseModel):
            schema = type(node)
        if schema is not None and isinstance(node, (BaseModel, dict)):
            for name, field in schema.model_fields.items():
                value = (
                    getattr(node, name)
                    if isinstance(node, BaseModel)
                    else node.get(name)
                )
                metadata = field.json_schema_extra
                if isinstance(metadata, dict) and metadata.get(OUTPUT_MEDIA):
                    values = value if isinstance(value, list) else [value]
                    paths.extend(
                        path for path in values if isinstance(path, str) and path
                    )
                else:
                    annotation = field.annotation
                    args = get_args(annotation)
                    child = args[0] if args else annotation
                    if isinstance(child, type) and issubclass(child, BaseModel):
                        for item in value if isinstance(value, list) else [value]:
                            walk(item, child)
        elif isinstance(node, list):
            for item in node:
                walk(item)

    walk(response, model)
    return list(dict.fromkeys(paths))

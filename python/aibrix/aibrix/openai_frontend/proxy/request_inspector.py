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

"""Locate remote media URLs in a request body so they can be fetched locally.

Recognised shapes (vLLM ``v0.33.0rc1``, SGLang ``v0.5.22``):

* Chat content parts (OpenAI, vLLM, SGLang, TRT-LLM)::

      {"type": "image_url", "image_url": {"url": ...}}
      {"type": "video_url", "video_url": {"url": ...}}
      {"type": "audio_url", "audio_url": {"url": ...}}

  also the bare-string form ``{"type": "image_url", "image_url": "..."}``.
* Responses API input items::

      {"type": "input_image", "image_url": "..."}
      {"type": "input_file", "file_url": "..."}

* SGLang native ``/generate``: ``image_data`` / ``video_data`` /
  ``audio_data`` as a string, list of strings, or nested list.

Only remote URLs a scheme ``aibrix.runtime.downloaders`` can fetch are
reported; ``file://`` URLs are left untouched. Inline ``data:`` URLs are
reported only when ``materialize_inline`` is enabled, so the proxy can decode
their base64 payload to a local file for engines that only read ``file://``.
"""

from __future__ import annotations

from abc import ABC, abstractmethod
from dataclasses import dataclass
from typing import Any, Dict, Iterable, List, Optional, Union

from aibrix.openai_frontend.common.data_url import is_data_url
from aibrix.openai_frontend.schemas.media import media_url_fields
from aibrix.openai_frontend.schemas.openai import (
    ChatCompletionRequestMessageContentPart,
)
from aibrix.openai_frontend.schemas.responses import CreateResponseRequest
from aibrix.openai_frontend.schemas.sglang import SglangGenerateMediaFields

_MEDIA_URL_FIELDS = {
    **media_url_fields(ChatCompletionRequestMessageContentPart),
    **media_url_fields(CreateResponseRequest),
}


@dataclass(frozen=True)
class URLLocation:
    url: str
    path: List[Union[str, int]]  # JSON-pointer-like path into the request dict
    media_type: str = "image_url"


class RequestInspector(ABC):
    @abstractmethod
    def extract_urls(self, request_dict: Dict[str, Any]) -> List[URLLocation]: ...


class MediaURLInspector(RequestInspector):
    """Walks a request body and reports every remote media URL.

    ``media_types`` optionally narrows the recognized request shapes for a
    custom inspector.
    """

    def __init__(
        self,
        media_types: Optional[Iterable[str]] = None,
        materialize_inline: bool = False,
    ):
        self._media_types = (
            frozenset(media_types)
            if media_types is not None
            else frozenset(field for field, _ in _MEDIA_URL_FIELDS.values())
        )
        # When set, inline data:;base64 media is also reported so the rewriter
        # can decode it to a local file; otherwise it passes through untouched.
        self._materialize_inline = materialize_inline

    def _is_handled_url(self, url: str) -> bool:
        return (self._materialize_inline and is_data_url(url)) or _is_remote_url(url)

    def extract_urls(self, request_dict: Dict[str, Any]) -> List[URLLocation]:
        locations: List[URLLocation] = []
        self._walk(request_dict, [], locations)
        return locations

    def _walk(
        self,
        node: Any,
        path: List[Union[str, int]],
        out: List[URLLocation],
    ) -> None:
        if isinstance(node, list):
            for i, item in enumerate(node):
                self._walk(item, path + [i], out)
            return
        if not isinstance(node, dict):
            return

        part_type = node.get("type")
        if isinstance(part_type, str) and part_type in _MEDIA_URL_FIELDS:
            url_field, nested_url = _MEDIA_URL_FIELDS[part_type]
            if url_field in self._media_types:
                self._add_part_url(node, url_field, nested_url, path, out)
            return

        for key, value in node.items():
            if isinstance(value, (dict, list)):
                self._walk(value, path + [key], out)

    def _add_part_url(
        self,
        node: Dict[str, Any],
        url_field: str,
        nested_url: str,
        path: List[Union[str, int]],
        out: List[URLLocation],
    ) -> None:
        payload = node.get(url_field)
        url_path = path + [url_field]
        if isinstance(payload, dict) and nested_url:
            url = payload.get(nested_url)
            url_path += [nested_url]
        else:
            url = payload
        if isinstance(url, str) and self._is_handled_url(url):
            out.append(URLLocation(url, url_path, url_field))


class SglangGenerateInspector(MediaURLInspector):
    """Inspect only schema-declared fields of the native /generate request."""

    def extract_urls(self, request_dict: Dict[str, Any]) -> List[URLLocation]:
        locations: List[URLLocation] = []
        for field_name, field in SglangGenerateMediaFields.model_fields.items():
            metadata = field.json_schema_extra
            if not isinstance(metadata, dict):
                continue
            media_type = metadata.get("media_type")
            if (
                isinstance(media_type, str)
                and media_type in self._media_types
                and field_name in request_dict
            ):
                self._walk_media_list(
                    request_dict[field_name], [field_name], media_type, locations
                )
        return locations

    def _walk_media_list(
        self,
        node: Any,
        path: List[Union[str, int]],
        media_type: str,
        out: List[URLLocation],
    ) -> None:
        if isinstance(node, str):
            if self._is_handled_url(node):
                out.append(URLLocation(node, path, media_type))
        elif isinstance(node, list):
            for i, item in enumerate(node):
                self._walk_media_list(item, path + [i], media_type, out)
        elif isinstance(node, dict):
            # SGLang ImageData-style {"url": ..., ...}
            url = node.get("url")
            if isinstance(url, str) and self._is_handled_url(url):
                out.append(URLLocation(url, path + ["url"], media_type))


def _is_remote_url(url: str) -> bool:
    from aibrix.runtime.downloaders import get_downloader

    if not url or url.startswith(("data:", "file:")):
        return False
    try:
        get_downloader(url)
        return True
    except ValueError:
        return False

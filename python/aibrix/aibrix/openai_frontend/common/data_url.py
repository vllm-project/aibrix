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

"""Decode inline ``data:`` URLs (RFC 2397) carrying base64-encoded media.

OpenAI-style chat parts may embed media directly instead of referencing a
remote object::

    {"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBOR..."}}

When the proxy materialises media into local files for an engine that reads
``file://``, such an inline payload has to be decoded to bytes. Decoding uses
``fastpybase64`` (SIMD-accelerated libbase64), which is a required
dependency of the frontend.
"""

from __future__ import annotations

import binascii
import mimetypes
from dataclasses import dataclass

from fastpybase64 import b64decode


class DataURLError(ValueError):
    """A ``data:`` URL is malformed or not a supported base64 payload."""


@dataclass(frozen=True)
class DecodedDataURL:
    media_type: str
    extension: str
    data: bytes


def is_data_url(url: str) -> bool:
    return isinstance(url, str) and url.startswith("data:")


def extension_for_media_type(media_type: str) -> str:
    """File extension for a (possibly parameterised) MIME type.

    Uses the stdlib ``mimetypes`` registry (which also incorporates the
    platform's MIME database), the same lookup vLLM's multimodal utilities
    rely on. When the registry has no entry (e.g. ``audio/wav``,
    ``audio/flac``), fall back to the MIME subtype, so no per-type table has
    to be maintained here.
    """
    base = media_type.split(";", 1)[0].strip().lower()
    extension = mimetypes.guess_extension(base, strict=False)
    if extension:
        return extension
    subtype = base.rsplit("/", 1)[-1]
    return f".{subtype}" if subtype.replace("x-", "").isalnum() else ".bin"


def decode_data_url(url: str) -> DecodedDataURL:
    """Parse and base64-decode a ``data:[<mediatype>][;base64],<data>`` URL.

    Only base64 payloads are supported; URL-encoded payloads are rejected
    because inline media in model requests is always base64 in practice.
    """
    if not is_data_url(url):
        raise DataURLError("not a data: URL")
    try:
        header, encoded = url[len("data:") :].split(",", 1)
    except ValueError as exc:
        raise DataURLError("missing comma separator") from exc

    media_type = "text/plain"
    is_base64 = False
    for segment in header.split(";") if header else []:
        segment = segment.strip()
        if segment == "base64":
            is_base64 = True
        elif segment and "/" in segment:
            media_type = segment
    if not is_base64:
        raise DataURLError("only base64-encoded data URLs are supported")

    try:
        # validate=True rejects stray non-base64 characters instead of
        # silently dropping them.
        data = b64decode(encoded, validate=True)
    except (binascii.Error, ValueError) as exc:
        raise DataURLError(f"invalid base64 payload: {exc}") from exc

    return DecodedDataURL(
        media_type=media_type,
        extension=extension_for_media_type(media_type),
        data=data,
    )

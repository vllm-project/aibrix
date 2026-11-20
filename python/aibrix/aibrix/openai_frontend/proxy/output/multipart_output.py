# Copyright 2026 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#       http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Read frontend output metadata while the original upload streams to the engine."""

from __future__ import annotations

import json
from typing import Optional

from python_multipart import MultipartParser
from python_multipart.exceptions import MultipartParseError
from python_multipart.multipart import parse_options_header

from aibrix.openai_frontend.schemas.output import OutputRequest
from aibrix.openai_frontend.utils.utils import ClientError

MAX_METADATA_BYTES = 64 * 1024


class MultipartOutputExtractor:
    """Keep only output/model text; never spool or rewrite the request body."""

    def __init__(self, content_type: str) -> None:
        self._fields: dict[bytes, Optional[bytearray]] = {}
        self._data: Optional[bytearray] = None
        self._name = b""
        self._header_name = bytearray()
        self._header_value = bytearray()
        self._disposition = b""
        self._error: Optional[str] = None
        self._output_error: Optional[str] = None
        _, options = parse_options_header(content_type)
        boundary = options.get(b"boundary")
        self._parser = (
            MultipartParser(
                boundary,
                {
                    "on_part_begin": self._part_begin,
                    "on_header_field": self._header_field,
                    "on_header_value": self._header_value_chunk,
                    "on_header_end": self._header_end,
                    "on_headers_finished": self._headers_finished,
                    "on_part_data": self._part_data,
                },
            )
            if boundary
            else None
        )
        if self._parser is None:
            self._error = "Missing multipart boundary"

    def feed(self, chunk: bytes) -> None:
        if self._parser is None:
            return
        try:
            self._parser.write(chunk)
        except (MultipartParseError, ValueError) as exc:
            # Metadata extraction must not interrupt upload or mask native errors.
            self._error = str(exc)
            self._parser = None

    def output_request(self) -> Optional[OutputRequest]:
        """Resolve the frontend extension only after the engine has succeeded."""
        error = self._error or self._output_error
        if error is not None:
            raise ClientError(f"Cannot read video output metadata: {error}")
        value = self._fields.get(b"output")
        if value is None:
            return None
        try:
            output = json.loads(value)
            return OutputRequest.model_validate(output) if output is not None else None
        except (ValueError, UnicodeError) as exc:
            raise ClientError(f"Invalid 'output': {exc}") from exc

    @property
    def model(self) -> Optional[str]:
        value = self._fields.get(b"model")
        try:
            return value.decode("utf-8") if value is not None else None
        except UnicodeError:
            return None

    def _part_begin(self) -> None:
        self._data = None
        self._name = b""
        self._disposition = b""
        self._header_name.clear()
        self._header_value.clear()

    def _header_field(self, data: bytes, start: int, end: int) -> None:
        self._append_header(self._header_name, data, start, end)

    def _header_value_chunk(self, data: bytes, start: int, end: int) -> None:
        if self._header_name.lower() == b"content-disposition":
            self._append_header(self._header_value, data, start, end)

    @staticmethod
    def _append_header(target: bytearray, data: bytes, start: int, end: int) -> None:
        if len(target) + end - start > MAX_METADATA_BYTES:
            raise ValueError("Multipart metadata header exceeds 64 KiB")
        target.extend(data[start:end])

    def _header_end(self) -> None:
        if self._header_name.lower() == b"content-disposition":
            self._disposition = bytes(self._header_value)
        self._header_name.clear()
        self._header_value.clear()

    def _headers_finished(self) -> None:
        _, options = parse_options_header(self._disposition)
        self._name = options.get(b"name", b"")
        if self._name not in (b"output", b"model"):
            return
        # Match native form lookup: the last field wins; files are not form text.
        self._fields[self._name] = None
        if self._name == b"output":
            self._output_error = None
        if b"filename" in options:
            if self._name == b"output":
                self._output_error = "'output' must be a JSON form field, not a file"
            return
        self._data = self._fields[self._name] = bytearray()

    def _part_data(self, data: bytes, start: int, end: int) -> None:
        if self._data is None:
            return
        if len(self._data) + end - start > MAX_METADATA_BYTES:
            if self._name == b"output":
                self._output_error = "'output' form field exceeds 64 KiB"
            self._fields[self._name] = self._data = None
            return
        self._data.extend(data[start:end])

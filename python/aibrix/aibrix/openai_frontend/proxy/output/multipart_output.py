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

"""Remove frontend output metadata while the upload streams to the engine."""

from __future__ import annotations

import json
from typing import AsyncIterator, Optional

from python_multipart import MultipartParser
from python_multipart.exceptions import MultipartParseError
from python_multipart.multipart import parse_options_header

from aibrix.openai_frontend.schemas.output import OutputRequest
from aibrix.openai_frontend.utils.utils import ClientError

MAX_METADATA_BYTES = 64 * 1024


class MultipartOutputFilter:
    """Rebuild multipart framing without output parts; never spool file data."""

    def __init__(self, content_type: str) -> None:
        self._fields: dict[bytes, Optional[bytearray]] = {}
        self._data: Optional[bytearray] = None
        self._name = b""
        self._header_name = bytearray()
        self._header_value = bytearray()
        self._headers = bytearray()
        self._forwarded = bytearray()
        self._disposition = b""
        self._drop_part = False
        self._has_part = False
        self._complete = False
        self._error: Optional[Exception] = None
        self._output_error: Optional[Exception] = None
        _, options = parse_options_header(content_type)
        boundary = options.get(b"boundary")
        self._boundary = b"--" + boundary if boundary else b""
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
                    "on_end": self._end,
                },
            )
            if boundary
            else None
        )
        if self._parser is None:
            self._error = ClientError(
                "Cannot filter video multipart upload: Missing multipart boundary"
            )

    async def stream(self, body: AsyncIterator[bytes]) -> AsyncIterator[bytes]:
        async for chunk in body:
            if chunk:
                forwarded = self.feed(chunk)
                if forwarded:
                    yield forwarded
        self.finish()

    def feed(self, chunk: bytes) -> bytes:
        if self._parser is None:
            if self._error is not None:
                raise self._error
            raise RuntimeError("Multipart parser is unavailable")
        try:
            self._parser.write(chunk)
        except (MultipartParseError, ValueError) as exc:
            # Never fall back to raw forwarding: an unparsed part may hold a
            # pre-signed PUT URL that must not reach the engine.
            raise ClientError(f"Cannot filter video multipart upload: {exc}") from exc
        forwarded = bytes(self._forwarded)
        self._forwarded.clear()
        return forwarded

    def finish(self) -> None:
        # MultipartParser.finalize() does not currently detect truncated bodies.
        if not self._complete:
            raise ClientError("Incomplete video multipart upload")

    def output_request(self) -> Optional[OutputRequest]:
        """Resolve the frontend extension only after the engine has succeeded."""
        error = self._error or self._output_error
        if error is not None:
            raise error
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
        self._headers.clear()
        self._drop_part = False

    def _header_field(self, data: bytes, start: int, end: int) -> None:
        self._append_header(self._header_name, data, start, end)

    def _header_value_chunk(self, data: bytes, start: int, end: int) -> None:
        self._append_header(self._header_value, data, start, end)

    def _append_header(
        self, target: bytearray, data: bytes, start: int, end: int
    ) -> None:
        size = len(self._headers) + len(self._header_name) + len(self._header_value)
        if size + end - start + 4 > MAX_METADATA_BYTES:
            raise ValueError("Multipart metadata header exceeds 64 KiB")
        target.extend(data[start:end])

    def _header_end(self) -> None:
        if self._header_name.lower() == b"content-disposition":
            self._disposition = bytes(self._header_value)
        self._headers.extend(self._header_name)
        self._headers.extend(b": ")
        self._headers.extend(self._header_value)
        self._headers.extend(b"\r\n")
        self._header_name.clear()
        self._header_value.clear()

    def _headers_finished(self) -> None:
        _, options = parse_options_header(self._disposition)
        self._name = options.get(b"name", b"")
        self._drop_part = self._name == b"output"
        if not self._drop_part:
            if self._has_part:
                self._forwarded.extend(b"\r\n")
            self._forwarded.extend(self._boundary + b"\r\n")
            self._forwarded.extend(self._headers)
            self._forwarded.extend(b"\r\n")
            self._has_part = True
        if self._name not in (b"output", b"model"):
            return
        # Match native form lookup: the last field wins; files are not form text.
        self._fields[self._name] = None
        if self._name == b"output":
            self._output_error = None
        if b"filename" in options:
            if self._name == b"output":
                self._output_error = ClientError(
                    "Cannot read video output metadata: "
                    "'output' must be a JSON form field, not a file"
                )
            return
        self._data = self._fields[self._name] = bytearray()

    def _part_data(self, data: bytes, start: int, end: int) -> None:
        if not self._drop_part:
            self._forwarded.extend(data[start:end])
        if self._data is None:
            return
        if len(self._data) + end - start > MAX_METADATA_BYTES:
            if self._name == b"output":
                self._output_error = ClientError(
                    "Cannot read video output metadata: "
                    "'output' form field exceeds 64 KiB"
                )
            self._fields[self._name] = self._data = None
            return
        self._data.extend(data[start:end])

    def _end(self) -> None:
        if self._has_part:
            self._forwarded.extend(b"\r\n")
        self._forwarded.extend(self._boundary + b"--\r\n")
        self._complete = True

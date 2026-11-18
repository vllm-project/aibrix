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

"""Extract a video's output extension before submitting its multipart upload.

Both the original and output-free form are spooled to caller-owned files.
Only small headers and control fields are retained in memory. The original
body is used verbatim when there is no extension; otherwise part bodies and
order are preserved while framing is rebuilt without the output part.
"""

from __future__ import annotations

import asyncio
import json
from collections.abc import AsyncIterable, AsyncIterator
from typing import BinaryIO, Optional

from python_multipart import MultipartParser
from python_multipart.exceptions import MultipartParseError
from python_multipart.multipart import parse_options_header

from aibrix.openai_frontend.proxy.output.output_writer import OUTPUT_CHUNK_SIZE_BYTES
from aibrix.openai_frontend.schemas.engine_common import NativeRequestOptions
from aibrix.openai_frontend.schemas.output import OutputRequest
from aibrix.openai_frontend.utils.utils import ClientError, ServerError
from aibrix.runtime.downloaders import await_thread_completion

MAX_CONTROL_BYTES = 64 * 1024
MAX_PART_HEADER_BYTES = 64 * 1024


class MultipartOutputBody:
    """Disk-backed, output-free request body; caller owns both file lifetimes."""

    def __init__(self, content_type: str, original: BinaryIO, filtered: BinaryIO):
        media_type, options = parse_options_header(content_type)
        boundary = options.get(b"boundary")
        if media_type != b"multipart/form-data" or not boundary:
            raise ClientError(
                "Video requests require multipart/form-data with a boundary"
            )
        self._boundary = boundary
        self._original = original
        self._filtered = filtered
        self._source = original
        self._output_seen = False
        self._output_data = bytearray()
        self._stream_data: Optional[bytearray] = None
        self._model_data: Optional[bytearray] = None
        self._ended = False
        self._control = b""
        self._headers: list[tuple[bytes, bytes]] = []
        self._header_name = bytearray()
        self._header_value = bytearray()
        self._header_bytes = 0
        self.output: Optional[OutputRequest] = None
        self.model: Optional[str] = None
        self.length = 0
        self._parser = MultipartParser(
            boundary,
            {
                "on_part_begin": self._part_begin,
                "on_header_field": self._header_field,
                "on_header_value": self._header_value_chunk,
                "on_header_end": self._header_end,
                "on_headers_finished": self._headers_finished,
                "on_part_data": self._part_data,
                "on_part_end": self._part_end,
                "on_end": self._end,
            },
        )

    async def prepare(self, body: AsyncIterable[bytes]) -> None:
        try:
            async for chunk in body:
                if chunk:
                    await await_thread_completion(asyncio.to_thread(self._feed, chunk))
            await await_thread_completion(asyncio.to_thread(self._finish))
        except MultipartParseError as exc:
            raise ClientError(f"Invalid multipart video request: {exc}") from exc
        except OSError as exc:
            raise ServerError(f"Failed to spool video upload: {exc}") from exc

    def _feed(self, chunk: bytes) -> None:
        self._original.write(chunk)
        self._parser.write(chunk)

    def _finish(self) -> None:
        # The library's finalize() does not currently check for a closing boundary.
        self._parser.finalize()
        if not self._ended:
            raise ClientError("Incomplete multipart video request")
        if self._output_seen:
            try:
                value = json.loads(self._output_data)
                # Match JSON requests: output:null removes the extension without saving.
                if value is not None:
                    self.output = OutputRequest.model_validate(value)
            except (ValueError, UnicodeError) as exc:
                raise ClientError(f"Invalid 'output': {exc}") from exc
        if self.output is not None and self._stream_data is not None:
            try:
                stream = NativeRequestOptions.model_validate(
                    {"stream": self._stream_data.decode("utf-8")}
                ).stream
            except ValueError as exc:
                raise ClientError(f"Invalid 'stream': {exc}") from exc
            if stream:
                raise ClientError("'output' cannot be combined with 'stream': true")
        if self._model_data is not None:
            try:
                self.model = self._model_data.decode("utf-8")
            except UnicodeError:
                # Metadata extraction must not add native model validation.
                pass
        self._source = self._filtered if self._output_seen else self._original
        self.length = self._source.tell()
        self._source.seek(0)

    def _part_begin(self) -> None:
        self._headers = []
        self._header_name.clear()
        self._header_value.clear()
        self._header_bytes = 0
        self._control = b""

    def _header_field(self, data: bytes, start: int, end: int) -> None:
        self._count_header(end - start)
        self._header_name.extend(data[start:end])

    def _header_value_chunk(self, data: bytes, start: int, end: int) -> None:
        self._count_header(end - start)
        self._header_value.extend(data[start:end])

    def _count_header(self, size: int) -> None:
        self._header_bytes += size
        if self._header_bytes > MAX_PART_HEADER_BYTES:
            raise ClientError("Multipart part headers exceed 64 KiB")

    def _header_end(self) -> None:
        self._headers.append((bytes(self._header_name), bytes(self._header_value)))
        self._header_name.clear()
        self._header_value.clear()

    def _headers_finished(self) -> None:
        disposition = next(
            (
                value
                for name, value in self._headers
                if name.lower() == b"content-disposition"
            ),
            b"",
        )
        _, options = parse_options_header(disposition)
        if b"name" not in options:
            raise ClientError("Multipart parts require Content-Disposition with a name")
        name = options[b"name"]
        if name == b"output":
            if self._output_seen:
                raise ClientError("Specify the 'output' form field only once")
            if b"filename" in options:
                raise ClientError("'output' must be a JSON form field, not a file")
            self._output_seen = True
            self._control = b"output"
            return
        if name == b"stream" and b"filename" not in options:
            self._control = b"stream"
            self._stream_data = bytearray()
        if name == b"model":
            # Like native form lookup, the last part wins. A file is not model text.
            self._model_data = None
            if b"filename" not in options:
                self._control = b"model"
                self._model_data = bytearray()
        self._filtered.write(b"--" + self._boundary + b"\r\n")
        for key, value in self._headers:
            self._filtered.write(key + b": " + value + b"\r\n")
        self._filtered.write(b"\r\n")

    def _part_data(self, data: bytes, start: int, end: int) -> None:
        if self._control == b"output":
            if len(self._output_data) + end - start > MAX_CONTROL_BYTES:
                raise ClientError("'output' form field exceeds 64 KiB")
            self._output_data.extend(data[start:end])
            return
        if self._control == b"stream" and self._stream_data is not None:
            if len(self._stream_data) + end - start > MAX_CONTROL_BYTES:
                raise ClientError("'stream' form field exceeds 64 KiB")
            self._stream_data.extend(data[start:end])
        if self._control == b"model" and self._model_data is not None:
            if len(self._model_data) + end - start > MAX_CONTROL_BYTES:
                # Bound receipt metadata without rejecting or changing the upload.
                self._model_data = None
            else:
                self._model_data.extend(data[start:end])
        self._filtered.write(data[start:end])

    def _part_end(self) -> None:
        if self._control != b"output":
            self._filtered.write(b"\r\n")

    def _end(self) -> None:
        self._filtered.write(b"--" + self._boundary + b"--\r\n")
        self._ended = True

    def __aiter__(self) -> AsyncIterator[bytes]:
        return self._iterate()

    async def _iterate(self) -> AsyncIterator[bytes]:
        while chunk := await await_thread_completion(
            asyncio.to_thread(self._source.read, OUTPUT_CHUNK_SIZE_BYTES)
        ):
            yield chunk

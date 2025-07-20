# Copyright 2024 The Aibrix Team.
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

from abc import ABC, abstractmethod
from dataclasses import dataclass
from io import BytesIO, StringIO
from typing import AsyncIterator, BinaryIO, Optional, TextIO, Union

from .utils import ObjectMetadata, Reader


@dataclass
class StorageConfig:
    """Configuration for storage implementations."""

    # Common configuration
    max_retries: int = 3
    timeout: int = 30

    # Upload configuration
    multipart_threshold: int = 5 * 1024 * 1024  # 5MB
    multipart_chunksize: int = 5 * 1024 * 1024  # 5MB
    max_concurrency: int = 10

    # Read configuration
    range_chunksize: int = 1024 * 1024  # 1MB for range reads
    readline_buffer_size: int = 8192  # 8KB buffer for readline


class BaseStorage(ABC):
    """Base class for all storage implementations.

    Provides S3-like interface with support for:
    - Raw file read/write operations
    - Multipart uploads for large files
    - Range gets for partial file reads
    - Readline functionality backed by range gets
    """

    def __init__(self, config: Optional[StorageConfig] = None):
        self.config = config or StorageConfig()

    @abstractmethod
    async def put_object(
        self,
        key: str,
        data: Union[bytes, str, BinaryIO, TextIO, Reader],
        content_type: Optional[str] = None,
        metadata: Optional[dict[str, str]] = None,
    ) -> None:
        """Put an object to storage.

        Args:
            key: Object key/path
            data: Data to write (bytes, string, or file-like object)
            content_type: MIME type of the content
            metadata: Additional metadata to store with object
        """
        pass

    @abstractmethod
    async def get_object(
        self,
        key: str,
        range_start: Optional[int] = None,
        range_end: Optional[int] = None,
    ) -> bytes:
        """Get an object from storage.

        Args:
            key: Object key/path
            range_start: Start byte position for range get
            range_end: End byte position for range get (inclusive)

        Returns:
            Object data as bytes
        """
        pass

    @abstractmethod
    async def delete_object(self, key: str) -> None:
        """Delete an object from storage.

        Args:
            key: Object key/path
        """
        pass

    @abstractmethod
    async def list_objects(
        self, prefix: str = "", delimiter: Optional[str] = None
    ) -> list[str]:
        """List objects with given prefix.

        Args:
            prefix: Key prefix to filter objects
            delimiter: Delimiter for hierarchical listing

        Returns:
            List of object keys
        """
        pass

    @abstractmethod
    async def object_exists(self, key: str) -> bool:
        """Check if object exists.

        Args:
            key: Object key/path

        Returns:
            True if object exists, False otherwise
        """
        pass

    @abstractmethod
    async def get_object_size(self, key: str) -> int:
        """Get object size in bytes.

        Args:
            key: Object key/path

        Returns:
            Object size in bytes
        """
        pass

    @abstractmethod
    async def head_object(self, key: str) -> ObjectMetadata:
        """Get object metadata without downloading the object content.

        Args:
            key: Object key/path

        Returns:
            Object metadata with normalized fields

        Raises:
            FileNotFoundError: If object does not exist
        """
        pass

    @abstractmethod
    async def create_multipart_upload(
        self,
        key: str,
        content_type: Optional[str] = None,
        metadata: Optional[dict[str, str]] = None,
    ) -> str:
        """Create a multipart upload session.

        Args:
            key: Object key/path
            content_type: MIME type of the content
            metadata: Additional metadata to store with object

        Returns:
            Upload ID for the multipart upload session
        """
        pass

    @abstractmethod
    async def upload_part(
        self,
        key: str,
        upload_id: str,
        part_number: int,
        data: Union[str, bytes, BinaryIO, TextIO, Reader],
    ) -> str:
        """Upload a part in a multipart upload.

        Args:
            key: Object key/path
            upload_id: Upload ID from create_multipart_upload
            part_number: Part number (1-based)
            data: Part data

        Returns:
            ETag for the uploaded part
        """
        pass

    @abstractmethod
    async def complete_multipart_upload(
        self,
        key: str,
        upload_id: str,
        parts: list[dict[str, Union[str, int]]],
    ) -> None:
        """Complete a multipart upload.

        Args:
            key: Object key/path
            upload_id: Upload ID from create_multipart_upload
            parts: List of parts with 'part_number' and 'etag' keys
        """
        pass

    @abstractmethod
    async def abort_multipart_upload(
        self,
        key: str,
        upload_id: str,
    ) -> None:
        """Abort a multipart upload.

        Args:
            key: Object key/path
            upload_id: Upload ID from create_multipart_upload
        """
        pass

    async def multipart_upload(
        self,
        key: str,
        data: Union[str, bytes, BinaryIO, TextIO, Reader],
        content_type: Optional[str] = None,
        metadata: Optional[dict[str, str]] = None,
        byline: int = 0,
        bysize: int = 0,
        parts: int = 1,
    ) -> None:
        """Upload large object using multipart upload.

        This is a default implementation using the S3-like multipart APIs.

        Args:
            key: Object key/path
            data: File-like object to upload
            content_type: MIME type of the content
            metadata: Additional metadata to store with object
            byline: Upload parts by line(s) - number of lines per part (priority 1)
            bysize: Upload parts by chunk size in bytes (priority 2)
            parts: Automatically determine chunk size by number of parts (priority 3)

        Note: Priority order is byline > bysize > parts. Only one strategy is used.
        """
        upload_id = await self.create_multipart_upload(key, content_type, metadata)
        upload_parts: list[dict[str, Union[str, int]]] = []

        content: Optional[Union[BinaryIO, TextIO, Reader]] = None
        if isinstance(data, str):
            content = StringIO(data)
        elif isinstance(data, bytes):
            content = BytesIO(data)
        else:
            content = data

        try:
            # Priority 1: Upload by lines
            if byline > 0:
                await self._upload_by_lines(
                    content, key, upload_id, byline, upload_parts
                )

            # Priority 2: Upload by size
            elif bysize > 0:
                await self._upload_by_size(
                    content, key, upload_id, bysize, upload_parts
                )

            # Priority 3: Upload by parts (auto-determine chunk size)
            else:
                await self._upload_by_parts(
                    content, key, upload_id, parts, upload_parts
                )

            # Complete multipart upload
            await self.complete_multipart_upload(key, upload_id, upload_parts)

        except Exception:
            # Abort upload on error
            await self.abort_multipart_upload(key, upload_id)
            raise

    async def _upload_by_lines(
        self,
        data: Union[BinaryIO, TextIO, Reader],
        key: str,
        upload_id: str,
        lines_per_part: int,
        parts: list[dict[str, Union[str, int]]],
    ) -> None:
        """Upload parts by number of lines per part."""
        from io import BytesIO, StringIO

        part_number = 1
        line_count = 0

        # Determine if we're working with text or binary data by checking first line
        first_line = None
        for line in data:
            first_line = line
            break

        # Reset data to beginning if possible
        if hasattr(data, "seek"):
            data.seek(0)

        is_binary_data = isinstance(first_line, bytes)

        # Create appropriate buffer for accumulating lines
        current_buffer: Union[StringIO, BytesIO]
        if is_binary_data:
            current_buffer = BytesIO()
        else:
            current_buffer = StringIO()

        for line in data:
            # Write line to buffer
            current_buffer.write(line)  # type: ignore
            line_count += 1

            if line_count >= lines_per_part:
                # Create TextIO/BinaryIO object for upload_part
                current_buffer.seek(0)  # Reset to beginning for reading

                etag = await self.upload_part(
                    key, upload_id, part_number, current_buffer
                )
                parts.append(
                    {
                        "part_number": part_number,
                        "etag": etag,
                    }
                )

                # Reset for next part
                if is_binary_data:
                    current_buffer = BytesIO()
                else:
                    current_buffer = StringIO()
                line_count = 0
                part_number += 1

        # Upload remaining lines if any
        if line_count > 0:
            current_buffer.seek(0)  # Reset to beginning for reading

            etag = await self.upload_part(key, upload_id, part_number, current_buffer)
            parts.append(
                {
                    "part_number": part_number,
                    "etag": etag,
                }
            )

    async def _upload_by_size(
        self,
        data: Union[BinaryIO, TextIO, Reader],
        key: str,
        upload_id: str,
        chunk_size: int,
        parts: list[dict[str, Union[str, int]]],
    ) -> None:
        """Upload parts by specified chunk size in bytes."""
        part_number = 1

        while True:
            raw_chunk: Union[bytes, str]
            # Now Reader is synchronous, so no await needed
            raw_chunk = data.read(chunk_size)

            if not raw_chunk:
                break

            # Convert string chunks to bytes
            if isinstance(raw_chunk, str):
                chunk_bytes = raw_chunk.encode("utf-8")
            elif isinstance(raw_chunk, bytes):
                chunk_bytes = raw_chunk
            else:
                # Handle other types
                chunk_bytes = bytes(raw_chunk) if raw_chunk else b""

            etag = await self.upload_part(key, upload_id, part_number, chunk_bytes)
            parts.append(
                {
                    "part_number": part_number,
                    "etag": etag,
                }
            )
            part_number += 1

    async def _upload_by_parts(
        self,
        data: Union[BinaryIO, TextIO, Reader],
        key: str,
        upload_id: str,
        num_parts: int,
        parts: list[dict[str, Union[str, int]]],
    ) -> None:
        """Upload by automatically determining chunk size based on total parts."""
        # Try to determine file size to calculate chunk size
        file_size = None
        current_pos = None

        try:
            if hasattr(data, "seek") and hasattr(data, "tell"):
                # Handle Reader and other file-like objects (now all synchronous)
                current_pos = data.tell()
                data.seek(0, 2)  # Seek to end
                file_size = data.tell()
                data.seek(current_pos)  # Restore position
        except (OSError, IOError):
            # Can't determine size, fall back to default chunk size
            pass

        if file_size is not None and file_size > 0:
            # Calculate chunk size based on desired number of parts
            chunk_size = max(file_size // num_parts, 1)
            # Ensure minimum chunk size for practical purposes
            chunk_size = max(chunk_size, 1024)  # At least 1KB per part
        else:
            # Fall back to default config chunk size
            chunk_size = self.config.multipart_chunksize

        # Use the size-based upload with calculated chunk size
        await self._upload_by_size(data, key, upload_id, chunk_size, parts)

    async def readline_iter(self, key: str, start_line: int = 0) -> AsyncIterator[str]:
        """Iterator that reads lines from an object using range gets.

        This provides memory-efficient line-by-line reading of large files
        by using range requests to read chunks and yielding complete lines.
        Storage implementations can override this for native readline support.

        Args:
            key: Object key/path

        Yields:
            Lines from the file
        """
        try:
            file_size = await self.get_object_size(key)
        except Exception:
            return

        if file_size == 0:
            return

        buffer = b""
        offset = 0
        current_line = -1

        while offset < file_size:
            # Read a chunk
            chunk_end = min(offset + self.config.range_chunksize - 1, file_size - 1)
            chunk = await self.get_object(key, offset, chunk_end)

            if not chunk:
                break

            buffer += chunk
            offset = chunk_end + 1

            # Extract complete lines from buffer
            while b"\n" in buffer:
                line, buffer = buffer.split(b"\n", 1)
                current_line += 1
                if current_line >= start_line:
                    yield line.decode("utf-8") + "\n"

            # If we're at the end of file and there's remaining buffer, yield it
            if offset >= file_size and buffer:
                current_line += 1
                if current_line >= start_line:
                    yield buffer.decode("utf-8")
                break

    async def read_lines(
        self, key: str, start_line: int = 0, num_lines: Optional[int] = None
    ) -> list[str]:
        """Read specific lines from an object.

        Args:
            key: Object key/path
            start_line: Zero-based line number to start reading from
            num_lines: Number of lines to read (None for all remaining lines)

        Returns:
            List of lines
        """
        lines = []

        async for line in self.readline_iter(key, start_line):
            lines.append(line)
            if num_lines is not None and len(lines) >= num_lines:
                break

        return lines

    async def copy_object(self, source_key: str, dest_key: str) -> None:
        """Copy an object within storage.

        Default implementation downloads and re-uploads the object.
        Storage implementations can override this for native copy support.

        Args:
            source_key: Source object key/path
            dest_key: Destination object key/path
        """
        data = await self.get_object(source_key)
        await self.put_object(dest_key, data)

    def _wrap_data(self, data: Union[bytes, str, BinaryIO, TextIO, Reader]) -> Reader:
        """Wrap data in Reader if necessary."""
        # Wrap non-Reader objects in Reader for consistent handling
        if isinstance(data, str):
            return Reader(StringIO(data))
        elif isinstance(data, bytes):
            return Reader(BytesIO(data))
        elif not isinstance(data, Reader):
            # Assume it's a file-like object (BinaryIO, TextIO, etc.)
            return Reader(data)

        return data

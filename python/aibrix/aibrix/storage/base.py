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

import hashlib
import uuid
from abc import ABC, abstractmethod
from dataclasses import dataclass
from io import BytesIO, StringIO
from typing import AsyncIterator, BinaryIO, Optional, TextIO, Union

from aibrix.storage.reader import Reader
from aibrix.storage.types import StorageType
from aibrix.storage.utils import ObjectMetadata


@dataclass
class StorageConfig:
    """Configuration for storage implementations."""

    # Common configuration
    max_retries: int = 3
    timeout: int = 30

    # Upload configuration
    multipart_threshold: int = 5 * 1024 * 1024  # 5MB
    max_concurrency: int = 10

    # Read configuration
    range_chunksize: int = 1024 * 1024  # 1MB for range reads
    readline_buffer_size: int = 8192  # 8KB buffer for readline


@dataclass
class PutObjectOptions:
    """Options for put_object operations with advanced features."""

    # TTL support
    ttl_seconds: Optional[int] = None
    ttl_milliseconds: Optional[int] = None

    # Conditional operations
    set_if_not_exists: bool = False  # Like Redis NX
    set_if_exists: bool = False  # Like Redis XX

    def __post_init__(self):
        """Validate options."""
        if self.set_if_not_exists and self.set_if_exists:
            raise ValueError("Cannot specify both set_if_not_exists and set_if_exists")

        if self.ttl_seconds is not None and self.ttl_milliseconds is not None:
            raise ValueError("Cannot specify both ttl_seconds and ttl_milliseconds")


class PutObjectOptionsBuilder:
    """Helper class to construct PutObjectOptions."""

    def __init__(self):
        self._options = PutObjectOptions()

    def ttl_seconds(self, seconds: int) -> "PutObjectOptionsBuilder":
        """Set TTL in seconds."""
        self._options.ttl_seconds = seconds
        return self

    def ttl_milliseconds(self, milliseconds: int) -> "PutObjectOptionsBuilder":
        """Set TTL in milliseconds."""
        self._options.ttl_milliseconds = milliseconds
        return self

    def if_not_exists(self) -> "PutObjectOptionsBuilder":
        """Only set if key doesn't exist (like Redis NX)."""
        self._options.set_if_not_exists = True
        return self

    def if_exists(self) -> "PutObjectOptionsBuilder":
        """Only set if key exists (like Redis XX)."""
        self._options.set_if_exists = True
        return self

    def build(self) -> PutObjectOptions:
        """Build the options object."""
        return self._options


class BaseStorage(ABC):
    """Base class for all storage implementations.

    Provides S3-like interface with support for:
    - Raw file read/write operations
    - Multipart uploads for large files
    - Range gets for partial file reads
    - Readline functionality backed by range gets
    - Advanced put_object options (TTL, conditional operations)
    """

    def __init__(self, config: Optional[StorageConfig] = None):
        self.config = config or StorageConfig()

    @abstractmethod
    def get_type(self) -> StorageType:
        """Get the type of storage.

        Returns:
            Type of storage
        """
        pass

    def is_ttl_supported(self) -> bool:
        """Check if TTL (Time To Live) is supported.

        Returns:
            True if TTL is supported, False otherwise
        """
        return False

    def is_set_if_not_exists_supported(self) -> bool:
        """Check if conditional SET IF NOT EXISTS is supported.

        Returns:
            True if SET IF NOT EXISTS is supported, False otherwise
        """
        return False

    def is_set_if_exists_supported(self) -> bool:
        """Check if conditional SET IF EXISTS is supported.

        Returns:
            True if SET IF EXISTS is supported, False otherwise
        """
        return False

    @abstractmethod
    async def put_object(
        self,
        key: str,
        data: Union[bytes, str, BinaryIO, TextIO, Reader],
        content_type: Optional[str] = None,
        metadata: Optional[dict[str, str]] = None,
        options: Optional[PutObjectOptions] = None,
    ) -> bool:
        """Put an object to storage.

        Args:
            key: Object key/path
            data: Data to write (bytes, string, or file-like object)
            content_type: MIME type of the content
            metadata: Additional metadata to store with object
            options: Advanced options for put operation

        Returns:
            True if object was stored, False if conditional operation failed

        Raises:
            ValueError: If unsupported options are specified
        """
        pass

    # [TODO][NOW] Should return Reader object
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
        self,
        prefix: str = "",
        delimiter: Optional[str] = None,
        limit: Optional[int] = None,
        continuation_token: Optional[str] = None,
    ) -> tuple[list[str], Optional[str]]:
        """List objects with given prefix using token-based pagination.

        Args:
            prefix: Key prefix to filter objects
            delimiter: Delimiter for hierarchical listing
            limit: Maximum number of objects to return (None for no limit)
            continuation_token: Token to continue from previous pagination call

        Returns:
            Tuple of (object_keys, next_continuation_token)
            - object_keys: List of object keys
            - next_continuation_token: Token for next page (None if no more pages)
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

    def is_native_multipart_supported(self) -> bool:
        """Check if native multipart upload is supported.

        Returns:
            True if native multipart upload is supported, False otherwise
        """
        return False

    @abstractmethod
    async def _native_create_multipart_upload(
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
    async def _native_upload_part(
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
    async def _native_complete_multipart_upload(
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
    async def _native_abort_multipart_upload(
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

    def _validate_put_options(self, options: Optional[PutObjectOptions]) -> None:
        """Validate put_object options against backend capabilities.

        Args:
            options: Options to validate

        Raises:
            ValueError: If unsupported options are specified
        """
        if options is None:
            return

        if options.ttl_seconds is not None or options.ttl_milliseconds is not None:
            if not self.is_ttl_supported():
                raise ValueError(
                    f"TTL not supported by {self.get_type().value} storage"
                )

        if options.set_if_not_exists and not self.is_set_if_not_exists_supported():
            raise ValueError(
                f"SET IF NOT EXISTS not supported by {self.get_type().value} storage"
            )

        if options.set_if_exists and not self.is_set_if_exists_supported():
            raise ValueError(
                f"SET IF EXISTS not supported by {self.get_type().value} storage"
            )

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
        if byline <= 0 and bysize <= 0 and parts <= 1:
            await self.put_object(key, data, content_type, metadata)
            return

        upload_parts: list[dict[str, Union[str, int]]] = []
        small_parts = False

        reader = self._wrap_data(data)

        try:
            # Priority 1: Upload by lines
            if byline > 0:
                small_parts = True
                upload_id = await self.create_multipart_upload(
                    key, content_type, metadata, small_parts=True
                )
                await self._upload_by_lines(
                    reader, key, upload_id, byline, upload_parts
                )
                await self.complete_multipart_upload(key, upload_id, upload_parts)
                return

            # Priority 3: Upload by parts (auto-determine chunk size)
            if bysize <= 0 and parts > 1:
                bysize = self.config.multipart_threshold
                try:
                    file_size = reader.get_size()
                    bysize = file_size // parts
                except Exception:
                    # Use default multipart_threshold
                    pass

            # Priority 2: Upload by size
            if bysize >= self.config.multipart_threshold:
                upload_id = await self.create_multipart_upload(
                    key, content_type, metadata
                )
                await self._upload_by_size(reader, key, upload_id, bysize, upload_parts)
                await self._native_complete_multipart_upload(
                    key, upload_id, upload_parts
                )
            else:
                small_parts = True
                upload_id = await self.create_multipart_upload(
                    key, content_type, metadata, small_parts=True
                )
                await self._upload_by_size(reader, key, upload_id, bysize, upload_parts)
                await self.complete_multipart_upload(key, upload_id, upload_parts)

        except Exception:
            if small_parts:
                # Abort small parts upload on error
                await self.abort_multipart_upload(key, upload_id)
            else:
                # Abort upload on error
                await self._native_abort_multipart_upload(key, upload_id)
            raise

    async def create_multipart_upload(
        self,
        key: str,
        content_type: Optional[str] = None,
        metadata: Optional[dict[str, str]] = None,
        small_parts: bool = False,
    ) -> str:
        """Default implementation for multipart upload creation.

        If small_parts is specified, creates a unique upload ID and stores upload metadata as a special object.
        Suitable for S3/TOS backends that don't have native multipart support for small files.

        Args:
            key: Object key/path
            content_type: MIME type of the content
            metadata: Additional metadata to store with object
            small_part: Use implementation for small parts

        Returns:
            Upload ID for the multipart upload session
        """
        if not small_parts and self.is_native_multipart_supported():
            return await self._native_create_multipart_upload(
                key, content_type, metadata
            )

        upload_id = str(uuid.uuid4())

        # Store upload metadata as a special object
        from datetime import datetime

        upload_metadata = {
            "key": key,
            "content_type": content_type,
            "metadata": metadata or {},
            "parts": {},
            "created_at": datetime.now().isoformat(),
        }

        await self.put_object(
            self._multipart_upload_key(upload_id),
            str(upload_metadata).encode("utf-8"),
            content_type="application/json",
        )

        return upload_id

    async def upload_part(
        self,
        key: str,
        upload_id: str,
        part_number: int,
        data: Union[str, bytes, BinaryIO, TextIO, Reader],
    ) -> str:
        """Default implementation for uploading a part.

        Stores each part as an individual object for later aggregation.
        Suitable for S3/TOS backends that don't have native multipart support for small files.

        Args:
            key: Object key/path
            upload_id: Upload ID from create_multipart_upload
            part_number: Part number (1-based)
            data: Part data

        Returns:
            ETag for the uploaded part
        """
        # Use native_upload_part if metadata object doesn't exist
        if self.is_native_multipart_supported() and not await self.object_exists(
            self._multipart_upload_key(upload_id)
        ):
            return await self._native_upload_part(key, upload_id, part_number, data)

        part_data = self._wrap_data(data).read_all()

        # Store part as individual object
        await self.put_object(
            self._multipart_upload_key(upload_id, f"part_{part_number:05d}"),
            part_data,
            content_type="application/octet-stream",
        )

        # Calculate ETag (MD5 hash)
        etag = hashlib.md5(part_data).hexdigest()

        return etag

    async def complete_multipart_upload(
        self,
        key: str,
        upload_id: str,
        parts: list[dict[str, Union[str, int]]],
    ) -> None:
        """Default implementation for completing multipart upload.

        Downloads all part objects, aggregates them locally, and uploads the final object.
        Suitable for S3/TOS backends that don't have native multipart support for small files.

        Args:
            key: Object key/path
            upload_id: Upload ID from create_multipart_upload
            parts: List of parts with 'part_number' and 'etag' keys
        """
        try:
            # Get upload metadata
            metadata_data = await self.get_object(self._multipart_upload_key(upload_id))
            upload_metadata = eval(
                metadata_data.decode("utf-8")
            )  # Simple parsing for dict
        except Exception:
            # Use _native_complete_multipart_upload if metadata object doesn't exist
            if self.is_native_multipart_supported():
                return await self._native_complete_multipart_upload(
                    key, upload_id, parts
                )
            raise ValueError(f"Upload ID {upload_id} not found or corrupted")

        content_type = upload_metadata.get("content_type")
        metadata = upload_metadata.get("metadata", {})

        # Sort parts by part number
        sorted_parts = sorted(parts, key=lambda p: p["part_number"])

        # Download and aggregate all parts locally
        aggregated_data = BytesIO()

        for part in sorted_parts:
            part_number = part["part_number"]
            try:
                part_data = await self.get_object(
                    self._multipart_upload_key(upload_id, f"part_{part_number:05d}")
                )
                aggregated_data.write(part_data)
            except Exception:
                # Clean up and raise error
                await self.abort_multipart_upload(key, upload_id)
                raise ValueError(
                    f"Failed to retrieve part {part_number} for upload {upload_id}"
                )

        # Upload the final aggregated object
        aggregated_data.seek(0)
        await self.put_object(key, aggregated_data, content_type, metadata)

        # Clean up multipart upload objects
        await self.abort_multipart_upload(key, upload_id)

    async def abort_multipart_upload(
        self,
        key: str,
        upload_id: str,
    ) -> None:
        """Default implementation for aborting multipart upload.

        Cleans up all part objects and metadata for the upload.
        Suitable for S3/TOS backends that don't have native multipart support for small files.

        Args:
            key: Object key/path
            upload_id: Upload ID from create_multipart_upload
        """
        # List and delete all objects related to this upload
        prefix = self._multipart_upload_key(upload_id, "")
        no_multipart_data = True
        try:
            objects_to_delete, _ = await self.list_objects(prefix)

            # Delete all related objects
            for obj_key in objects_to_delete:
                try:
                    no_multipart_data = False
                    await self.delete_object(obj_key)
                except Exception:
                    # Continue deletion even if some objects fail
                    pass
        except Exception:
            # If listing fails, try to delete known objects
            try:
                await self.delete_object(self._multipart_upload_key(upload_id))
                no_multipart_data = False
            except Exception:
                pass

        # Last attempt, nothing we can do for small parts
        if no_multipart_data and self.is_native_multipart_supported():
            await self._native_abort_multipart_upload(key, upload_id)

    async def _upload_by_lines(
        self,
        reader: Reader,
        key: str,
        upload_id: str,
        lines_per_part: int,
        parts: list[dict[str, Union[str, int]]],
    ) -> None:
        """Upload parts by number of lines per part."""
        from io import BytesIO

        part_number = 1
        line_count = 0

        # Reset data to beginning if possible
        if hasattr(reader, "seek"):
            reader.seek(0)

        # Create appropriate buffer for accumulating lines
        current_buffer = BytesIO()

        for line in reader.iter_lines():
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
                current_buffer = BytesIO()

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
        reader: Reader,
        key: str,
        upload_id: str,
        chunk_size: int,
        parts: list[dict[str, Union[str, int]]],
    ) -> None:
        """Upload parts by specified chunk size in bytes."""
        part_number = 1

        while True:
            raw_chunk = reader.read(chunk_size)
            if not raw_chunk:
                break

            if chunk_size >= self.config.multipart_threshold:
                etag = await self._native_upload_part(
                    key, upload_id, part_number, raw_chunk
                )
            else:
                etag = await self.upload_part(key, upload_id, part_number, raw_chunk)

            parts.append(
                {
                    "part_number": part_number,
                    "etag": etag,
                }
            )
            part_number += 1

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

    def _multipart_upload_key(self, upload_id: str, subkey: str = "metadata") -> str:
        """Return the key for multipart upload metadata. For small parts multipart upload only."""
        return f".multipart/{upload_id}/{subkey}"

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

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

import asyncio
import hashlib
import os
import shutil
from datetime import datetime
from pathlib import Path
from typing import AsyncIterator, BinaryIO, Optional, TextIO, Union

from .base import BaseStorage, StorageConfig
from .reader import Reader
from .utils import ObjectMetadata, generate_filename

LOCAL_STORAGE_PATH_VAR = "LOCAL_STORAGE_PATH"


class LocalStorage(BaseStorage):
    """Local filesystem storage implementation."""

    def __init__(
        self, base_path: Optional[str] = None, config: Optional[StorageConfig] = None
    ):
        super().__init__(config)
        if base_path is None:
            if LOCAL_STORAGE_PATH_VAR in os.environ:
                base_path = os.environ[LOCAL_STORAGE_PATH_VAR]
            else:
                base_path = os.path.join(
                    os.path.abspath(os.path.dirname(__file__)), "data"
                )

        self.base_path = Path(base_path)
        self.base_path.mkdir(parents=True, exist_ok=True)

    def _get_full_path(
        self,
        key: str,
        content_type: Optional[str] = None,
        metadata: Optional[dict[str, str]] = None,
    ) -> Path:
        """Get full filesystem path for a key."""
        return self.base_path / generate_filename(key, content_type, metadata)

    def _get_metadata_path(self, key: str) -> Path:
        """Get metadata file path for a key."""
        return self.base_path / f"{key}.metadata"

    def _infer_content_type(self, key: str) -> Optional[str]:
        """Infer content type from file extension."""
        from pathlib import Path

        suffix = Path(key).suffix.lower()
        content_type_map = {
            ".json": "application/json",
            ".jsonl": "application/jsonl",
            ".txt": "text/plain",
            ".csv": "text/csv",
            ".xml": "application/xml",
            ".html": "text/html",
            ".pdf": "application/pdf",
            ".png": "image/png",
            ".jpg": "image/jpeg",
            ".jpeg": "image/jpeg",
            ".gif": "image/gif",
            ".zip": "application/zip",
            ".tar": "application/x-tar",
            ".gz": "application/gzip",
        }
        return content_type_map.get(suffix)

    async def put_object(
        self,
        key: str,
        data: Union[bytes, str, BinaryIO, TextIO, Reader],
        content_type: Optional[str] = None,
        metadata: Optional[dict[str, str]] = None,
    ) -> None:
        """Put an object to local filesystem."""
        # Infer content type from file extension if not provided
        if content_type is None:
            content_type = self._infer_content_type(key)

        full_path = self._get_full_path(key, content_type, metadata)
        full_path.parent.mkdir(parents=True, exist_ok=True)

        # Handle Reader separately since it requires async operations
        reader = self._wrap_data(data)
        await asyncio.get_event_loop().run_in_executor(
            None, self._write_file, full_path, reader
        )

        # Calculate metadata fields after writing
        def _get_file_metadata():
            stat = full_path.stat()
            content_length = stat.st_size

            # Generate etag based on file size and modification time
            etag_data = f"{content_length}-{stat.st_mtime}"
            etag = hashlib.md5(etag_data.encode()).hexdigest()

            last_modified = datetime.fromtimestamp(stat.st_mtime)
            return content_length, etag, last_modified

        (
            file_content_length,
            file_etag,
            file_last_modified,
        ) = await asyncio.get_event_loop().run_in_executor(None, _get_file_metadata)

        # Store metadata with file information
        await self._store_metadata(
            key,
            content_type,
            metadata,
            file_content_length,
            file_etag,
            file_last_modified,
        )

    def _write_file(self, path: Path, reader: Reader) -> None:
        """Write data to file (synchronous helper)."""
        if reader.is_binary():
            # Write as bytes
            with open(path, "wb") as f:
                f.write(bytes(reader))
        else:
            # Write as text string
            with open(path, "w", encoding="utf-8") as f:
                f.write(str(reader))

    async def _store_metadata(
        self,
        key: str,
        content_type: Optional[str] = None,
        metadata: Optional[dict[str, str]] = None,
        content_length: Optional[int] = None,
        etag: Optional[str] = None,
        last_modified: Optional[datetime] = None,
    ) -> None:
        """Store metadata for an object."""
        metadata_path = self._get_metadata_path(key)
        metadata_path.parent.mkdir(parents=True, exist_ok=True)

        metadata_data = {
            "content_type": content_type,
            "metadata": metadata or {},
            "content_length": content_length,
            "etag": etag,
            "last_modified": last_modified.isoformat() if last_modified else None,
        }

        await asyncio.get_event_loop().run_in_executor(
            None, self._write_json_file, metadata_path, metadata_data
        )

    async def _load_metadata(
        self, key: str
    ) -> tuple[
        Optional[str], dict[str, str], Optional[int], Optional[str], Optional[datetime]
    ]:
        """Load metadata for an object."""
        metadata_path = self._get_metadata_path(key)

        if not metadata_path.exists():
            return None, {}, None, None, None

        metadata_data = await asyncio.get_event_loop().run_in_executor(
            None, self._read_json_file, metadata_path
        )

        # Parse last_modified from ISO format
        last_modified = None
        if last_modified_str := metadata_data.get("last_modified"):
            try:
                last_modified = datetime.fromisoformat(last_modified_str)
            except ValueError:
                pass

        return (
            metadata_data.get("content_type"),
            metadata_data.get("metadata", {}),
            metadata_data.get("content_length"),
            metadata_data.get("etag"),
            last_modified,
        )

    async def get_object(
        self,
        key: str,
        range_start: Optional[int] = None,
        range_end: Optional[int] = None,
    ) -> bytes:
        """Get an object from local filesystem."""
        content_type, metadata, _, _, _ = await self._load_metadata(key)
        full_path = self._get_full_path(key, content_type, metadata)

        if not full_path.exists():
            raise FileNotFoundError(f"Object not found: {key}")

        return await asyncio.get_event_loop().run_in_executor(
            None, self._read_file, full_path, range_start, range_end
        )

    def _read_file(
        self, path: Path, range_start: Optional[int], range_end: Optional[int]
    ) -> bytes:
        """Read file data (synchronous helper)."""
        with open(path, "rb") as f:
            if range_start is not None:
                f.seek(range_start)
                if range_end is not None:
                    size = range_end - range_start + 1
                    return f.read(size)
                else:
                    return f.read()
            else:
                return f.read()

    async def delete_object(self, key: str) -> None:
        """Delete an object from local filesystem."""
        content_type, metadata, _, _, _ = await self._load_metadata(key)
        full_path = self._get_full_path(key, content_type, metadata)
        metadata_path = self._get_metadata_path(key)

        if full_path.exists():
            await asyncio.get_event_loop().run_in_executor(None, full_path.unlink)

        # Clean up metadata file if it exists
        if metadata_path.exists():
            await asyncio.get_event_loop().run_in_executor(None, metadata_path.unlink)

    async def list_objects(
        self,
        prefix: str = "",
        delimiter: Optional[str] = None,
        limit: Optional[int] = None,
        continuation_token: Optional[str] = None,
    ) -> tuple[list[str], Optional[str]]:
        """List objects with given prefix."""
        prefix_path = self.base_path / prefix if prefix else self.base_path

        def _list_files():
            # Parse continuation token as offset (default to 0)
            offset = 0
            if continuation_token:
                try:
                    offset = int(continuation_token)
                except (ValueError, TypeError):
                    offset = 0

            files = []
            if prefix_path.is_dir():
                if delimiter:
                    # List immediate children only
                    for item in prefix_path.iterdir():
                        if item.is_file():
                            relative_path = str(item.relative_to(self.base_path))
                            # Filter out metadata files
                            if not relative_path.endswith(".metadata"):
                                files.append(relative_path)
                        elif item.is_dir():
                            files.append(
                                str(item.relative_to(self.base_path)) + delimiter
                            )
                else:
                    # Recursive listing
                    for item in prefix_path.rglob("*"):
                        if item.is_file():
                            relative_path = str(item.relative_to(self.base_path))
                            # Filter out metadata files
                            if not relative_path.endswith(".metadata"):
                                files.append(relative_path)
            elif prefix_path.is_file():
                relative_path = str(prefix_path.relative_to(self.base_path))
                # Filter out metadata files
                if not relative_path.endswith(".metadata"):
                    files.append(relative_path)

            # Sort files for consistent pagination (by filename)
            files.sort()

            # Apply pagination
            remaining_files = files[offset:] if offset > 0 else files
            paginated_files = (
                remaining_files[:limit] if limit is not None else remaining_files
            )

            # Check if there are more files for next page
            has_more = limit is not None and len(remaining_files) > limit
            next_token = str(offset + len(paginated_files)) if has_more else None

            return paginated_files, next_token

        return await asyncio.get_event_loop().run_in_executor(None, _list_files)

    async def object_exists(self, key: str) -> bool:
        """Check if object exists."""
        content_type, metadata, _, _, _ = await self._load_metadata(key)
        full_path = self._get_full_path(key, content_type, metadata)
        return full_path.exists() and full_path.is_file()

    async def get_object_size(self, key: str) -> int:
        """Get object size in bytes."""
        content_type, metadata, _, _, _ = await self._load_metadata(key)
        full_path = self._get_full_path(key, content_type, metadata)

        if not full_path.exists():
            raise FileNotFoundError(f"Object not found: {key}")

        return await asyncio.get_event_loop().run_in_executor(
            None, lambda: full_path.stat().st_size
        )

    async def head_object(self, key: str) -> ObjectMetadata:
        """Get object metadata without downloading the object content."""
        # Load stored metadata only (as per requirement #3)
        (
            stored_content_type,
            stored_metadata,
            stored_content_length,
            stored_etag,
            stored_last_modified,
        ) = await self._load_metadata(key)

        # Check if metadata exists (which implies object exists)
        # We consider the object exists if we have content_length, etag, or last_modified
        if (
            stored_content_length is None
            and stored_etag is None
            and stored_last_modified is None
        ):
            raise FileNotFoundError(f"Object not found: {key}")

        return ObjectMetadata(
            content_length=stored_content_length or 0,
            content_type=stored_content_type,
            etag=stored_etag or "",
            last_modified=stored_last_modified,
            metadata=stored_metadata,
            storage_class="STANDARD",  # Local storage uses standard class
        )

    async def copy_object(self, source_key: str, dest_key: str) -> None:
        """Copy an object within local filesystem."""
        # Load source metadata first
        source_content_type, source_metadata, _, _, _ = await self._load_metadata(
            source_key
        )

        source_path = self._get_full_path(
            source_key, source_content_type, source_metadata
        )
        dest_path = self._get_full_path(dest_key, source_content_type, source_metadata)
        source_metadata_path = self._get_metadata_path(source_key)
        dest_metadata_path = self._get_metadata_path(dest_key)

        if not source_path.exists():
            raise FileNotFoundError(f"Source object not found: {source_key}")

        dest_path.parent.mkdir(parents=True, exist_ok=True)

        # Copy the main file
        await asyncio.get_event_loop().run_in_executor(
            None, shutil.copy2, source_path, dest_path
        )

        # Copy metadata file if it exists
        if source_metadata_path.exists():
            dest_metadata_path.parent.mkdir(parents=True, exist_ok=True)
            await asyncio.get_event_loop().run_in_executor(
                None, shutil.copy2, source_metadata_path, dest_metadata_path
            )

    async def _native_create_multipart_upload(
        self,
        key: str,
        content_type: Optional[str] = None,
        metadata: Optional[dict[str, str]] = None,
    ) -> str:
        """Create a multipart upload session for local storage."""
        raise NotImplementedError("Multipart upload not needed for local storage")

    async def _native_upload_part(
        self,
        key: str,
        upload_id: str,
        part_number: int,
        data: Union[str, bytes, BinaryIO, TextIO, Reader],
    ) -> str:
        """Upload a part for a multipart upload."""
        raise NotImplementedError("Multipart upload not needed for local storage")

    async def _native_complete_multipart_upload(
        self,
        key: str,
        upload_id: str,
        parts: list[dict[str, Union[str, int]]],
    ) -> None:
        """Complete a multipart upload by combining all parts."""
        raise NotImplementedError("Multipart upload not needed for local storage")

    async def _native_abort_multipart_upload(
        self,
        key: str,
        upload_id: str,
    ) -> None:
        """Abort a multipart upload and clean up."""
        upload_dir = self.base_path / self._multipart_upload_key(upload_id, "")
        if upload_dir.exists():
            await asyncio.get_event_loop().run_in_executor(
                None, shutil.rmtree, upload_dir
            )

    async def abort_multipart_upload(
        self,
        key: str,
        upload_id: str,
    ) -> None:
        """Call _native_abort_multipart_upload for local storage."""
        await self._native_abort_multipart_upload(key, upload_id)

    def _write_json_file(self, path: Path, data: dict) -> None:
        """Write JSON data to file (synchronous helper)."""
        import json

        with open(path, "w") as f:
            json.dump(data, f)

    def _read_json_file(self, path: Path) -> dict:
        """Read JSON data from file (synchronous helper)."""
        import json

        with open(path, "r") as f:
            return json.load(f)

    def _write_bytes_file(self, path: Path, data: bytes) -> None:
        """Write bytes to file (synchronous helper)."""
        with open(path, "wb") as f:
            f.write(data)

    def _combine_parts(self, upload_dir: Path, parts: list, final_path: Path) -> None:
        """Combine multipart upload parts into final file (synchronous helper)."""
        with open(final_path, "wb") as final_file:
            for part in parts:
                part_number = part["part_number"]
                part_file = upload_dir / f"part_{part_number:05d}"
                if part_file.exists():
                    with open(part_file, "rb") as pf:
                        shutil.copyfileobj(pf, final_file)

    async def readline_iter(self, key: str, start_index: int = 0) -> AsyncIterator[str]:
        """Native readline implementation for local storage using file streaming."""
        content_type, metadata, _, _, _ = await self._load_metadata(key)
        full_path = self._get_full_path(key, content_type, metadata)

        if not full_path.exists():
            return

        def _read_lines():
            """Generator that reads lines from file."""
            try:
                with open(full_path, "r", encoding="utf-8") as f:
                    for line in f:
                        yield line

            except UnicodeDecodeError:
                # Fallback to binary mode for non-UTF8 files
                with open(full_path, "rb") as f:
                    for b_line in f:
                        yield b_line.decode("utf-8", errors="replace")

        # Use thread pool to avoid blocking the event loop
        line_no = -1
        for line in await asyncio.get_event_loop().run_in_executor(
            None, list, _read_lines()
        ):
            line_no += 1
            if line_no >= start_index:
                yield line

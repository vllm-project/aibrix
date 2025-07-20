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
import uuid
from datetime import datetime
from pathlib import Path
from typing import AsyncIterator, BinaryIO, Optional, TextIO, Union

from aibrix.storage.base import BaseStorage, StorageConfig
from aibrix.storage.utils import ObjectMetadata

from .utils import Reader

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

    def _get_full_path(self, key: str) -> Path:
        """Get full filesystem path for a key."""
        return self.base_path / key

    async def put_object(
        self,
        key: str,
        data: Union[bytes, str, BinaryIO, TextIO, Reader],
        content_type: Optional[str] = None,
        metadata: Optional[dict[str, str]] = None,
    ) -> None:
        """Put an object to local filesystem."""
        # [TODO][NOW] content_type should stored in key.metadata
        full_path = self._get_full_path(key)
        full_path.parent.mkdir(parents=True, exist_ok=True)

        # Handle Reader separately since it requires async operations
        reader = self._wrap_data(data)
        await asyncio.get_event_loop().run_in_executor(
            None, self._write_file, full_path, reader
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

    async def get_object(
        self,
        key: str,
        range_start: Optional[int] = None,
        range_end: Optional[int] = None,
    ) -> bytes:
        """Get an object from local filesystem."""
        full_path = self._get_full_path(key)

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
        full_path = self._get_full_path(key)

        if full_path.exists():
            await asyncio.get_event_loop().run_in_executor(None, full_path.unlink)

    async def list_objects(
        self, prefix: str = "", delimiter: Optional[str] = None
    ) -> list[str]:
        """List objects with given prefix."""
        prefix_path = self.base_path / prefix if prefix else self.base_path

        def _list_files():
            files = []
            if prefix_path.is_dir():
                if delimiter:
                    # List immediate children only
                    for item in prefix_path.iterdir():
                        if item.is_file():
                            files.append(str(item.relative_to(self.base_path)))
                        elif item.is_dir():
                            files.append(
                                str(item.relative_to(self.base_path)) + delimiter
                            )
                else:
                    # Recursive listing
                    for item in prefix_path.rglob("*"):
                        if item.is_file():
                            files.append(str(item.relative_to(self.base_path)))
            elif prefix_path.is_file():
                files.append(str(prefix_path.relative_to(self.base_path)))
            return files

        return await asyncio.get_event_loop().run_in_executor(None, _list_files)

    async def object_exists(self, key: str) -> bool:
        """Check if object exists."""
        full_path = self._get_full_path(key)
        return full_path.exists() and full_path.is_file()

    async def get_object_size(self, key: str) -> int:
        """Get object size in bytes."""
        full_path = self._get_full_path(key)

        if not full_path.exists():
            raise FileNotFoundError(f"Object not found: {key}")

        return await asyncio.get_event_loop().run_in_executor(
            None, lambda: full_path.stat().st_size
        )

    async def head_object(self, key: str) -> ObjectMetadata:
        """Get object metadata without downloading the object content."""
        full_path = self._get_full_path(key)

        if not full_path.exists():
            raise FileNotFoundError(f"Object not found: {key}")

        def _get_metadata():
            stat = full_path.stat()

            # Generate simple etag based on file size and modification time
            etag_data = f"{stat.st_size}-{stat.st_mtime}"
            etag = hashlib.md5(etag_data.encode()).hexdigest()

            # Try to determine content type from file extension
            content_type = None
            suffix = full_path.suffix.lower()
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
            content_type = content_type_map.get(suffix)

            return ObjectMetadata(
                content_length=stat.st_size,
                content_type=content_type,
                etag=etag,
                last_modified=datetime.fromtimestamp(stat.st_mtime),
                metadata={},
                storage_class="STANDARD",  # Local storage uses standard class
            )

        return await asyncio.get_event_loop().run_in_executor(None, _get_metadata)

    async def copy_object(self, source_key: str, dest_key: str) -> None:
        """Copy an object within local filesystem."""
        source_path = self._get_full_path(source_key)
        dest_path = self._get_full_path(dest_key)

        if not source_path.exists():
            raise FileNotFoundError(f"Source object not found: {source_key}")

        dest_path.parent.mkdir(parents=True, exist_ok=True)

        await asyncio.get_event_loop().run_in_executor(
            None, shutil.copy2, source_path, dest_path
        )

    async def create_multipart_upload(
        self,
        key: str,
        content_type: Optional[str] = None,
        metadata: Optional[dict[str, str]] = None,
    ) -> str:
        """Create a multipart upload session for local storage."""
        upload_id = str(uuid.uuid4())
        upload_dir = self.base_path / ".uploads" / upload_id
        upload_dir.mkdir(parents=True, exist_ok=True)

        # Store metadata for the upload
        metadata_file = upload_dir / "metadata.json"
        upload_metadata = {
            "key": key,
            "content_type": content_type,
            "metadata": metadata or {},
            "parts": {},
        }

        await asyncio.get_event_loop().run_in_executor(
            None, self._write_json_file, metadata_file, upload_metadata
        )

        return upload_id

    async def upload_part(
        self,
        key: str,
        upload_id: str,
        part_number: int,
        data: Union[str, bytes, BinaryIO, TextIO, Reader],
    ) -> str:
        """Upload a part for a multipart upload."""
        upload_dir = self.base_path / ".uploads" / upload_id
        if not upload_dir.exists():
            raise ValueError(f"Upload ID {upload_id} not found")

        # Convert data to bytes
        if isinstance(data, str):
            part_data = data.encode("utf-8")
        elif isinstance(data, bytes):
            part_data = data
        elif isinstance(data, Reader):
            # Handle Reader (async file-like object) - stream the data
            chunks = []
            async for chunk in data.iter_chunks():
                chunks.append(chunk)
            part_data = b"".join(chunks)
        else:
            # File-like object
            content = data.read()
            part_data = content.encode("utf-8") if isinstance(content, str) else content

        # Write part to file
        part_file = upload_dir / f"part_{part_number:05d}"
        await asyncio.get_event_loop().run_in_executor(
            None, self._write_bytes_file, part_file, part_data
        )

        # Calculate ETag (MD5 hash for local storage)
        etag = hashlib.md5(part_data).hexdigest()

        # Update metadata
        metadata_file = upload_dir / "metadata.json"
        metadata = await asyncio.get_event_loop().run_in_executor(
            None, self._read_json_file, metadata_file
        )
        metadata["parts"][str(part_number)] = {
            "etag": etag,
            "size": len(part_data),
        }

        await asyncio.get_event_loop().run_in_executor(
            None, self._write_json_file, metadata_file, metadata
        )

        return etag

    async def complete_multipart_upload(
        self,
        key: str,
        upload_id: str,
        parts: list[dict[str, Union[str, int]]],
    ) -> None:
        """Complete a multipart upload by combining all parts."""
        upload_dir = self.base_path / ".uploads" / upload_id
        if not upload_dir.exists():
            raise ValueError(f"Upload ID {upload_id} not found")

        # Read metadata (not used but validates upload exists)
        metadata_file = upload_dir / "metadata.json"
        await asyncio.get_event_loop().run_in_executor(
            None, self._read_json_file, metadata_file
        )

        # Sort parts by part number
        sorted_parts = sorted(parts, key=lambda p: p["part_number"])

        # Combine all parts into final file
        final_path = self._get_full_path(key)
        final_path.parent.mkdir(parents=True, exist_ok=True)

        await asyncio.get_event_loop().run_in_executor(
            None, self._combine_parts, upload_dir, sorted_parts, final_path
        )

        # Clean up upload directory
        await asyncio.get_event_loop().run_in_executor(None, shutil.rmtree, upload_dir)

    async def abort_multipart_upload(
        self,
        key: str,
        upload_id: str,
    ) -> None:
        """Abort a multipart upload and clean up."""
        upload_dir = self.base_path / ".uploads" / upload_id
        if upload_dir.exists():
            await asyncio.get_event_loop().run_in_executor(
                None, shutil.rmtree, upload_dir
            )

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
        full_path = self._get_full_path(key)

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

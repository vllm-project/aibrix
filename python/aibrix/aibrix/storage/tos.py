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
from io import BytesIO
from typing import BinaryIO, Optional, TextIO, Union

import tos
from tos.exceptions import TosClientError, TosServerError

from aibrix.storage.base import BaseStorage, StorageConfig
from aibrix.storage.utils import ObjectMetadata

from .utils import Reader


class TOSPart:
    """Simple wrapper for TOS multipart upload part data."""

    def __init__(self, part_number: int, etag: str):
        self.part_number = part_number
        self.etag = etag


class TOSStorage(BaseStorage):
    """TOS (Volcano Object Storage) implementation with multipart upload and range get support."""

    def __init__(
        self,
        bucket_name: str,
        access_key: str,
        secret_key: str,
        endpoint: str,
        region: str,
        config: Optional[StorageConfig] = None,
    ):
        super().__init__(config)
        self.bucket_name = bucket_name

        try:
            self.client = tos.TosClientV2(access_key, secret_key, endpoint, region)
        except (TosClientError, TosServerError) as e:
            raise ValueError(f"Failed to create TOS client: {e}")

    async def put_object(
        self,
        key: str,
        data: Union[bytes, str, BinaryIO, TextIO, Reader],
        content_type: Optional[str] = None,
        metadata: Optional[dict[str, str]] = None,
    ) -> None:
        """Put an object to TOS."""
        # Unify all data types using Reader wrapper
        reader = self._wrap_data(data)

        # Check if we should use multipart upload
        try:
            size = reader.get_size()
            if size >= self.config.multipart_threshold:
                await self.multipart_upload(key, reader, content_type, metadata)
                return
        except (OSError, IOError, ValueError):
            # Can't determine size, use multipart upload for safety to avoid loading all data
            await self.multipart_upload(key, reader, content_type, metadata)
            return

        # For small files, read all data and upload directly as BytesIO
        # TOS client has issues with custom file-like objects for CRC calculation
        file_content = reader.read_all()
        tos_content = BytesIO(file_content)

        put_kwargs = {
            "bucket": self.bucket_name,
            "key": key,
            "content": tos_content,
        }

        if content_type:
            put_kwargs["content_type"] = content_type

        if metadata:
            put_kwargs["meta"] = metadata  # type: ignore

        def _put_object():
            try:
                self.client.put_object(**put_kwargs)
            except (TosClientError, TosServerError) as e:
                raise ValueError(f"Failed to put object {key}: {e}")

        await asyncio.get_event_loop().run_in_executor(None, _put_object)

        # Close the reader if we created it
        if not isinstance(data, Reader):
            reader.close()

    async def get_object(
        self,
        key: str,
        range_start: Optional[int] = None,
        range_end: Optional[int] = None,
    ) -> bytes:
        """Get an object from TOS with optional range support."""
        kwargs: dict[str, Union[str, int]] = {
            "bucket": self.bucket_name,
            "key": key,
        }

        if range_start is not None:
            kwargs["range_start"] = range_start
            if range_end is not None:
                kwargs["range_end"] = range_end

        def _get_object():
            try:
                response = self.client.get_object(**kwargs)
                return response.read()
            except (TosClientError, TosServerError) as e:
                if "NoSuchKey" in str(e) or "404" in str(e):
                    raise FileNotFoundError(f"Object not found: {key}")
                raise ValueError(f"Failed to get object {key}: {e}")

        return await asyncio.get_event_loop().run_in_executor(None, _get_object)

    async def delete_object(self, key: str) -> None:
        """Delete an object from TOS."""

        def _delete_object():
            try:
                self.client.delete_object(bucket=self.bucket_name, key=key)
            except (TosClientError, TosServerError) as e:
                if "NoSuchKey" not in str(e) and "404" not in str(e):
                    raise ValueError(f"Failed to delete object {key}: {e}")

        await asyncio.get_event_loop().run_in_executor(None, _delete_object)

    async def list_objects(
        self, prefix: str = "", delimiter: Optional[str] = None
    ) -> list[str]:
        """List objects with given prefix."""

        def _list_objects():
            objects = []
            marker = ""
            is_truncated = True

            kwargs = {
                "bucket": self.bucket_name,
                "prefix": prefix,
            }

            if delimiter:
                kwargs["delimiter"] = delimiter

            try:
                while is_truncated:
                    if marker:
                        kwargs["marker"] = marker

                    if delimiter:
                        # Use list_objects_type2 for hierarchical listing
                        response = self.client.list_objects_type2(**kwargs)

                        # Add files
                        for obj in response.contents:
                            objects.append(obj.key)

                        # Add "directories" (common prefixes)
                        for prefix_info in getattr(response, "common_prefixes", []):
                            objects.append(prefix_info.prefix)

                        is_truncated = response.is_truncated
                        if is_truncated:
                            marker = response.next_continuation_token
                    else:
                        # Use list_objects for flat listing
                        response = self.client.list_objects(**kwargs)

                        for obj in response.contents:
                            objects.append(obj.key)

                        is_truncated = response.is_truncated
                        if is_truncated:
                            marker = response.next_marker

            except (TosClientError, TosServerError) as e:
                raise ValueError(f"Failed to list objects with prefix {prefix}: {e}")

            return objects

        return await asyncio.get_event_loop().run_in_executor(None, _list_objects)

    async def object_exists(self, key: str) -> bool:
        """Check if object exists in TOS."""

        def _head_object():
            try:
                self.client.head_object(bucket=self.bucket_name, key=key)
                return True
            except (TosClientError, TosServerError) as e:
                if "NoSuchKey" in str(e) or "404" in str(e):
                    return False
                raise ValueError(f"Failed to check object existence {key}: {e}")

        return await asyncio.get_event_loop().run_in_executor(None, _head_object)

    async def get_object_size(self, key: str) -> int:
        """Get object size in bytes."""

        def _head_object():
            try:
                response = self.client.head_object(bucket=self.bucket_name, key=key)
                return response.content_length
            except (TosClientError, TosServerError) as e:
                if "NoSuchKey" in str(e) or "404" in str(e):
                    raise FileNotFoundError(f"Object not found: {key}")
                raise ValueError(f"Failed to get object size {key}: {e}")

        return await asyncio.get_event_loop().run_in_executor(None, _head_object)

    async def head_object(self, key: str) -> ObjectMetadata:
        """Get object metadata without downloading the object content."""

        def _head_object():
            try:
                response = self.client.head_object(bucket=self.bucket_name, key=key)

                # Parse last modified time from TOS response
                last_modified = None
                if hasattr(response, "last_modified") and response.last_modified:
                    last_modified = response.last_modified
                    if hasattr(last_modified, "replace"):
                        # Convert TOS datetime to naive datetime (remove timezone info)
                        last_modified = last_modified.replace(tzinfo=None)

                # Extract user metadata from TOS headers (x-tos-meta-* headers)
                user_metadata = {}
                if hasattr(response, "meta") and response.meta:
                    user_metadata = response.meta

                # Extract TOS-specific fields
                storage_class = getattr(response, "storage_class", None)
                version_id = getattr(response, "version_id", None)
                encryption = getattr(response, "server_side_encryption", None)
                checksum = getattr(response, "hash_crc64_ecma", None)

                return ObjectMetadata(
                    content_length=response.content_length,
                    content_type=getattr(response, "content_type", None),
                    etag=response.etag.strip('"')
                    if response.etag
                    else "",  # Remove quotes from ETag
                    last_modified=last_modified,
                    metadata=user_metadata,
                    storage_class=storage_class,
                    version_id=version_id,
                    encryption=encryption,
                    checksum=checksum,
                    cache_control=getattr(response, "cache_control", None),
                    content_disposition=getattr(response, "content_disposition", None),
                    content_encoding=getattr(response, "content_encoding", None),
                    content_language=getattr(response, "content_language", None),
                    expires=getattr(response, "expires", None),
                )

            except (TosClientError, TosServerError) as e:
                if "NoSuchKey" in str(e) or "404" in str(e):
                    raise FileNotFoundError(f"Object not found: {key}")
                raise ValueError(f"Failed to get object metadata {key}: {e}")

        return await asyncio.get_event_loop().run_in_executor(None, _head_object)

    async def create_multipart_upload(
        self,
        key: str,
        content_type: Optional[str] = None,
        metadata: Optional[dict[str, str]] = None,
    ) -> str:
        """Create a multipart upload session."""

        def _create_multipart_upload():
            kwargs = {
                "bucket": self.bucket_name,
                "key": key,
            }

            if content_type:
                kwargs["content_type"] = content_type

            if metadata:
                kwargs["meta"] = metadata  # type: ignore

            try:
                response = self.client.create_multipart_upload(**kwargs)
                return response.upload_id
            except (TosClientError, TosServerError) as e:
                raise ValueError(f"Failed to create multipart upload for {key}: {e}")

        return await asyncio.get_event_loop().run_in_executor(
            None, _create_multipart_upload
        )

    async def upload_part(
        self,
        key: str,
        upload_id: str,
        part_number: int,
        data: Union[str, bytes, BinaryIO, TextIO, Reader],
    ) -> str:
        """Upload a part in a multipart upload."""

        # Unify all data types using Reader wrapper
        reader = self._wrap_data(data)

        def _upload_part():
            try:
                part_response = self.client.upload_part(
                    bucket=self.bucket_name,
                    key=key,
                    part_number=part_number,
                    upload_id=upload_id,
                    content=reader,
                )
                return part_response.etag
            except (TosClientError, TosServerError) as e:
                raise ValueError(f"Failed to upload part {part_number} for {key}: {e}")

        try:
            return await asyncio.get_event_loop().run_in_executor(None, _upload_part)
        finally:
            # Close the reader if we created it
            if not isinstance(data, Reader):
                reader.close()

    async def complete_multipart_upload(
        self,
        key: str,
        upload_id: str,
        parts: list[dict[str, Union[str, int]]],
    ) -> None:
        """Complete a multipart upload."""

        def _complete_multipart_upload():
            # Convert parts to TOS format with objects
            tos_parts = []
            for part in parts:
                tos_parts.append(
                    TOSPart(
                        part_number=part["part_number"],
                        etag=part["etag"],
                    )
                )

            try:
                self.client.complete_multipart_upload(
                    bucket=self.bucket_name,
                    key=key,
                    upload_id=upload_id,
                    parts=tos_parts,
                )
            except (TosClientError, TosServerError) as e:
                raise ValueError(f"Failed to complete multipart upload for {key}: {e}")

        await asyncio.get_event_loop().run_in_executor(None, _complete_multipart_upload)

    async def abort_multipart_upload(
        self,
        key: str,
        upload_id: str,
    ) -> None:
        """Abort a multipart upload."""

        def _abort_multipart_upload():
            try:
                self.client.abort_multipart_upload(
                    bucket=self.bucket_name,
                    key=key,
                    upload_id=upload_id,
                )
            except (TosClientError, TosServerError) as e:
                raise ValueError(f"Failed to abort multipart upload for {key}: {e}")

        await asyncio.get_event_loop().run_in_executor(None, _abort_multipart_upload)

    async def copy_object(self, source_key: str, dest_key: str) -> None:
        """Copy an object within TOS."""

        def _copy_object():
            try:
                self.client.copy_object(
                    bucket=self.bucket_name,
                    key=dest_key,
                    src_bucket=self.bucket_name,
                    src_key=source_key,
                )
            except (TosClientError, TosServerError) as e:
                if "NoSuchKey" in str(e) or "404" in str(e):
                    raise FileNotFoundError(f"Source object not found: {source_key}")
                raise ValueError(
                    f"Failed to copy object {source_key} to {dest_key}: {e}"
                )

        await asyncio.get_event_loop().run_in_executor(None, _copy_object)

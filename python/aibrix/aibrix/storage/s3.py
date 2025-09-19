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
from io import BytesIO, TextIOBase
from typing import BinaryIO, Optional, TextIO, Union

import boto3
from botocore.config import Config
from botocore.exceptions import ClientError

from aibrix.storage.base import BaseStorage, StorageConfig
from aibrix.storage.utils import ObjectMetadata

from aibrix.storage.reader import Reader


class S3Storage(BaseStorage):
    """AWS S3 storage implementation with multipart upload and range get support."""

    def __init__(
        self,
        bucket_name: str,
        region_name: Optional[str] = None,
        endpoint_url: Optional[str] = None,
        aws_access_key_id: Optional[str] = None,
        aws_secret_access_key: Optional[str] = None,
        config: Optional[StorageConfig] = None,
    ):
        super().__init__(config)
        self.bucket_name = bucket_name

        # Configure client with connection pooling
        client_config = Config(
            region_name=region_name,
            max_pool_connections=max(self.config.max_concurrency, 10),
            retries={"max_attempts": self.config.max_retries},
        )

        session = boto3.Session(
            aws_access_key_id=aws_access_key_id,
            aws_secret_access_key=aws_secret_access_key,
        )

        self.client = session.client(
            "s3",
            endpoint_url=endpoint_url,
            config=client_config,
        )

        # Validate bucket exists
        try:
            self.client.head_bucket(Bucket=bucket_name)
        except ClientError as e:
            raise ValueError(f"Bucket {bucket_name} not accessible: {e}")

    async def put_object(
        self,
        key: str,
        data: Union[bytes, str, BinaryIO, TextIO, Reader],
        content_type: Optional[str] = None,
        metadata: Optional[dict[str, str]] = None,
    ) -> None:
        """Put an object to S3."""
        # Unify all data types using Reader wrapper
        reader = self._wrap_s3_data(data)

        # Check if we should use multipart upload
        try:
            if isinstance(reader, Reader):
                size = reader.get_size()
            else:
                size = len(reader)
            if size >= self.config.multipart_threshold:
                await self.multipart_upload(key, reader, content_type, metadata)
                return
        except (OSError, IOError, ValueError):
            # Can't determine size, give up multipart upload
            pass

        # Prepare kwargs
        kwargs = {
            "Bucket": self.bucket_name,
            "Key": key,
            "Body": reader,
        }

        if content_type:
            kwargs["ContentType"] = content_type

        if metadata:
            kwargs["Metadata"] = metadata  # type: ignore

        # Execute in thread pool
        try:
            await asyncio.get_event_loop().run_in_executor(
                None, lambda: self.client.put_object(**kwargs)
            )
        finally:
            # Close the reader if we created it
            if isinstance(reader, Reader) and not isinstance(data, Reader):
                reader.close()

    async def get_object(
        self,
        key: str,
        range_start: Optional[int] = None,
        range_end: Optional[int] = None,
    ) -> bytes:
        """Get an object from S3 with optional range support."""
        kwargs = {
            "Bucket": self.bucket_name,
            "Key": key,
        }

        if range_start is not None:
            if range_end is not None:
                kwargs["Range"] = f"bytes={range_start}-{range_end}"
            else:
                kwargs["Range"] = f"bytes={range_start}-"

        def _get_object():
            try:
                response = self.client.get_object(**kwargs)
                return response["Body"].read()
            except ClientError as e:
                if e.response["Error"]["Code"] == "NoSuchKey":
                    raise FileNotFoundError(f"Object not found: {key}")
                raise

        return await asyncio.get_event_loop().run_in_executor(None, _get_object)

    async def delete_object(self, key: str) -> None:
        """Delete an object from S3."""
        await asyncio.get_event_loop().run_in_executor(
            None, lambda: self.client.delete_object(Bucket=self.bucket_name, Key=key)
        )

    async def list_objects(
        self,
        prefix: str = "",
        delimiter: Optional[str] = None,
        limit: Optional[int] = None,
        continuation_token: Optional[str] = None,
    ) -> tuple[list[str], Optional[str]]:
        """List objects with given prefix using native S3 continuation tokens."""

        def _list_objects():
            kwargs = {
                "Bucket": self.bucket_name,
                "Prefix": prefix,
            }

            if delimiter:
                kwargs["Delimiter"] = delimiter

            # Use native S3 continuation token for pagination
            if continuation_token:
                kwargs["ContinuationToken"] = continuation_token

            # Set MaxKeys for limit (S3 native pagination)
            if limit is not None:
                kwargs["MaxKeys"] = limit

            objects = []

            # Make single request with continuation token (no paginator needed)
            response = self.client.list_objects_v2(**kwargs)

            # Add files
            for obj in response.get("Contents", []):
                objects.append(obj["Key"])

            # Add "directories" (common prefixes) if using delimiter
            if delimiter:
                for prefix_info in response.get("CommonPrefixes", []):
                    objects.append(prefix_info["Prefix"])

            # Get next continuation token from response
            next_token = response.get("NextContinuationToken")

            return objects, next_token

        return await asyncio.get_event_loop().run_in_executor(None, _list_objects)

    async def object_exists(self, key: str) -> bool:
        """Check if object exists in S3."""

        def _head_object():
            try:
                self.client.head_object(Bucket=self.bucket_name, Key=key)
                return True
            except ClientError as e:
                if e.response["Error"]["Code"] == "404":
                    return False
                raise

        return await asyncio.get_event_loop().run_in_executor(None, _head_object)

    async def get_object_size(self, key: str) -> int:
        """Get object size in bytes."""

        def _head_object():
            try:
                response = self.client.head_object(Bucket=self.bucket_name, Key=key)
                return response["ContentLength"]
            except ClientError as e:
                if e.response["Error"]["Code"] == "404":
                    raise FileNotFoundError(f"Object not found: {key}")
                raise

        return await asyncio.get_event_loop().run_in_executor(None, _head_object)

    async def head_object(self, key: str) -> ObjectMetadata:
        """Get object metadata without downloading the object content."""

        def _head_object():
            try:
                response = self.client.head_object(Bucket=self.bucket_name, Key=key)

                # Parse last modified time
                last_modified = response.get("LastModified")
                if last_modified and hasattr(last_modified, "replace"):
                    # Convert AWS datetime to naive datetime (remove timezone info)
                    last_modified = last_modified.replace(tzinfo=None)

                # Extract user metadata (remove 'x-amz-meta-' prefix)
                user_metadata = {}
                if "Metadata" in response:
                    user_metadata = response["Metadata"]

                return ObjectMetadata(
                    content_length=response["ContentLength"],
                    content_type=response.get("ContentType"),
                    etag=response["ETag"].strip('"'),  # Remove quotes from ETag
                    last_modified=last_modified,
                    metadata=user_metadata,
                    storage_class=response.get("StorageClass"),
                    version_id=response.get("VersionId"),
                    encryption=response.get("ServerSideEncryption"),
                    checksum=response.get("ChecksumSHA256"),
                    cache_control=response.get("CacheControl"),
                    content_disposition=response.get("ContentDisposition"),
                    content_encoding=response.get("ContentEncoding"),
                    content_language=response.get("ContentLanguage"),
                    expires=response.get("Expires"),
                )

            except ClientError as e:
                if e.response["Error"]["Code"] == "404":
                    raise FileNotFoundError(f"Object not found: {key}")
                raise

        return await asyncio.get_event_loop().run_in_executor(None, _head_object)

    def is_native_multipart_supported(self) -> bool:
        """Check if native multipart upload is supported.

        Returns:
            True for S3 Storage
        """
        return True

    async def _native_create_multipart_upload(
        self,
        key: str,
        content_type: Optional[str] = None,
        metadata: Optional[dict[str, str]] = None,
    ) -> str:
        """Create a multipart upload session."""

        def _create_multipart_upload():
            kwargs = {
                "Bucket": self.bucket_name,
                "Key": key,
            }

            if content_type:
                kwargs["ContentType"] = content_type

            if metadata:
                kwargs["Metadata"] = metadata  # type: ignore

            response = self.client.create_multipart_upload(**kwargs)
            return response["UploadId"]

        return await asyncio.get_event_loop().run_in_executor(
            None, _create_multipart_upload
        )

    async def _native_upload_part(
        self,
        key: str,
        upload_id: str,
        part_number: int,
        data: Union[str, bytes, BinaryIO, TextIO, Reader],
    ) -> str:
        """Upload a part in a multipart upload."""

        # Unify all data types using Reader wrapper
        reader = self._wrap_s3_data(data)

        def _upload_part():
            part_response = self.client.upload_part(
                Bucket=self.bucket_name,
                Key=key,
                PartNumber=part_number,
                UploadId=upload_id,
                Body=reader,
            )
            return part_response["ETag"]

        try:
            return await asyncio.get_event_loop().run_in_executor(None, _upload_part)
        finally:
            # Close the reader if we created it
            if isinstance(reader, Reader) and not isinstance(data, Reader):
                reader.close()

    async def _native_complete_multipart_upload(
        self,
        key: str,
        upload_id: str,
        parts: list[dict[str, Union[str, int]]],
    ) -> None:
        """Complete a multipart upload."""

        def _complete_multipart_upload():
            # Convert parts to S3 format
            s3_parts = []
            for part in parts:
                s3_parts.append(
                    {
                        "ETag": part["etag"],
                        "PartNumber": part["part_number"],
                    }
                )

            self.client.complete_multipart_upload(
                Bucket=self.bucket_name,
                Key=key,
                UploadId=upload_id,
                MultipartUpload={"Parts": s3_parts},
            )

        await asyncio.get_event_loop().run_in_executor(None, _complete_multipart_upload)

    async def _native_abort_multipart_upload(
        self,
        key: str,
        upload_id: str,
    ) -> None:
        """Abort a multipart upload."""

        def _abort_multipart_upload():
            self.client.abort_multipart_upload(
                Bucket=self.bucket_name,
                Key=key,
                UploadId=upload_id,
            )

        await asyncio.get_event_loop().run_in_executor(None, _abort_multipart_upload)

    async def copy_object(self, source_key: str, dest_key: str) -> None:
        """Copy an object within S3."""
        copy_source = {
            "Bucket": self.bucket_name,
            "Key": source_key,
        }

        try:
            await asyncio.get_event_loop().run_in_executor(
                None,
                lambda: self.client.copy_object(
                    CopySource=copy_source,
                    Bucket=self.bucket_name,
                    Key=dest_key,
                ),
            )
        except self.client.exceptions.NoSuchKey:
            raise FileNotFoundError(f"Source object not found: {source_key}")

    def _wrap_s3_data(
        self, data: Union[bytes, str, BinaryIO, TextIO, Reader]
    ) -> Union[bytes, Reader]:
        """Wrap data in Reader if necessary."""
        # Wrap non-Reader objects in Reader for consistent handling
        if isinstance(data, str):
            return data.encode("utf-8")
        elif isinstance(data, TextIOBase):
            reader = Reader(data)
            ret = reader.read_all()
            reader.close()
            return ret
        elif isinstance(data, bytes):
            return Reader(BytesIO(data))
        elif not isinstance(data, Reader):
            # Assume it's a file-like object (BinaryIO, TextIO, etc.)
            return Reader(data)

        return data

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

import os
import tempfile
from pathlib import Path
from typing import Any, Iterator, Union

import pytest

from aibrix.storage import LocalStorage, S3Storage, TOSStorage
from aibrix.storage.base import StorageConfig
from aibrix.storage.base2 import BaseStorage2
from aibrix.storage.reader import Reader
from aibrix.storage.types import StorageType
from aibrix.storage.utils import ObjectMetadata


class NativeMultipartTestStorage(BaseStorage2):
    """In-memory storage double for the alternative buffered aggregation path."""

    def __init__(
        self,
        multipart_threshold: int = 8,
        fail_native_complete: bool = False,
        strict_multipart_min_part_size: bool | None = None,
    ) -> None:
        super().__init__(
            StorageConfig(
                multipart_threshold=multipart_threshold,
                strict_multipart_min_part_size=strict_multipart_min_part_size,
            )
        )
        self.fail_native_complete = fail_native_complete
        self.objects: dict[str, bytes] = {}
        self.object_metadata: dict[str, ObjectMetadata] = {}
        self.put_object_calls: list[str] = []
        self.delete_object_calls: list[str] = []
        self.list_objects_calls: list[str] = []
        self.native_create_calls: list[dict[str, object]] = []
        self.native_upload_part_calls: list[dict[str, object]] = []
        self.native_complete_calls: list[dict[str, object]] = []
        self.native_abort_calls: list[dict[str, str]] = []
        self.native_uploads: dict[str, dict[str, object]] = {}
        self._native_upload_counter = 0

    def get_type(self) -> StorageType:
        return StorageType.LOCAL

    async def put_object(
        self,
        key: str,
        data: Any,
        content_type: str | None = None,
        metadata: dict[str, str] | None = None,
        options: Any = None,
    ) -> bool:
        reader = self._wrap_data(data)
        payload = reader.read_all()
        if not isinstance(data, Reader):
            reader.close()

        self.put_object_calls.append(key)
        self.objects[key] = payload
        self.object_metadata[key] = ObjectMetadata(
            content_length=len(payload),
            content_type=content_type,
            metadata=metadata or {},
        )
        return True

    async def get_object(
        self,
        key: str,
        range_start: int | None = None,
        range_end: int | None = None,
    ) -> bytes:
        if key not in self.objects:
            raise FileNotFoundError(f"Object not found: {key}")

        data = self.objects[key]
        start = 0 if range_start is None else range_start
        end = len(data) - 1 if range_end is None else range_end
        return data[start : end + 1]

    async def delete_object(self, key: str) -> None:
        self.delete_object_calls.append(key)
        self.objects.pop(key, None)
        self.object_metadata.pop(key, None)

    async def list_objects(
        self,
        prefix: str = "",
        delimiter: str | None = None,
        limit: int | None = None,
        continuation_token: str | None = None,
        after_key: str | None = None,
    ) -> tuple[list[str], str | None]:
        self.list_objects_calls.append(prefix)
        keys = sorted(key for key in self.objects if key.startswith(prefix))
        offset = int(continuation_token) if continuation_token else 0
        page_size = 1000 if limit is None else min(limit, 1000)
        page = keys[offset : offset + page_size]
        next_offset = offset + len(page)
        return page, str(next_offset) if next_offset < len(keys) else None

    async def object_exists(self, key: str) -> bool:
        return key in self.objects

    async def get_object_size(self, key: str) -> int:
        return len(await self.get_object(key))

    async def head_object(self, key: str) -> ObjectMetadata:
        if key not in self.object_metadata:
            raise FileNotFoundError(f"Object not found: {key}")
        return self.object_metadata[key]

    def is_native_multipart_supported(self) -> bool:
        return True

    async def _native_create_multipart_upload(
        self,
        key: str,
        content_type: str | None = None,
        metadata: dict[str, str] | None = None,
    ) -> str:
        self._native_upload_counter += 1
        upload_id = f"native-{self._native_upload_counter}"
        self.native_create_calls.append(
            {
                "key": key,
                "content_type": content_type,
                "metadata": metadata or {},
                "upload_id": upload_id,
            }
        )
        self.native_uploads[upload_id] = {
            "key": key,
            "content_type": content_type,
            "metadata": metadata or {},
            "parts": {},
        }
        return upload_id

    async def _native_upload_part(
        self,
        key: str,
        upload_id: str,
        part_number: int,
        data: Any,
    ) -> str:
        reader = self._wrap_data(data)
        payload = reader.read_all()
        if not isinstance(data, Reader):
            reader.close()

        self.native_upload_part_calls.append(
            {
                "key": key,
                "upload_id": upload_id,
                "part_number": part_number,
                "size": len(payload),
            }
        )
        upload = self.native_uploads[upload_id]
        upload_parts = upload["parts"]
        assert isinstance(upload_parts, dict)
        upload_parts[part_number] = payload
        return f"native-etag-{part_number}"

    async def _native_complete_multipart_upload(
        self,
        key: str,
        upload_id: str,
        parts: list[dict[str, Union[str, int]]],
    ) -> None:
        self.native_complete_calls.append(
            {"key": key, "upload_id": upload_id, "parts": list(parts)}
        )
        if self.fail_native_complete:
            raise ValueError("native completion failed")

        upload = self.native_uploads[upload_id]
        upload_parts = upload["parts"]
        assert isinstance(upload_parts, dict)
        final_data = b"".join(
            upload_parts[int(part["part_number"])]
            for part in sorted(parts, key=lambda item: int(item["part_number"]))
        )
        self.objects[key] = final_data
        self.object_metadata[key] = ObjectMetadata(
            content_length=len(final_data),
            content_type=upload["content_type"]
            if isinstance(upload["content_type"], str) or upload["content_type"] is None
            else None,
            metadata=upload["metadata"]
            if isinstance(upload["metadata"], dict)
            else None,
        )

    async def _native_abort_multipart_upload(self, key: str, upload_id: str) -> None:
        self.native_abort_calls.append({"key": key, "upload_id": upload_id})
        self.native_uploads.pop(upload_id, None)


@pytest.fixture
def temp_dir() -> Iterator[Path]:
    """Create a temporary directory for local storage tests."""
    with tempfile.TemporaryDirectory() as tmp_dir:
        yield Path(tmp_dir)


@pytest.fixture
def local_storage(temp_dir: Path) -> LocalStorage:
    """Create a LocalStorage instance for testing."""
    # Add test-specific configuration with reasonable multipart threshold
    return LocalStorage(str(temp_dir))


@pytest.fixture
def s3_config() -> dict:
    """S3 configuration from environment or AWS config."""
    # Check for AWS credentials
    aws_access_key_id = os.getenv("AWS_ACCESS_KEY_ID")
    aws_secret_access_key = os.getenv("AWS_SECRET_ACCESS_KEY")
    aws_region = os.getenv("AWS_DEFAULT_REGION", "us-east-1")

    # Check for test bucket
    test_bucket = os.getenv("AIBRIX_TEST_S3_BUCKET")

    # Try to get credentials from AWS config files
    aws_config_dir = Path.home() / ".aws"
    has_aws_config = (
        aws_config_dir.exists() and (aws_config_dir / "credentials").exists()
    )

    return {
        "has_credentials": bool(aws_access_key_id and aws_secret_access_key)
        or has_aws_config,
        "bucket_name": test_bucket,
        "region_name": aws_region,
        "aws_access_key_id": aws_access_key_id,
        "aws_secret_access_key": aws_secret_access_key,
    }


@pytest.fixture
def s3_storage(s3_config: dict) -> S3Storage:
    """Create an S3Storage instance for testing (if configured)."""
    if not s3_config["has_credentials"]:
        pytest.skip("S3 credentials not available")

    if not s3_config["bucket_name"]:
        pytest.skip("AIBRIX_TEST_S3_BUCKET environment variable not set")

    # Filter out None values
    kwargs = {
        k: v for k, v in s3_config.items() if v is not None and k != "has_credentials"
    }

    return S3Storage(**kwargs)


@pytest.fixture
def tos_config() -> dict:
    """TOS configuration from environment variables."""
    # TOS credentials and configuration
    tos_access_key = os.getenv("TOS_ACCESS_KEY")
    tos_secret_key = os.getenv("TOS_SECRET_KEY")
    tos_endpoint = os.getenv("TOS_ENDPOINT")
    tos_region = os.getenv("TOS_REGION")
    tos_bucket = os.getenv("AIBRIX_TEST_TOS_BUCKET")

    return {
        "has_credentials": bool(
            tos_access_key and tos_secret_key and tos_endpoint and tos_region
        ),
        "bucket_name": tos_bucket,
        "access_key": tos_access_key,
        "secret_key": tos_secret_key,
        "endpoint": tos_endpoint,
        "region": tos_region,
    }


@pytest.fixture
def tos_storage(tos_config: dict) -> TOSStorage:
    """Create a TOSStorage instance for testing (if configured)."""
    if not tos_config["has_credentials"]:
        pytest.skip("TOS credentials not available")

    if not tos_config["bucket_name"]:
        pytest.skip("AIBRIX_TEST_TOS_BUCKET environment variable not set")

    # Filter out None values and has_credentials flag
    kwargs = {
        k: v for k, v in tos_config.items() if v is not None and k != "has_credentials"
    }

    return TOSStorage(**kwargs)

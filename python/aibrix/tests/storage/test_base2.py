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

"""Tests for the alternative BaseStorage2 aggregation path."""

import asyncio
from types import SimpleNamespace
from typing import Any, Union

import pytest
import tos

from aibrix.storage.base import StorageConfig
from aibrix.storage.base2 import BaseStorage2
from aibrix.storage.reader import Reader
from aibrix.storage.s3 import S3Storage
from aibrix.storage.tos import TOSStorage
from aibrix.storage.types import StorageType
from aibrix.storage.utils import ObjectMetadata


class FakeS3Client:
    def __init__(self, responses: list[dict] | None = None) -> None:
        self.responses = responses or [{}]
        self.calls: list[dict] = []

    def delete_objects(self, **kwargs):
        self.calls.append(kwargs)
        return self.responses.pop(0)


class FakeTOSClient:
    def __init__(self, responses: list[object] | None = None) -> None:
        self.responses = responses or [SimpleNamespace(error=[])]
        self.calls: list[dict] = []

    def delete_multi_objects(self, **kwargs):
        self.calls.append(kwargs)
        return self.responses.pop(0)


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
        if limit is not None:
            keys = keys[:limit]
        return keys, None

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


class DelayedMultipartTestStorage(NativeMultipartTestStorage):
    """Storage double that delays staged part reads to expose concurrency."""

    def __init__(
        self, multipart_threshold: int = 8, max_session_concurrency: int = 3
    ) -> None:
        super().__init__(multipart_threshold=multipart_threshold)
        self.config.max_session_concurrency = max_session_concurrency
        self.active_part_gets = 0
        self.max_parallel_part_gets = 0

    async def get_object(
        self,
        key: str,
        range_start: int | None = None,
        range_end: int | None = None,
    ) -> bytes:
        is_staged_part = ".multipart/" in key and "/part_" in key
        if is_staged_part:
            self.active_part_gets += 1
            self.max_parallel_part_gets = max(
                self.max_parallel_part_gets, self.active_part_gets
            )
            await asyncio.sleep(0.01)
        try:
            return await super().get_object(key, range_start, range_end)
        finally:
            if is_staged_part:
                self.active_part_gets -= 1


class TestBaseStorage2:
    @pytest.mark.asyncio
    async def test_small_parts_completion_uses_native_buffered_aggregation(self):
        storage = NativeMultipartTestStorage(multipart_threshold=8)
        key = "test/native_small_parts.txt"

        upload_id = await storage.create_multipart_upload(
            key,
            content_type="text/plain",
            metadata={"source": "batch"},
            small_parts=True,
        )

        parts: list[dict[str, Union[str, int]]] = []
        for part_number, payload in enumerate((b"abc", b"def", b"ghi", b"jk"), start=1):
            etag = await storage.upload_part(key, upload_id, part_number, payload)
            parts.append({"part_number": part_number, "etag": etag})

        await storage.complete_multipart_upload(key, upload_id, parts)

        assert await storage.get_object(key) == b"abcdefghijk"
        assert key not in storage.put_object_calls
        assert len(storage.native_create_calls) == 1
        assert [call["size"] for call in storage.native_upload_part_calls] == [8, 3]

        final_metadata = await storage.head_object(key)
        assert final_metadata.content_type == "text/plain"
        assert final_metadata.metadata == {"source": "batch"}

        multipart_prefix = storage._multipart_upload_key(upload_id, "")
        assert storage.list_objects_calls == [multipart_prefix]
        assert not [
            obj_key
            for obj_key in storage.objects
            if obj_key.startswith(multipart_prefix)
        ]
        assert storage.delete_object_calls == [
            storage._multipart_upload_key(upload_id),
            storage._multipart_upload_part_key(upload_id, 1),
            storage._multipart_upload_part_key(upload_id, 2),
            storage._multipart_upload_part_key(upload_id, 3),
            storage._multipart_upload_part_key(upload_id, 4),
        ]

    @pytest.mark.asyncio
    async def test_small_parts_native_completion_failure_preserves_staged_parts(self):
        storage = NativeMultipartTestStorage(
            multipart_threshold=8, fail_native_complete=True
        )
        key = "test/native_small_parts_failure.txt"

        upload_id = await storage.create_multipart_upload(
            key,
            content_type="text/plain",
            metadata={"source": "batch"},
            small_parts=True,
        )

        parts: list[dict[str, Union[str, int]]] = []
        for part_number, payload in enumerate((b"abc", b"def", b"ghi", b"jk"), start=1):
            etag = await storage.upload_part(key, upload_id, part_number, payload)
            parts.append({"part_number": part_number, "etag": etag})

        with pytest.raises(ValueError, match="native completion failed"):
            await storage.complete_multipart_upload(key, upload_id, parts)

        assert not await storage.object_exists(key)
        assert storage.native_abort_calls == [
            {
                "key": key,
                "upload_id": storage.native_create_calls[0]["upload_id"],
            }
        ]
        assert storage.delete_object_calls == []
        assert await storage.object_exists(storage._multipart_upload_key(upload_id))
        assert await storage.object_exists(
            storage._multipart_upload_part_key(upload_id, 1)
        )
        assert await storage.object_exists(
            storage._multipart_upload_part_key(upload_id, 2)
        )
        assert await storage.object_exists(
            storage._multipart_upload_part_key(upload_id, 3)
        )
        assert await storage.object_exists(
            storage._multipart_upload_part_key(upload_id, 4)
        )

    @pytest.mark.asyncio
    async def test_small_parts_completion_prefetches_staged_part_reads(self):
        storage = DelayedMultipartTestStorage(
            multipart_threshold=64, max_session_concurrency=3
        )
        key = "test/native_small_parts_prefetch.txt"

        upload_id = await storage.create_multipart_upload(
            key,
            content_type="text/plain",
            metadata={"source": "batch"},
            small_parts=True,
        )

        parts: list[dict[str, Union[str, int]]] = []
        for part_number, payload in enumerate((b"ab", b"cd", b"ef", b"gh"), start=1):
            etag = await storage.upload_part(key, upload_id, part_number, payload)
            parts.append({"part_number": part_number, "etag": etag})

        await storage.complete_multipart_upload(key, upload_id, parts)

        assert await storage.get_object(key) == b"abcdefgh"
        assert storage.max_parallel_part_gets >= 2
        assert storage.max_parallel_part_gets <= storage.config.max_session_concurrency

    @pytest.mark.asyncio
    async def test_strict_small_parts_completion_avoids_small_final_native_part(self):
        storage = NativeMultipartTestStorage(
            multipart_threshold=8, strict_multipart_min_part_size=True
        )
        key = "test/native_small_parts_strict.txt"

        upload_id = await storage.create_multipart_upload(
            key,
            content_type="text/plain",
            metadata={"source": "batch"},
            small_parts=True,
        )

        parts: list[dict[str, Union[str, int]]] = []
        for part_number, payload in enumerate(
            (b"abc", b"def", b"ghi", b"jklmno", b"pq"),
            start=1,
        ):
            etag = await storage.upload_part(key, upload_id, part_number, payload)
            parts.append({"part_number": part_number, "etag": etag})

        await storage.complete_multipart_upload(key, upload_id, parts)

        assert await storage.get_object(key) == b"abcdefghijklmnopq"
        assert [call["size"] for call in storage.native_upload_part_calls] == [8, 9]
        assert key not in storage.put_object_calls

    @pytest.mark.asyncio
    async def test_strict_small_parts_completion_falls_back_to_put_object(self):
        storage = NativeMultipartTestStorage(
            multipart_threshold=8, strict_multipart_min_part_size=True
        )
        key = "test/native_small_parts_strict_fallback.txt"

        upload_id = await storage.create_multipart_upload(
            key,
            content_type="text/plain",
            metadata={"source": "batch"},
            small_parts=True,
        )

        parts: list[dict[str, Union[str, int]]] = []
        for part_number, payload in enumerate((b"ab", b"cd", b"efg"), start=1):
            etag = await storage.upload_part(key, upload_id, part_number, payload)
            parts.append({"part_number": part_number, "etag": etag})

        await storage.complete_multipart_upload(key, upload_id, parts)

        assert await storage.get_object(key) == b"abcdefg"
        assert storage.put_object_calls[-1] == key
        assert storage.native_create_calls == []
        assert storage.native_upload_part_calls == []
        assert storage.native_complete_calls == []
        assert storage.native_abort_calls == []


class TestTOSStorageConfig:
    def test_tos_storage_enables_strict_min_part_size_by_default(self, monkeypatch):
        mock_client = object()
        monkeypatch.setattr(tos, "TosClientV2", lambda *args: mock_client)

        storage = TOSStorage(
            bucket_name="bucket",
            access_key="ak",
            secret_key="sk",
            endpoint="https://tos.example.com",
            region="cn-beijing",
        )

        assert storage.config.strict_multipart_min_part_size is True

    def test_tos_storage_preserves_explicit_strict_override(self, monkeypatch):
        mock_client = object()
        monkeypatch.setattr(tos, "TosClientV2", lambda *args: mock_client)

        storage = TOSStorage(
            bucket_name="bucket",
            access_key="ak",
            secret_key="sk",
            endpoint="https://tos.example.com",
            region="cn-beijing",
            config=StorageConfig(strict_multipart_min_part_size=False),
        )

        assert storage.config.strict_multipart_min_part_size is False


class TestBackendBulkDelete:
    @pytest.mark.asyncio
    async def test_s3_delete_objects_uses_native_batch_request(self):
        storage = object.__new__(S3Storage)
        storage.config = StorageConfig()
        storage.bucket_name = "bucket"
        storage.client = FakeS3Client()

        await storage.delete_objects(["a", "b"])

        assert storage.client.calls == [
            {
                "Bucket": "bucket",
                "Delete": {
                    "Objects": [{"Key": "a"}, {"Key": "b"}],
                    "Quiet": False,
                },
            }
        ]

    @pytest.mark.asyncio
    async def test_s3_delete_objects_raises_on_batch_errors(self):
        storage = object.__new__(S3Storage)
        storage.config = StorageConfig()
        storage.bucket_name = "bucket"
        storage.client = FakeS3Client(
            responses=[{"Errors": [{"Key": "b", "Code": "AccessDenied"}]}]
        )

        with pytest.raises(ValueError, match="Failed to delete S3 objects"):
            await storage.delete_objects(["a", "b"])

    @pytest.mark.asyncio
    async def test_tos_delete_objects_uses_native_batch_request(self):
        storage = object.__new__(TOSStorage)
        storage.config = StorageConfig()
        storage.bucket_name = "bucket"
        storage.client = FakeTOSClient()

        await storage.delete_objects(["a", "b"])

        assert len(storage.client.calls) == 1
        call = storage.client.calls[0]
        assert call["bucket"] == "bucket"
        assert call["quiet"] is False
        assert [obj.key for obj in call["objects"]] == ["a", "b"]
        assert all(
            isinstance(obj, tos.models2.ObjectTobeDeleted) for obj in call["objects"]
        )

    @pytest.mark.asyncio
    async def test_tos_delete_objects_raises_on_batch_errors(self):
        storage = object.__new__(TOSStorage)
        storage.config = StorageConfig()
        storage.bucket_name = "bucket"
        storage.client = FakeTOSClient(
            responses=[
                SimpleNamespace(
                    error=[
                        tos.models2.DeleteError(
                            key="b",
                            version_id=None,
                            code="AccessDenied",
                            message="denied",
                        )
                    ]
                )
            ]
        )

        with pytest.raises(ValueError, match="Failed to delete TOS objects"):
            await storage.delete_objects(["a", "b"])

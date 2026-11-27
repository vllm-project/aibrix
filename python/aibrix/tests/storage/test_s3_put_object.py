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

"""Tests for S3Storage.put_object multipart error handling."""

import io
from unittest.mock import patch

import pytest
from botocore.exceptions import ReadTimeoutError

from aibrix.storage.base import StorageConfig
from aibrix.storage.reader import Reader
from aibrix.storage.s3 import S3Storage


class FakeMultipartS3Client:
    """Records S3 calls; ``upload_part`` times out on ``fail_part``."""

    def __init__(self, fail_part: int | None) -> None:
        self.fail_part = fail_part
        self.calls: list[str] = []
        self.put_bodies: list[bytes] = []

    def create_multipart_upload(self, **kwargs):
        self.calls.append("create_multipart_upload")
        return {"UploadId": "upload-1"}

    def upload_part(self, **kwargs):
        self.calls.append(f"upload_part:{kwargs['PartNumber']}")
        if kwargs["PartNumber"] == self.fail_part:
            raise ReadTimeoutError(endpoint_url="https://s3.example.com")
        return {"ETag": f'"etag-{kwargs["PartNumber"]}"'}

    def complete_multipart_upload(self, **kwargs):
        self.calls.append("complete_multipart_upload")

    def abort_multipart_upload(self, **kwargs):
        self.calls.append("abort_multipart_upload")

    def put_object(self, **kwargs):
        self.calls.append("put_object")
        self.put_bodies.append(kwargs["Body"])


@pytest.mark.asyncio
async def test_s3_put_object_raises_when_a_multipart_part_fails():
    storage = object.__new__(S3Storage)
    storage.config = StorageConfig(multipart_threshold=8)
    storage.bucket_name = "bucket"
    storage.client = FakeMultipartS3Client(fail_part=2)

    # botocore's ReadTimeoutError is an OSError.
    with pytest.raises(ReadTimeoutError):
        await storage.put_object("out.jsonl", b"A" * 8 + b"B" * 8 + b"C" * 3)

    assert "abort_multipart_upload" in storage.client.calls
    # The rest of the data must not be stored as the whole object.
    assert "put_object" not in storage.client.calls


@pytest.mark.asyncio
@pytest.mark.parametrize("fail_part", [2, None])
async def test_s3_put_object_closes_its_reader_after_a_multipart_upload(
    fail_part,
):
    storage = object.__new__(S3Storage)
    storage.config = StorageConfig(multipart_threshold=8)
    storage.bucket_name = "bucket"
    storage.client = FakeMultipartS3Client(fail_part=fail_part)

    # put_object() first wraps the bytes in a Reader, which it must close.
    # Later wraps are the per-part uploads.
    wrapped = []
    wrap = storage._wrap_s3_data

    def wrap_and_record(data):
        reader = wrap(data)
        wrapped.append(reader)
        return reader

    with patch.object(storage, "_wrap_s3_data", wrap_and_record):
        if fail_part is None:
            assert await storage.put_object("out.jsonl", b"A" * 19)
        else:
            with pytest.raises(ReadTimeoutError):
                await storage.put_object("out.jsonl", b"A" * 19)

    assert isinstance(wrapped[0], Reader)
    assert wrapped[0].closed


@pytest.mark.asyncio
@pytest.mark.parametrize("fail_part", [2, None])
async def test_s3_put_object_leaves_a_callers_reader_open(fail_part):
    storage = object.__new__(S3Storage)
    storage.config = StorageConfig(multipart_threshold=8)
    storage.bucket_name = "bucket"
    storage.client = FakeMultipartS3Client(fail_part=fail_part)

    reader = Reader(io.BytesIO(b"A" * 19))
    if fail_part is None:
        assert await storage.put_object("out.jsonl", reader)
    else:
        with pytest.raises(ReadTimeoutError):
            await storage.put_object("out.jsonl", reader)

    assert "create_multipart_upload" in storage.client.calls
    assert not reader.closed


@pytest.mark.asyncio
@pytest.mark.parametrize("error", [OSError, ValueError])
async def test_s3_put_object_uses_a_single_put_when_the_size_is_unknown(error):
    storage = object.__new__(S3Storage)
    storage.config = StorageConfig(multipart_threshold=8)
    storage.bucket_name = "bucket"
    storage.client = FakeMultipartS3Client(fail_part=None)

    with patch.object(Reader, "get_size", side_effect=error("size unknown")):
        assert await storage.put_object("out.jsonl", b"A" * 19)

    assert storage.client.calls == ["put_object"]
    assert storage.client.put_bodies == [b"A" * 19]

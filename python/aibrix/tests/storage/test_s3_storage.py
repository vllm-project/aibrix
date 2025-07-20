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

"""
S3Storage specific tests.

Tests functionality specific to the AWS S3 storage implementation.
These tests require S3 credentials and will be skipped if not available.
"""

import io

import pytest

from aibrix.storage import S3Storage, StorageConfig


class TestS3Storage:
    """Test S3Storage specific functionality."""

    @pytest.mark.asyncio
    async def test_s3_storage_initialization(self, s3_config: dict):
        """Test S3Storage initialization with various configurations."""
        if not s3_config["has_credentials"] or not s3_config["bucket_name"]:
            pytest.skip("S3 credentials or bucket not available")

        # Test with minimal config
        kwargs = {
            k: v
            for k, v in s3_config.items()
            if v is not None and k != "has_credentials"
        }
        storage = S3Storage(**kwargs)
        assert storage.bucket_name == s3_config["bucket_name"]

    @pytest.mark.asyncio
    async def test_multipart_upload_threshold(self, s3_storage: S3Storage):
        """Test that multipart upload is triggered for large files."""
        # Create a storage config with low multipart threshold
        config = StorageConfig(multipart_threshold=1024)  # 1KB threshold
        storage = S3Storage(
            bucket_name=s3_storage.bucket_name,
            region_name=s3_storage.client.meta.region_name,
            config=config,
        )

        key = "test/multipart.bin"
        # Create 2KB of data to trigger multipart upload
        content = b"A" * 2048
        content_io = io.BytesIO(content)

        # Store using multipart (should be triggered automatically)
        await storage.put_object(key, content_io)

        # Verify content
        result = await storage.get_object(key)
        assert result == content

        # Cleanup
        await storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_s3_specific_range_reads(self, s3_storage: S3Storage):
        """Test S3-specific range read functionality."""
        key = "test/s3_range.txt"
        content = "0123456789" * 100  # 1000 characters

        # Store content
        await s3_storage.put_object(key, content)

        # Test various S3 range patterns
        test_cases = [
            (0, 99, content[:100]),  # First 100 chars
            (100, 199, content[100:200]),  # Second 100 chars
            (900, None, content[900:]),  # Last 100 chars
            (500, 599, content[500:600]),  # Middle 100 chars
        ]

        for start, end, expected in test_cases:
            result = await s3_storage.get_object(key, start, end)
            assert result.decode("utf-8") == expected, f"Range {start}:{end} failed"

        # Cleanup
        await s3_storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_s3_metadata_handling(self, s3_storage: S3Storage):
        """Test S3 metadata storage and retrieval."""
        key = "test/with_metadata.txt"
        content = "Content with S3 metadata"
        metadata = {"author": "test-user", "version": "1.0", "purpose": "testing"}

        # Store with metadata
        await s3_storage.put_object(key, content, "text/plain", metadata)

        # Verify content (metadata is stored but not returned in get_object)
        result = await s3_storage.get_object(key)
        assert result.decode("utf-8") == content

        # Cleanup
        await s3_storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_s3_list_pagination(self, s3_storage: S3Storage):
        """Test S3 listing with many objects (pagination)."""
        # Create many objects to test pagination
        prefix = "test/pagination/"
        keys = [f"{prefix}file_{i:03d}.txt" for i in range(25)]

        # Store all objects
        for key in keys:
            await s3_storage.put_object(key, f"content of {key}")

        # List all objects with prefix
        listed = await s3_storage.list_objects(prefix)

        # Should include all created files
        for key in keys:
            assert key in listed

        # Cleanup
        for key in keys:
            await s3_storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_s3_error_handling(self, s3_storage: S3Storage):
        """Test S3-specific error handling."""
        # Test with non-existent key
        with pytest.raises(FileNotFoundError):
            await s3_storage.get_object("nonexistent/s3/key.txt")

        # Test invalid range (beyond file size)
        key = "test/small_file.txt"
        content = "small"
        await s3_storage.put_object(key, content)

        # This should not raise an error, but return available content
        result = await s3_storage.get_object(key, 0, 1000)
        assert result.decode("utf-8") == content

        # Cleanup
        await s3_storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_s3_concurrent_operations(self, s3_storage: S3Storage):
        """Test concurrent S3 operations."""
        import asyncio

        async def upload_and_verify(index: int):
            key = f"test/concurrent/file_{index}.txt"
            content = f"Concurrent content {index}"

            # Upload
            await s3_storage.put_object(key, content)

            # Verify
            result = await s3_storage.get_object(key)
            assert result.decode("utf-8") == content

            return key

        # Run multiple concurrent operations
        tasks = [upload_and_verify(i) for i in range(10)]
        keys = await asyncio.gather(*tasks)

        # Verify all files exist
        for key in keys:
            assert await s3_storage.object_exists(key)

        # Cleanup
        for key in keys:
            await s3_storage.delete_object(key)

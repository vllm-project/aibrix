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
Storage functionality tests.

These tests run against any storage implementation to verify
core functionality like put/get operations, range reads, multipart upload APIs, etc.
"""

import asyncio
import io
import os
from pathlib import Path
from typing import List, Union

import pytest

from aibrix.storage.base import BaseStorage


class TestStorageFunctionality:
    """Test storage functionality that should work for all implementations."""

    @pytest.mark.asyncio
    async def test_put_and_get_string(self, storage: BaseStorage):
        """Test storing and retrieving string data."""
        key = "test/string.txt"
        content = "Hello, World! 🌍"

        # Store string
        await storage.put_object(key, content)

        # Retrieve and verify
        result = await storage.get_object(key)
        assert result.decode("utf-8") == content

        # Cleanup
        await storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_put_and_get_text_io(self, storage: BaseStorage):
        """Test storing and retrieving string data."""
        key = "test/string.txt"
        content = "Hello, World! 🌍"

        # Store string
        data = io.StringIO(content)
        assert isinstance(data, io.TextIOBase)
        await storage.put_object(key, data)

        # Retrieve and verify
        result = await storage.get_object(key)
        assert result.decode("utf-8") == content

        # Cleanup
        await storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_put_and_get_bytes(self, storage: BaseStorage):
        """Test storing and retrieving binary data."""
        key = "test/binary.bin"
        content = b"\x00\x01\x02\xff\xfe\xfd"

        # Store bytes
        await storage.put_object(key, content)

        # Retrieve and verify
        result = await storage.get_object(key)
        assert result == content

        # Cleanup
        await storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_put_and_get_file_like(self, storage: BaseStorage):
        """Test storing and retrieving from file-like objects."""
        key = "test/filelike.txt"
        content = "File-like content"

        # Store from StringIO
        string_io = io.StringIO(content)
        await storage.put_object(key, string_io)

        # Retrieve and verify
        result = await storage.get_object(key)
        assert result.decode("utf-8") == content

        # Test with BytesIO
        key2 = "test/filelike.bin"
        content_bytes = content.encode("utf-8")
        bytes_io = io.BytesIO(content_bytes)
        await storage.put_object(key2, bytes_io)

        result2 = await storage.get_object(key2)
        assert result2 == content_bytes

        # Cleanup
        await storage.delete_object(key)
        await storage.delete_object(key2)

    @pytest.mark.asyncio
    async def test_range_get(self, storage: BaseStorage):
        """Test range reads functionality."""
        key = "test/range.txt"
        content = "0123456789ABCDEFGHIJ"

        # Store content
        await storage.put_object(key, content)

        # Test various range reads
        test_cases = [
            (0, 4, "01234"),  # Start to middle
            (5, 9, "56789"),  # Middle to middle
            (15, None, "FGHIJ"),  # Middle to end
            (None, None, content),  # Full content
        ]

        for start, end, expected in test_cases:
            result = await storage.get_object(key, start, end)
            assert result.decode("utf-8") == expected, f"Range {start}:{end} failed"

        # Cleanup
        await storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_object_exists(self, storage: BaseStorage):
        """Test object existence checking."""
        key = "test/exists.txt"

        # Should not exist initially
        assert not await storage.object_exists(key)

        # Create object
        await storage.put_object(key, "test content")

        # Should exist now
        assert await storage.object_exists(key)

        # Delete object
        await storage.delete_object(key)

        # Should not exist after deletion
        assert not await storage.object_exists(key)

    @pytest.mark.asyncio
    async def test_get_object_size(self, storage: BaseStorage):
        """Test getting object size."""
        key = "test/size.txt"
        content = "Hello, World!"
        expected_size = len(content.encode("utf-8"))

        # Store content
        await storage.put_object(key, content)

        # Check size
        size = await storage.get_object_size(key)
        assert size == expected_size

        # Cleanup
        await storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_list_objects(self, storage: BaseStorage):
        """Test object listing functionality."""
        # Create test objects
        test_objects = [
            "test/list/file1.txt",
            "test/list/file2.txt",
            "test/list/subdir/file3.txt",
            "test/other/file4.txt",
        ]

        for key in test_objects:
            await storage.put_object(key, f"content of {key}")

        # Test listing with prefix
        listed, _ = await storage.list_objects("test/list/")

        # Should include files with the prefix
        assert "test/list/file1.txt" in listed
        assert "test/list/file2.txt" in listed
        assert "test/list/subdir/file3.txt" in listed
        assert "test/other/file4.txt" not in listed

        # Test listing with delimiter
        listed_delimited, _ = await storage.list_objects("test/", "/")
        # Should include both files and "directories"
        assert len(listed_delimited) > 0

        # Cleanup
        for key in test_objects:
            await storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_copy_object(self, storage: BaseStorage):
        """Test object copying."""
        source_key = "test/source.txt"
        dest_key = "test/destination.txt"
        content = "Content to copy"

        # Create source object
        await storage.put_object(source_key, content)

        await asyncio.sleep(1)

        # Copy object
        await storage.copy_object(source_key, dest_key)

        # Verify both objects exist and have same content
        source_content = await storage.get_object(source_key)
        dest_content = await storage.get_object(dest_key)

        assert source_content == dest_content
        assert source_content.decode("utf-8") == content

        # Cleanup
        # await storage.delete_object(source_key)
        # await storage.delete_object(dest_key)

    @pytest.mark.asyncio
    async def test_readline_functionality(self, storage: BaseStorage):
        """Test readline iterator functionality."""
        key = "test/multiline.txt"
        lines = ["Line 1\n", "Line 2\n", "Line 3\n", "Line 4 no newline"]
        content = "".join(lines)

        # Store multiline content
        await storage.put_object(key, content)

        # Read lines using iterator
        read_lines: List[str] = []
        async for line in storage.readline_iter(key):
            read_lines.append(line)

        # Verify lines (note: readline_iter adds newlines for consistency)
        expected_lines = ["Line 1\n", "Line 2\n", "Line 3\n", "Line 4 no newline"]
        assert read_lines == expected_lines

        # Cleanup
        await storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_read_specific_lines(self, storage: BaseStorage):
        """Test reading specific lines from a file."""
        key = "test/numbered.txt"
        lines = [f"Line {i}\n" for i in range(10)]
        content = "".join(lines)

        # Store content
        await storage.put_object(key, content)

        # Read specific lines
        result = await storage.read_lines(key, start_line=2, num_lines=3)
        expected = ["Line 2\n", "Line 3\n", "Line 4\n"]
        assert result == expected

        # Read from middle to end
        result = await storage.read_lines(key, start_line=7)
        expected = ["Line 7\n", "Line 8\n", "Line 9\n"]
        assert result == expected

        # Cleanup
        await storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_error_handling(self, storage: BaseStorage):
        """Test error handling for various edge cases."""
        # Test getting non-existent object
        with pytest.raises(FileNotFoundError):
            await storage.get_object("nonexistent/key.txt")

        # Test getting size of non-existent object
        with pytest.raises(FileNotFoundError):
            await storage.get_object_size("nonexistent/key.txt")

        # Test copying from non-existent object
        with pytest.raises(FileNotFoundError):
            await storage.copy_object("nonexistent/source.txt", "test/dest.txt")

        # Deleting non-existent object should not raise error
        await storage.delete_object("nonexistent/key.txt")  # Should not raise

    @pytest.mark.asyncio
    async def test_metadata_handling(self, storage: BaseStorage):
        """Test storing and retrieving objects with metadata."""
        key = "test/with-metadata.txt"
        content = "Content with metadata"
        metadata = {"author": "test", "version": "1.0"}

        # Store with metadata
        await storage.put_object(key, content, "text/plain", metadata)

        # Retrieve and verify content (metadata handling is implementation-specific)
        result = await storage.get_object(key)
        assert result.decode("utf-8") == content

        # Cleanup
        await storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_head_object_basic(self, storage: BaseStorage):
        """Test head_object returns correct metadata for basic objects."""
        key = "test/head_object_basic.txt"
        content = "Hello, head_object test!"

        # Store object
        await storage.put_object(key, content)

        try:
            # Get metadata via head_object
            metadata = await storage.head_object(key)

            # Verify basic metadata
            assert metadata.content_length == len(content.encode("utf-8"))
            assert metadata.etag is not None and metadata.etag != ""
            assert metadata.last_modified is not None
            # Storage class varies by implementation (local: "STANDARD", s3: None, tos: enum)
            assert (
                metadata.storage_class is not None
                or storage.__class__.__name__ == "S3Storage"
            )

        finally:
            # Cleanup
            await storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_head_object_with_metadata(self, storage: BaseStorage):
        """Test head_object preserves custom metadata and content type."""
        key = "test/head_object_metadata.json"
        content = '{"message": "test"}'
        content_type = "application/json"
        custom_metadata = {
            "author": "test_user",
            "version": "1.0",
            "category": "test_data",
        }

        # Store object with metadata
        await storage.put_object(key, content, content_type, custom_metadata)

        try:
            # Get metadata via head_object
            metadata = await storage.head_object(key)

            # Verify content type is preserved
            assert metadata.content_type == content_type

            # Verify custom metadata is preserved
            assert metadata.metadata == custom_metadata

            # Verify other metadata fields
            assert metadata.content_length == len(content.encode("utf-8"))
            assert metadata.etag is not None
            assert metadata.last_modified is not None

        finally:
            # Cleanup
            await storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_head_object_content_type_inference(self, storage: BaseStorage):
        """Test head_object infers content type from file extension when not explicitly set."""
        test_cases = [
            ("test.json", "application/json"),
            ("test.txt", "text/plain"),
            ("test.csv", "text/csv"),
            ("test.html", "text/html"),
        ]

        for filename, expected_content_type in test_cases:
            key = f"test/head_object/{filename}"
            content = "test content"

            # Store without explicit content type
            await storage.put_object(key, content)

            try:
                # Get metadata via head_object
                metadata = await storage.head_object(key)

                # Content type inference varies by storage implementation
                # Local storage can infer from extension, S3/TOS may not
                if storage.__class__.__name__ == "LocalStorage":
                    assert (
                        metadata.content_type == expected_content_type
                    ), f"Expected {expected_content_type} for {filename}, got {metadata.content_type}"
                else:
                    # For other storage types, just verify content type is set to something
                    assert (
                        metadata.content_type is not None
                    ), f"Content type should be set for {filename}"

            finally:
                # Cleanup
                await storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_head_object_binary_files(self, storage: BaseStorage):
        """Test head_object with binary files."""
        key = "test/head_object_binary.dat"
        content = b"\x00\x01\x02\x03\xff\xfe\xfd"  # Binary data

        # Store binary object
        await storage.put_object(key, content)

        try:
            # Get metadata via head_object
            metadata = await storage.head_object(key)

            # Verify binary file metadata
            assert metadata.content_length == len(content)
            assert metadata.etag is not None
            assert metadata.last_modified is not None

        finally:
            # Cleanup
            await storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_head_object_large_file(self, storage: BaseStorage):
        """Test head_object with larger files."""
        key = "test/head_object_large.txt"
        content = "A" * 10000  # 10KB file

        # Store large object
        await storage.put_object(key, content)

        try:
            # Get metadata via head_object
            metadata = await storage.head_object(key)

            # Verify large file metadata
            assert metadata.content_length == len(content.encode("utf-8"))
            assert metadata.etag is not None
            assert metadata.last_modified is not None

        finally:
            # Cleanup
            await storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_head_object_metadata_persistence(self, storage: BaseStorage):
        """Test that metadata persists across storage operations."""
        key = "test/head_object_persistence.txt"
        content = "Persistent metadata test"
        content_type = "text/plain"
        metadata = {"persistent": "true", "test_id": "12345"}

        # Store object with metadata
        await storage.put_object(key, content, content_type, metadata)

        try:
            # Get metadata immediately
            initial_metadata = await storage.head_object(key)
            assert initial_metadata.content_type == content_type
            assert initial_metadata.metadata == metadata

            # Simulate some time passing or other operations
            await asyncio.sleep(0.1)

            # Get metadata again to verify persistence
            persistent_metadata = await storage.head_object(key)
            assert persistent_metadata.content_type == content_type
            assert persistent_metadata.metadata == metadata
            assert persistent_metadata.content_length == initial_metadata.content_length

        finally:
            # Cleanup
            await storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_head_object_error_handling(self, storage: BaseStorage):
        """Test head_object error handling for non-existent objects."""
        key = "test/non_existent_object.txt"

        # Ensure object doesn't exist
        if await storage.object_exists(key):
            await storage.delete_object(key)

        # head_object should raise FileNotFoundError for non-existent objects
        with pytest.raises(FileNotFoundError):
            await storage.head_object(key)

    @pytest.mark.asyncio
    async def test_local_storage_metadata_file_format(self, storage: BaseStorage):
        """Test that LocalStorage stores metadata in the correct format."""
        # Skip test for non-local storage
        if storage.__class__.__name__ != "LocalStorage":
            pytest.skip("This test is specific to LocalStorage")

        key = "test/metadata_format_test.txt"
        content = "Test content for metadata"
        content_type = "text/plain"
        custom_metadata = {"key1": "value1", "key2": "value2"}

        # Store object with metadata
        await storage.put_object(key, content, content_type, custom_metadata)

        try:
            # Cast to LocalStorage to access private methods
            from aibrix.storage.local import LocalStorage

            local_storage = storage
            assert isinstance(local_storage, LocalStorage)

            # Verify the metadata file exists and has correct format
            metadata_path = local_storage._get_metadata_path(key)
            assert metadata_path.exists(), "Metadata file should exist"

            # Read and verify metadata file content
            import json

            with open(metadata_path, "r") as f:
                stored_metadata = json.load(f)

            assert stored_metadata["content_type"] == content_type
            assert stored_metadata["metadata"] == custom_metadata

            # Verify head_object returns the same metadata
            obj_metadata = await storage.head_object(key)
            assert obj_metadata.content_type == content_type
            assert obj_metadata.metadata == custom_metadata

        finally:
            # Cleanup
            await storage.delete_object(key)

            # Verify metadata file is also deleted
            from aibrix.storage.local import LocalStorage

            local_storage = storage
            assert isinstance(local_storage, LocalStorage)
            metadata_path = local_storage._get_metadata_path(key)
            assert (
                not metadata_path.exists()
            ), "Metadata file should be deleted with object"

    @pytest.mark.asyncio
    async def test_multipart_apis(self, storage: BaseStorage):
        """Test the S3-like multipart APIs directly."""
        key = "test/multipart_api_test.txt"

        # Create multipart upload
        upload_id = await storage.create_multipart_upload(
            key, content_type="text/plain", metadata={"test": "multipart"}
        )
        assert isinstance(upload_id, str)
        assert len(upload_id) > 0

        # Upload parts - use larger data for S3/TOS (minimum 5MB per part except last)
        parts: list[dict[str, Union[str, int]]] = []
        from aibrix.storage.s3 import S3Storage
        from aibrix.storage.tos import TOSStorage

        # Use appropriately sized test data that meets TOS minimum requirements
        if isinstance(storage, (S3Storage, TOSStorage)):
            # TOS requires minimum 5MB per part except the last one
            test_data = [
                b"Part 1 data: " + b"A" * (5 * 1024 * 1024),  # 5MB+ per part
                b"Part 2 data: " + b"B" * (5 * 1024 * 1024),  # 5MB+ per part
                b"Part 3 final data\n",  # Last part can be smaller
            ]
        else:
            test_data = [b"Part 1 data\n", b"Part 2 data\n", b"Part 3 data\n"]

        for i, part_data in enumerate(test_data, 1):
            etag = await storage.upload_part(key, upload_id, i, part_data)
            assert isinstance(etag, str)
            assert len(etag) > 0
            parts.append({"part_number": i, "etag": etag})

        # Complete multipart upload
        await storage.complete_multipart_upload(key, upload_id, parts)

        # Verify content
        result = await storage.get_object(key)
        expected = b"".join(test_data)
        assert result == expected

        # Cleanup
        await storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_abort_multipart_upload(self, storage: BaseStorage):
        """Test aborting a multipart upload."""
        key = "test/abort_multipart.txt"

        # Create multipart upload
        upload_id = await storage.create_multipart_upload(key)

        # Upload one part
        await storage.upload_part(key, upload_id, 1, b"test data")

        # Abort the upload
        await storage.abort_multipart_upload(key, upload_id)

        # Verify the file was not created
        assert not await storage.object_exists(key)

    @pytest.mark.asyncio
    async def test_multipart_upload_integration(self, storage: BaseStorage):
        """Test that the default multipart_upload method uses the new APIs."""
        key = "test/multipart_integration.txt"
        content = "A" * 1000  # Large enough content

        from io import StringIO

        content_io = StringIO(content)

        # Use the default multipart_upload method
        await storage.multipart_upload(key, content_io, "text/plain")

        # Verify content
        result = await storage.get_object(key)
        assert result.decode("utf-8") == content

        # Cleanup
        await storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_multipart_upload_byline_strategy(self, storage: BaseStorage):
        """Test multipart upload with byline strategy."""
        key = "test/multipart_byline.txt"

        from io import StringIO

        # Use small test data for all storage types since we've configured a small multipart threshold
        lines = [f"Line {i} with some content\n" for i in range(1, 7)]
        content = "".join(lines)
        lines_per_part = 2  # 2 lines per part

        content_io = StringIO(content)

        # Upload by lines per part
        await storage.multipart_upload(
            key,
            content_io,
            "text/plain",
            byline=lines_per_part,  # Lines per part (priority 1)
        )

        # Verify content
        result = await storage.get_object(key)
        assert result.decode("utf-8") == content

        # Cleanup
        await storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_multipart_upload_bysize_strategy(self, storage: BaseStorage):
        """Test multipart upload with bysize strategy."""
        key = "test/multipart_bysize.txt"

        from io import StringIO

        # Use small test data for all storage types since we've configured a small multipart threshold
        content = "A" * 200  # 200 bytes for testing
        chunk_size = 50  # 50 bytes per part

        content_io = StringIO(content)

        # Upload by size per part
        await storage.multipart_upload(
            key,
            content_io,
            "text/plain",
            bysize=chunk_size,  # Size per part (priority 2)
        )

        # Verify content
        result = await storage.get_object(key)
        assert result.decode("utf-8") == content

        # Cleanup
        await storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_multipart_upload_parts_strategy(self, storage: BaseStorage):
        """Test multipart upload with parts strategy."""
        key = "test/multipart_parts.txt"
        content = "B" * 400  # 400 bytes

        from io import StringIO

        content_io = StringIO(content)

        # Split into 4 parts (auto-calculate chunk size)
        await storage.multipart_upload(
            key,
            content_io,
            "text/plain",
            parts=4,  # 4 parts (priority 3)
        )

        # Verify content
        result = await storage.get_object(key)
        assert result.decode("utf-8") == content

        # Cleanup
        await storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_multipart_upload_priority_order(self, storage: BaseStorage):
        """Test that multipart upload strategies follow priority order."""
        key = "test/multipart_priority.txt"

        from io import StringIO

        # Use small test data for all storage types since we've configured a small multipart threshold
        content = "Line 1\nLine 2\nLine 3\nLine 4\n"
        bysize_chunk = 10

        content_io = StringIO(content)

        # byline should override other parameters
        await storage.multipart_upload(
            key,
            content_io,
            "text/plain",
            byline=2,  # Should be used (priority 1)
            bysize=1000,  # Should be ignored
            parts=10,  # Should be ignored
        )

        # Verify content
        result = await storage.get_object(key)
        assert result.decode("utf-8") == content

        # Test bysize priority over parts
        key2 = "test/multipart_priority2.txt"
        content_io2 = StringIO(content)

        await storage.multipart_upload(
            key2,
            content_io2,
            "text/plain",
            byline=0,  # Disabled
            bysize=bysize_chunk,  # Should be used (priority 2)
            parts=10,  # Should be ignored
        )

        result2 = await storage.get_object(key2)
        assert result2.decode("utf-8") == content

        # Cleanup
        await storage.delete_object(key)
        await storage.delete_object(key2)

    @pytest.mark.asyncio
    async def test_multipart_upload_edge_cases(self, storage: BaseStorage):
        """Test multipart upload edge cases."""
        from io import StringIO

        # Test empty content
        key1 = "test/multipart_empty.txt"
        empty_io = StringIO("")

        await storage.multipart_upload(key1, empty_io, "text/plain", byline=1)
        result1 = await storage.get_object(key1)
        assert result1 == b""

        # Test single line with byline
        key2 = "test/multipart_single_line.txt"
        # Use small test data for all storage types since we've configured a small multipart threshold
        single_line = "Single line content"
        single_io = StringIO(single_line)

        await storage.multipart_upload(key2, single_io, "text/plain", byline=3)
        result2 = await storage.get_object(key2)
        assert result2.decode("utf-8") == single_line

        # Test parts=1 (should use entire file as one part)
        key3 = "test/multipart_one_part.txt"
        # Use small test data for all storage types since we've configured a small multipart threshold
        content = "This is a single part upload"
        content_io = StringIO(content)

        await storage.multipart_upload(key3, content_io, "text/plain", parts=1)
        result3 = await storage.get_object(key3)
        assert result3.decode("utf-8") == content

        # Cleanup
        await storage.delete_object(key1)
        await storage.delete_object(key2)
        await storage.delete_object(key3)


# Conditionally add S3 and TOS storage to test parameters if available
def pytest_generate_tests(metafunc):
    """Dynamically add S3 and TOS storage to tests if credentials are available."""
    if "storage" in metafunc.fixturenames:
        storage_params = ["local_storage"]

        # Check if S3 credentials are available
        aws_access_key_id = os.getenv("AWS_ACCESS_KEY_ID")
        aws_secret_access_key = os.getenv("AWS_SECRET_ACCESS_KEY")
        test_bucket = os.getenv("AIBRIX_TEST_S3_BUCKET")
        aws_config_dir = Path.home() / ".aws"
        has_aws_config = (
            aws_config_dir.exists() and (aws_config_dir / "credentials").exists()
        )

        # Add S3 storage if credentials and bucket are available
        if (
            (aws_access_key_id and aws_secret_access_key) or has_aws_config
        ) and test_bucket:
            storage_params.append("s3_storage")

        # Check if TOS credentials are available
        tos_access_key = os.getenv("TOS_ACCESS_KEY")
        tos_secret_key = os.getenv("TOS_SECRET_KEY")
        tos_endpoint = os.getenv("TOS_ENDPOINT")
        tos_region = os.getenv("TOS_REGION")
        tos_bucket = os.getenv("AIBRIX_TEST_TOS_BUCKET")

        # Add TOS storage if credentials and bucket are available
        if (
            tos_access_key
            and tos_secret_key
            and tos_endpoint
            and tos_region
            and tos_bucket
        ):
            storage_params.append("tos_storage")

        metafunc.parametrize("storage", storage_params, indirect=True)


@pytest.fixture
def storage(request) -> BaseStorage:
    """Parametrized fixture that provides different storage implementations."""
    return request.getfixturevalue(request.param)

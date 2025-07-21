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
LocalStorage specific tests.

Tests functionality specific to the local filesystem storage implementation.
"""

import os
import tempfile
from pathlib import Path

import pytest

from aibrix.storage import LocalStorage


class TestLocalStorage:
    """Test LocalStorage specific functionality."""

    @pytest.mark.asyncio
    async def test_local_storage_initialization(self):
        """Test LocalStorage initialization with different base paths."""
        with tempfile.TemporaryDirectory() as tmp_dir:
            # Test with explicit base path
            storage = LocalStorage(base_path=tmp_dir)
            assert storage.base_path == Path(tmp_dir)
            assert storage.base_path.exists()

    @pytest.mark.asyncio
    async def test_environment_variable_override(self):
        """Test that LOCAL_STORAGE_PATH environment variable is respected."""
        with tempfile.TemporaryDirectory() as tmp_dir:
            # Set environment variable
            original_value = os.environ.get("LOCAL_STORAGE_PATH")
            os.environ["LOCAL_STORAGE_PATH"] = tmp_dir

            try:
                # Create storage without base_path - should use env var
                storage = LocalStorage()
                assert str(storage.base_path) == tmp_dir
            finally:
                # Restore original environment
                if original_value is not None:
                    os.environ["LOCAL_STORAGE_PATH"] = original_value
                else:
                    os.environ.pop("LOCAL_STORAGE_PATH", None)

    @pytest.mark.asyncio
    async def test_directory_creation(self, local_storage: LocalStorage):
        """Test that directories are created automatically."""
        key = "deep/nested/path/file.txt"
        content = "Nested file content"

        # Store file in nested path
        await local_storage.put_object(key, content)

        # Verify file exists
        assert await local_storage.object_exists(key)

        # Verify directory structure was created
        full_path = local_storage._get_full_path(key)
        assert full_path.parent.exists()
        assert full_path.parent.is_dir()

        # Cleanup
        await local_storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_file_permissions(self, local_storage: LocalStorage):
        """Test that files are created with appropriate permissions."""
        key = "test/permissions.txt"
        content = "Test file permissions"

        # Store file
        await local_storage.put_object(key, content)

        # Check file exists and is readable
        full_path = local_storage._get_full_path(key)
        assert full_path.exists()
        assert full_path.is_file()
        assert os.access(full_path, os.R_OK)
        assert os.access(full_path, os.W_OK)

        # Cleanup
        await local_storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_list_with_delimiter(self, local_storage: LocalStorage):
        """Test listing with delimiter for directory-like behavior."""
        # Create test structure
        test_files = [
            "dir1/file1.txt",
            "dir1/file2.txt",
            "dir1/subdir/file3.txt",
            "dir2/file4.txt",
        ]

        for key in test_files:
            await local_storage.put_object(key, f"content of {key}")

        # List with delimiter - should show directories
        result, _ = await local_storage.list_objects("", "/")

        # Should include files at root and directories
        dir_entries = [item for item in result if item.endswith("/")]
        assert "dir1/" in dir_entries
        assert "dir2/" in dir_entries

        # List specific directory
        dir1_contents, _ = await local_storage.list_objects("dir1/", "/")
        file_entries = [item for item in dir1_contents if not item.endswith("/")]

        assert "dir1/file1.txt" in file_entries
        assert "dir1/file2.txt" in file_entries

        # Cleanup
        for key in test_files:
            await local_storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_concurrent_operations(self, local_storage: LocalStorage):
        """Test concurrent read/write operations."""
        import asyncio

        async def write_file(key: str, content: str):
            await local_storage.put_object(key, content)

        async def read_file(key: str) -> str:
            data = await local_storage.get_object(key)
            return data.decode("utf-8")

        # Write multiple files concurrently
        keys = [f"concurrent/file_{i}.txt" for i in range(10)]
        write_tasks = [write_file(key, f"content_{i}") for i, key in enumerate(keys)]
        await asyncio.gather(*write_tasks)

        # Read multiple files concurrently
        read_tasks = [read_file(key) for key in keys]
        results = await asyncio.gather(*read_tasks)

        # Verify results
        for i, result in enumerate(results):
            assert result == f"content_{i}"

        # Cleanup
        for key in keys:
            await local_storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_large_file_handling(self, local_storage: LocalStorage):
        """Test handling of larger files."""
        key = "test/large_file.bin"
        # Create 1MB of data
        chunk_size = 1024
        num_chunks = 1024
        content = b"A" * chunk_size * num_chunks

        # Store large file
        await local_storage.put_object(key, content)

        # Verify size
        size = await local_storage.get_object_size(key)
        assert size == len(content)

        # Test range reads on large file
        start_chunk = await local_storage.get_object(key, 0, chunk_size - 1)
        assert len(start_chunk) == chunk_size
        assert start_chunk == b"A" * chunk_size

        # Test end chunk
        end_start = len(content) - chunk_size
        end_chunk = await local_storage.get_object(key, end_start, None)
        assert len(end_chunk) == chunk_size
        assert end_chunk == b"A" * chunk_size

        # Cleanup
        await local_storage.delete_object(key)

    @pytest.mark.asyncio
    async def test_path_traversal_protection(self, local_storage: LocalStorage):
        """Test that path traversal attempts are handled safely."""
        # These should be treated as regular keys, not path traversal
        dangerous_keys = [
            "../etc/passwd",
            "../../sensitive/file.txt",
            "normal/../../../etc/hosts",
        ]

        for key in dangerous_keys:
            content = f"content for {key}"
            await local_storage.put_object(key, content)

            # Should be stored safely within base directory
            full_path = local_storage._get_full_path(key)
            assert (
                local_storage.base_path in full_path.parents
                or full_path == local_storage.base_path
            )

            # Should be retrievable
            result = await local_storage.get_object(key)
            assert result.decode("utf-8") == content

            # Cleanup
            await local_storage.delete_object(key)

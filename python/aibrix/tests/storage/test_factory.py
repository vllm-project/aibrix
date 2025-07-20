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
Storage factory tests.

Tests the storage factory functionality for creating different storage types.
"""

import tempfile
from pathlib import Path

import pytest

from aibrix.storage import (
    LocalStorage,
    StorageConfig,
    StorageType,
    create_storage,
)


class TestStorageFactory:
    """Test storage factory functionality."""

    def test_create_local_storage(self):
        """Test creating local storage through factory."""
        with tempfile.TemporaryDirectory() as tmp_dir:
            storage = create_storage(StorageType.LOCAL, base_path=tmp_dir)

            assert isinstance(storage, LocalStorage)
            assert storage.base_path == Path(tmp_dir)

    def test_create_local_storage_with_string(self):
        """Test creating local storage with string type."""
        with tempfile.TemporaryDirectory() as tmp_dir:
            storage = create_storage("local", base_path=tmp_dir)

            assert isinstance(storage, LocalStorage)
            assert storage.base_path == Path(tmp_dir)

    def test_create_storage_with_config(self):
        """Test creating storage with custom configuration."""
        config = StorageConfig(
            multipart_threshold=1024 * 1024,  # 1MB
            max_concurrency=5,
            range_chunksize=512 * 1024,  # 512KB
        )

        with tempfile.TemporaryDirectory() as tmp_dir:
            storage = create_storage(
                StorageType.LOCAL, config=config, base_path=tmp_dir
            )

            assert isinstance(storage, LocalStorage)
            assert storage.config.multipart_threshold == 1024 * 1024
            assert storage.config.max_concurrency == 5
            assert storage.config.range_chunksize == 512 * 1024

    def test_create_s3_storage_missing_bucket(self):
        """Test that S3 storage creation fails without bucket name."""
        with pytest.raises(ValueError, match="bucket_name is required"):
            create_storage(StorageType.S3)

    def test_create_s3_storage_with_params(self):
        """Test creating S3 storage with parameters."""
        # This will fail due to invalid credentials, but tests parameter passing
        with pytest.raises(ValueError, match="not accessible"):
            create_storage(
                StorageType.S3,
                bucket_name="test-bucket",
                region_name="us-east-1",
                aws_access_key_id="fake-key",
                aws_secret_access_key="fake-secret",
            )

    def test_create_tos_storage_missing_params(self):
        """Test that TOS storage creation fails without required parameters."""
        with pytest.raises(ValueError, match="bucket_name is required"):
            create_storage(StorageType.TOS)

        with pytest.raises(ValueError, match="access_key is required"):
            create_storage(StorageType.TOS, bucket_name="test-bucket")

        with pytest.raises(ValueError, match="secret_key is required"):
            create_storage(StorageType.TOS, bucket_name="test-bucket", access_key="key")

        with pytest.raises(ValueError, match="endpoint is required"):
            create_storage(
                StorageType.TOS,
                bucket_name="test-bucket",
                access_key="key",
                secret_key="secret",
            )

        with pytest.raises(ValueError, match="region is required"):
            create_storage(
                StorageType.TOS,
                bucket_name="test-bucket",
                access_key="key",
                secret_key="secret",
                endpoint="http://example.com",
            )

    def test_unsupported_storage_type(self):
        """Test error handling for unsupported storage types."""
        with pytest.raises(ValueError, match="Unsupported storage type"):
            create_storage("unsupported")

        with pytest.raises(ValueError, match="Unsupported storage type"):
            create_storage("invalid_type")

    def test_case_insensitive_storage_type(self):
        """Test that storage type strings are case insensitive."""
        with tempfile.TemporaryDirectory() as tmp_dir:
            # Test various case combinations
            for type_str in ["LOCAL", "local", "Local", "LOCAL"]:
                storage = create_storage(type_str, base_path=tmp_dir)
                assert isinstance(storage, LocalStorage)

    def test_storage_type_enum_vs_string(self):
        """Test that enum and string types produce equivalent results."""
        with tempfile.TemporaryDirectory() as tmp_dir:
            storage_enum = create_storage(StorageType.LOCAL, base_path=tmp_dir)
            storage_string = create_storage("local", base_path=tmp_dir)

            assert type(storage_enum) is type(storage_string)
            assert storage_enum.base_path == storage_string.base_path

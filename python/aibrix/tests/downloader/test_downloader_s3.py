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

import types
from pathlib import Path
from unittest import mock

import pytest

from aibrix.common.errors import (
    ArgNotCongiuredError,
    ModelNotFoundError,
)
from aibrix.downloader.base import get_downloader
from aibrix.downloader.s3 import S3Downloader

S3_BOTO3_MODULE = "aibrix.downloader.s3.boto3"
ENVS_MODULE = "aibrix.downloader.s3.envs"


def mock_not_exist_boto3(mock_boto3):
    mock_client = mock.Mock()
    mock_boto3.client.return_value = mock_client
    mock_client.head_bucket.side_effect = Exception("head bucket error")


def mock_exist_boto3(mock_boto3):
    mock_client = mock.Mock()
    mock_boto3.client.return_value = mock_client
    mock_client.head_bucket.return_value = mock.Mock()


env_group = mock.Mock()
env_group.DOWNLOADER_NUM_THREADS = 4
env_group.DOWNLOADER_AWS_ACCESS_KEY_ID = "AWS_ACCESS_KEY_ID"
env_group.DOWNLOADER_AWS_SECRET_ACCESS_KEY = "AWS_SECRET_ACCESS_KEY"

env_no_ak = mock.Mock()
env_no_ak.DOWNLOADER_NUM_THREADS = 4
env_no_ak.DOWNLOADER_AWS_ACCESS_KEY_ID = ""
env_no_ak.DOWNLOADER_AWS_SECRET_ACCESS_KEY = "AWS_SECRET_ACCESS_KEY"

env_no_sk = mock.Mock()
env_no_sk.DOWNLOADER_NUM_THREADS = 4
env_no_sk.DOWNLOADER_AWS_ACCESS_KEY_ID = "AWS_ACCESS_KEY_ID"
env_no_sk.DOWNLOADER_AWS_SECRET_ACCESS_KEY = ""

env_no_credentials = mock.Mock()
env_no_credentials.DOWNLOADER_NUM_THREADS = 4
env_no_credentials.DOWNLOADER_AWS_ACCESS_KEY_ID = None
env_no_credentials.DOWNLOADER_AWS_SECRET_ACCESS_KEY = None
env_no_credentials.DOWNLOADER_AWS_REGION = "us-west-2"
env_no_credentials.DOWNLOADER_AWS_ENDPOINT_URL = None


@mock.patch(ENVS_MODULE, env_group)
@mock.patch(S3_BOTO3_MODULE)
def test_get_downloader_s3(mock_boto3):
    mock_exist_boto3(mock_boto3)

    downloader = get_downloader("s3://bucket/path")
    assert isinstance(downloader, S3Downloader)


@mock.patch(ENVS_MODULE, env_group)
@mock.patch(S3_BOTO3_MODULE)
def test_get_downloader_s3_path_not_exist(mock_boto3):
    mock_not_exist_boto3(mock_boto3)

    with pytest.raises(ModelNotFoundError) as exception:
        get_downloader("s3://bucket/not_exist_path")
    assert "Model not found" in str(exception.value)


@mock.patch(ENVS_MODULE, env_group)
@mock.patch(S3_BOTO3_MODULE)
def test_get_downloader_s3_path_empty(mock_boto3):
    mock_exist_boto3(mock_boto3)

    # Bucket name and path both are empty,
    # will first assert the name
    with pytest.raises(ArgNotCongiuredError) as exception:
        get_downloader("s3://")
    assert "`bucket_name` is not configured" in str(exception.value)


@mock.patch(ENVS_MODULE, env_group)
@mock.patch(S3_BOTO3_MODULE)
def test_get_downloader_s3_path_empty_path(mock_boto3):
    mock_exist_boto3(mock_boto3)

    # bucket path is empty
    with pytest.raises(ArgNotCongiuredError) as exception:
        get_downloader("s3://bucket/")
    assert "`bucket_path` is not configured" in str(exception.value)


@mock.patch(ENVS_MODULE, env_no_ak)
@mock.patch(S3_BOTO3_MODULE)
def test_get_downloader_s3_no_ak(mock_boto3):
    mock_exist_boto3(mock_boto3)

    with pytest.raises(ArgNotCongiuredError) as exception:
        get_downloader("s3://bucket/")
    assert "`ak` is not configured" in str(exception.value)


@mock.patch(ENVS_MODULE, env_no_sk)
@mock.patch(S3_BOTO3_MODULE)
def test_get_downloader_s3_no_sk(mock_boto3):
    mock_exist_boto3(mock_boto3)

    with pytest.raises(ArgNotCongiuredError) as exception:
        get_downloader("s3://bucket/")
    assert "`sk` is not configured" in str(exception.value)


@mock.patch(ENVS_MODULE, env_no_credentials)
@mock.patch(S3_BOTO3_MODULE)
def test_get_downloader_s3_irsa_support(mock_boto3):
    """Test that S3Downloader works with IRSA when no credentials are provided."""
    mock_exist_boto3(mock_boto3)

    # This should succeed when no credentials are provided (IRSA scenario)
    downloader = get_downloader("s3://bucket/path")
    assert isinstance(downloader, S3Downloader)

    # Verify that boto3.client was called without aws_access_key_id and aws_secret_access_key
    mock_boto3.client.assert_called_once()
    call_args = mock_boto3.client.call_args
    call_kwargs = call_args[1]

    # Should not contain aws credentials but should contain region
    assert "aws_access_key_id" not in call_kwargs
    assert "aws_secret_access_key" not in call_kwargs
    assert call_kwargs.get("region_name") == "us-west-2"


@mock.patch(ENVS_MODULE, env_no_credentials)
@mock.patch(S3_BOTO3_MODULE)
def test_get_downloader_s3_irsa_auth_config(mock_boto3):
    """Test that _get_auth_config returns correct config for IRSA."""
    mock_exist_boto3(mock_boto3)

    downloader = S3Downloader("s3://bucket/path")
    auth_config = downloader._get_auth_config()

    # Should only contain region, no credentials
    assert "aws_access_key_id" not in auth_config
    assert "aws_secret_access_key" not in auth_config
    assert auth_config.get("region_name") == "us-west-2"
    assert "endpoint_url" not in auth_config  # Should not be present when None


def test_s3_atomic_write(monkeypatch, tmp_path):
    # Validate that S3 download writes to .part then renames atomically
    from aibrix.downloader import s3 as s3_mod

    class FakePaginator:
        def __init__(self, pages):
            self._pages = pages

        def paginate(self, **kwargs):
            for p in self._pages:
                yield p

    class FakeS3Client:
        def __init__(self):
            self.objects = {
                "bucket": {"path/file.txt": {"ETag": "etag", "ContentLength": 4}}
            }

        def head_bucket(self, Bucket):
            return {}

        def head_object(self, Bucket, Key):
            return self.objects[Bucket][Key]

        def get_paginator(self, name):
            assert name == "list_objects_v2"
            return FakePaginator(
                [{"Contents": [{"Key": "path/file.txt"}], "KeyCount": 1}]
            )

        def download_file(self, Bucket, Key, Filename, Config, Callback=None):
            # Write to the provided temporary file path
            Path(Filename).parent.mkdir(parents=True, exist_ok=True)
            Path(Filename).write_bytes(b"data")

    def fake_boto3_client(service_name, config, **auth):
        return FakeS3Client()

    monkeypatch.setattr(
        s3_mod, "boto3", types.SimpleNamespace(client=fake_boto3_client)
    )

    d = s3_mod.S3Downloader("s3://bucket/path/file.txt", model_name="m")
    d.download_model(local_path=str(tmp_path))

    final = tmp_path / "m" / "path" / "file.txt"
    assert final.exists()
    assert not Path(str(final) + ".part").exists()


def test_s3_recursive_download(monkeypatch, tmp_path):
    # Mock S3 client with a directory structure
    from aibrix.downloader import s3 as s3_mod

    class FakePaginator:
        def __init__(self, pages):
            self._pages = pages

        def paginate(self, **kwargs):
            for p in self._pages:
                yield p

    class FakeS3Client:
        def __init__(self):
            self.objects = {
                "bucket": {
                    "models/model1": {"ETag": "etag-dir", "ContentLength": 0},
                    "models/model1/config.json": {"ETag": "etag1", "ContentLength": 10},
                    "models/model1/weights.bin": {"ETag": "etag2", "ContentLength": 20},
                    "models/model1/subfolder/vocab.txt": {"ETag": "etag3", "ContentLength": 15}
                }
            }

        def head_bucket(self, Bucket):
            return {}

        def head_object(self, Bucket, Key):
            return self.objects[Bucket][Key]

        def get_paginator(self, name):
            assert name == "list_objects_v2"
            return FakePaginator([
                {"Contents": [
                    {"Key": "models/model1"},
                    {"Key": "models/model1/config.json"},
                    {"Key": "models/model1/weights.bin"},
                    {"Key": "models/model1/subfolder/vocab.txt"}
                ], "KeyCount": 4}
            ])

        def download_file(self, Bucket, Key, Filename, Config, Callback=None):
            # Write to the provided temporary file path
            Path(Filename).parent.mkdir(parents=True, exist_ok=True)
            Path(Filename).write_bytes(b"mock_data")

    def fake_boto3_client(service_name, config, **auth):
        return FakeS3Client()

    monkeypatch.setattr(s3_mod, "boto3", types.SimpleNamespace(client=fake_boto3_client))

    # Test recursive download
    d = s3_mod.S3Downloader("s3://bucket/models/model1", model_name="test_model")
    d.download_model(local_path=str(tmp_path))

    # Verify all files were downloaded with correct directory structure
    assert (tmp_path / "test_model" / "config.json").exists()
    assert (tmp_path / "test_model" / "weights.bin").exists()
    assert (tmp_path / "test_model" / "subfolder" / "vocab.txt").exists()
  
  
def test_s3_empty_allow_file_suffix(monkeypatch, tmp_path):
    # Mock S3 client with various file types
    from aibrix.downloader import s3 as s3_mod

    class FakePaginator:
        def __init__(self, pages):
            self._pages = pages

        def paginate(self, **kwargs):
            for p in self._pages:
                yield p

    class FakeS3Client:
        def __init__(self):
            self.objects = {
                "bucket": {
                    "models/all": {"ETag": "etag-dir", "ContentLength": 0},
                    "models/all/file1.txt": {"ETag": "etag1", "ContentLength": 10},
                    "models/all/file2.json": {"ETag": "etag2", "ContentLength": 20},
                }
            }
            self.downloaded_files = []

        def head_bucket(self, Bucket):
            return {}

        def head_object(self, Bucket, Key):
            return self.objects[Bucket][Key]

        def get_paginator(self, name):
            assert name == "list_objects_v2"
            return FakePaginator([
                {"Contents": [
                    {"Key": "models/all"},
                    {"Key": "models/all/file1.txt"},
                    {"Key": "models/all/file2.json"},
                ], "KeyCount": 3}
            ])

        def download_file(self, Bucket, Key, Filename, Config, Callback=None):
            # Record which files were downloaded
            self.downloaded_files.append(Key)
            # Write to the provided temporary file path
            Path(Filename).parent.mkdir(parents=True, exist_ok=True)
            Path(Filename).write_bytes(b"mock_data")

    def fake_boto3_client(service_name, config, **auth):
        return FakeS3Client()

    # Mock environment variables with empty allowed file suffixes
    mock_envs = mock.Mock()
    mock_envs.DOWNLOADER_ALLOW_FILE_SUFFIX = None  # Empty list means download all files
    mock_envs.DOWNLOADER_FORCE_DOWNLOAD = False
    mock_envs.DOWNLOADER_CHECK_FILE_EXIST = True
    mock_envs.DOWNLOADER_NUM_THREADS = 4
    mock_envs.DOWNLOADER_S3_MAX_IO_QUEUE = 100
    mock_envs.DOWNLOADER_S3_IO_CHUNKSIZE = 16777216
    mock_envs.DOWNLOADER_PART_THRESHOLD = 67108864
    mock_envs.DOWNLOADER_PART_CHUNKSIZE = 67108864

    monkeypatch.setattr(s3_mod, "boto3", types.SimpleNamespace(client=fake_boto3_client))
    monkeypatch.setattr(s3_mod, "envs", mock_envs)

    # Test download with empty file suffix filtering (should download all files)
    d = s3_mod.S3Downloader("s3://bucket/models/all", model_name="all_files_model")
    d.download_model(local_path=str(tmp_path))

    # Get the client instance to check which files were downloaded
    client = s3_mod.boto3.client('s3', {})
    
    # Verify all files were downloaded
    # assert "models/all/file1.txt" in client.downloaded_files
    # assert "models/all/file2.json" in client.downloaded_files
    
    # Verify all files exist in the target directory
    assert (tmp_path / "all_files_model" / "file1.txt").exists()
    assert (tmp_path / "all_files_model" / "file2.json").exists()
    
    
def test_s3_recursive_download_nested_dirs(monkeypatch, tmp_path):
    # Mock S3 client with deeply nested directories
    from aibrix.downloader import s3 as s3_mod

    class FakePaginator:
        def __init__(self, pages):
            self._pages = pages

        def paginate(self, **kwargs):
            for p in self._pages:
                yield p

    class FakeS3Client:
        def __init__(self):
            self.objects = {
                "bucket": {
                    "models/nested": {"ETag": "etag-dir", "ContentLength": 0},
                    "models/nested/level1": {"ETag": "etag-dir", "ContentLength": 0},
                    "models/nested/level1/file1.txt": {"ETag": "etag1", "ContentLength": 10},
                    "models/nested/level1/level2/file2.txt": {"ETag": "etag2", "ContentLength": 20},
                    "models/nested/level1/level2/level3/file3.txt": {"ETag": "etag3", "ContentLength": 30}
                }
            }

        def head_bucket(self, Bucket):
            return {}

        def head_object(self, Bucket, Key):
            return self.objects[Bucket][Key]

        def get_paginator(self, name):
            assert name == "list_objects_v2"
            return FakePaginator([
                {"Contents": [
                    {"Key": "models/nested/level1/file1.txt"},
                    {"Key": "models/nested/level1/level2/file2.txt"},
                    {"Key": "models/nested/level1/level2/level3/file3.txt"}
                ], "KeyCount": 3}
            ])

        def download_file(self, Bucket, Key, Filename, Config, Callback=None):
            # Write to the provided temporary file path
            Path(Filename).parent.mkdir(parents=True, exist_ok=True)
            Path(Filename).write_bytes(b"mock_data")

    def fake_boto3_client(service_name, config, **auth):
        return FakeS3Client()

    monkeypatch.setattr(s3_mod, "boto3", types.SimpleNamespace(client=fake_boto3_client))

    # Test recursive download with nested directories
    d = s3_mod.S3Downloader("s3://bucket/models/nested", model_name="nested_model")
    d.download_model(local_path=str(tmp_path))

    # Verify all files were downloaded with correct nested directory structure
    assert (tmp_path / "nested_model" / "level1" / "file1.txt").exists()
    assert (tmp_path / "nested_model" / "level1" / "level2" / "file2.txt").exists()
    assert (tmp_path / "nested_model" / "level1" / "level2" / "level3" / "file3.txt").exists()
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
)
from aibrix.uploader.base import UploadExtraConfig
from aibrix.uploader.factory import create_uploader as get_uploader
from aibrix.uploader.s3 import S3Uploader

S3_BOTO3_MODULE = "aibrix.uploader.s3.boto3"
ENVS_MODULE = "aibrix.uploader.s3.envs"


def mock_not_exist_boto3(mock_boto3):
    mock_client = mock.Mock()
    mock_boto3.client.return_value = mock_client
    mock_client.head_bucket.side_effect = Exception("head bucket error")


def mock_exist_boto3(mock_boto3):
    mock_client = mock.Mock()
    mock_boto3.client.return_value = mock_client
    mock_client.head_bucket.return_value = mock.Mock()

env_group = mock.Mock()
env_group.UPLOADER_NUM_THREADS = 4
env_group.UPLOADER_AWS_ACCESS_KEY_ID = "AWS_ACCESS_KEY_ID"
env_group.UPLOADER_AWS_SECRET_ACCESS_KEY = "AWS_SECRET_ACCESS_KEY"

env_no_ak = mock.Mock()
env_no_ak.UPLOADER_NUM_THREADS = 4
env_no_ak.UPLOADER_AWS_ACCESS_KEY_ID = ""
env_no_ak.UPLOADER_AWS_SECRET_ACCESS_KEY = "AWS_SECRET_ACCESS_KEY"

env_no_sk = mock.Mock()
env_no_sk.UPLOADER_NUM_THREADS = 4
env_no_sk.UPLOADER_AWS_ACCESS_KEY_ID = "AWS_ACCESS_KEY_ID"
env_no_sk.UPLOADER_AWS_SECRET_ACCESS_KEY = ""

env_no_credentials = mock.Mock()
env_no_credentials.UPLOADER_NUM_THREADS = 4
env_no_credentials.UPLOADER_AWS_ACCESS_KEY_ID = None
env_no_credentials.UPLOADER_AWS_SECRET_ACCESS_KEY = None
env_no_credentials.UPLOADER_AWS_REGION = "us-west-2"
env_no_credentials.UPLOADER_AWS_ENDPOINT_URL = None


@mock.patch(ENVS_MODULE, env_group)
@mock.patch(S3_BOTO3_MODULE)
def test_get_uploader_s3(mock_boto3):
    mock_exist_boto3(mock_boto3)

    uploader = get_uploader("s3://bucket/path")
    assert isinstance(uploader, S3Uploader)


@mock.patch(ENVS_MODULE, env_group)
@mock.patch(S3_BOTO3_MODULE)
def test_get_uploader_s3_path_not_exist(mock_boto3):
    mock_not_exist_boto3(mock_boto3)

    with pytest.raises(Exception) as exception:
        get_uploader("s3://bucket/not_exist_path")
    assert "head bucket error" in str(exception.value)


@mock.patch(ENVS_MODULE, env_group)
@mock.patch(S3_BOTO3_MODULE)
def test_get_uploader_s3_path_empty(mock_boto3):
    mock_exist_boto3(mock_boto3)

    # Bucket name and path both are empty,
    # will first assert the name
    with pytest.raises(ArgNotCongiuredError) as exception:
        get_uploader("s3://")
    assert "`bucket_name` is not configured" in str(exception.value)


@mock.patch(ENVS_MODULE, env_group)
@mock.patch(S3_BOTO3_MODULE)
def test_get_uploader_s3_path_empty_path(mock_boto3):
    mock_exist_boto3(mock_boto3)

    # bucket path is empty
    # Note: Unlike downloader, uploader may allow empty bucket path to upload to root
    uploader = get_uploader("s3://bucket/")
    assert isinstance(uploader, S3Uploader)
    assert uploader.bucket_name == "bucket"
    assert uploader.bucket_path == ""


@mock.patch(ENVS_MODULE, env_no_ak)
@mock.patch(S3_BOTO3_MODULE)
def test_get_uploader_s3_no_ak(mock_boto3):
    mock_exist_boto3(mock_boto3)

    with pytest.raises(ArgNotCongiuredError) as exception:
        get_uploader("s3://bucket/path")
    assert "`ak` is not configured" in str(exception.value)


@mock.patch(ENVS_MODULE, env_no_sk)
@mock.patch(S3_BOTO3_MODULE)
def test_get_uploader_s3_no_sk(mock_boto3):
    mock_exist_boto3(mock_boto3)

    with pytest.raises(ArgNotCongiuredError) as exception:
        get_uploader("s3://bucket/path")
    assert "`sk` is not configured" in str(exception.value)


@mock.patch(ENVS_MODULE, env_no_credentials)
@mock.patch(S3_BOTO3_MODULE)
def test_get_uploader_s3_irsa_support(mock_boto3):
    """Test that S3Uploader works with IRSA when no credentials are provided."""
    mock_exist_boto3(mock_boto3)

    # This should succeed when no credentials are provided (IRSA scenario)
    uploader = get_uploader("s3://bucket/path")
    assert isinstance(uploader, S3Uploader)

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
def test_get_uploader_s3_irsa_auth_config(mock_boto3):
    """Test that _get_auth_config returns correct config for IRSA."""
    mock_exist_boto3(mock_boto3)

    uploader = S3Uploader("s3://bucket/path")
    auth_config = uploader._get_auth_config()

    # Should only contain region, no credentials
    assert "aws_access_key_id" not in auth_config
    assert "aws_secret_access_key" not in auth_config
    assert auth_config.get("region_name") == "us-west-2"
    assert "endpoint_url" not in auth_config  # Should not be present when None


def test_s3_upload_file(monkeypatch, tmp_path):
    # Test that S3 upload correctly uploads a file
    from aibrix.uploader import s3 as s3_mod

    class FakeS3Client:
        def __init__(self):
            self.uploaded_files = {}

        def head_bucket(self, Bucket):
            return {}
            
        def head_object(self, Bucket, Key):
            # Simulate object not existing with proper ClientError
            from botocore.exceptions import ClientError
            error_response = {'Error': {'Code': '404'}}
            raise ClientError(error_response, 'head_object')

        def upload_file(self, Filename, Bucket, Key, Config, Callback=None):
            # Record the uploaded file
            with open(Filename, 'rb') as f:
                self.uploaded_files[(Bucket, Key)] = f.read()

    def fake_boto3_client(service_name, config, **auth):
        return FakeS3Client()

    monkeypatch.setattr(
        s3_mod, "boto3", types.SimpleNamespace(client=fake_boto3_client)
    )

    # Create a test file to upload
    test_file = tmp_path / "test.txt"
    test_file.write_text("test content")

    # Create uploader and upload the file
    uploader = s3_mod.S3Uploader("s3://bucket/path", model_name="test_model")
    uploader.upload_path(local_path=str(test_file))

    # Verify the file was uploaded
    assert ("bucket", "path/test.txt") in uploader.client.uploaded_files
    assert uploader.client.uploaded_files[("bucket", "path/test.txt")] == b"test content"


def test_s3_upload_directory(monkeypatch, tmp_path):
    # Test that S3 upload correctly uploads a directory recursively
    from aibrix.uploader import s3 as s3_mod

    class FakeS3Client:
        def __init__(self):
            self.uploaded_files = {}

        def head_bucket(self, Bucket):
            return {}
            
        def head_object(self, Bucket, Key):
            # Simulate object not existing with proper ClientError
            from botocore.exceptions import ClientError
            error_response = {'Error': {'Code': '404'}}
            raise ClientError(error_response, 'head_object')

        def upload_file(self, Filename, Bucket, Key, Config, Callback=None):
            # Record the uploaded file
            with open(Filename, 'rb') as f:
                self.uploaded_files[(Bucket, Key)] = f.read()

    def fake_boto3_client(service_name, config, **auth):
        return FakeS3Client()

    monkeypatch.setattr(
        s3_mod, "boto3", types.SimpleNamespace(client=fake_boto3_client)
    )

    # Create a test directory structure
    test_dir = tmp_path / "test_dir"
    test_dir.mkdir()
    (test_dir / "file1.txt").write_text("content1")
    (test_dir / "file2.json").write_text("{\"key\": \"value\"}")
    subdir = test_dir / "subdir"
    subdir.mkdir()
    (subdir / "file3.txt").write_text("content3")

    # Create uploader and upload the directory
    uploader = s3_mod.S3Uploader("s3://bucket/path", model_name="test_model")
    uploader.upload_path(local_path=str(test_dir))

    # Verify all files were uploaded with correct paths
    s3_client = uploader.client
    assert ("bucket", "path/file1.txt") in s3_client.uploaded_files
    assert s3_client.uploaded_files[("bucket", "path/file1.txt")] == b"content1"
    assert ("bucket", "path/file2.json") in s3_client.uploaded_files
    assert s3_client.uploaded_files[("bucket", "path/file2.json")] == b'{"key": "value"}'
    assert ("bucket", "path/subdir/file3.txt") in s3_client.uploaded_files
    assert s3_client.uploaded_files[("bucket", "path/subdir/file3.txt")] == b"content3"


def test_s3_upload_with_file_suffix_filter(monkeypatch, tmp_path):
    # Test that S3 upload correctly filters files by suffix
    from aibrix.uploader import s3 as s3_mod

    class FakeS3Client:
        def __init__(self):
            self.uploaded_files = {}

        def head_bucket(self, Bucket):
            return {}
            
        def head_object(self, Bucket, Key):
            # Simulate object not existing with proper ClientError
            from botocore.exceptions import ClientError
            error_response = {'Error': {'Code': '404'}}
            raise ClientError(error_response, 'head_object')

        def upload_file(self, Filename, Bucket, Key, Config, Callback=None):
            # Record the uploaded file
            with open(Filename, 'rb') as f:
                self.uploaded_files[(Bucket, Key)] = f.read()

    def fake_boto3_client(service_name, config, **auth):
        return FakeS3Client()

    monkeypatch.setattr(
        s3_mod, "boto3", types.SimpleNamespace(client=fake_boto3_client)
    )

    # Create a test directory with various file types
    test_dir = tmp_path / "test_dir"
    test_dir.mkdir()
    (test_dir / "file1.txt").write_text("content1")
    (test_dir / "file2.json").write_text("{\"key\": \"value\"}")
    (test_dir / "file3.log").write_text("log content")

    # Create uploader with file suffix filter
    upload_extra_config = UploadExtraConfig(allow_file_suffix=[".txt", ".json"])
    uploader = s3_mod.S3Uploader(
        "s3://bucket/path", 
        model_name="test_model",
        upload_extra_config=upload_extra_config
    )
    uploader.upload_path(local_path=str(test_dir))

    # Verify only files with allowed suffixes were uploaded
    s3_client = uploader.client
    assert ("bucket", "path/file1.txt") in s3_client.uploaded_files
    assert ("bucket", "path/file2.json") in s3_client.uploaded_files
    assert ("bucket", "path/file3.log") not in s3_client.uploaded_files


def test_s3_upload_with_force_upload(monkeypatch, tmp_path):
    # Test that force_upload parameter works correctly
    from aibrix.uploader import s3 as s3_mod

    class FakeS3Client:
        def __init__(self):
            self.uploaded_files = {}
            self.object_exists_count = 0

        def head_bucket(self, Bucket):
            return {}

        def head_object(self, Bucket, Key):
            # Simulate object existence
            self.object_exists_count += 1
            raise Exception("Object does not exist")  # This would be handled in the actual code

        def upload_file(self, Filename, Bucket, Key, Config, Callback=None):
            # Record the uploaded file
            with open(Filename, 'rb') as f:
                self.uploaded_files[(Bucket, Key)] = f.read()

    def fake_boto3_client(service_name, config, **auth):
        return FakeS3Client()

    monkeypatch.setattr(
        s3_mod, "boto3", types.SimpleNamespace(client=fake_boto3_client)
    )

    # Create a test file
    test_file = tmp_path / "test.txt"
    test_file.write_text("test content")

    # Create uploader with force_upload=True
    upload_extra_config = UploadExtraConfig(force_upload=True)
    uploader = s3_mod.S3Uploader(
        "s3://bucket/path", 
        model_name="test_model",
        upload_extra_config=upload_extra_config
    )
    uploader.upload_path(local_path=str(test_file))

    # Verify the file was uploaded
    assert ("bucket", "path/test.txt") in uploader.client.uploaded_files


def test_s3_upload_metadata(monkeypatch, tmp_path):
    # Test that S3 upload correctly handles metadata paths
    from aibrix.uploader import s3 as s3_mod

    class FakeS3Client:
        def __init__(self):
            self.uploaded_files = {}

        def head_bucket(self, Bucket):
            return {}
            
        def head_object(self, Bucket, Key):
            # Simulate object not existing with proper ClientError
            from botocore.exceptions import ClientError
            error_response = {'Error': {'Code': '404'}}
            raise ClientError(error_response, 'head_object')

        def upload_file(self, Filename, Bucket, Key, Config, Callback=None):
            # Record the uploaded file
            with open(Filename, 'rb') as f:
                self.uploaded_files[(Bucket, Key)] = f.read()

    def fake_boto3_client(service_name, config, **auth):
        return FakeS3Client()

    monkeypatch.setattr(
        s3_mod, "boto3", types.SimpleNamespace(client=fake_boto3_client)
    )

    # Create a test file
    test_file = tmp_path / "test.txt"
    test_file.write_text("test content")

    # Create uploader and upload the file
    uploader = s3_mod.S3Uploader("s3://bucket/path", model_name="test_model")
    uploader.upload_path(local_path=str(test_file))

    # Verify the file was uploaded
    assert ("bucket", "path/test.txt") in uploader.client.uploaded_files
    
    # Test that metadata paths can be generated correctly
    from aibrix.uploader import utils
    meta_file_path = utils.meta_file(str(test_file), test_file.name, "s3")
    assert str(meta_file_path).endswith(f".cache/s3/download/{test_file.name}.metadata")
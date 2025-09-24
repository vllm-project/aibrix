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
    final = tmp_path / "m" / "file.txt"
    assert final.exists()
    assert not Path(str(final) + ".part").exists()

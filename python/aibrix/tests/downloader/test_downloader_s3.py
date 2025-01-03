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


def mock_not_exsit_boto3(mock_boto3):
    mock_client = mock.Mock()
    mock_boto3.client.return_value = mock_client
    mock_client.head_bucket.side_effect = Exception("head bucket error")


def mock_exsit_boto3(mock_boto3):
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


@mock.patch(ENVS_MODULE, env_group)
@mock.patch(S3_BOTO3_MODULE)
def test_get_downloader_s3(mock_boto3):
    mock_exsit_boto3(mock_boto3)

    downloader = get_downloader("s3://bucket/path")
    assert isinstance(downloader, S3Downloader)


@mock.patch(ENVS_MODULE, env_group)
@mock.patch(S3_BOTO3_MODULE)
def test_get_downloader_s3_path_not_exist(mock_boto3):
    mock_not_exsit_boto3(mock_boto3)

    with pytest.raises(ModelNotFoundError) as exception:
        get_downloader("s3://bucket/not_exsit_path")
    assert "Model not found" in str(exception.value)


@mock.patch(ENVS_MODULE, env_group)
@mock.patch(S3_BOTO3_MODULE)
def test_get_downloader_s3_path_empty(mock_boto3):
    mock_exsit_boto3(mock_boto3)

    # Bucket name and path both are empty,
    # will first assert the name
    with pytest.raises(ArgNotCongiuredError) as exception:
        get_downloader("s3://")
    assert "`bucket_name` is not configured" in str(exception.value)


@mock.patch(ENVS_MODULE, env_group)
@mock.patch(S3_BOTO3_MODULE)
def test_get_downloader_s3_path_empty_path(mock_boto3):
    mock_exsit_boto3(mock_boto3)

    # bucket path is empty
    with pytest.raises(ArgNotCongiuredError) as exception:
        get_downloader("s3://bucket/")
    assert "`bucket_path` is not configured" in str(exception.value)


@mock.patch(ENVS_MODULE, env_no_ak)
@mock.patch(S3_BOTO3_MODULE)
def test_get_downloader_s3_no_ak(mock_boto3):
    mock_exsit_boto3(mock_boto3)

    with pytest.raises(ArgNotCongiuredError) as exception:
        get_downloader("s3://bucket/")
    assert "`ak` is not configured" in str(exception.value)


@mock.patch(ENVS_MODULE, env_no_sk)
@mock.patch(S3_BOTO3_MODULE)
def test_get_downloader_s3_no_sk(mock_boto3):
    mock_exsit_boto3(mock_boto3)

    with pytest.raises(ArgNotCongiuredError) as exception:
        get_downloader("s3://bucket/")
    assert "`sk` is not configured" in str(exception.value)

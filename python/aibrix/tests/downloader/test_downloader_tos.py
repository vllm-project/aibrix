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
from aibrix.downloader.tos import TOSDownloaderV2

S3_BOTO3_MODULE = "aibrix.downloader.s3.boto3"
ENVS_MODULE = "aibrix.downloader.tos.envs"


def mock_not_exsit_tos(mock_boto3):
    mock_client = mock.Mock()
    mock_boto3.client.return_value = mock_client
    mock_client.head_bucket.side_effect = Exception("head bucket error")


def mock_exsit_tos(mock_boto3):
    mock_client = mock.Mock()
    mock_boto3.client.return_value = mock_client
    mock_client.head_bucket.return_value = mock.Mock()


env_group = mock.Mock()
env_group.DOWNLOADER_NUM_THREADS = 4
env_group.DOWNLOADER_TOS_VERSION = "v2"
env_group.DOWNLOADER_TOS_REGION = None
env_group.DOWNLOADER_TOS_ENDPOINT = None
env_group.DOWNLOADER_TOS_ACCESS_KEY = None
env_group.DOWNLOADER_TOS_SECRET_KEY = None


@mock.patch(ENVS_MODULE, env_group)
@mock.patch(S3_BOTO3_MODULE)
def test_get_downloader_tos(mock_boto3):
    mock_exsit_tos(mock_boto3)

    downloader = get_downloader("tos://bucket/path")
    assert isinstance(downloader, TOSDownloaderV2)


@mock.patch(ENVS_MODULE, env_group)
@mock.patch(S3_BOTO3_MODULE)
def test_get_downloader_tos_path_not_exist(mock_boto3):
    mock_not_exsit_tos(mock_boto3)

    with pytest.raises(ModelNotFoundError) as exception:
        get_downloader("tos://bucket/not_exist_path")
    assert "Model not found" in str(exception.value)


@mock.patch(ENVS_MODULE, env_group)
@mock.patch(S3_BOTO3_MODULE)
def test_get_downloader_tos_path_empty(mock_boto3):
    mock_exsit_tos(mock_boto3)

    # Bucket name and path both are empty,
    # will first assert the name
    with pytest.raises(ArgNotCongiuredError) as exception:
        get_downloader("tos://")
    assert "`bucket_name` is not configured" in str(exception.value)


@mock.patch(ENVS_MODULE, env_group)
@mock.patch(S3_BOTO3_MODULE)
def test_get_downloader_tos_path_empty_path(mock_boto3):
    mock_exsit_tos(mock_boto3)

    # bucket path is empty
    with pytest.raises(ArgNotCongiuredError) as exception:
        get_downloader("tos://bucket/")
    assert "`bucket_path` is not configured" in str(exception.value)


@mock.patch(ENVS_MODULE, env_group)
@mock.patch(S3_BOTO3_MODULE)
def test_tos_v2_auth_omits_empty_ak_sk(mock_boto3):
    # Ensure client exists and head_bucket succeeds
    mock_exsit_tos(mock_boto3)

    # Instantiate and verify auth config omits empty AK/SK
    downloader = TOSDownloaderV2("tos://bucket/path", model_name="m")
    cfg = downloader._get_auth_config()
    assert "aws_access_key_id" not in cfg
    assert "aws_secret_access_key" not in cfg

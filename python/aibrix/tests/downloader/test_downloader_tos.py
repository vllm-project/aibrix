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

from aibrix.downloader.base import get_downloader
from aibrix.downloader.tos import TOSDownloader

TOS_MODULE = "aibrix.downloader.tos.tos"


def mock_not_exsit_tos(mock_tos):
    mock_client = mock.Mock()
    mock_tos.TosClientV2.return_value = mock_client
    mock_client.head_bucket.side_effect = Exception("head bucket error")


def mock_exsit_tos(mock_tos):
    mock_client = mock.Mock()
    mock_tos.TosClientV2.return_value = mock_client
    mock_client.head_bucket.return_value = mock.Mock()


@mock.patch(TOS_MODULE)
def test_get_downloader_s3(mock_tos):
    mock_exsit_tos(mock_tos)

    downloader = get_downloader("tos://bucket/path")
    assert isinstance(downloader, TOSDownloader)


@mock.patch(TOS_MODULE)
def test_get_downloader_s3_path_not_exist(mock_tos):
    mock_not_exsit_tos(mock_tos)

    with pytest.raises(AssertionError) as exception:
        get_downloader("tos://bucket/not_exsit_path")
    assert "not exist" in str(exception.value)


@mock.patch(TOS_MODULE)
def test_get_downloader_s3_path_empty(mock_tos):
    mock_exsit_tos(mock_tos)

    # Bucket name and path both are empty,
    # will first assert the name
    with pytest.raises(AssertionError) as exception:
        get_downloader("tos://")
    assert "TOS bucket name is not set." in str(exception.value)


@mock.patch(TOS_MODULE)
def test_get_downloader_s3_path_empty_path(mock_tos):
    mock_exsit_tos(mock_tos)

    # bucket path is empty
    with pytest.raises(AssertionError) as exception:
        get_downloader("tos://bucket/")
    assert "TOS bucket path is not set." in str(exception.value)

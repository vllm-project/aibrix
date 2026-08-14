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
from aibrix.downloader.base import DownloadExtraConfig, get_downloader
from aibrix.downloader.tos import TOSDownloaderV1
from aibrix.downloader.utils import meta_file, save_meta_data

TOS_MODULE = "aibrix.downloader.tos.tos"
ENVS_MODULE = "aibrix.downloader.tos.envs"
ENVS_DOWNLOADER_TOS_VERSION = "aibrix.downloader.base.envs.DOWNLOADER_TOS_VERSION"
DOWNLOADER_TOS_VERSION = "v1"


def mock_not_exsit_tos(mock_tos):
    mock_client = mock.Mock()
    mock_tos.TosClientV2.return_value = mock_client
    mock_client.head_bucket.side_effect = Exception("head bucket error")


def mock_exsit_tos(mock_tos):
    mock_client = mock.Mock()
    mock_tos.TosClientV2.return_value = mock_client
    mock_client.head_bucket.return_value = mock.Mock()


env_group = mock.Mock()


@mock.patch(ENVS_DOWNLOADER_TOS_VERSION, DOWNLOADER_TOS_VERSION)
@mock.patch(ENVS_MODULE, env_group)
@mock.patch(TOS_MODULE)
def test_get_downloader_tos(mock_tos):
    mock_exsit_tos(mock_tos)

    downloader = get_downloader("tos://bucket/path")
    assert isinstance(downloader, TOSDownloaderV1)


@mock.patch(ENVS_DOWNLOADER_TOS_VERSION, DOWNLOADER_TOS_VERSION)
@mock.patch(ENVS_MODULE, env_group)
@mock.patch(TOS_MODULE)
def test_get_downloader_tos_path_not_exist(mock_tos):
    mock_not_exsit_tos(mock_tos)

    with pytest.raises(ModelNotFoundError) as exception:
        get_downloader("tos://bucket/not_exist_path")
    assert "Model not found" in str(exception.value)


@mock.patch(ENVS_DOWNLOADER_TOS_VERSION, DOWNLOADER_TOS_VERSION)
@mock.patch(ENVS_MODULE, env_group)
@mock.patch(TOS_MODULE)
def test_get_downloader_tos_path_empty(mock_tos):
    mock_exsit_tos(mock_tos)

    # Bucket name and path both are empty,
    # will first assert the name
    with pytest.raises(ArgNotCongiuredError) as exception:
        get_downloader("tos://")
    assert "`bucket_name` is not configured" in str(exception.value)


@mock.patch(ENVS_DOWNLOADER_TOS_VERSION, DOWNLOADER_TOS_VERSION)
@mock.patch(ENVS_MODULE, env_group)
@mock.patch(TOS_MODULE)
def test_get_downloader_tos_path_empty_path(mock_tos):
    mock_exsit_tos(mock_tos)

    # bucket path is empty
    with pytest.raises(ArgNotCongiuredError) as exception:
        get_downloader("tos://bucket/")
    assert "`bucket_path` is not configured" in str(exception.value)


def _make_fake_tos_client_for_force_download_test():
    class FakeTosClient:
        def __init__(self):
            self.download_calls = 0

        def head_bucket(self, bucket):
            return {}

        def list_objects_type2(self, bucket, prefix, delimiter=None):
            # single-file model_uri: exactly one object, matching bucket_path,
            # so TOSDownloaderV1._is_directory() returns False.
            return types.SimpleNamespace(
                contents=[types.SimpleNamespace(key="file.txt")]
            )

        def head_object(self, bucket, key):
            return types.SimpleNamespace(etag="etag", content_length=4)

        def download_file(self, bucket, key, file_path, task_num, **kwargs):
            self.download_calls += 1
            Path(file_path).parent.mkdir(parents=True, exist_ok=True)
            Path(file_path).write_bytes(b"data")

    return FakeTosClient()


def _prepopulate_matching_local_file(model_path, source_value):
    local_file = model_path / "file.txt"
    local_file.parent.mkdir(parents=True, exist_ok=True)
    local_file.write_bytes(b"data")
    meta_data_file = meta_file(model_path, "file.txt", source=source_value)
    save_meta_data(meta_data_file, "etag")


@mock.patch(ENVS_DOWNLOADER_TOS_VERSION, DOWNLOADER_TOS_VERSION)
@mock.patch(ENVS_MODULE, env_group)
@mock.patch(TOS_MODULE)
def test_tos_v1_skips_matching_file_when_not_forced(mock_tos, tmp_path):
    # Control for test_tos_v1_force_download_override_bypasses_exist_check:
    # with no force_download override, a local file matching the remote
    # etag/size is skipped, as before.
    fake_client = _make_fake_tos_client_for_force_download_test()
    mock_tos.TosClientV2.return_value = fake_client

    d = TOSDownloaderV1("tos://bucket/file.txt", model_name="m")
    _prepopulate_matching_local_file(tmp_path / "m", d.source.value)

    d.download_model(local_path=str(tmp_path))

    assert fake_client.download_calls == 0


@mock.patch(ENVS_DOWNLOADER_TOS_VERSION, DOWNLOADER_TOS_VERSION)
@mock.patch(ENVS_MODULE, env_group)
@mock.patch(TOS_MODULE)
def test_tos_v1_force_download_override_bypasses_exist_check(mock_tos, tmp_path):
    # Regression test: DownloadExtraConfig(force_download=True) must force a
    # re-download even when the local file already matches remote etag/size,
    # mirroring test_s3_force_download_override_bypasses_exist_check.
    fake_client = _make_fake_tos_client_for_force_download_test()
    mock_tos.TosClientV2.return_value = fake_client

    d = TOSDownloaderV1(
        "tos://bucket/file.txt",
        model_name="m",
        download_extra_config=DownloadExtraConfig(force_download=True),
    )
    _prepopulate_matching_local_file(tmp_path / "m", d.source.value)

    d.download_model(local_path=str(tmp_path))

    assert fake_client.download_calls == 1

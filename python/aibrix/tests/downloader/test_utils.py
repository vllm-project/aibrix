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


import os
import tempfile
from pathlib import Path
from unittest import mock

import pytest

from aibrix import envs
from aibrix.config import DOWNLOAD_CACHE_DIR
from aibrix.downloader.base import BaseDownloader
from aibrix.downloader.entity import RemoteSource
from aibrix.downloader.utils import (
    check_file_exist,
    infer_model_name,
    load_meta_data,
    meta_file,
    need_to_download,
    save_meta_data,
)


def prepare_file_and_meta_data(file_path, meta_path, file_size, etag):
    save_meta_data(meta_path, etag)
    # create file
    with open(file_path, "wb") as f:
        f.write(os.urandom(file_size))


def test_meta_file():
    with tempfile.TemporaryDirectory() as tmp_dir:
        source = "s3"
        meta_file_path = meta_file(tmp_dir, "test", source=source)
        assert str(meta_file_path).endswith(
            f"{DOWNLOAD_CACHE_DIR % source}/test.metadata"
        )


def test_save_load_meta_data():
    with tempfile.TemporaryDirectory() as tmp_dir:
        file_path = Path(tmp_dir).joinpath("test.metadata")
        etag = "here_is_etag_value_xyz"
        save_meta_data(file_path, etag)
        assert file_path.exists()

        load_etag = load_meta_data(file_path)
        assert etag == load_etag

        not_exist_file = Path(tmp_dir).joinpath("not_exist.metadata")
        not_exist_etag = load_meta_data(not_exist_file)
        assert not_exist_etag is None


def test_check_file_exist():
    source = "s3"
    with tempfile.TemporaryDirectory() as tmp_dir:
        # prepare file and meta data
        file_size = 10
        file_name = "test"
        etag = "here_is_etag_value_xyz"
        file_path = Path(tmp_dir).joinpath(file_name)
        # create meta data file
        meta_path = meta_file(tmp_dir, file_name, source=source)
        prepare_file_and_meta_data(file_path, meta_path, file_size, etag)

        assert check_file_exist(file_path, meta_path, file_size, etag)
        assert not check_file_exist(file_path, meta_path, None, etag)
        assert not check_file_exist(file_path, meta_path, -1, etag)
        assert not check_file_exist(file_path, meta_path, file_size, None)
        # The remote etag has been updated
        assert not check_file_exist(file_path, meta_path, file_size, "new_etag")

        not_exist_file_name = "not_exist"
        not_exist_file_path = Path(tmp_dir).joinpath(not_exist_file_name)
        not_exist_meta_path = meta_file(tmp_dir, not_exist_file_name, source=source)
        assert not check_file_exist(
            not_exist_file_path, not_exist_meta_path, file_size, etag
        )
        # meta data not exist
        assert not check_file_exist(file_path, not_exist_meta_path, file_size, etag)


CHECK_FUNC = "aibrix.downloader.utils.check_file_exist"


@mock.patch(CHECK_FUNC)
def test_need_to_download_not_call(mock_check: mock.Mock):
    origin_force_download_env = envs.DOWNLOADER_FORCE_DOWNLOAD
    origin_check_file_exist = envs.DOWNLOADER_CHECK_FILE_EXIST

    envs.DOWNLOADER_FORCE_DOWNLOAD = True
    envs.DOWNLOADER_CHECK_FILE_EXIST = False
    need_to_download("file", "file.metadata", 10, "etag")
    mock_check.assert_not_called()

    envs.DOWNLOADER_FORCE_DOWNLOAD = True
    envs.DOWNLOADER_CHECK_FILE_EXIST = True
    need_to_download("file", "file.metadata", 10, "etag")
    mock_check.assert_not_called()

    envs.DOWNLOADER_FORCE_DOWNLOAD = False
    envs.DOWNLOADER_CHECK_FILE_EXIST = False
    need_to_download("file", "file.metadata", 10, "etag")
    mock_check.assert_not_called()

    # recover envs
    envs.DOWNLOADER_FORCE_DOWNLOAD = origin_force_download_env
    envs.DOWNLOADER_CHECK_FILE_EXIST = origin_check_file_exist


@mock.patch(CHECK_FUNC)
def test_need_to_download(mock_check: mock.Mock):
    origin_force_download_env = envs.DOWNLOADER_FORCE_DOWNLOAD
    origin_check_file_exist = envs.DOWNLOADER_CHECK_FILE_EXIST

    envs.DOWNLOADER_FORCE_DOWNLOAD = False
    envs.DOWNLOADER_CHECK_FILE_EXIST = True
    file_exist = True
    # file exist, no need to download
    mock_check.return_value = file_exist
    assert not need_to_download("file", "file.metadata", 10, "etag")
    mock_check.assert_called_once()
    mock_check.reset_mock()

    # file not exist, need to download
    mock_check.return_value = not file_exist
    assert need_to_download("file", "file.metadata", 10, "etag")
    mock_check.assert_called_once()
    mock_check.reset_mock()

    # recover envs
    envs.DOWNLOADER_FORCE_DOWNLOAD = origin_force_download_env
    envs.DOWNLOADER_CHECK_FILE_EXIST = origin_check_file_exist


def test_infer_model_name():
    with pytest.raises(ValueError):
        infer_model_name("")

    with pytest.raises(ValueError):
        infer_model_name(None)

    model_name = infer_model_name("s3://bucket/path/to/model")
    assert model_name == "model"


class FailingDownloader(BaseDownloader):
    _source = RemoteSource.UNKNOWN

    def _valid_config(self):
        pass

    def _is_directory(self) -> bool:
        return True

    def _directory_list(self, path: str):
        return ["a.txt", "b.txt", "c.txt"]

    def _support_range_download(self) -> bool:
        return False

    def download(
        self, local_path: Path, bucket_path: str, bucket_name=None, enable_range=True
    ):
        if bucket_path == "b.txt":
            raise RuntimeError("boom")
        # simulate success
        (local_path / bucket_path).write_text("ok")


def test_threadpool_exceptions_surface(tmp_path: Path):
    dl = FailingDownloader(
        model_uri="mock://x",
        model_name="m",
        bucket_path="p",
        bucket_name=None,
    )
    with pytest.raises(RuntimeError):
        dl.download_directory(local_path=tmp_path)


def test_cli_prints_model_path(monkeypatch, capsys, tmp_path: Path):
    # Monkeypatch downloader.download_model to return a path
    import aibrix.downloader.__main__ as cli

    def fake_download_model(
        model_uri, local_dir, model_name, download_extra_config, enable_progress_bar
    ):
        return tmp_path / "m"

    monkeypatch.setattr(cli, "download_model", fake_download_model)

    # Simulate args
    monkeypatch.setattr("sys.argv", ["aibrix_download", "--model-uri", "repo/name"])
    cli.main()
    out = capsys.readouterr().out.strip()
    assert out.endswith("/m") or out.endswith("\\m")

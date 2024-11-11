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
from pathlib import Path
from typing import Union

from aibrix import envs
from aibrix.config import DOWNLOAD_CACHE_DIR
from aibrix.logger import init_logger

logger = init_logger(__name__)


def meta_file(local_path: Union[Path, str], file_name: str) -> Path:
    return (
        Path(local_path)
        .joinpath(DOWNLOAD_CACHE_DIR)
        .joinpath(f"{file_name}.metadata")
        .absolute()
    )


def save_meta_data(file_path: Union[Path, str], etag: str):
    Path(file_path).parent.mkdir(parents=True, exist_ok=True)
    with open(file_path, "w") as f:
        f.write(etag)


def load_meta_data(file_path: Union[Path, str]):
    if Path(file_path).exists():
        with open(file_path, "r") as f:
            return f.read()
    return None


def check_file_exist(
    local_file: Union[Path, str],
    meta_file: Union[Path, str],
    expected_file_size: int,
    expected_etag: str,
) -> bool:
    if expected_file_size is None or expected_file_size <= 0:
        return False

    if expected_etag is None or expected_etag == "":
        return False

    if not Path(local_file).exists():
        return False

    file_size = os.path.getsize(local_file)
    if file_size != expected_file_size:
        return False

    if not Path(meta_file).exists():
        return False

    etag = load_meta_data(meta_file)
    return etag == expected_etag


def need_to_download(
    local_file: Union[Path, str],
    meta_data_file: Union[Path, str],
    expected_file_size: int,
    expected_etag: str,
) -> bool:
    _file_name = Path(local_file).name
    if not envs.DOWNLOADER_FORCE_DOWNLOAD and envs.DOWNLOADER_CHECK_FILE_EXIST:
        if check_file_exist(
            local_file, meta_data_file, expected_file_size, expected_etag
        ):
            logger.info(f"File {_file_name} exist in local, skip download.")
            return False
        else:
            logger.info(f"File {_file_name} not exist in local, start to download...")
    else:
        logger.info(
            f"File {_file_name} start downloading directly "
            f"for DOWNLOADER_FORCE_DOWNLOAD={envs.DOWNLOADER_FORCE_DOWNLOAD}, "
            f"DOWNLOADER_CHECK_FILE_EXIST={envs.DOWNLOADER_CHECK_FILE_EXIST}"
        )
    return True


def infer_model_name(uri: str):
    if uri is None or uri == "":
        raise ValueError("Model uri is empty.")

    return uri.strip().strip("/").split("/")[-1]

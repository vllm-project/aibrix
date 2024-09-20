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

from aibrix.config import DOWNLOAD_CACHE_DIR


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

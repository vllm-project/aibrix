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
import logging
from functools import lru_cache
from pathlib import Path
from typing import List, Optional, Tuple
from urllib.parse import urlparse

import tos
from tos import DataTransferType
from tqdm import tqdm

from aibrix import envs
from aibrix.downloader.base import BaseDownloader

tos_logger = logging.getLogger("tos")
tos_logger.setLevel(logging.WARNING)


def _parse_bucket_info_from_uri(uri: str) -> Tuple[str, str]:
    parsed = urlparse(uri, scheme="tos")
    bucket_name = parsed.netloc
    bucket_path = parsed.path.lstrip("/")
    return bucket_name, bucket_path


class TOSDownloader(BaseDownloader):
    def __init__(self, model_uri):
        model_name = envs.DOWNLOADER_MODEL_NAME
        ak = envs.DOWNLOADER_TOS_ACCESS_KEY
        sk = envs.DOWNLOADER_TOS_SECRET_KEY
        endpoint = envs.DOWNLOADER_TOS_ENDPOINT or ""
        region = envs.DOWNLOADER_TOS_REGION or ""
        enable_crc = envs.DOWNLOADER_TOS_ENABLE_CRC
        bucket_name, bucket_path = _parse_bucket_info_from_uri(model_uri)

        self.client = tos.TosClientV2(
            ak=ak, sk=sk, endpoint=endpoint, region=region, enable_crc=enable_crc
        )

        super().__init__(
            model_uri=model_uri,
            model_name=model_name,
            bucket_path=bucket_path,
            bucket_name=bucket_name,
        )  # type: ignore

    def _valid_config(self):
        assert (
            self.bucket_name is not None and self.bucket_name != ""
        ), "TOS bucket name is not set."
        assert (
            self.bucket_path is not None and self.bucket_path != ""
        ), "TOS bucket path is not set."
        try:
            self.client.head_bucket(self.bucket_name)
        except Exception as e:
            assert False, f"TOS bucket {self.bucket_name} not exist for {e}."

    @lru_cache()
    def _is_directory(self) -> bool:
        """Check if model_uri is a directory."""
        if self.bucket_path.endswith("/"):
            return True
        objects_out = self.client.list_objects_type2(
            self.bucket_name, prefix=self.bucket_path, delimiter="/"
        )
        if (
            len(objects_out.contents) == 1
            and objects_out.contents[0].key == self.bucket_path
        ):
            return False
        return True

    def _directory_list(self, path: str) -> List[str]:
        # TODO cache list_objects_type2 result to avoid too many requests
        objects_out = self.client.list_objects_type2(
            self.bucket_name, prefix=path, delimiter="/"
        )
        return [obj.key for obj in objects_out.contents]

    def _support_range_download(self) -> bool:
        return True

    def download(
        self,
        local_path: Path,
        bucket_path: str,
        bucket_name: Optional[str] = None,
        enable_range: bool = True,
    ):
        # check if file exist
        try:
            meta_data = self.client.head_object(bucket=bucket_name, key=bucket_path)
        except Exception as e:
            raise ValueError(f"TOS bucket path {bucket_path} not exist for {e}.")

        _file_name = bucket_path.split("/")[-1]
        # TOS client does not support Path, convert it to str
        local_file = str(local_path.joinpath(_file_name).absolute())
        task_num = envs.DOWNLOADER_NUM_THREADS if enable_range else 1

        download_kwargs = {}
        if envs.DOWNLOADER_PART_CHUNKSIZE is not None:
            download_kwargs["part_size"] = envs.DOWNLOADER_PART_CHUNKSIZE

        # download file
        total_length = meta_data.content_length
        with tqdm(total=total_length, unit="b", unit_scale=True) as pbar:

            def download_progress(
                consumed_bytes, total_bytes, rw_once_bytes, type: DataTransferType
            ):
                pbar.update(rw_once_bytes)

            self.client.download_file(
                bucket=bucket_name,
                key=bucket_path,
                file_path=local_file,
                task_num=task_num,
                data_transfer_listener=download_progress,
                **download_kwargs,
            )

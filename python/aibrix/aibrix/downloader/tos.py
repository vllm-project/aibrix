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
import re
from pathlib import Path
from typing import List, Tuple
from urllib.parse import urlparse

import tos

from aibrix import envs
from aibrix.downloader.base import BaseDownloader


def _parse_bucket_info_from_uri(model_uri: str) -> Tuple[str, str, str, str]:
    if not re.match(envs.DOWNLOADER_TOS_REGEX, model_uri):
        raise ValueError(f"TOS uri {model_uri} not valid format.")
    parsed = urlparse(model_uri)
    bucket_name, endpoint = parsed.netloc.split(".", 1)
    bucket_path = parsed.path.lstrip("/")
    matched = re.match(r"tos-(.+?).volces.com", endpoint)
    if matched is None:
        raise ValueError(f"TOS endpoint {endpoint} not valid format.")

    region = matched.group(1)
    return bucket_name, bucket_path, endpoint, region


class TOSDownloader(BaseDownloader):
    def __init__(self, model_uri):
        model_name = envs.DOWNLOADER_MODEL_NAME
        ak = envs.DOWNLOADER_TOS_ACCESS_KEY
        sk = envs.DOWNLOADER_TOS_SECRET_KEY
        bucket_name, bucket_path, endpoint, region = _parse_bucket_info_from_uri(
            model_uri
        )
        self.bucket_name = bucket_name
        self.bucket_path = bucket_path
        self.client = tos.TosClientV2(ak, sk, endpoint, region)
        super().__init__(model_uri=model_uri, model_name=model_name)  # type: ignore

    def _valid_config(self):
        assert self.bucket_name is not None, "TOS bucket name is not set."
        assert self.bucket_path is not None, "TOS bucket path is not set."
        assert self.bucket_path.endswith("/"), "TOS bucket path must end with /."
        # TODO 检查 bucket 是否存在
        try:
            self.client.head_bucket(self.bucket_name)
        except Exception as e:
            assert False, f"TOS bucket {self.bucket_name} not exist for {e}."

    def _is_directory(self) -> bool:
        """Check if model_uri is a directory."""
        objects_out = self.client.list_objects_type2(
            self.bucket_name, prefix=self.bucket_path, delimiter="/"
        )
        if len(objects_out.contents) > 0:
            return True
        return False

    def _directory_list(self, path: str) -> List[str]:
        # TODO cache list_objects_type2 result to avoid too many requests
        objects_out = self.client.list_objects_type2(
            self.bucket_name, prefix=self.bucket_path, delimiter="/"
        )

        return [obj.key for obj in objects_out.contents]

    def _support_range_download(self) -> bool:
        return True

    def download(self, filename: str, local_path: Path, enable_range: bool = True):
        _file_name = filename.split("/")[-1]
        # TOS client does not support Path, convert it to str
        local_file = str(local_path.joinpath(_file_name).absolute())

        # check if file exist
        try:
            self.client.head_object(bucket=self.bucket_name, key=filename)
        except Exception as e:
            raise ValueError(f"TOS file {filename} not exist for {e}.")

        if not enable_range:
            self.client.get_object_to_file(
                bucket=self.bucket_name, key=filename, file_path=local_file
            )
        else:
            self.client.download_file(
                bucket=self.bucket_name,
                key=filename,
                file_path=local_file,
                task_num=envs.DOWNLOADER_NUM_THREADS,
            )

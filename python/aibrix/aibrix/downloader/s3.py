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
from functools import lru_cache
from pathlib import Path
from typing import List, Optional, Tuple
from urllib.parse import urlparse

import boto3
from boto3.s3.transfer import TransferConfig
from botocore.config import MAX_POOL_CONNECTIONS, Config
from tqdm import tqdm

from aibrix import envs
from aibrix.downloader.base import BaseDownloader


def _parse_bucket_info_from_uri(uri: str) -> Tuple[str, str]:
    parsed = urlparse(uri, scheme="s3")
    bucket_name = parsed.netloc
    bucket_path = parsed.path.lstrip("/")
    return bucket_name, bucket_path


class S3Downloader(BaseDownloader):
    def __init__(self, model_uri):
        model_name = envs.DOWNLOADER_MODEL_NAME
        ak = envs.DOWNLOADER_AWS_ACCESS_KEY
        sk = envs.DOWNLOADER_AWS_SECRET_KEY
        endpoint = envs.DOWNLOADER_AWS_ENDPOINT
        region = envs.DOWNLOADER_AWS_REGION
        bucket_name, bucket_path = _parse_bucket_info_from_uri(model_uri)

        # Avoid warning log "Connection pool is full"
        # Refs: https://github.com/boto/botocore/issues/619#issuecomment-583511406
        max_pool_connections = (
            envs.DOWNLOADER_NUM_THREADS
            if envs.DOWNLOADER_NUM_THREADS > MAX_POOL_CONNECTIONS
            else MAX_POOL_CONNECTIONS
        )
        client_config = Config(
            max_pool_connections=max_pool_connections,
        )

        self.client = boto3.client(
            service_name="s3",
            region_name=region,
            endpoint_url=endpoint,
            aws_access_key_id=ak,
            aws_secret_access_key=sk,
            config=client_config,
        )

        super().__init__(
            model_uri=model_uri,
            model_name=model_name,
            bucket_path=bucket_path,
            bucket_name=bucket_name,
        )  # type: ignore

    def _valid_config(self):
        assert (
            self.bucket_name is not None or self.bucket_name == ""
        ), "S3 bucket name is not set."
        assert (
            self.bucket_path is not None or self.bucket_path == ""
        ), "S3 bucket path is not set."
        try:
            self.client.head_bucket(Bucket=self.bucket_name)
        except Exception as e:
            assert False, f"S3 bucket {self.bucket_name} not exist for {e}."

    @lru_cache()
    def _is_directory(self) -> bool:
        """Check if model_uri is a directory."""
        if self.bucket_path.endswith("/"):
            return True
        objects_out = self.client.list_objects_v2(
            Bucket=self.bucket_name, Delimiter="/", Prefix=self.bucket_path
        )
        contents = objects_out.get("Contents", [])
        if len(contents) == 1 and contents[0].get("Key") == self.bucket_path:
            return False
        return True

    def _directory_list(self, path: str) -> List[str]:
        objects_out = self.client.list_objects_v2(
            Bucket=self.bucket_name, Delimiter="/", Prefix=path
        )
        contents = objects_out.get("Contents", [])
        return [content.get("Key") for content in contents]

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
            meta_data = self.client.head_object(Bucket=bucket_name, Key=bucket_path)
        except Exception as e:
            raise ValueError(f"S3 bucket path {bucket_path} not exist for {e}.")

        _file_name = bucket_path.split("/")[-1]
        # S3 client does not support Path, convert it to str
        local_file = str(local_path.joinpath(_file_name).absolute())

        # construct TransferConfig
        config_kwargs = {
            "max_concurrency": envs.DOWNLOADER_NUM_THREADS,
            "use_threads": enable_range,
        }
        if envs.DOWNLOADER_PART_THRESHOLD is not None:
            config_kwargs["multipart_threshold"] = envs.DOWNLOADER_PART_THRESHOLD
        if envs.DOWNLOADER_PART_CHUNKSIZE is not None:
            config_kwargs["multipart_chunksize"] = envs.DOWNLOADER_PART_CHUNKSIZE

        config = TransferConfig(**config_kwargs)

        # download file
        total_length = int(meta_data.get("ContentLength", 0))
        with tqdm(total=total_length, unit="b", unit_scale=True) as pbar:

            def download_progress(bytes_transferred):
                pbar.update(bytes_transferred)

            self.client.download_file(
                Bucket=bucket_name,
                Key=bucket_path,
                Filename=local_file,
                Config=config,
                Callback=download_progress,
            )

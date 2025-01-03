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
from abc import abstractmethod
from contextlib import nullcontext
from functools import lru_cache
from pathlib import Path
from typing import ClassVar, Dict, List, Optional, Tuple
from urllib.parse import urlparse

import boto3
from boto3.s3.transfer import TransferConfig
from botocore.config import MAX_POOL_CONNECTIONS, Config
from tqdm import tqdm

from aibrix import envs
from aibrix.common.errors import ArgNotCongiuredError, ModelNotFoundError
from aibrix.downloader.base import (
    DEFAULT_DOWNLOADER_EXTRA_CONFIG,
    BaseDownloader,
    DownloadExtraConfig,
)
from aibrix.downloader.entity import RemoteSource, get_local_download_paths
from aibrix.downloader.utils import (
    infer_model_name,
    meta_file,
    need_to_download,
    save_meta_data,
)
from aibrix.logger import init_logger

logger = init_logger(__name__)


def _parse_bucket_info_from_uri(uri: str, scheme: str = "s3") -> Tuple[str, str]:
    parsed = urlparse(uri, scheme=scheme)
    bucket_name = parsed.netloc
    bucket_path = parsed.path.lstrip("/")
    return bucket_name, bucket_path


class S3BaseDownloader(BaseDownloader):
    _source: ClassVar[RemoteSource] = RemoteSource.S3

    def __init__(
        self,
        scheme: str,
        model_uri: str,
        model_name: Optional[str] = None,
        download_extra_config: DownloadExtraConfig = DEFAULT_DOWNLOADER_EXTRA_CONFIG,
        enable_progress_bar: bool = False,
    ):
        if model_name is None:
            model_name = infer_model_name(model_uri)
            logger.info(f"model_name is not set, using `{model_name}` as model_name")

        self.download_extra_config = download_extra_config
        auth_config = self._get_auth_config()
        bucket_name, bucket_path = _parse_bucket_info_from_uri(model_uri, scheme=scheme)

        # Avoid warning log "Connection pool is full"
        # Refs: https://github.com/boto/botocore/issues/619#issuecomment-583511406
        _num_threads = (
            self.download_extra_config.num_threads or envs.DOWNLOADER_NUM_THREADS
        )

        max_pool_connections = (
            _num_threads
            if _num_threads > MAX_POOL_CONNECTIONS
            else MAX_POOL_CONNECTIONS
        )
        client_config = Config(
            s3={"addressing_style": "virtual"},
            max_pool_connections=max_pool_connections,
        )

        self.client = boto3.client(
            service_name="s3", config=client_config, **auth_config
        )

        super().__init__(
            model_uri=model_uri,
            model_name=model_name,
            bucket_path=bucket_path,
            bucket_name=bucket_name,
            download_extra_config=download_extra_config,
            enable_progress_bar=enable_progress_bar,
        )  # type: ignore

    def _valid_config(self):
        if self.model_name is None or self.model_name == "":
            raise ArgNotCongiuredError(arg_name="model_name", arg_source="--model-name")

        if self.bucket_name is None or self.bucket_name == "":
            raise ArgNotCongiuredError(arg_name="bucket_name", arg_source="--model-uri")

        if self.bucket_path is None or self.bucket_path == "":
            raise ArgNotCongiuredError(arg_name="bucket_path", arg_source="--model-uri")

        try:
            self.client.head_bucket(Bucket=self.bucket_name)
        except Exception as e:
            logger.error(
                f"Bucket {self.bucket_name} not exist in {self.model_uri}\nFor {e}"
            )
            raise ModelNotFoundError(model_uri=self.model_uri, detail_msg=str(e))

    @abstractmethod
    def _get_auth_config(self) -> Dict[str, Optional[str]]:
        """Get auth config for S3 client.

        Returns:
            Dict[str, str]: auth config for S3 client, containing following keys:
            - region_name: region name of S3 bucket
            - endpoint_url: endpoint url of S3 bucket
            - aws_access_key_id: access key id of S3 bucket
            - aws_secret_access_key: secret access key of S3 bucket

        Example return value:
            {
                region_name: "region-name",
                endpoint_url: "URL_ADDRESS3.region-name.com",
                aws_access_key_id: "AK****",
                aws_secret_access_key: "SK****",,
            }
        """
        pass

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
        try:
            meta_data = self.client.head_object(Bucket=bucket_name, Key=bucket_path)
        except Exception as e:
            raise ValueError(f"S3 bucket path {bucket_path} not exist for {e}.")

        _file_name = bucket_path.split("/")[-1]
        local_file = local_path.joinpath(_file_name).absolute()

        # check if file exist
        etag = meta_data.get("ETag", "")
        file_size = meta_data.get("ContentLength", 0)
        meta_data_file = meta_file(
            local_path=local_path, file_name=_file_name, source=self._source.value
        )

        if not need_to_download(local_file, meta_data_file, file_size, etag):
            return

        # construct TransferConfig
        config_kwargs = {
            "max_concurrency": self.download_extra_config.num_threads
            or envs.DOWNLOADER_NUM_THREADS,
            "use_threads": enable_range,
            "max_io_queue": self.download_extra_config.max_io_queue
            or envs.DOWNLOADER_S3_MAX_IO_QUEUE,
            "io_chunksize": self.download_extra_config.io_chunksize
            or envs.DOWNLOADER_S3_IO_CHUNKSIZE,
            "multipart_threshold": self.download_extra_config.part_threshold
            or envs.DOWNLOADER_PART_THRESHOLD,
            "multipart_chunksize": self.download_extra_config.part_chunksize
            or envs.DOWNLOADER_PART_CHUNKSIZE,
        }

        config = TransferConfig(**config_kwargs)

        # download file
        total_length = int(meta_data.get("ContentLength", 0))
        with tqdm(
            desc=_file_name, total=total_length, unit="b", unit_scale=True
        ) if self.enable_progress_bar else nullcontext() as pbar:

            def download_progress(bytes_transferred):
                pbar.update(bytes_transferred)

            download_file = get_local_download_paths(
                local_path, _file_name, self._source
            )
            with download_file.download_lock():
                self.client.download_file(
                    Bucket=bucket_name,
                    Key=bucket_path,
                    Filename=str(
                        local_file
                    ),  # S3 client does not support Path, convert it to str
                    Config=config,
                    Callback=download_progress if self.enable_progress_bar else None,
                )
                save_meta_data(meta_data_file, etag)


class S3Downloader(S3BaseDownloader):
    _source: ClassVar[RemoteSource] = RemoteSource.S3

    def __init__(
        self,
        model_uri,
        model_name: Optional[str] = None,
        download_extra_config: DownloadExtraConfig = DEFAULT_DOWNLOADER_EXTRA_CONFIG,
        enable_progress_bar: bool = False,
    ):
        super().__init__(
            scheme="s3",
            model_uri=model_uri,
            model_name=model_name,
            download_extra_config=download_extra_config,
            enable_progress_bar=enable_progress_bar,
        )  # type: ignore

    def _get_auth_config(self) -> Dict[str, Optional[str]]:
        ak, sk = (
            self.download_extra_config.ak or envs.DOWNLOADER_AWS_ACCESS_KEY_ID,
            self.download_extra_config.sk or envs.DOWNLOADER_AWS_SECRET_ACCESS_KEY,
        )
        if ak is None or ak == "":
            raise ArgNotCongiuredError(
                arg_name="ak", arg_source="--download-extra-config"
            )
        if sk is None or sk == "":
            raise ArgNotCongiuredError(
                arg_name="sk", arg_source="--download-extra-config"
            )

        return {
            "region_name": self.download_extra_config.region
            or envs.DOWNLOADER_AWS_REGION,
            "endpoint_url": self.download_extra_config.endpoint
            or envs.DOWNLOADER_AWS_ENDPOINT_URL,
            "aws_access_key_id": ak,
            "aws_secret_access_key": sk,
        }

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
import time
from abc import ABC, abstractmethod
from concurrent.futures import ThreadPoolExecutor, wait
from dataclasses import dataclass, field
from pathlib import Path
from typing import ClassVar, Dict, List, Optional

from aibrix import envs
from aibrix.downloader.entity import RemoteSource
from aibrix.logger import init_logger

logger = init_logger(__name__)


@dataclass
class UploadExtraConfig:
    """Uploader extra config."""

    # Auth config for s3 or tos
    ak: Optional[str] = None
    sk: Optional[str] = None
    endpoint: Optional[str] = None
    region: Optional[str] = None

    # parallel config
    num_threads: Optional[int] = None
    max_io_queue: Optional[int] = None
    io_chunksize: Optional[int] = None
    part_threshold: Optional[int] = None
    part_chunksize: Optional[int] = None

    # other config
    allow_file_suffix: Optional[List[str]] = None
    force_upload: Optional[bool] = None


DEFAULT_UPLOADER_EXTRA_CONFIG = UploadExtraConfig()


@dataclass
class BaseUploader(ABC):
    """Base class for uploader."""

    model_uri: str
    model_name: str
    bucket_path: str
    bucket_name: Optional[str]
    upload_extra_config: UploadExtraConfig = field(
        default_factory=UploadExtraConfig
    )
    enable_progress_bar: bool = False
    _source: ClassVar[RemoteSource] = RemoteSource.UNKNOWN

    def __post_init__(self):
        # valid uploader config
        self._valid_config()
        self.model_name_path = self.model_name
        self.allow_file_suffix = (
            self.upload_extra_config.allow_file_suffix
            or getattr(envs, "UPLOADER_ALLOW_FILE_SUFFIX", None)
        )
        self.force_upload = (
            self.upload_extra_config.force_upload or getattr(envs, "UPLOADER_FORCE_UPLOAD", False)
        )

    @property
    def source(self) -> RemoteSource:
        return self._source

    @abstractmethod
    def _valid_config(self):
        pass

    @abstractmethod
    def _is_directory(self, local_path: Path) -> bool:
        """Check if local_path is a directory."""
        pass

    @abstractmethod
    def _directory_list(self, local_path: Path) -> List[str]:
        """List all files in the directory."""
        pass

    @abstractmethod
    def _support_multipart_upload(self) -> bool:
        """Check if the uploader supports multipart upload."""
        pass

    @abstractmethod
    def upload(
        self,
        local_file: Path,
        bucket_path: str,
        bucket_name: Optional[str] = None,
        enable_multipart: bool = True,
    ):
        """Upload a single file to remote storage."""
        pass

    def upload_directory(self, local_path: Path):
        """Upload directory from local_path to remote storage.
        Overwrite the method directly when there is a corresponding upload
        directory method for ``Uploader``. Otherwise, the following logic will be
        used to upload the directory.
        """
        directory_list = self._directory_list(local_path)

        if self.allow_file_suffix is None or len(self.allow_file_suffix) == 0:
            logger.info(f"All files from {local_path} will be uploaded.")
            filtered_files = directory_list
        else:
            filtered_files = [
                file
                for file in directory_list
                if any(file.endswith(suffix) for suffix in self.allow_file_suffix)
            ]

        if not self._support_multipart_upload():
            # upload using multi threads
            num_threads = (
                self.upload_extra_config.num_threads or getattr(envs, "UPLOADER_NUM_THREADS", 10)
            )
            logger.info(
                f"Uploader {self.__class__.__name__} upload "
                f"{len(filtered_files)} files from {local_path} "
                f"to {self.model_uri} using {num_threads} threads."
            )

            with ThreadPoolExecutor(num_threads) as executor:
                futures = [
                    executor.submit(
                        self.upload,
                        local_file=Path(file),
                        bucket_path=str(Path(self.bucket_path).joinpath(Path(file).relative_to(local_path))),
                        bucket_name=self.bucket_name,
                        enable_multipart=False,
                    )
                    for file in filtered_files
                ]
                # Wait for completion and surface any exceptions from workers.
                wait(futures)
                for f in futures:
                    f.result()

        else:
            logger.info(
                f"Uploader {self.__class__.__name__} upload "
                f"{len(filtered_files)} files from {local_path} "
                f"to {self.model_uri} using multipart upload."
            )
            for file in filtered_files:
                # use multipart upload to speedup upload
                self.upload(
                    local_file=Path(file),
                    bucket_path=str(Path(self.bucket_path).joinpath(Path(file).relative_to(local_path))),
                    bucket_name=self.bucket_name,
                    enable_multipart=True
                )

    def upload_path(self, local_path: str):
        if local_path is None:
            raise ValueError("local_path must be provided for upload")

        local_path_obj = Path(local_path)
        if not local_path_obj.exists():
            raise ValueError(f"Local path {local_path} does not exist")

        st = time.perf_counter()
        if self._is_directory(local_path_obj):
            self.upload_directory(local_path=local_path_obj)
        else:
            # If it's a single file, use the filename as the bucket path
            filename = local_path_obj.name
            self.upload(
                local_file=local_path_obj,
                bucket_path=str(Path(self.bucket_path).joinpath(filename)),
                bucket_name=self.bucket_name,
                enable_multipart=self._support_multipart_upload(),
            )
        duration = time.perf_counter() - st
        logger.info(
            f"Uploader {self.__class__.__name__} upload "
            f"from {local_path} to {self.model_uri} "
            f"duration: {duration:.2f} seconds."
        )
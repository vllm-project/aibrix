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
from typing import List, Optional

from aibrix import envs
from aibrix.logger import init_logger

logger = init_logger(__name__)


@dataclass
class BaseDownloader(ABC):
    """Base class for downloader."""

    model_uri: str
    model_name: str
    bucket_path: str
    bucket_name: Optional[str]
    enable_progress_bar: bool = False
    allow_file_suffix: Optional[List[str]] = field(
        default_factory=lambda: envs.DOWNLOADER_ALLOW_FILE_SUFFIX
    )

    def __post_init__(self):
        # valid downloader config
        self._valid_config()
        self.model_name_path = self.model_name

    @abstractmethod
    def _valid_config(self):
        pass

    @abstractmethod
    def _is_directory(self) -> bool:
        """Check if model_uri is a directory."""
        pass

    @abstractmethod
    def _directory_list(self, path: str) -> List[str]:
        pass

    @abstractmethod
    def _support_range_download(self) -> bool:
        pass

    @abstractmethod
    def download(
        self,
        local_path: Path,
        bucket_path: str,
        bucket_name: Optional[str] = None,
        enable_range: bool = True,
    ):
        pass

    def download_directory(self, local_path: Path):
        """Download directory from model_uri to local_path.
        Overwrite the method directly when there is a corresponding download
        directory method for ``Downloader``. Otherwise, the following logic will be
        used to download the directory.
        """
        directory_list = self._directory_list(self.bucket_path)
        if self.allow_file_suffix is None:
            logger.info(f"All files from {self.bucket_path} will be downloaded.")
            filtered_files = directory_list
        else:
            filtered_files = [
                file
                for file in directory_list
                if any(file.endswith(suffix) for suffix in self.allow_file_suffix)
            ]

        if not self._support_range_download():
            # download using multi threads
            num_threads = envs.DOWNLOADER_NUM_THREADS
            logger.info(
                f"Downloader {self.__class__.__name__} download "
                f"{len(filtered_files)} files from {self.model_uri} "
                f"use {num_threads} threads to download."
            )

            executor = ThreadPoolExecutor(num_threads)
            futures = [
                executor.submit(
                    self.download,
                    local_path=local_path,
                    bucket_path=file,
                    bucket_name=self.bucket_name,
                    enable_range=False,
                )
                for file in filtered_files
            ]
            wait(futures)

        else:
            logger.info(
                f"Downloader {self.__class__.__name__} download "
                f"{len(filtered_files)} files from {self.model_uri} "
                f"using range support methods."
            )
            for file in filtered_files:
                # use range download to speedup download
                self.download(local_path, file, self.bucket_name, True)

    def download_model(self, local_path: Optional[str] = None):
        if local_path is None:
            local_path = envs.DOWNLOADER_LOCAL_DIR
            Path(local_path).mkdir(parents=True, exist_ok=True)

        # ensure model local path exists
        model_path = Path(local_path).joinpath(self.model_name_path)
        model_path.mkdir(parents=True, exist_ok=True)

        # TODO check local file exists
        st = time.perf_counter()
        if self._is_directory():
            self.download_directory(local_path=model_path)
        else:
            self.download(
                local_path=model_path,
                bucket_path=self.bucket_path,
                bucket_name=self.bucket_name,
                enable_range=self._support_range_download(),
            )
        duration = time.perf_counter() - st
        logger.info(
            f"Downloader {self.__class__.__name__} download "
            f"from {self.model_uri} "
            f"duration: {duration:.2f} seconds."
        )

        return model_path

    def __hash__(self):
        return hash(tuple(self.__dict__))


def get_downloader(
    model_uri: str, model_name: Optional[str] = None, enable_progress_bar: bool = False
) -> BaseDownloader:
    """Get downloader for model_uri."""
    if re.match(envs.DOWNLOADER_S3_REGEX, model_uri):
        from aibrix.downloader.s3 import S3Downloader

        return S3Downloader(model_uri, model_name, enable_progress_bar)
    elif re.match(envs.DOWNLOADER_TOS_REGEX, model_uri):
        from aibrix.downloader.tos import TOSDownloader

        return TOSDownloader(model_uri, model_name, enable_progress_bar)
    else:
        from aibrix.downloader.huggingface import HuggingFaceDownloader

        return HuggingFaceDownloader(model_uri, model_name, enable_progress_bar)

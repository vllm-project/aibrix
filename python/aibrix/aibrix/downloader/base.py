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
    required_envs: List[str] = field(default_factory=list)
    optional_envs: List[str] = field(default_factory=list)
    allow_file_suffix: List[str] = field(default_factory=list)

    def __post_init__(self):
        # ensure downloader required envs are set
        self._check_config()
        self.model_name_path = self.model_name.replace("/", "_")

    @abstractmethod
    def _check_config(self):
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
    def download(self, path: str, local_path: Path, enable_range: bool = True):
        pass

    def download_directory(self, local_path: Path):
        """Download directory from model_uri to local_path.
        Overwrite the method directly when there is a corresponding download
        directory method for ``Downloader``. Otherwise, the following logic will be
        used to download the directory.
        """
        directory_list = self._directory_list(self.model_uri)
        if len(self.allow_file_suffix) == 0:
            logger.info("All files from {self.model_uri} will be downloaded.")
            filtered_files = directory_list
        else:
            filtered_files = [
                file
                for file in directory_list
                if any(file.endswith(suffix) for suffix in self.allow_file_suffix)
            ]

        if not self._support_range_download():
            # download using multi threads
            st = time.perf_counter()
            num_threads = envs.DOWNLOADER_NUM_THREADS
            logger.info(
                f"Downloader {self.__class__.__name__} does not support "
                f"range download, use {num_threads} threads to download."
            )

            executor = ThreadPoolExecutor(num_threads)
            futures = [
                executor.submit(
                    self.download, path=file, local_path=local_path, enable_range=False
                )
                for file in filtered_files
            ]
            wait(futures)
            duration = time.perf_counter() - st
            logger.info(
                f"Downloader {self.__class__.__name__} download "
                f"{len(filtered_files)} files from {self.model_uri} "
                f"using {num_threads} threads, "
                f"duration: {duration:.2f} seconds."
            )

        else:
            st = time.perf_counter()
            for file in filtered_files:
                # use range download to speedup download
                self.download(file, local_path, True)
            duration = time.perf_counter() - st
            logger.info(
                f"Downloader {self.__class__.__name__} download "
                f"{len(filtered_files)} files from {self.model_uri} "
                f"using range support methods, "
                f"duration: {duration:.2f} seconds."
            )

    def download_model(self, local_path: Optional[str]):
        if local_path is None:
            local_path = envs.DOWNLOADER_LOCAL_DIR
            Path(local_path).mkdir(parents=True, exist_ok=True)

        # ensure model local path exists
        model_path = Path(local_path).joinpath(self.model_name_path)
        model_path.mkdir(parents=True, exist_ok=True)

        # TODO check local file exists

        if self._is_directory():
            self.download_directory(model_path)
        else:
            self.download(self.model_uri, model_path)
        return model_path


def get_downloader(model_uri: str) -> BaseDownloader:
    """Get downloader for model_uri."""
    if re.match(envs.DOWNLOADER_S3_REGEX, model_uri):
        from aibrix.downloader.s3 import S3Downloader

        return S3Downloader(model_uri)
    elif re.match(envs.DOWNLOADER_TOS_REGEX, model_uri):
        from aibrix.downloader.tos import TOSDownloader

        return TOSDownloader(model_uri)
    else:
        from aibrix.downloader.huggingface import HuggingFaceDownloader

        return HuggingFaceDownloader(model_uri)

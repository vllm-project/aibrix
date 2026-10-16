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
from aibrix.common.errors import ModelNotFoundError
from aibrix.downloader.entity import RemoteSource
from aibrix.logger import init_logger

logger = init_logger(__name__)


@dataclass
class DownloadExtraConfig:
    """Downloader extra config."""

    # Auth config for s3 or tos
    ak: Optional[str] = None
    sk: Optional[str] = None
    endpoint: Optional[str] = None
    region: Optional[str] = None

    # Auth config for huggingface
    hf_endpoint: Optional[str] = None
    hf_token: Optional[str] = None
    hf_revision: Optional[str] = None

    # parrallel config
    num_threads: Optional[int] = None
    max_io_queue: Optional[int] = None
    io_chunksize: Optional[int] = None
    part_threshold: Optional[int] = None
    part_chunksize: Optional[int] = None

    # other config
    allow_file_suffix: Optional[List[str]] = None
    # Suffixes always downloaded even when ``allow_file_suffix`` is set, for
    # small runtime-critical files (remote code, tokenizer vocabularies, chat
    # templates). ``None`` falls back to envs.DOWNLOADER_ALWAYS_ALLOW_FILE_SUFFIX;
    # an empty list disables the mechanism.
    always_allow_file_suffix: Optional[List[str]] = None
    force_download: Optional[bool] = None


DEFAULT_DOWNLOADER_EXTRA_CONFIG = DownloadExtraConfig()


@dataclass
class BaseDownloader(ABC):
    """Base class for downloader."""

    model_uri: str
    model_name: str
    bucket_path: str
    bucket_name: Optional[str]
    download_extra_config: DownloadExtraConfig = field(
        default_factory=DownloadExtraConfig
    )
    enable_progress_bar: bool = False
    _source: ClassVar[RemoteSource] = RemoteSource.UNKNOWN

    def __post_init__(self):
        # valid downloader config
        self._valid_config()
        self.model_name_path = self.model_name
        self.allow_file_suffix = (
            self.download_extra_config.allow_file_suffix
            or envs.DOWNLOADER_ALLOW_FILE_SUFFIX
        )
        always_allow = self.download_extra_config.always_allow_file_suffix
        self.always_allow_file_suffix = (
            envs.DOWNLOADER_ALWAYS_ALLOW_FILE_SUFFIX
            if always_allow is None
            else always_allow
        )
        self.force_download = (
            self.download_extra_config.force_download or envs.DOWNLOADER_FORCE_DOWNLOAD
        )

    @property
    def source(self) -> RemoteSource:
        return self._source

    @property
    def effective_allow_file_suffix(self) -> Optional[List[str]]:
        """Suffix filter actually applied to downloads.

        When a user-specified ``allow_file_suffix`` filter is set, small
        runtime-critical files (remote code ``*.py``, tokenizer vocabularies
        ``*.txt``/``*.model``/``*.tiktoken`` and chat templates ``*.jinja``)
        are always kept, since the filter is meant to exclude large weight
        files only. See https://github.com/vllm-project/aibrix/issues/834.

        Suffixes are normalized (whitespace stripped, leading dots removed)
        so that ``".json"`` and ``"json"`` behave identically; otherwise the
        HuggingFace pattern construction would produce ``*..json``, which
        silently matches nothing.

        This merged list feeds the HuggingFace ``*.{suffix}`` patterns. The
        S3/TOS path uses :meth:`_matches_allow_file_suffix` instead, which
        matches always-allow suffixes at the dot boundary.
        """
        if self.allow_file_suffix is None or len(self.allow_file_suffix) == 0:
            return None

        merged: List[str] = []
        for suffix in self._normalize_suffixes(
            self.allow_file_suffix
        ) + self._normalize_suffixes(self.always_allow_file_suffix):
            if suffix not in merged:
                merged.append(suffix)
        return merged or None

    @staticmethod
    def _normalize_suffixes(suffixes: Optional[List[str]]) -> List[str]:
        """Strip whitespace and leading dots, drop empty strings."""
        return [
            s for s in (suffix.strip().lstrip(".") for suffix in suffixes or []) if s
        ]

    def _matches_allow_file_suffix(self, file: str) -> bool:
        """Whether ``file`` passes the suffix filter on the S3/TOS path.

        User-specified suffixes keep the historical bare ``endswith``
        semantics, since some users pass whole-name suffixes such as
        ``tokenizer.json``. Always-allow suffixes are matched at the dot
        boundary (``"." + suffix``) so defaults like ``py`` or ``model`` do
        not over-match ``*.npy`` arrays or extensionless ``*_model`` files;
        this keeps the path consistent with the HuggingFace ``*.{suffix}``
        pattern semantics. See the review on #3119.
        """
        if any(
            file.endswith(suffix)
            for suffix in self._normalize_suffixes(self.allow_file_suffix)
        ):
            return True
        return any(
            file.endswith("." + suffix)
            for suffix in self._normalize_suffixes(self.always_allow_file_suffix)
        )

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
        # Check existence before excluding directory markers or unwanted suffixes.
        if not directory_list:
            raise ModelNotFoundError(
                model_uri=self.model_uri,
                detail_msg="No objects found under the prefix.",
            )
        # filter the directory path
        files = [file for file in directory_list if not file.endswith("/")]

        effective_suffix = self.effective_allow_file_suffix
        if effective_suffix is None:
            logger.info(f"All files from {self.bucket_path} will be downloaded.")
            filtered_files = files
        else:
            filtered_files = [
                file for file in files if self._matches_allow_file_suffix(file)
            ]

        if not self._support_range_download():
            # download using multi threads
            num_threads = (
                self.download_extra_config.num_threads or envs.DOWNLOADER_NUM_THREADS
            )
            logger.info(
                f"Downloader {self.__class__.__name__} download "
                f"{len(filtered_files)} files from {self.model_uri} "
                f"use {num_threads} threads to download."
            )

            with ThreadPoolExecutor(num_threads) as executor:
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
                # Wait for completion and surface any exceptions from workers.
                wait(futures)
                for f in futures:
                    f.result()

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
    model_uri: str,
    model_name: Optional[str] = None,
    download_extra_config: Optional[Dict] = None,
    enable_progress_bar: bool = False,
) -> BaseDownloader:
    """Get downloader for model_uri."""
    download_config: DownloadExtraConfig = (
        DEFAULT_DOWNLOADER_EXTRA_CONFIG
        if download_extra_config is None
        else DownloadExtraConfig(**download_extra_config)
    )

    if re.match(envs.DOWNLOADER_S3_REGEX, model_uri):
        from aibrix.downloader.s3 import S3Downloader

        return S3Downloader(model_uri, model_name, download_config, enable_progress_bar)
    elif re.match(envs.DOWNLOADER_TOS_REGEX, model_uri):
        if envs.DOWNLOADER_TOS_VERSION == "v1":
            from aibrix.downloader.tos import TOSDownloaderV1

            return TOSDownloaderV1(
                model_uri, model_name, download_config, enable_progress_bar
            )
        else:
            from aibrix.downloader.tos import TOSDownloaderV2

            return TOSDownloaderV2(
                model_uri, model_name, download_config, enable_progress_bar
            )
    else:
        from aibrix.downloader.huggingface import HuggingFaceDownloader

        return HuggingFaceDownloader(
            model_uri, model_name, download_config, enable_progress_bar
        )

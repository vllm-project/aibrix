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

from pathlib import Path
from typing import List, Optional

from huggingface_hub import HfApi, hf_hub_download, snapshot_download

from aibrix import envs
from aibrix.common.errors import (
    ArgNotCongiuredError,
    ArgNotFormatError,
    ModelNotFoundError,
)
from aibrix.downloader.base import (
    DEFAULT_DOWNLOADER_EXTRA_CONFIG,
    BaseDownloader,
    DownloadExtraConfig,
)
from aibrix.logger import init_logger

logger = init_logger(__name__)


def _parse_model_name_from_uri(model_uri: str) -> str:
    return model_uri


class HuggingFaceDownloader(BaseDownloader):
    def __init__(
        self,
        model_uri: str,
        model_name: Optional[str] = None,
        download_extra_config: DownloadExtraConfig = DEFAULT_DOWNLOADER_EXTRA_CONFIG,
        enable_progress_bar: bool = False,
    ):
        if model_name is None:
            model_name = _parse_model_name_from_uri(model_uri)
            logger.info(f"model_name is not set, using `{model_name}` as model_name")

        self.download_extra_config = download_extra_config

        self.hf_token = self.download_extra_config.hf_token or envs.DOWNLOADER_HF_TOKEN
        self.hf_endpoint = (
            self.download_extra_config.hf_endpoint or envs.DOWNLOADER_HF_ENDPOINT
        )
        self.hf_revision = (
            self.download_extra_config.hf_revision or envs.DOWNLOADER_HF_REVISION
        )
        self.hf_api = HfApi(endpoint=self.hf_endpoint, token=self.hf_token)

        super().__init__(
            model_uri=model_uri,
            model_name=model_name,
            bucket_path=model_uri,
            bucket_name=None,
            download_extra_config=download_extra_config,
            enable_progress_bar=enable_progress_bar,
        )  # type: ignore

        # Dependent on the attributes generated in the base class,
        # so place it after the super().__init__() call.
        self.allow_patterns = (
            None
            if self.allow_file_suffix is None or len(self.allow_file_suffix) == 0
            else [f"*.{suffix}" for suffix in self.allow_file_suffix]
        )
        logger.debug(
            f"Downloader {self.__class__.__name__} initialized."
            f"HF Settings are followed: \n"
            f"hf_token={self.hf_token}, \n"
            f"hf_endpoint={self.hf_endpoint}"
        )

    def _valid_config(self):
        if len(self.model_uri.split("/")) != 2:
            raise ArgNotFormatError(arg_name="model_uri", expected_format="repo/name")

        if self.model_name is None:
            raise ArgNotCongiuredError(arg_name="model_name", arg_source="--model-name")

        if not self.hf_api.repo_exists(repo_id=self.model_uri):
            raise ModelNotFoundError(model_uri=self.model_uri)

    def _is_directory(self) -> bool:
        """Check if model_uri is a directory.
        model_uri in `repo/name` format must be a directory.
        """
        return True

    def _directory_list(self, path: str) -> List[str]:
        return self.hf_api.list_repo_files(repo_id=self.model_uri)

    def _support_range_download(self) -> bool:
        return False

    def download(
        self,
        local_path: Path,
        bucket_path: str,
        bucket_name: Optional[str] = None,
        enable_range: bool = True,
    ):
        hf_hub_download(
            repo_id=self.model_uri,
            filename=bucket_path,
            local_dir=local_path,
            revision=self.hf_revision,
            token=self.hf_token,
            endpoint=self.hf_endpoint,
            local_dir_use_symlinks=False,
            force_download=self.force_download,
        )

    def download_directory(self, local_path: Path):
        max_workers = (
            self.download_extra_config.num_threads or envs.DOWNLOADER_NUM_THREADS
        )
        snapshot_download(
            self.model_uri,
            local_dir=local_path,
            revision=self.hf_revision,
            token=self.hf_token,
            allow_patterns=self.allow_patterns,
            max_workers=max_workers,
            endpoint=self.hf_endpoint,
            local_dir_use_symlinks=False,
            force_download=self.force_download,
        )

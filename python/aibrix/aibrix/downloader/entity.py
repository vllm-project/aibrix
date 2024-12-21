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


import contextlib
from dataclasses import dataclass
from enum import Enum
from pathlib import Path
from typing import Generator, List

from filelock import BaseFileLock, FileLock, Timeout

from aibrix.config import DOWNLOAD_CACHE_DIR, DOWNLOAD_FILE_LOCK_CHECK_TIMEOUT
from aibrix.logger import init_logger

logger = init_logger(__name__)


class RemoteSource(Enum):
    S3 = "s3"
    TOS = "tos"
    HUGGINGFACE = "huggingface"


class DownloadStatus(Enum):
    DOWNLOADING = "downloading"
    DOWNLOADED = "downloaded"
    NO_OPETATION = "no_operation"  # Interrupted from downloading
    UNKNOWN = "unknown"


@dataclass
class DownloadFile:
    file_path: Path
    lock_path: Path
    metadata_path: Path

    @property
    def status(self):
        if self.file_path.exists() and self.metadata_path.exists():
            return DownloadStatus.DOWNLOADED

        try:
            # Downloading process will acquire the lock,
            # and other process will raise Timeout Exception
            lock = FileLock(self.lock_path)
            lock.acquire(blocking=False)
        except Timeout:
            return DownloadStatus.DOWNLOADING
        except Exception:
            return DownloadStatus.UNKNOWN
        else:
            return DownloadStatus.NO_OPETATION

    @contextlib.contextmanager
    def download_lock(self) -> Generator[BaseFileLock, None, None]:
        """Download process should acquire the lock to prevent other process from downloading the same file.
        Same implementation as WeakFileLock in huggingface_hub"""
        lock = FileLock(self.lock_path, timeout=DOWNLOAD_FILE_LOCK_CHECK_TIMEOUT)
        while True:
            try:
                lock.acquire()
            except Timeout:
                logger.info(f"still waiting to acquire lock on {self.lock_path}, status is {self.status}")
            else:
                break

        yield lock

        try:
            return lock.release()
        except OSError:
            try:
                Path(self.lock_path).unlink()
            except OSError:
                pass


@dataclass
class DownloadModel:
    model_source: RemoteSource
    local_dir: Path
    model_name: str
    download_files: List[DownloadFile]
    
    def __post_init__(self):
        if len(self.download_files) == 0:
            logger.warning(f"No download files found for model {self.model_name}")

    @property
    def status(self):
        all_status = []
        for file in self.download_files:
            file_status = file.status
            if file_status == DownloadStatus.DOWNLOADING:
                return DownloadStatus.DOWNLOADING
            elif file_status == DownloadStatus.NO_OPETATION:
                return DownloadStatus.NO_OPETATION
            elif file_status == DownloadStatus.UNKNOWN:
                return DownloadStatus.UNKNOWN
            else:
                all_status.append(file.file_status)
        if all(status == DownloadStatus.DOWNLOADED for status in all_status):
            return DownloadStatus.DOWNLOADED
        return DownloadStatus.UNKNOWN

    @classmethod
    def infer_from_model_path(cls, local_path: Path, model_name: str) -> "DownloadModel":
        # TODO, infer downloadfiles from cached dir
        pass
    
    @classmethod
    def infer_from_local_path(cls, remote_path: Path) -> List["DownloadModel"]:
        # TODO
        pass


def get_local_download_paths(
    model_base_dir: Path, filename: str, source_type: RemoteSource
) -> DownloadFile:
    file_path = model_base_dir.joinpath(filename)
    cache_dir = model_base_dir.joinpath(DOWNLOAD_CACHE_DIR % source_type.value)
    lock_path = cache_dir.joinpath(f"{filename}.lock")
    metadata_path = cache_dir.joinpath(f"{filename}.metadata")
    return DownloadFile(file_path, lock_path, metadata_path)

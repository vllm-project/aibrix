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
from typing import Generator, List, Optional

from filelock import BaseFileLock, FileLock, Timeout

from aibrix.config import DOWNLOAD_CACHE_DIR, DOWNLOAD_FILE_LOCK_CHECK_TIMEOUT
from aibrix.logger import init_logger

logger = init_logger(__name__)


class RemoteSource(Enum):
    S3 = "s3"
    TOS = "tos"
    HUGGINGFACE = "huggingface"
    UNKNOWN = "unknown"

    def __str__(self):
        return self.value


class FileDownloadStatus(Enum):
    DOWNLOADING = "downloading"
    DOWNLOADED = "downloaded"
    NO_OPERATION = "no_operation"  # Interrupted from downloading
    UNKNOWN = "unknown"

    def __str__(self):
        return self.value


class ModelDownloadStatus(Enum):
    NOT_EXIST = "not_exist"
    DOWNLOADING = "downloading"
    DOWNLOADED = "downloaded"
    NO_OPERATION = "no_operation"  # Interrupted from downloading
    UNKNOWN = "unknown"

    def __str__(self):
        return self.value


@dataclass
class DownloadFile:
    file_path: Path
    lock_path: Path
    metadata_path: Path

    @property
    def status(self):
        if self.file_path.exists() and self.metadata_path.exists():
            return FileDownloadStatus.DOWNLOADED

        try:
            # Downloading process will acquire the lock,
            # and other process will raise Timeout Exception
            lock = FileLock(self.lock_path)
            lock.acquire(blocking=False)
        except Timeout:
            return FileDownloadStatus.DOWNLOADING
        except Exception as e:
            logger.warning(f"Failed to acquire lock failed for error: {e}")
            return FileDownloadStatus.UNKNOWN
        else:
            return FileDownloadStatus.NO_OPERATION

    @contextlib.contextmanager
    def download_lock(self) -> Generator[BaseFileLock, None, None]:
        """A filelock that download process should be acquired.
        Same implementation as WeakFileLock in huggingface_hub."""
        lock = FileLock(self.lock_path, timeout=DOWNLOAD_FILE_LOCK_CHECK_TIMEOUT)
        while True:
            try:
                lock.acquire()
            except Timeout:
                logger.info(
                    f"Still waiting to acquire download lock on {self.lock_path}"
                )
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
    local_path: Path
    model_name: str
    download_files: List[DownloadFile]

    def __post_init__(self):
        if len(self.download_files) == 0:
            logger.warning(f"No download files found for model {self.model_name}")

    @property
    def status(self):
        all_status = [file.status for file in self.download_files]
        if all(status == FileDownloadStatus.DOWNLOADED for status in all_status):
            return ModelDownloadStatus.DOWNLOADED

        if any(status == FileDownloadStatus.DOWNLOADING for status in all_status):
            return ModelDownloadStatus.DOWNLOADING

        if all(
            status in [FileDownloadStatus.DOWNLOADED, FileDownloadStatus.NO_OPERATION]
            for status in all_status
        ):
            return ModelDownloadStatus.NO_OPERATION

        return ModelDownloadStatus.UNKNOWN

    @property
    def model_root_path(self) -> Path:
        return Path(self.local_path).joinpath(self.model_name)

    @classmethod
    def infer_from_model_path(
        cls, local_path: Path, model_name: str, source: RemoteSource
    ) -> Optional["DownloadModel"]:
        assert source is not None

        model_base_dir = Path(local_path).joinpath(model_name)

        # model not exists
        if not model_base_dir.exists():
            return None

        cache_sub_dir = (DOWNLOAD_CACHE_DIR % source.value).strip("/")
        cache_dir = Path(model_base_dir).joinpath(cache_sub_dir)
        lock_files = list(Path(cache_dir).glob("*.lock"))

        download_files = []
        for lock_file in lock_files:
            lock_name = lock_file.name
            lock_suffix = ".lock"
            if lock_name.endswith(lock_suffix):
                filename = lock_name[: -len(lock_suffix)]
                download_file = get_local_download_paths(
                    model_base_dir=model_base_dir, filename=filename, source=source
                )
                download_files.append(download_file)

        return cls(
            model_source=source,
            local_path=local_path,
            model_name=model_name,
            download_files=download_files,
        )

    @classmethod
    def infer_from_local_path(cls, local_path: Path) -> List["DownloadModel"]:
        models: List["DownloadModel"] = []

        def remove_suffix(input_string, suffix):
            if not input_string.endswith(suffix):
                return input_string
            return input_string[: -len(suffix)]

        for source in RemoteSource:
            cache_sub_dir = (DOWNLOAD_CACHE_DIR % source.value).strip("/")
            cache_dirs = list(Path(local_path).glob(f"**/{cache_sub_dir}"))
            if not cache_dirs:
                continue

            for cache_dir in cache_dirs:
                relative_path = cache_dir.relative_to(local_path)
                relative_str = str(relative_path).strip("/")
                model_name = remove_suffix(relative_str, cache_sub_dir).strip("/")

                download_model = cls.infer_from_model_path(
                    local_path, model_name, source
                )
                if download_model is None:
                    continue
                models.append(download_model)

        return models

    def __str__(self):
        return (
            "DownloadModel(\n"
            + f"\tmodel_source={self.model_source.value},\n"
            + f"\tmodel_name={self.model_name},\n"
            + f"\tstatus={self.status.value},\n"
            + f"\tlocal_path={self.local_path},\n"
            + f"\tdownload_files_count={len(self.download_files)}\n)"
        )


def get_local_download_paths(
    model_base_dir: Path, filename: str, source: RemoteSource
) -> DownloadFile:
    file_path = model_base_dir.joinpath(filename)
    sub_cache_dir = (DOWNLOAD_CACHE_DIR % source.value).strip("/")
    cache_dir = model_base_dir.joinpath(sub_cache_dir)
    lock_path = cache_dir.joinpath(f"{filename}.lock")
    metadata_path = cache_dir.joinpath(f"{filename}.metadata")
    return DownloadFile(file_path, lock_path, metadata_path)

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
from typing import ClassVar, List, Optional, Tuple
from urllib.parse import urlparse

from google.auth.exceptions import DefaultCredentialsError
from google.cloud import storage
from requests.adapters import DEFAULT_POOLSIZE, HTTPAdapter

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


def _parse_bucket_info_from_uri(uri: str) -> Tuple[str, str]:
    parsed = urlparse(uri)
    bucket_name = parsed.netloc
    bucket_path = parsed.path.lstrip("/")
    return bucket_name, bucket_path


class GCSDownloader(BaseDownloader):
    """Download a model from Google Cloud Storage (``gcs://`` or ``gs://``).

    Credentials come from Application Default Credentials, e.g.
    ``GOOGLE_APPLICATION_CREDENTIALS`` or GKE Workload Identity.
    """

    _source: ClassVar[RemoteSource] = RemoteSource.GCS

    def __init__(
        self,
        model_uri,
        model_name: Optional[str] = None,
        download_extra_config: DownloadExtraConfig = DEFAULT_DOWNLOADER_EXTRA_CONFIG,
        enable_progress_bar: bool = False,
    ):
        if model_name is None:
            model_name = infer_model_name(model_uri)
            logger.info(f"model_name is not set, using `{model_name}` as model_name")

        self.download_extra_config = download_extra_config
        bucket_name, bucket_path = _parse_bucket_info_from_uri(model_uri)

        # Reading objects needs no project. Passing None stops the client from
        # failing when the credentials do not name one.
        try:
            self.client = storage.Client(project=None)
        except DefaultCredentialsError as e:
            raise ModelNotFoundError(
                model_uri=model_uri,
                detail_msg=f"GCS credentials not found: {e}",
            )

        # base.download_directory fetches up to num_threads files at once over
        # this one client, but requests keeps DEFAULT_POOLSIZE (10) connections
        # per host and discards the rest with a "Connection pool is full"
        # warning. A mutual TLS adapter, mounted when client certificates are
        # enabled, is left in place.
        num_threads = (
            self.download_extra_config.num_threads or envs.DOWNLOADER_NUM_THREADS
        )
        session = self.client._http
        if (
            num_threads > DEFAULT_POOLSIZE
            and type(session.get_adapter("https://")) is HTTPAdapter
        ):
            session.mount("https://", HTTPAdapter(pool_maxsize=num_threads))

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

        # Check access by listing rather than reading bucket metadata: the
        # read-only object roles (e.g. roles/storage.objectViewer) grant
        # storage.objects.list but not storage.buckets.get.
        try:
            next(
                iter(
                    self.client.list_blobs(
                        self.bucket_name, prefix=self.bucket_path, max_results=1
                    )
                ),
                None,
            )
        except Exception as e:
            logger.error(
                f"Failed to access GCS bucket {self.bucket_name} in {self.model_uri}: {e}"
            )
            raise ModelNotFoundError(model_uri=self.model_uri, detail_msg=str(e))

    @lru_cache()
    def _is_directory(self) -> bool:
        """Check if model_uri is a directory."""
        if self.bucket_path.endswith("/"):
            return True
        # If the exact object exists and there are no objects under path+"/",
        # treat it as a file. Otherwise, it's a directory-like prefix.
        key_exists = (
            self.client.bucket(self.bucket_name).get_blob(self.bucket_path) is not None
        )
        prefix = self.bucket_path + "/"
        children = self.client.list_blobs(
            self.bucket_name, prefix=prefix, max_results=1
        )
        if next(iter(children), None) is not None:
            return True
        return not key_exists

    def _directory_list(self, path: str) -> List[str]:
        # The iterator fetches further pages as it is consumed.
        prefix = path if path.endswith("/") else f"{path}/"
        return [
            blob.name
            for blob in self.client.list_blobs(self.bucket_name, prefix=prefix)
        ]

    def _support_range_download(self) -> bool:
        # Files are fetched whole, one per thread, by download_directory.
        return False

    def download(
        self,
        local_path: Path,
        bucket_path: str,
        bucket_name: Optional[str] = None,
        enable_range: bool = True,
    ):
        blob = self.client.bucket(bucket_name).get_blob(bucket_path)
        if blob is None:
            raise ValueError(f"GCS bucket path {bucket_path} not exist.")

        if self._is_directory():
            prefix = self.bucket_path.rstrip("/") + "/"
            # For directory downloads, calculate path relative to the directory.
            relative_path = (
                bucket_path[len(prefix) :]
                if bucket_path.startswith(prefix)
                else bucket_path
            )
        else:
            # For single file downloads, just use the filename.
            relative_path = bucket_path.split("/")[-1]
        local_file = local_path.joinpath(relative_path).absolute()
        # Ensure parent directories exist
        local_file.parent.mkdir(parents=True, exist_ok=True)
        tmp_file = (
            local_file.with_suffix(local_file.suffix + ".part")
            if local_file.suffix
            else Path(str(local_file) + ".part")
        )

        # check if file exist
        etag = blob.etag or ""
        file_size = blob.size or 0
        meta_data_file = meta_file(
            local_path=local_path, file_name=relative_path, source=self._source.value
        )

        if not need_to_download(
            local_file, meta_data_file, file_size, etag, self.force_download
        ):
            return

        download_file = get_local_download_paths(
            local_path, relative_path, self._source
        )
        with download_file.download_lock():
            # get_blob() pins the blob's generation, so the bytes written
            # match the etag saved below even if the object is replaced.
            blob.download_to_filename(str(tmp_file))
            # Atomically move into place then write metadata
            tmp_file.replace(local_file)
            save_meta_data(meta_data_file, etag)

# Copyright 2026 The Aibrix Team.
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

"""Fetch media referenced by a request into local files.

Remote artifacts are downloaded through ``aibrix.runtime.downloaders`` (tos,
s3, gs/gcs, hf, http(s)); inline ``data:;base64`` payloads are decoded to a
file. Each result lands in its own temporary directory under
``base_temp_dir`` and is reported as a ``file://`` URI of the *file* (not the
directory), which is what vLLM / SGLang / TRT-LLM media loaders expect.
"""

from __future__ import annotations

import asyncio
import logging
import os
import shutil
import tempfile
import uuid
from abc import ABC, abstractmethod
from dataclasses import dataclass
from pathlib import Path
from typing import Dict, List, Optional

from aibrix.openai_frontend.common.data_url import (
    DataURLError,
    decode_data_url,
    is_data_url,
)
from aibrix.runtime.downloaders import (
    ArtifactDownloader,
    await_thread_completion,
    get_downloader,
)

logger = logging.getLogger(__name__)

DEFAULT_MAX_DOWNLOAD_BYTES = 512 * 1024 * 1024


class ArtifactFetchError(RuntimeError):
    """A remote artifact referenced by a request could not be fetched."""

    def __init__(self, url: str, reason: str):
        super().__init__(f"Failed to fetch '{url}': {reason}")
        self.url = url
        self.reason = reason


@dataclass(frozen=True)
class FetchResult:
    original_url: str
    local_path: str  # file:// URI handed to the engine (or the URL unchanged)
    temp_dir: Optional[tempfile.TemporaryDirectory]  # None if not a temp download
    size_bytes: int = 0  # Fetched payload bytes; passthrough URLs contribute zero


class ArtifactFetcher(ABC):
    @abstractmethod
    async def fetch(
        self,
        url: str,
        credentials: Optional[Dict] = None,
    ) -> FetchResult: ...

    @abstractmethod
    async def fetch_many(
        self,
        urls: List[str],
        credentials_map: Optional[Dict[str, Dict]] = None,
    ) -> List[FetchResult]: ...

    @abstractmethod
    async def cleanup(self, results: List[FetchResult]) -> None: ...


# Downloader credential derivation per URL scheme.
# A provider returns a credentials dict understood by the matching downloader
# in aibrix.runtime.downloaders, or None when credentials are not configured.
# Registering a new scheme = adding one entry; the dispatch below is generic.
def _tos_downloader_credentials() -> Optional[Dict[str, str]]:
    # Read the live process env: --setenv-* is applied at CLI parse time,
    # after aibrix.envs may already have been imported and snapshotted.
    ak = os.environ.get("TOS_ACCESS_KEY")
    sk = os.environ.get("TOS_SECRET_KEY")
    if not ak or not sk:
        return None
    creds: Dict[str, str] = {"TOS_ACCESS_KEY": ak, "TOS_SECRET_KEY": sk}
    endpoint = os.environ.get("TOS_ENDPOINT")
    region = os.environ.get("TOS_REGION")
    if endpoint:
        creds["endpoint"] = endpoint
    if region:
        creds["region"] = region
    return creds


_DOWNLOADER_CREDENTIALS_PROVIDERS = {"tos": _tos_downloader_credentials}


def default_credentials_for(url: str) -> Optional[Dict]:
    """Credentials for ``url`` derived from the ``DOWNLOADER_*`` env vars.

    The download side is separate from the ``STORAGE_*`` family that configures
    output uploads. Some downloaders (TOS) require an explicit credentials
    dict; without this every such URL would fail unless the operator duplicated
    the keys into ``--proxy-credentials``. Schemes without a provider (s3, gs,
    hf, http rely on ambient/SDK credentials) return None.
    """
    scheme = url.split("://", 1)[0].lower() if "://" in url else ""
    provider = _DOWNLOADER_CREDENTIALS_PROVIDERS.get(scheme)
    return provider() if provider is not None else None


class DefaultArtifactFetcher(ArtifactFetcher):
    def __init__(
        self,
        base_temp_dir: Optional[str] = None,
        max_concurrent: int = 5,
        max_download_bytes: Optional[int] = DEFAULT_MAX_DOWNLOAD_BYTES,
    ):
        self._base_temp_dir = base_temp_dir
        if base_temp_dir:
            Path(base_temp_dir).mkdir(parents=True, exist_ok=True)
        self._max_concurrent = max_concurrent
        self._max_download_bytes = max_download_bytes
        # temp dir name -> handle, for cleanup on shutdown
        self._active_tmpdirs: Dict[str, tempfile.TemporaryDirectory] = {}

    async def fetch(
        self,
        url: str,
        credentials: Optional[Dict] = None,
    ) -> FetchResult:
        if is_data_url(url):
            return await self._materialize_data_url(url)
        try:
            downloader: ArtifactDownloader = get_downloader(url)
        except ValueError:
            return FetchResult(original_url=url, local_path=url, temp_dir=None)

        req_id = uuid.uuid4().hex[:8]
        tmpdir = tempfile.TemporaryDirectory(
            prefix=f"aibrix_artifact_{req_id}_",
            dir=self._base_temp_dir,
        )
        self._active_tmpdirs[tmpdir.name] = tmpdir
        succeeded = False
        try:
            creds = (
                credentials if credentials is not None else default_credentials_for(url)
            )
            await downloader.download(url, tmpdir.name, creds)
            local_file = self._resolve_downloaded_file(tmpdir.name, url)
            size = local_file.stat().st_size
            if self._max_download_bytes is not None:
                if size > self._max_download_bytes:
                    raise ArtifactFetchError(
                        url,
                        f"{size} bytes exceeds limit of {self._max_download_bytes}",
                    )
            result = FetchResult(
                original_url=url,
                local_path=local_file.resolve().as_uri(),
                temp_dir=tmpdir,
                size_bytes=size,
            )
            succeeded = True
            return result
        except ArtifactFetchError:
            raise
        except Exception as exc:
            raise ArtifactFetchError(url, f"{type(exc).__name__}: {exc}") from exc
        finally:
            # Until a FetchResult is returned, this call owns the directory.
            # Thread-backed downloaders settle their writer before cancellation.
            if not succeeded:
                self._discard(tmpdir)

    async def _materialize_data_url(self, url: str) -> FetchResult:
        """Decode an inline ``data:;base64`` media URL to a local file."""
        try:
            # SIMD decoding of a large payload still takes wall-clock time;
            # keep it off the event loop.
            decoded = await asyncio.to_thread(decode_data_url, url)
        except DataURLError as exc:
            raise ArtifactFetchError(url, str(exc)) from exc
        if self._max_download_bytes is not None:
            if len(decoded.data) > self._max_download_bytes:
                raise ArtifactFetchError(
                    url,
                    f"{len(decoded.data)} bytes exceeds limit of "
                    f"{self._max_download_bytes}",
                )

        req_id = uuid.uuid4().hex[:8]
        tmpdir = tempfile.TemporaryDirectory(
            prefix=f"aibrix_inline_{req_id}_",
            dir=self._base_temp_dir,
        )
        self._active_tmpdirs[tmpdir.name] = tmpdir
        local_file = Path(tmpdir.name) / f"inline_{req_id}{decoded.extension}"
        succeeded = False
        try:
            await await_thread_completion(
                asyncio.to_thread(local_file.write_bytes, decoded.data)
            )
            result = FetchResult(
                original_url=url,
                local_path=local_file.resolve().as_uri(),
                temp_dir=tmpdir,
                size_bytes=len(decoded.data),
            )
            succeeded = True
            return result
        except OSError as exc:
            raise ArtifactFetchError(url, f"{type(exc).__name__}: {exc}") from exc
        finally:
            if not succeeded:
                self._discard(tmpdir)

    @staticmethod
    def _resolve_downloaded_file(local_dir: str, url: str) -> Path:
        files = [p for p in Path(local_dir).rglob("*") if p.is_file()]
        files = [p for p in files if not p.name.endswith((".part", ".etag"))]
        if not files:
            raise ArtifactFetchError(url, "download produced no file")
        if len(files) > 1:
            raise ArtifactFetchError(
                url, "URL resolved to a directory; media URLs must name a single file"
            )
        return files[0]

    def _discard(self, tmpdir: tempfile.TemporaryDirectory) -> None:
        self._active_tmpdirs.pop(tmpdir.name, None)
        try:
            tmpdir.cleanup()
        except OSError:
            shutil.rmtree(tmpdir.name, ignore_errors=True)

    async def fetch_many(
        self,
        urls: List[str],
        credentials_map: Optional[Dict[str, Dict]] = None,
    ) -> List[FetchResult]:
        sem = asyncio.Semaphore(self._max_concurrent)

        async def _fetch_one(url: str) -> FetchResult:
            async with sem:
                creds = credentials_map.get(url) if credentials_map else None
                return await self.fetch(url, creds)

        tasks = [asyncio.ensure_future(_fetch_one(u)) for u in urls]
        try:
            return list(await asyncio.gather(*tasks))
        except BaseException:
            # Drain cancelled writers and clean successful siblings exactly once.
            async def cleanup_failed_batch() -> None:
                for task in tasks:
                    task.cancel()
                done = await asyncio.gather(*tasks, return_exceptions=True)
                await self.cleanup([r for r in done if isinstance(r, FetchResult)])

            cleanup_task = asyncio.create_task(cleanup_failed_batch())
            while not cleanup_task.done():
                try:
                    await asyncio.shield(cleanup_task)
                except asyncio.CancelledError:
                    continue
            cleanup_task.result()
            raise

    async def cleanup(self, results: List[FetchResult]) -> None:
        # Only this request's directories: other in-flight requests share the
        # fetcher and must keep theirs.
        for result in results:
            if result.temp_dir is not None:
                self._discard(result.temp_dir)

    def cleanup_all(self) -> None:
        for tmpdir in list(self._active_tmpdirs.values()):
            self._discard(tmpdir)

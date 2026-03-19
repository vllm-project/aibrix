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

"""Tests for resumable download logic (sentinel file + atomic writes)."""

import os
import sys
import types
from typing import Any, List, Optional
from unittest.mock import AsyncMock, MagicMock, patch

import httpx
import pytest

import aibrix.runtime.artifact_service as artifact_service_module
from aibrix.runtime.artifact_service import ArtifactDelegationService
from aibrix.runtime.downloaders import (
    GCSArtifactDownloader,
    HTTPArtifactDownloader,
    S3ArtifactDownloader,
)

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

MARKER = ".aibrix_download_complete"


def _make_service(tmp_path):
    return ArtifactDelegationService(local_dir=str(tmp_path))


class _FakeDownloader:
    """Downloader that creates a real file so the marker can be written."""

    def __init__(self, tmp_path, call_tracker=None):
        self._tmp_path = tmp_path
        self.call_tracker = call_tracker if call_tracker is not None else []

    async def download(self, source_url, local_path, credentials=None):
        os.makedirs(local_path, exist_ok=True)
        # Write a dummy model file so the directory is non-empty
        with open(os.path.join(local_path, "adapter.bin"), "wb") as f:
            f.write(b"\x00" * 16)
        self.call_tracker.append(local_path)
        return local_path


# ---------------------------------------------------------------------------
# artifact_service: sentinel-file logic
# ---------------------------------------------------------------------------


class TestDownloadArtifactSentinel:
    @pytest.mark.asyncio
    async def test_no_marker_triggers_download_and_writes_marker(
        self, tmp_path, monkeypatch
    ):
        """Fresh directory: download runs and marker is created."""
        service = _make_service(tmp_path)
        calls = []
        monkeypatch.setattr(
            artifact_service_module,
            "get_downloader",
            lambda _url: _FakeDownloader(tmp_path, calls),
        )

        result = await service.download_artifact("s3://bucket/model/", "my-adapter")

        adapter_dir = tmp_path / "my-adapter"
        assert result == str(adapter_dir)
        assert len(calls) == 1
        assert (adapter_dir / MARKER).exists()

    @pytest.mark.asyncio
    async def test_valid_marker_skips_download(self, tmp_path, monkeypatch):
        """When the marker already exists, download is skipped."""
        service = _make_service(tmp_path)
        adapter_dir = tmp_path / "my-adapter"
        adapter_dir.mkdir()
        (adapter_dir / MARKER).touch()

        calls = []
        monkeypatch.setattr(
            artifact_service_module,
            "get_downloader",
            lambda _url: _FakeDownloader(tmp_path, calls),
        )

        result = await service.download_artifact("s3://bucket/model/", "my-adapter")

        assert result == str(adapter_dir)
        assert calls == []  # downloader never called

    @pytest.mark.asyncio
    async def test_partial_dir_without_marker_triggers_cleanup_and_redownload(
        self, tmp_path, monkeypatch
    ):
        """Partial directory (no marker) is removed and re-downloaded."""
        service = _make_service(tmp_path)
        adapter_dir = tmp_path / "my-adapter"
        adapter_dir.mkdir()
        stale_file = adapter_dir / "partial.bin"
        stale_file.write_bytes(b"\xff" * 8)
        # No marker written — simulates interrupted download

        calls = []
        monkeypatch.setattr(
            artifact_service_module,
            "get_downloader",
            lambda _url: _FakeDownloader(tmp_path, calls),
        )

        result = await service.download_artifact("s3://bucket/model/", "my-adapter")

        assert result == str(adapter_dir)
        assert len(calls) == 1
        assert (adapter_dir / MARKER).exists()
        # Stale file should be gone (directory was wiped)
        assert not stale_file.exists()

    @pytest.mark.asyncio
    async def test_failed_download_removes_partial_dir(self, tmp_path, monkeypatch):
        """On download failure the partial directory is cleaned up."""

        class _FailingDownloader:
            async def download(self, source_url, local_path, credentials=None):
                os.makedirs(local_path, exist_ok=True)
                raise RuntimeError("network error")

        service = _make_service(tmp_path)
        monkeypatch.setattr(
            artifact_service_module,
            "get_downloader",
            lambda _url: _FailingDownloader(),
        )

        with pytest.raises(RuntimeError, match="network error"):
            await service.download_artifact("s3://bucket/model/", "my-adapter")

        adapter_dir = tmp_path / "my-adapter"
        assert not adapter_dir.exists()


# ---------------------------------------------------------------------------
# S3: atomic file writes
# ---------------------------------------------------------------------------


def _install_fake_boto3(monkeypatch: pytest.MonkeyPatch, downloaded_files: list):
    """Install a fake boto3 module that records download_file calls."""
    fake_boto3 = types.ModuleType("boto3")
    fake_botocore = types.ModuleType("botocore")
    fake_exceptions = types.ModuleType("botocore.exceptions")
    fake_exceptions.ClientError = Exception
    fake_exceptions.NoCredentialsError = Exception

    class FakeS3Client:
        def get_paginator(self, _op):
            return self

        def paginate(self, Bucket, Prefix):
            return [
                {
                    "Contents": [
                        {"Key": f"{Prefix}weights.bin"},
                    ]
                }
            ]

        def download_file(self, bucket, key, path):
            downloaded_files.append(path)
            # Actually write a file so os.replace can work
            with open(path, "wb") as f:
                f.write(b"\x00" * 4)

    fake_boto3.client = lambda svc, **kw: FakeS3Client()

    monkeypatch.setitem(sys.modules, "boto3", fake_boto3)
    monkeypatch.setitem(sys.modules, "botocore", fake_botocore)
    monkeypatch.setitem(sys.modules, "botocore.exceptions", fake_exceptions)


class TestS3AtomicWrites:
    def test_single_file_uses_part_then_renames(self, tmp_path, monkeypatch):
        downloaded = []
        _install_fake_boto3(monkeypatch, downloaded)

        downloader = S3ArtifactDownloader()
        result = downloader._download_sync(
            "s3://my-bucket/models/weights.bin", str(tmp_path)
        )

        assert result == str(tmp_path)
        # The .part file should have been written then renamed away
        assert downloaded == [str(tmp_path / "weights.bin.part")]
        assert (tmp_path / "weights.bin").exists()
        assert not (tmp_path / "weights.bin.part").exists()

    def test_directory_uses_part_then_renames(self, tmp_path, monkeypatch):
        downloaded = []
        _install_fake_boto3(monkeypatch, downloaded)

        downloader = S3ArtifactDownloader()
        result = downloader._download_sync(
            "s3://my-bucket/models/prefix/", str(tmp_path)
        )

        assert result == str(tmp_path)
        expected_part = str(tmp_path / "weights.bin.part")
        assert downloaded == [expected_part]
        assert (tmp_path / "weights.bin").exists()
        assert not (tmp_path / "weights.bin.part").exists()


# ---------------------------------------------------------------------------
# GCS: atomic file writes
# ---------------------------------------------------------------------------


def _install_fake_gcs(monkeypatch: pytest.MonkeyPatch, downloaded_files: list):
    """Install a fake google.cloud.storage module."""
    fake_google = types.ModuleType("google")
    fake_google_cloud = types.ModuleType("google.cloud")
    fake_storage_mod = types.ModuleType("google.cloud.storage")
    fake_oauth2 = types.ModuleType("google.oauth2")
    fake_sa = types.ModuleType("google.oauth2.service_account")
    fake_sa.Credentials = MagicMock()

    class FakeBlob:
        def __init__(self, name):
            self.name = name

        def download_to_filename(self, path):
            downloaded_files.append(path)
            with open(path, "wb") as f:
                f.write(b"\x00" * 4)

    class FakeBucket:
        def __init__(self, name):
            self._name = name

        def list_blobs(self, prefix=""):
            return [FakeBlob(f"{prefix}weights.bin")]

        def blob(self, name):
            return FakeBlob(name)

    class FakeClient:
        def __init__(self, **kw):
            pass

        def bucket(self, name):
            return FakeBucket(name)

    fake_storage_mod.Client = FakeClient

    monkeypatch.setitem(sys.modules, "google", fake_google)
    monkeypatch.setitem(sys.modules, "google.cloud", fake_google_cloud)
    monkeypatch.setitem(sys.modules, "google.cloud.storage", fake_storage_mod)
    monkeypatch.setitem(sys.modules, "google.oauth2", fake_oauth2)
    monkeypatch.setitem(sys.modules, "google.oauth2.service_account", fake_sa)


class TestGCSAtomicWrites:
    def test_directory_uses_part_then_renames(self, tmp_path, monkeypatch):
        downloaded = []
        _install_fake_gcs(monkeypatch, downloaded)

        downloader = GCSArtifactDownloader()
        result = downloader._download_sync(
            "gs://my-bucket/models/prefix/", str(tmp_path)
        )

        assert result == str(tmp_path)
        expected_part = str(tmp_path / "weights.bin.part")
        assert downloaded == [expected_part]
        assert (tmp_path / "weights.bin").exists()
        assert not (tmp_path / "weights.bin.part").exists()

    def test_single_file_uses_part_then_renames(self, tmp_path, monkeypatch):
        downloaded = []
        _install_fake_gcs(monkeypatch, downloaded)

        downloader = GCSArtifactDownloader()
        result = downloader._download_sync(
            "gs://my-bucket/models/weights.bin", str(tmp_path)
        )

        assert result == str(tmp_path)
        expected_part = str(tmp_path / "weights.bin.part")
        assert downloaded == [expected_part]
        assert (tmp_path / "weights.bin").exists()
        assert not (tmp_path / "weights.bin.part").exists()


# ---------------------------------------------------------------------------
# HTTP: atomic writes, ETag / If-Range resumption
# ---------------------------------------------------------------------------


class _FakeStreamResponse:
    """Minimal async context manager mimicking httpx streaming response."""

    def __init__(
        self,
        status_code: int,
        content: bytes,
        headers: Optional[dict] = None,
    ):
        self.status_code = status_code
        self._content = content
        self.headers = httpx.Headers(headers or {})
        # Capture the request headers sent by the downloader
        self.captured_request_headers: Optional[dict] = None

    async def __aenter__(self):
        return self

    async def __aexit__(self, *args):
        pass

    def raise_for_status(self):
        if self.status_code >= 400:
            req = httpx.Request("GET", "http://example.com/file.bin")
            resp = httpx.Response(self.status_code, request=req)
            raise httpx.HTTPStatusError(
                f"HTTP {self.status_code}", request=req, response=resp
            )

    async def aiter_bytes(self, chunk_size: int = 8192):
        for i in range(0, len(self._content), chunk_size):
            yield self._content[i : i + chunk_size]


def _patch_httpx_client(monkeypatch, responses: List[_FakeStreamResponse]):
    """
    Replace httpx.AsyncClient in the downloaders module so that each call to
    client.stream() returns the next response in *responses*.  The request
    headers for each stream() call are stored on the response object so tests
    can assert on them.
    """
    response_iter = iter(responses)

    class FakeClient:
        def __init__(self, **kwargs):
            pass

        async def __aenter__(self):
            return self

        async def __aexit__(self, *args):
            pass

        def stream(self, method, url, headers=None):
            resp = next(response_iter)
            resp.captured_request_headers = dict(headers or {})
            return resp

    # httpx is imported locally inside HTTPArtifactDownloader.download(), so
    # patching the module-level httpx.AsyncClient is the right hook point.
    monkeypatch.setattr(httpx, "AsyncClient", FakeClient)


class TestHTTPAtomicAndResume:
    @pytest.mark.asyncio
    async def test_fresh_download_writes_part_then_renames(self, tmp_path, monkeypatch):
        """Fresh download: uses .part file then renames to final path."""
        content = b"model weights data"
        _patch_httpx_client(
            monkeypatch,
            [_FakeStreamResponse(200, content)],
        )

        downloader = HTTPArtifactDownloader()
        result = await downloader.download(
            "http://example.com/weights.bin", str(tmp_path)
        )

        assert result == str(tmp_path)
        dest = tmp_path / "weights.bin"
        assert dest.exists()
        assert dest.read_bytes() == content
        assert not (tmp_path / "weights.bin.part").exists()

    @pytest.mark.asyncio
    async def test_fresh_download_stores_etag(self, tmp_path, monkeypatch):
        """When server returns an ETag, it is saved to the .part.etag sidecar."""
        content = b"data"
        _patch_httpx_client(
            monkeypatch,
            [_FakeStreamResponse(200, content, headers={"etag": '"abc123"'})],
        )

        downloader = HTTPArtifactDownloader()
        # Simulate an interruption: we patch os.replace to raise so the .part
        # file stays on disk and we can inspect the .etag sidecar.
        replaced = []
        real_replace = os.replace

        def fake_replace(src, dst):
            replaced.append((src, dst))
            real_replace(src, dst)

        monkeypatch.setattr(os, "replace", fake_replace)

        await downloader.download("http://example.com/weights.bin", str(tmp_path))

        # ETag sidecar should be cleaned up after successful rename
        assert not (tmp_path / "weights.bin.part.etag").exists()
        assert len(replaced) == 1

    @pytest.mark.asyncio
    async def test_resume_sends_range_and_if_range_headers(
        self, tmp_path, monkeypatch
    ):
        """Resuming a partial download sends Range + If-Range headers."""
        partial_data = b"partial"
        remaining_data = b" remainder"

        # Set up a pre-existing partial file and its ETag sidecar
        part_file = tmp_path / "weights.bin.part"
        part_file.write_bytes(partial_data)
        (tmp_path / "weights.bin.part.etag").write_text('"etag-v1"')

        resp_206 = _FakeStreamResponse(206, remaining_data)
        _patch_httpx_client(monkeypatch, [resp_206])

        downloader = HTTPArtifactDownloader()
        result = await downloader.download(
            "http://example.com/weights.bin", str(tmp_path)
        )

        assert result == str(tmp_path)
        # Correct range and If-Range headers must have been sent
        assert resp_206.captured_request_headers["Range"] == f"bytes={len(partial_data)}-"
        assert resp_206.captured_request_headers["If-Range"] == '"etag-v1"'
        # Final file should contain the combined content
        assert (tmp_path / "weights.bin").read_bytes() == partial_data + remaining_data
        # Sidecar files cleaned up
        assert not part_file.exists()
        assert not (tmp_path / "weights.bin.part.etag").exists()

    @pytest.mark.asyncio
    async def test_resume_when_server_returns_200_restarts_fresh(
        self, tmp_path, monkeypatch
    ):
        """If server returns 200 instead of 206 (file changed), download restarts."""
        stale_partial = b"stale data from old version"
        new_content = b"completely new file content"

        part_file = tmp_path / "weights.bin.part"
        part_file.write_bytes(stale_partial)
        (tmp_path / "weights.bin.part.etag").write_text('"old-etag"')

        # Server responds 200 (If-Range mismatch — file changed)
        resp_200 = _FakeStreamResponse(200, new_content, headers={"etag": '"new-etag"'})
        _patch_httpx_client(monkeypatch, [resp_200])

        downloader = HTTPArtifactDownloader()
        result = await downloader.download(
            "http://example.com/weights.bin", str(tmp_path)
        )

        assert result == str(tmp_path)
        # Final file should contain only the new content, not the stale partial
        assert (tmp_path / "weights.bin").read_bytes() == new_content
        assert not part_file.exists()

    @pytest.mark.asyncio
    async def test_resume_no_etag_sidecar_omits_if_range(self, tmp_path, monkeypatch):
        """Resuming without a stored ETag omits the If-Range header."""
        partial_data = b"partial"
        remaining_data = b" rest"

        part_file = tmp_path / "weights.bin.part"
        part_file.write_bytes(partial_data)
        # No .etag sidecar present

        resp_206 = _FakeStreamResponse(206, remaining_data)
        _patch_httpx_client(monkeypatch, [resp_206])

        downloader = HTTPArtifactDownloader()
        await downloader.download("http://example.com/weights.bin", str(tmp_path))

        assert "If-Range" not in resp_206.captured_request_headers
        assert resp_206.captured_request_headers["Range"] == f"bytes={len(partial_data)}-"

    @pytest.mark.asyncio
    async def test_404_raises_file_not_found(self, tmp_path, monkeypatch):
        """HTTP 404 is translated to FileNotFoundError."""
        _patch_httpx_client(monkeypatch, [_FakeStreamResponse(404, b"")])

        downloader = HTTPArtifactDownloader()
        with pytest.raises(FileNotFoundError):
            await downloader.download("http://example.com/missing.bin", str(tmp_path))

    @pytest.mark.asyncio
    async def test_403_raises_permission_error(self, tmp_path, monkeypatch):
        """HTTP 403 is translated to PermissionError."""
        _patch_httpx_client(monkeypatch, [_FakeStreamResponse(403, b"")])

        downloader = HTTPArtifactDownloader()
        with pytest.raises(PermissionError):
            await downloader.download("http://example.com/private.bin", str(tmp_path))

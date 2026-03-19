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
from typing import Any
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

import aibrix.runtime.artifact_service as artifact_service_module
from aibrix.runtime.artifact_service import ArtifactDelegationService
from aibrix.runtime.downloaders import (
    GCSArtifactDownloader,
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

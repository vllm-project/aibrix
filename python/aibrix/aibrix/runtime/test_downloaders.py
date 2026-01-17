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

import os
import sys
import pytest
from unittest.mock import MagicMock, patch, ANY
from pathlib import Path

from aibrix.runtime.downloaders import (
    get_downloader,
    S3ArtifactDownloader,
    GCSArtifactDownloader,
    HuggingFaceArtifactDownloader,
    HTTPArtifactDownloader,
)


class TestGetDownloader:
    def test_supported_schemes(self):
        assert isinstance(get_downloader("s3://bucket/key"), S3ArtifactDownloader)
        assert isinstance(get_downloader("gs://bucket/key"), GCSArtifactDownloader)
        assert isinstance(
            get_downloader("huggingface://repo"), HuggingFaceArtifactDownloader
        )
        assert isinstance(get_downloader("http://example.com"), HTTPArtifactDownloader)
        assert isinstance(get_downloader("https://example.com"), HTTPArtifactDownloader)

    def test_unsupported_scheme(self):
        with pytest.raises(ValueError, match="Unsupported URL scheme"):
            get_downloader("ftp://example.com")


class TestS3ArtifactDownloader:
    @pytest.fixture
    def mock_boto3(self):
        # Mock boto3 and botocore since they are imported inside the method
        mock_boto = MagicMock()
        mock_botocore = MagicMock()

        # Setup exceptions
        class MockClientError(Exception):
            def __init__(self, response, operation_name):
                self.response = response

        mock_botocore.ClientError = MockClientError
        mock_botocore.NoCredentialsError = Exception

        with patch.dict(
            "sys.modules",
            {"boto3": mock_boto, "botocore.exceptions": mock_botocore},
        ):
            yield mock_boto, mock_botocore

    @pytest.mark.asyncio
    async def test_download_file_success(self, mock_boto3, tmp_path):
        mock_boto, _ = mock_boto3
        mock_client = MagicMock()
        mock_boto.client.return_value = mock_client

        downloader = S3ArtifactDownloader()
        local_path = tmp_path / "dest"
        
        # Run download
        result = await downloader.download(
            "s3://my-bucket/my-file.txt",
            str(local_path),
            {"aws_access_key_id": "test", "aws_secret_access_key": "test"},
        )

        assert result == str(local_path)
        mock_boto.client.assert_called_with(
            "s3", aws_access_key_id="test", aws_secret_access_key="test"
        )
        mock_client.download_file.assert_called_with(
            "my-bucket", "my-file.txt", os.path.join(str(local_path), "my-file.txt")
        )

    @pytest.mark.asyncio
    async def test_download_directory_success(self, mock_boto3, tmp_path):
        mock_boto, _ = mock_boto3
        mock_client = MagicMock()
        mock_boto.client.return_value = mock_client

        # Setup paginator for directory listing
        paginator = MagicMock()
        mock_client.get_paginator.return_value = paginator
        paginator.paginate.return_value = [
            {
                "Contents": [
                    {"Key": "prefix/file1.txt"},
                    {"Key": "prefix/subdir/file2.txt"},
                ]
            }
        ]

        downloader = S3ArtifactDownloader()
        local_path = tmp_path / "dest"

        await downloader.download("s3://my-bucket/prefix/", str(local_path))

        # Verify downloads
        assert mock_client.download_file.call_count == 2
        # Check calls
        calls = mock_client.download_file.call_args_list
        # file1.txt
        assert calls[0][0][0] == "my-bucket"
        assert calls[0][0][1] == "prefix/file1.txt"
        # file2.txt
        assert calls[1][0][1] == "prefix/subdir/file2.txt"

    @pytest.mark.asyncio
    async def test_download_not_found(self, mock_boto3, tmp_path):
        mock_boto, mock_botocore = mock_boto3
        mock_client = MagicMock()
        mock_boto.client.return_value = mock_client

        # Simulate 404
        error_response = {"Error": {"Code": "404"}}
        mock_client.download_file.side_effect = mock_botocore.ClientError(
            error_response, "HeadObject"
        )

        downloader = S3ArtifactDownloader()
        with pytest.raises(FileNotFoundError, match="S3 object not found"):
            await downloader.download("s3://bucket/missing", str(tmp_path))


class TestGCSArtifactDownloader:
    @pytest.fixture
    def mock_gcs(self):
        mock_storage = MagicMock()
        mock_oauth = MagicMock()
        
        with patch.dict(
            "sys.modules",
            {
                "google.cloud": MagicMock(),
                "google.cloud.storage": mock_storage,
                "google.oauth2": mock_oauth,
            },
        ):
            yield mock_storage

    @pytest.mark.asyncio
    async def test_download_file_success(self, mock_gcs, tmp_path):
        mock_client = MagicMock()
        mock_gcs.Client.return_value = mock_client
        mock_bucket = MagicMock()
        mock_client.bucket.return_value = mock_bucket
        mock_blob = MagicMock()
        mock_bucket.blob.return_value = mock_blob

        downloader = GCSArtifactDownloader()
        local_path = tmp_path / "dest"

        await downloader.download("gs://my-bucket/file.txt", str(local_path))

        mock_client.bucket.assert_called_with("my-bucket")
        mock_bucket.blob.assert_called_with("file.txt")
        mock_blob.download_to_filename.assert_called()

    @pytest.mark.asyncio
    async def test_download_directory_success(self, mock_gcs, tmp_path):
        mock_client = MagicMock()
        mock_gcs.Client.return_value = mock_client
        mock_bucket = MagicMock()
        mock_client.bucket.return_value = mock_bucket

        # Mock list_blobs
        blob1 = MagicMock()
        blob1.name = "prefix/file1.txt"
        blob2 = MagicMock()
        blob2.name = "prefix/subdir/file2.txt"
        mock_bucket.list_blobs.return_value = [blob1, blob2]

        downloader = GCSArtifactDownloader()
        local_path = tmp_path / "dest"

        await downloader.download("gs://my-bucket/prefix/", str(local_path))

        mock_bucket.list_blobs.assert_called_with(prefix="prefix/")
        assert blob1.download_to_filename.called
        assert blob2.download_to_filename.called


class TestHuggingFaceArtifactDownloader:
    @pytest.fixture
    def mock_hf(self):
        mock_hf_hub = MagicMock()
        with patch.dict("sys.modules", {"huggingface_hub": mock_hf_hub}):
            yield mock_hf_hub

    @pytest.mark.asyncio
    async def test_download_success(self, mock_hf, tmp_path):
        mock_hf.snapshot_download.return_value = str(tmp_path / "cache")

        downloader = HuggingFaceArtifactDownloader()
        local_path = tmp_path / "dest"
        
        result = await downloader.download(
            "huggingface://org/model",
            str(local_path),
            {"huggingface_token": "secret_token"},
        )

        assert result == str(tmp_path / "cache")
        mock_hf.snapshot_download.assert_called_with(
            repo_id="org/model",
            cache_dir=str(local_path),
            token="secret_token",
        )

    @pytest.mark.asyncio
    async def test_download_permission_error(self, mock_hf, tmp_path):
        mock_hf.snapshot_download.side_effect = Exception("401 Client Error")

        downloader = HuggingFaceArtifactDownloader()
        with pytest.raises(PermissionError, match="Access denied"):
            await downloader.download("huggingface://org/private", str(tmp_path))


class TestHTTPArtifactDownloader:
    @pytest.fixture
    def mock_httpx(self):
        with patch("httpx.AsyncClient") as mock_client:
            yield mock_client

    @pytest.mark.asyncio
    async def test_download_success(self, mock_httpx, tmp_path):
        # Setup mock client context manager
        mock_client_instance = mock_httpx.return_value
        mock_client_instance.__aenter__.return_value = mock_client_instance
        mock_client_instance.__aexit__.return_value = None

        # Setup mock response context manager
        mock_response = MagicMock()
        mock_response.raise_for_status.return_value = None
        
        # Mock async iterator for bytes
        async def async_iter_bytes(chunk_size=None):
            yield b"chunk1"
            yield b"chunk2"
        
        mock_response.aiter_bytes = async_iter_bytes
        
        mock_stream = mock_client_instance.stream.return_value
        mock_stream.__aenter__.return_value = mock_response
        mock_stream.__aexit__.return_value = None

        downloader = HTTPArtifactDownloader()
        local_path = tmp_path / "dest"
        
        await downloader.download("http://example.com/file.txt", str(local_path))

        # Verify file content
        downloaded_file = local_path / "file.txt"
        assert downloaded_file.exists()
        assert downloaded_file.read_bytes() == b"chunk1chunk2"

    @pytest.mark.asyncio
    async def test_download_404(self, tmp_path):
        import httpx
        
        with patch("httpx.AsyncClient") as mock_client:
            mock_client_instance = mock_client.return_value
            mock_client_instance.__aenter__.return_value = mock_client_instance
            
            # Simulate 404 error
            mock_response = MagicMock()
            mock_response.status_code = 404
            
            mock_stream = mock_client_instance.stream.return_value
            mock_stream.__aenter__.return_value = mock_response
            
            # raise_for_status should raise HTTPStatusError
            error = httpx.HTTPStatusError("404 Not Found", request=None, response=mock_response)
            mock_response.raise_for_status.side_effect = error

            downloader = HTTPArtifactDownloader()
            with pytest.raises(FileNotFoundError, match="HTTP resource not found"):
                await downloader.download("http://example.com/missing", str(tmp_path))
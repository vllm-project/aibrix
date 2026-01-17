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

"""Artifact downloaders for different storage backends."""

import os
from abc import ABC, abstractmethod
from pathlib import Path
from typing import Dict, Optional
from urllib.parse import urlparse

from aibrix.logger import init_logger

logger = init_logger(__name__)


class ArtifactDownloader(ABC):
    """Base class for artifact downloaders."""

    @abstractmethod
    async def download(
        self, source_url: str, local_path: str, credentials: Optional[Dict] = None
    ) -> str:
        """
        Download artifact from source to local path.

        Args:
            source_url: Source URL (s3://, gs://, etc.)
            local_path: Local directory path
            credentials: Optional credentials dict

        Returns:
            Local path where artifact was downloaded

        Raises:
            Exception: If download fails
        """
        pass

    def _ensure_directory(self, path: str) -> None:
        """Ensure directory exists and is writable."""
        Path(path).mkdir(parents=True, exist_ok=True)


class S3ArtifactDownloader(ArtifactDownloader):
    """Download artifacts from AWS S3."""

    async def download(
        self, source_url: str, local_path: str, credentials: Optional[Dict] = None
    ) -> str:
        """
        Download from S3.

        Credentials dict should contain:
        - aws_access_key_id
        - aws_secret_access_key
        - aws_region (optional)
        """
        try:
            import boto3
            from botocore.exceptions import ClientError, NoCredentialsError
        except ImportError:
            raise ImportError(
                "boto3 is required for S3 downloads. Install with: pip install boto3"
            )

        parsed = urlparse(source_url)
        bucket_name = parsed.netloc
        object_key = parsed.path.lstrip("/")

        if not bucket_name or not object_key:
            raise ValueError(f"Invalid S3 URL: {source_url}")

        logger.info(
            f"Downloading from S3: bucket={bucket_name}, key={object_key}, "
            f"destination={local_path}"
        )

        # Create S3 client with credentials
        session_kwargs = {}
        if credentials:
            if "aws_access_key_id" in credentials:
                session_kwargs["aws_access_key_id"] = credentials["aws_access_key_id"]
            if "aws_secret_access_key" in credentials:
                session_kwargs["aws_secret_access_key"] = credentials[
                    "aws_secret_access_key"
                ]
            if "aws_region" in credentials:
                session_kwargs["region_name"] = credentials["aws_region"]

        try:
            s3_client = boto3.client("s3", **session_kwargs)

            # Ensure local directory exists
            self._ensure_directory(local_path)

            # Check if it's a directory (ends with /) or single file
            if object_key.endswith("/"):
                # Download directory
                self._download_s3_directory(
                    s3_client, bucket_name, object_key, local_path
                )
            else:
                # Download single file
                destination_file = os.path.join(
                    local_path, os.path.basename(object_key)
                )
                s3_client.download_file(bucket_name, object_key, destination_file)
                logger.info(f"Downloaded S3 file to {destination_file}")

            return local_path

        except NoCredentialsError:
            raise ValueError(
                "AWS credentials not found. Please provide credentials in secret."
            )
        except ClientError as e:
            error_code = e.response["Error"]["Code"]
            if error_code == "404":
                raise FileNotFoundError(f"S3 object not found: {source_url}")
            elif error_code == "403":
                raise PermissionError(f"Access denied to S3 object: {source_url}")
            else:
                raise RuntimeError(f"S3 download error: {e}")

    def _download_s3_directory(
        self, s3_client, bucket_name: str, prefix: str, local_path: str
    ) -> None:
        """Download all objects under a prefix (directory)."""
        paginator = s3_client.get_paginator("list_objects_v2")
        for page in paginator.paginate(Bucket=bucket_name, Prefix=prefix):
            if "Contents" not in page:
                continue

            for obj in page["Contents"]:
                key = obj["Key"]
                # Skip the prefix itself if it's a directory marker
                if key == prefix:
                    continue

                # Calculate relative path
                relative_path = key[len(prefix) :]
                local_file_path = os.path.join(local_path, relative_path)

                # Ensure directory exists
                self._ensure_directory(os.path.dirname(local_file_path))

                # Download file
                s3_client.download_file(bucket_name, key, local_file_path)
                logger.debug(f"Downloaded {key} to {local_file_path}")

        logger.info(f"Downloaded S3 directory {prefix} to {local_path}")


class GCSArtifactDownloader(ArtifactDownloader):
    """Download artifacts from Google Cloud Storage."""

    async def download(
        self, source_url: str, local_path: str, credentials: Optional[Dict] = None
    ) -> str:
        """
        Download from GCS.

        Credentials dict should contain:
        - gcp_service_account_json: Service account JSON string
        """
        try:
            from google.cloud import storage
            from google.oauth2 import service_account
        except ImportError:
            raise ImportError(
                "google-cloud-storage is required for GCS downloads. "
                "Install with: pip install google-cloud-storage"
            )

        parsed = urlparse(source_url)
        bucket_name = parsed.netloc
        blob_name = parsed.path.lstrip("/")

        if not bucket_name or not blob_name:
            raise ValueError(f"Invalid GCS URL: {source_url}")

        logger.info(
            f"Downloading from GCS: bucket={bucket_name}, blob={blob_name}, "
            f"destination={local_path}"
        )

        # Create GCS client with credentials
        if credentials and "gcp_service_account_json" in credentials:
            import json

            service_account_info = json.loads(credentials["gcp_service_account_json"])
            credentials_obj = service_account.Credentials.from_service_account_info(
                service_account_info
            )
            client = storage.Client(credentials=credentials_obj)
        else:
            # Use default credentials
            client = storage.Client()

        try:
            bucket = client.bucket(bucket_name)

            # Ensure local directory exists
            self._ensure_directory(local_path)

            # Check if it's a directory (prefix) or single file
            if blob_name.endswith("/"):
                # Download directory
                blobs = bucket.list_blobs(prefix=blob_name)
                for blob in blobs:
                    if blob.name == blob_name:  # Skip directory marker
                        continue

                    relative_path = blob.name[len(blob_name) :]
                    local_file_path = os.path.join(local_path, relative_path)

                    # Ensure directory exists
                    self._ensure_directory(os.path.dirname(local_file_path))

                    # Download file
                    blob.download_to_filename(local_file_path)
                    logger.debug(f"Downloaded {blob.name} to {local_file_path}")

                logger.info(f"Downloaded GCS directory {blob_name} to {local_path}")
            else:
                # Download single file
                blob = bucket.blob(blob_name)
                destination_file = os.path.join(local_path, os.path.basename(blob_name))
                blob.download_to_filename(destination_file)
                logger.info(f"Downloaded GCS file to {destination_file}")

            return local_path

        except Exception as e:
            if "404" in str(e):
                raise FileNotFoundError(f"GCS object not found: {source_url}")
            elif "403" in str(e):
                raise PermissionError(f"Access denied to GCS object: {source_url}")
            else:
                raise RuntimeError(f"GCS download error: {e}")


class HuggingFaceArtifactDownloader(ArtifactDownloader):
    """Download artifacts from HuggingFace Hub."""

    async def download(
        self, source_url: str, local_path: str, credentials: Optional[Dict] = None
    ) -> str:
        """
        Download from HuggingFace Hub.

        source_url format: huggingface://repo_id (e.g., huggingface://meta-llama/Llama-2-7b or huggingface://my-org/my-model)

        Credentials dict should contain:
        - huggingface_token: HF token for private models
        """
        try:
            from huggingface_hub import snapshot_download
        except ImportError:
            raise ImportError(
                "huggingface_hub is required for HuggingFace downloads. "
                "Install with: pip install huggingface_hub"
            )

        # Parse huggingface:// URL - the entire path is the repo_id
        parsed = urlparse(source_url)
        repo_id = (parsed.netloc + parsed.path).strip("/")

        if not repo_id:
            raise ValueError(f"Invalid HuggingFace URL: {source_url}")

        logger.info(
            f"Downloading from HuggingFace: repo_id={repo_id}, destination={local_path}"
        )

        # Get token from credentials
        token = credentials.get("huggingface_token") if credentials else None

        try:
            # Ensure local directory exists
            self._ensure_directory(local_path)

            # Download model/adapter
            download_path = snapshot_download(
                repo_id=repo_id,
                cache_dir=local_path,
                token=token,
            )

            logger.info(f"Downloaded HuggingFace model to {download_path}")
            return download_path

        except Exception as e:
            if "401" in str(e) or "403" in str(e):
                raise PermissionError(
                    f"Access denied to HuggingFace model: {repo_id}. "
                    f"Check token permissions."
                )
            elif "404" in str(e):
                raise FileNotFoundError(f"HuggingFace model not found: {repo_id}")
            else:
                raise RuntimeError(f"HuggingFace download error: {e}")


class HTTPArtifactDownloader(ArtifactDownloader):
    """Download artifacts from HTTP/HTTPS URLs."""

    async def download(
        self, source_url: str, local_path: str, credentials: Optional[Dict] = None
    ) -> str:
        """
        Download from HTTP/HTTPS.

        Credentials dict can contain:
        - headers: Dict of HTTP headers for authentication
        """
        import httpx

        logger.info(
            f"Downloading from HTTP: url={source_url}, destination={local_path}"
        )

        # Prepare headers
        headers = {}
        if credentials and "headers" in credentials:
            headers = credentials["headers"]

        # Ensure local directory exists
        self._ensure_directory(local_path)

        # Determine filename from URL
        parsed = urlparse(source_url)
        filename = os.path.basename(parsed.path) or "downloaded_file"
        destination_file = os.path.join(local_path, filename)

        try:
            async with httpx.AsyncClient(follow_redirects=True) as client:
                async with client.stream(
                    "GET", source_url, headers=headers
                ) as response:
                    response.raise_for_status()

                    # Download file
                    with open(destination_file, "wb") as f:
                        async for chunk in response.aiter_bytes(chunk_size=8192):
                            f.write(chunk)

            logger.info(f"Downloaded HTTP file to {destination_file}")
            return local_path

        except httpx.HTTPStatusError as e:
            if e.response.status_code == 404:
                raise FileNotFoundError(f"HTTP resource not found: {source_url}")
            elif e.response.status_code == 403:
                raise PermissionError(f"Access denied to HTTP resource: {source_url}")
            else:
                raise RuntimeError(f"HTTP download error: {e}")
        except Exception as e:
            raise RuntimeError(f"HTTP download error: {e}")


def get_downloader(source_url: str) -> ArtifactDownloader:
    """
    Get appropriate downloader based on URL scheme.

    Args:
        source_url: Source URL

    Returns:
        ArtifactDownloader instance

    Raises:
        ValueError: If URL scheme is not supported
    """
    parsed = urlparse(source_url)
    scheme = parsed.scheme.lower()

    downloader_map = {
        "s3": S3ArtifactDownloader(),
        "gcs": GCSArtifactDownloader(),
        "huggingface": HuggingFaceArtifactDownloader(),
        "http": HTTPArtifactDownloader(),
        "https": HTTPArtifactDownloader(),
    }

    if scheme not in downloader_map:
        raise ValueError(
            f"Unsupported URL scheme: {scheme}. "
            f"Supported schemes: {', '.join(downloader_map.keys())}"
        )

    return downloader_map[scheme]

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

"""Artifact delegation service for LoRA adapters."""

import os
import shutil
from pathlib import Path
from typing import Dict, Optional

from aibrix.logger import init_logger
from aibrix.openapi.engine.base import InferenceEngine
from aibrix.openapi.protocol import (
    ErrorResponse,
    LoadLoraAdapterRequest,
    LoadLoraAdapterRuntimeRequest,
    LoadLoraAdapterRuntimeResponse,
    UnloadLoraAdapterRequest,
    UnloadLoraAdapterRuntimeRequest,
)
from aibrix.runtime.downloaders import get_downloader

logger = init_logger(__name__)


class ArtifactDelegationService:
    """
    Service for delegating artifact downloads and forwarding to inference engine.

    This service:
    1. Downloads artifacts from remote storage (S3, GCS, OSS, HuggingFace)
    2. Stores artifacts locally
    3. Forwards load requests to inference engine with local paths
    4. Manages artifact lifecycle (cleanup on unload)
    """

    def __init__(
        self,
        local_dir: str = "/tmp/aibrix/adapters",
        credentials_mount: str = "/var/run/secrets/aibrix",
    ):
        """
        Initialize artifact delegation service.

        Args:
            local_dir: Directory for downloaded artifacts
            credentials_mount: Path where K8s secrets are mounted
        """
        self.local_dir = local_dir
        self.credentials_mount = credentials_mount

        # Ensure local directory exists
        Path(local_dir).mkdir(parents=True, exist_ok=True)

        logger.info(
            f"Initialized ArtifactDelegationService: local_dir={local_dir}, "
            f"credentials_mount={credentials_mount}"
        )

    def _load_credentials(self, secret_name: Optional[str] = None) -> Dict:
        """
        Load credentials from mounted Kubernetes secret.

        Args:
            secret_name: Name of the secret (optional, uses default if not provided)

        Returns:
            Dictionary of credentials
        """
        if not secret_name:
            # Try to read from default credentials location
            secret_path = self.credentials_mount
        else:
            secret_path = os.path.join(self.credentials_mount, secret_name)

        credentials: Dict[str, str] = {}

        # If secret path doesn't exist, return empty credentials
        if not os.path.exists(secret_path):
            logger.warning(f"Credentials path not found: {secret_path}")
            return credentials

        # Read all files in secret directory
        if os.path.isdir(secret_path):
            for filename in os.listdir(secret_path):
                # Skip Kubernetes secret management symlinks (e.g., ..data, ..2024_..)
                if filename.startswith(".."):
                    continue
                file_path = os.path.join(secret_path, filename)
                if os.path.isfile(file_path):
                    try:
                        with open(file_path, "r") as f:
                            credentials[filename] = f.read().strip()
                    except Exception as e:
                        logger.warning(
                            f"Failed to read credential file {filename}: {e}"
                        )
        elif os.path.isfile(secret_path):
            # Single file secret
            try:
                with open(secret_path, "r") as f:
                    credentials["default"] = f.read().strip()
            except Exception as e:
                logger.warning(f"Failed to read credential file: {e}")

        logger.info(f"Loaded {len(credentials)} credential keys from {secret_path}")
        return credentials

    def _get_local_path_for_adapter(self, lora_name: str) -> str:
        """Get local path for storing adapter artifacts."""
        return os.path.join(self.local_dir, lora_name)

    async def download_artifact(
        self,
        artifact_url: str,
        lora_name: str,
        credentials: Optional[Dict] = None,
    ) -> str:
        """
        Download artifact from remote location.

        Args:
            artifact_url: Source URL (s3://, gs://, huggingface://, etc.)
            lora_name: Name of the LoRA adapter
            credentials: Optional credentials dict

        Returns:
            Local path where artifact was downloaded

        Raises:
            Exception: If download fails
        """
        local_path = self._get_local_path_for_adapter(lora_name)

        # Check if already downloaded
        if os.path.exists(local_path) and os.listdir(local_path):
            logger.info(
                f"Artifact already exists locally for {lora_name} at {local_path}"
            )
            return local_path

        logger.info(
            f"Downloading artifact for {lora_name} from {artifact_url} to {local_path}"
        )

        try:
            # Get appropriate downloader based on URL scheme
            downloader = get_downloader(artifact_url)

            # Download artifact
            downloaded_path = await downloader.download(
                artifact_url, local_path, credentials
            )

            logger.info(
                f"Successfully downloaded artifact for {lora_name} to {downloaded_path}"
            )
            return downloaded_path

        except Exception as e:
            # Clean up partial downloads
            if os.path.exists(local_path):
                shutil.rmtree(local_path, ignore_errors=True)

            logger.error(f"Failed to download artifact for {lora_name}: {e}")
            raise

    async def load_adapter_with_delegation(
        self,
        request: LoadLoraAdapterRuntimeRequest,
        engine: InferenceEngine,
    ) -> LoadLoraAdapterRuntimeResponse:
        """
        Load LoRA adapter with artifact delegation.

        This method:
        1. Downloads artifact from remote storage
        2. Forwards load request to inference engine with local path
        3. Returns response to controller

        Args:
            request: Runtime load request
            engine: Inference engine instance

        Returns:
            Response containing status and details
        """
        lora_name = request.lora_name
        artifact_url = request.artifact_url

        logger.info(
            f"Starting artifact delegation for adapter {lora_name} from {artifact_url}"
        )

        try:
            # Load credentials if specified
            credentials = None
            if request.credentials_secret:
                credentials = self._load_credentials(request.credentials_secret)

            # Merge additional config into credentials
            if request.additional_config:
                if credentials is None:
                    credentials = {}
                credentials.update(request.additional_config)

            # Download artifact
            local_path = await self.download_artifact(
                artifact_url, lora_name, credentials
            )

            # Forward to inference engine with local path
            engine_request = LoadLoraAdapterRequest(
                lora_name=lora_name,
                lora_path=local_path,
            )

            logger.info(
                f"Forwarding load request to engine for {lora_name} with local path {local_path}"
            )

            engine_response = await engine.load_lora_adapter(engine_request)

            # Check if engine returned error
            if isinstance(engine_response, ErrorResponse):
                return LoadLoraAdapterRuntimeResponse(
                    status="error",
                    message=f"Engine error: {engine_response.message}",
                    local_path=local_path,
                    engine_response={
                        "type": engine_response.type,
                        "code": engine_response.code,
                        "message": engine_response.message,
                    },
                )

            # Success
            return LoadLoraAdapterRuntimeResponse(
                status="success",
                message=f"Successfully loaded adapter {lora_name} from {artifact_url}",
                local_path=local_path,
                engine_response={"message": engine_response},
            )

        except FileNotFoundError as e:
            logger.error(f"Artifact not found: {e}")
            return LoadLoraAdapterRuntimeResponse(
                status="error",
                message=f"Artifact not found: {str(e)}",
            )

        except PermissionError as e:
            logger.error(f"Permission denied: {e}")
            return LoadLoraAdapterRuntimeResponse(
                status="error",
                message=f"Permission denied: {str(e)}",
            )

        except Exception as e:
            logger.error(f"Failed to load adapter with delegation: {e}", exc_info=True)
            return LoadLoraAdapterRuntimeResponse(
                status="error",
                message=f"Failed to load adapter: {str(e)}",
            )

    async def unload_adapter(
        self,
        request: UnloadLoraAdapterRuntimeRequest,
        engine: InferenceEngine,
    ) -> str:
        """
        Unload LoRA adapter from engine and optionally clean up local files.

        Args:
            request: Runtime unload request
            engine: Inference engine instance

        Returns:
            Success message

        Raises:
            Exception: If unload fails
        """
        lora_name = request.lora_name

        logger.info(f"Unloading adapter {lora_name}")

        try:
            # Unload from engine
            engine_request = UnloadLoraAdapterRequest(lora_name=lora_name)
            engine_response = await engine.unload_lora_adapter(engine_request)

            # Check if engine returned error
            if isinstance(engine_response, ErrorResponse):
                logger.error(
                    f"Engine error unloading adapter {lora_name}: {engine_response.message}"
                )
                # Continue with cleanup even if engine unload fails

            # Clean up local files if requested
            if request.cleanup_local:
                local_path = self._get_local_path_for_adapter(lora_name)
                if os.path.exists(local_path):
                    try:
                        shutil.rmtree(local_path)
                        logger.info(f"Cleaned up local artifacts for {lora_name}")
                    except Exception as e:
                        logger.warning(
                            f"Failed to clean up local artifacts for {lora_name}: {e}"
                        )

            return f"Successfully unloaded adapter {lora_name}"

        except Exception as e:
            logger.error(f"Failed to unload adapter {lora_name}: {e}")
            raise

    async def cleanup_artifact(self, lora_name: str) -> None:
        """
        Clean up downloaded artifacts from local storage.

        Args:
            lora_name: Name of the LoRA adapter
        """
        local_path = self._get_local_path_for_adapter(lora_name)

        if os.path.exists(local_path):
            try:
                shutil.rmtree(local_path)
                logger.info(f"Cleaned up artifacts for {lora_name}")
            except Exception as e:
                logger.warning(f"Failed to clean up artifacts for {lora_name}: {e}")

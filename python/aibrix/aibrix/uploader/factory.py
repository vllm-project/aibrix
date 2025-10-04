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

from typing import Optional
from urllib.parse import urlparse

from aibrix.logger import init_logger
from aibrix.uploader.base import BaseUploader, UploadExtraConfig
from aibrix.uploader.s3 import S3Uploader

logger = init_logger(__name__)


class UploaderFactory:
    """Factory class to create the appropriate uploader based on the URI scheme."""

    @staticmethod
    def create_uploader(
        remote_uri: str,
        model_name: Optional[str] = None,
        upload_extra_config: Optional[UploadExtraConfig] = None,
        enable_progress_bar: bool = False,
    ) -> BaseUploader:
        """Create an uploader based on the remote URI scheme.

        Args:
            remote_uri: The URI of the remote storage to upload to
            model_name: Optional model name
            upload_extra_config: Optional upload configuration
            enable_progress_bar: Whether to enable progress bar

        Returns:
            The appropriate uploader instance

        Raises:
            ValueError: If the URI scheme is not supported
        """
        if remote_uri is None or remote_uri == "":
            raise ValueError("Remote URI must be provided")

        parsed_uri = urlparse(remote_uri)
        scheme = parsed_uri.scheme.lower()

        upload_extra_config = upload_extra_config or UploadExtraConfig()

        if scheme == "s3":
            logger.info(f"Creating S3 uploader for URI: {remote_uri}")
            return S3Uploader(
                model_uri=remote_uri,
                model_name=model_name,
                upload_extra_config=upload_extra_config,
                enable_progress_bar=enable_progress_bar,
            )
        # Additional uploaders can be added here as needed
        # elif scheme == "tos":
        #     from aibrix.uploader.tos import TOSUploader
        #     return TOSUploader(...)
        else:
            supported_schemes = ["s3"]  # Add other supported schemes here
            raise ValueError(
                f"Unsupported URI scheme: {scheme}. Supported schemes: {supported_schemes}"
            )


# Convenience function to create an uploader
create_uploader = UploaderFactory.create_uploader
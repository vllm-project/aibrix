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
from abc import abstractmethod
from contextlib import nullcontext
from functools import lru_cache
from pathlib import Path
from typing import ClassVar, Dict, List, Optional, Tuple
from urllib.parse import urlparse

import boto3
from boto3.s3.transfer import TransferConfig
from botocore.config import MAX_POOL_CONNECTIONS, Config
from botocore.exceptions import (
    ClientError,
    CredentialRetrievalError,
    NoCredentialsError,
)
from tqdm import tqdm

from aibrix import envs
from aibrix.common.errors import ArgNotCongiuredError, ModelNotFoundError
from aibrix.downloader.entity import RemoteSource
from aibrix.logger import init_logger
from aibrix.uploader.base import (
    DEFAULT_UPLOADER_EXTRA_CONFIG,
    BaseUploader,
    UploadExtraConfig,
)

logger = init_logger(__name__)


def _parse_bucket_info_from_uri(uri: str, scheme: str = "s3") -> Tuple[str, str]:
    parsed = urlparse(uri, scheme=scheme)
    bucket_name = parsed.netloc
    bucket_path = parsed.path.lstrip("/")
    return bucket_name, bucket_path


class S3BaseUploader(BaseUploader):
    _source: ClassVar[RemoteSource] = RemoteSource.S3

    def __init__(
        self,
        scheme: str,
        model_uri: str,
        model_name: Optional[str] = None,
        upload_extra_config: UploadExtraConfig = DEFAULT_UPLOADER_EXTRA_CONFIG,
        enable_progress_bar: bool = False,
    ):
        # Infer model name from URI if not provided
        if model_name is None:
            model_name = model_uri.strip().strip("/").split("/")[-1]
            logger.info(f"model_name is not set, using `{model_name}` as model_name")

        self.upload_extra_config = upload_extra_config
        auth_config = self._get_auth_config()
        bucket_name, bucket_path = _parse_bucket_info_from_uri(model_uri, scheme=scheme)

        # Avoid warning log "Connection pool is full"
        _num_threads = (
            self.upload_extra_config.num_threads or getattr(envs, "UPLOADER_NUM_THREADS", 10)
        )

        max_pool_connections = (
            _num_threads
            if _num_threads > MAX_POOL_CONNECTIONS
            else MAX_POOL_CONNECTIONS
        )
        client_config = Config(
            s3={"addressing_style": "virtual"},
            max_pool_connections=max_pool_connections,
        )

        self.client = boto3.client(
            service_name="s3", config=client_config, **auth_config
        )

        super().__init__(
            model_uri=model_uri,
            model_name=model_name,
            bucket_path=bucket_path,
            bucket_name=bucket_name,
            upload_extra_config=upload_extra_config,
            enable_progress_bar=enable_progress_bar,
        )

    def _valid_config(self):
        if self.model_name is None or self.model_name == "":
            raise ArgNotCongiuredError(arg_name="model_name", arg_source="--model-name")

        if self.bucket_name is None or self.bucket_name == "":
            raise ArgNotCongiuredError(arg_name="bucket_name", arg_source="--model-uri")

        try:
            # Try to access the bucket to validate permissions
            self.client.head_bucket(Bucket=self.bucket_name)
        except NoCredentialsError as e:
            logger.error(
                "No AWS credentials found. If using IRSA, ensure the service account is properly annotated with the IAM role ARN."
            )
            raise ModelNotFoundError(
                model_uri=self.model_uri,
                detail_msg=f"AWS credentials not found: {str(e)}",
            )
        except CredentialRetrievalError as e:
            logger.error(
                f"Failed to retrieve AWS credentials. If using IRSA, check that:\n"
                f"1. Service account is annotated with eks.amazonaws.com/role-arn\n"
                f"2. IAM role trust policy allows the service account\n"
                f"3. Pod has the correct service account assigned\n"
                f"Error: {str(e)}"
            )
            raise ModelNotFoundError(
                model_uri=self.model_uri,
                detail_msg=f"AWS credential retrieval failed: {str(e)}",
            )
        except ClientError as e:
            error_code = e.response.get("Error", {}).get("Code", "Unknown")
            error_message = e.response.get("Error", {}).get("Message", str(e))

            if error_code in [
                "InvalidUserID.NotFound",
                "AccessDenied",
                "TokenRefreshRequired",
                "Unknown",
            ]:
                # Handle IRSA-specific authentication issues
                if error_code == "Unknown" and "AssumeRoleWithWebIdentity" in str(e):
                    # Get IRSA environment variables for debugging
                    aws_role_arn = os.environ.get("AWS_ROLE_ARN", "NOT_SET")
                    aws_web_identity_token_file = os.environ.get(
                        "AWS_WEB_IDENTITY_TOKEN_FILE", "NOT_SET"
                    )
                    aws_region = os.environ.get("AWS_REGION", "NOT_SET")

                    logger.error(
                        f"IRSA authentication failed. Please verify:\n"
                        f"1. Service account is annotated: eks.amazonaws.com/role-arn=<IAM_ROLE_ARN>\n"
                        f"2. IAM role trust policy includes the EKS OIDC provider\n"
                        f"3. Pod is using the correct service account\n"
                        f"4. IAM role has required S3 permissions\n"
                        f"\nCurrent Environment:\n"
                        f"   AWS_ROLE_ARN: {aws_role_arn}\n"
                        f"   AWS_WEB_IDENTITY_TOKEN_FILE: {aws_web_identity_token_file}\n"
                        f"   AWS_REGION: {aws_region}\n"
                        f"\nError Details: {error_message}\n"
                    )
                else:
                    logger.error(
                        f"AWS authentication failed with error {error_code}. If using IRSA, check that:\n"
                        f"1. IAM role has necessary S3 permissions (s3:ListBucket, s3:PutObject)\n"
                        f"2. IAM role trust policy includes the correct OIDC provider and service account\n"
                        f"3. Service account annotation matches the IAM role ARN\n"
                        f"Error: {error_message}"
                    )
                raise ModelNotFoundError(
                    model_uri=self.model_uri,
                    detail_msg=f"AWS authentication failed ({error_code}): {error_message}",
                )
            elif error_code == "NoSuchBucket":
                logger.error(
                    f"S3 bucket '{self.bucket_name}' does not exist or is not accessible"
                )
                raise ModelNotFoundError(
                    model_uri=self.model_uri,
                    detail_msg=f"S3 bucket '{self.bucket_name}' not found",
                )
            else:
                logger.error(
                    f"S3 operation failed with error {error_code}: {error_message}"
                )
                raise ModelNotFoundError(
                    model_uri=self.model_uri,
                    detail_msg=f"S3 error ({error_code}): {error_message}",
                )
        except Exception as e:
            logger.error(
                f"Unexpected error accessing bucket {self.bucket_name} in {self.model_uri}: {str(e)}"
            )
            raise ModelNotFoundError(model_uri=self.model_uri, detail_msg=str(e))

    @abstractmethod
    def _get_auth_config(self) -> Dict[str, Optional[str]]:
        """Get auth config for S3 client.

        Returns:
            Dict[str, str]: auth config for S3 client, containing following keys:
            - region_name: region name of S3 bucket
            - endpoint_url: endpoint url of S3 bucket
            - aws_access_key_id: access key id of S3 bucket
            - aws_secret_access_key: secret access key of S3 bucket

        Example return value:
            {
                region_name: "region-name",
                endpoint_url: "URL_ADDRESS3.region-name.com",
                aws_access_key_id: "AK****",
                aws_secret_access_key: "SK****",
            }
        """
        pass

    def _is_directory(self, local_path: Path) -> bool:
        """Check if local_path is a directory."""
        return local_path.is_dir()

    def _directory_list(self, local_path: Path) -> List[str]:
        """List all files in the directory."""
        files = []
        for root, _, filenames in os.walk(local_path):
            for filename in filenames:
                file_path = os.path.join(root, filename)
                files.append(file_path)
        return files

    def _support_multipart_upload(self) -> bool:
        return True

    def upload(
        self,
        local_file: Path,
        bucket_path: str,
        bucket_name: Optional[str] = None,
        enable_multipart: bool = True,
    ):
        # Ensure local file exists
        if not local_file.exists():
            raise ValueError(f"Local file {local_file} does not exist")

        # Check if we should skip upload based on force_upload flag
        if not self.force_upload:
            try:
                # Check if the object already exists in S3
                self.client.head_object(Bucket=bucket_name, Key=bucket_path)
                logger.info(f"File {local_file} already exists in S3 at {bucket_path}, skipping upload.")
                return
            except ClientError as e:
                if e.response.get("Error", {}).get("Code") not in ("404", "NoSuchKey"):
                    raise
                # Object doesn't exist, continue with upload
                pass
        else:
            logger.info(f"Forcing upload of {local_file} to S3 at {bucket_path}")

        # Construct TransferConfig
        config_kwargs = {
            "max_concurrency": self.upload_extra_config.num_threads
            or getattr(envs, "UPLOADER_NUM_THREADS", 10),
            "use_threads": enable_multipart,
            "max_io_queue": self.upload_extra_config.max_io_queue
            or getattr(envs, "UPLOADER_S3_MAX_IO_QUEUE", 100),
            "io_chunksize": self.upload_extra_config.io_chunksize
            or getattr(envs, "UPLOADER_S3_IO_CHUNKSIZE", 8 * 1024 * 1024),
            "multipart_threshold": self.upload_extra_config.part_threshold
            or getattr(envs, "UPLOADER_PART_THRESHOLD", 8 * 1024 * 1024),
            "multipart_chunksize": self.upload_extra_config.part_chunksize
            or getattr(envs, "UPLOADER_PART_CHUNKSIZE", 8 * 1024 * 1024),
        }

        config = TransferConfig(**config_kwargs)

        # Upload file
        total_length = os.path.getsize(local_file)
        with (
            tqdm(desc=str(local_file), total=total_length, unit="b", unit_scale=True)
            if self.enable_progress_bar
            else nullcontext()
        ) as pbar:

            def upload_progress(bytes_transferred):
                pbar.update(bytes_transferred)

            logger.info(f"Uploading {local_file} to S3 at {bucket_path}")
            self.client.upload_file(
                Filename=str(local_file),
                Bucket=bucket_name,
                Key=bucket_path,
                Config=config,
                Callback=upload_progress if self.enable_progress_bar else None,
            )
            logger.info(f"Upload of {local_file} to S3 completed successfully")


class S3Uploader(S3BaseUploader):
    _source: ClassVar[RemoteSource] = RemoteSource.S3

    def __init__(
        self,
        model_uri,
        model_name: Optional[str] = None,
        upload_extra_config: UploadExtraConfig = DEFAULT_UPLOADER_EXTRA_CONFIG,
        enable_progress_bar: bool = False,
    ):
        super().__init__(
            scheme="s3",
            model_uri=model_uri,
            model_name=model_name,
            upload_extra_config=upload_extra_config,
            enable_progress_bar=enable_progress_bar,
        )

    def _get_auth_config(self) -> Dict[str, Optional[str]]:
        ak, sk = (
            self.upload_extra_config.ak or getattr(envs, "UPLOADER_AWS_ACCESS_KEY_ID", None),
            self.upload_extra_config.sk or getattr(envs, "UPLOADER_AWS_SECRET_ACCESS_KEY", None),
        )

        auth_config: Dict[str, Optional[str]] = {}
        region = self.upload_extra_config.region or getattr(envs, "UPLOADER_AWS_REGION", None)
        if region:
            auth_config["region_name"] = region

        endpoint = (
            self.upload_extra_config.endpoint or getattr(envs, "UPLOADER_AWS_ENDPOINT_URL", None)
        )
        if endpoint:
            auth_config["endpoint_url"] = endpoint

        # If both access key and secret key are provided, use them
        if ak and sk:
            auth_config["aws_access_key_id"] = ak
            auth_config["aws_secret_access_key"] = sk
            return auth_config

        # If neither access key nor secret key are provided, use IRSA/default credential chain
        if not ak and not sk:
            logger.info(
                "No AWS access key or secret key provided, using IRSA or default credential chain"
            )
            return auth_config

        # If only one of them is provided, this is an error condition
        if not sk:
            raise ArgNotCongiuredError(
                arg_name="sk", arg_source="--upload-extra-config"
            )
        else:  # not ak
            raise ArgNotCongiuredError(
                arg_name="ak", arg_source="--upload-extra-config"
            )
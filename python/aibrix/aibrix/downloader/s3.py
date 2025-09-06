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


def _parse_bucket_info_from_uri(uri: str, scheme: str = "s3") -> Tuple[str, str]:
    parsed = urlparse(uri, scheme=scheme)
    bucket_name = parsed.netloc
    bucket_path = parsed.path.lstrip("/")
    return bucket_name, bucket_path


class S3BaseDownloader(BaseDownloader):
    _source: ClassVar[RemoteSource] = RemoteSource.S3

    def __init__(
        self,
        scheme: str,
        model_uri: str,
        model_name: Optional[str] = None,
        download_extra_config: DownloadExtraConfig = DEFAULT_DOWNLOADER_EXTRA_CONFIG,
        enable_progress_bar: bool = False,
    ):
        if model_name is None:
            model_name = infer_model_name(model_uri)
            logger.info(f"model_name is not set, using `{model_name}` as model_name")

        self.download_extra_config = download_extra_config
        auth_config = self._get_auth_config()
        bucket_name, bucket_path = _parse_bucket_info_from_uri(model_uri, scheme=scheme)

        # Avoid warning log "Connection pool is full"
        # Refs: https://github.com/boto/botocore/issues/619#issuecomment-583511406
        _num_threads = (
            self.download_extra_config.num_threads or envs.DOWNLOADER_NUM_THREADS
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

        try:
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

                    # Check if token file exists and is readable
                    token_file_status = "NOT_SET"
                    token_info = ""
                    if aws_web_identity_token_file != "NOT_SET":
                        try:
                            if os.path.exists(aws_web_identity_token_file):
                                with open(aws_web_identity_token_file, "r") as f:
                                    token_content = f.read().strip()
                                    token_file_status = (
                                        f"EXISTS (length: {len(token_content)} chars)"
                                    )

                                    # Parse JWT token to extract useful info (without verification)
                                    try:
                                        import base64
                                        import json

                                        # JWT tokens are base64 encoded, split by '.'
                                        parts = token_content.split(".")
                                        if len(parts) >= 2:
                                            # Decode payload (header not needed for our purposes)
                                            payload = json.loads(
                                                base64.urlsafe_b64decode(
                                                    parts[1]
                                                    + "=" * (-len(parts[1]) % 4)
                                                )
                                            )

                                            token_info = "\n   Token Info:"
                                            token_info += f"\n     iss (issuer): {payload.get('iss', 'N/A')}"
                                            token_info += f"\n     sub (subject): {payload.get('sub', 'N/A')}"
                                            token_info += f"\n     aud (audience): {payload.get('aud', 'N/A')}"
                                            if "exp" in payload:
                                                import datetime

                                                exp_time = (
                                                    datetime.datetime.fromtimestamp(
                                                        payload["exp"],
                                                        tz=datetime.timezone.utc,
                                                    )
                                                )
                                                token_info += (
                                                    f"\n     exp (expires): {exp_time}"
                                                )
                                    except Exception as parse_error:
                                        token_info = f"\n   Token Parse Error: {str(parse_error)}"
                            else:
                                token_file_status = "FILE_NOT_FOUND"
                        except Exception as token_error:
                            token_file_status = f"READ_ERROR: {str(token_error)}"

                    logger.error(
                        f"IRSA authentication failed. Please verify:\n"
                        f"1. Service account is annotated: eks.amazonaws.com/role-arn=<IAM_ROLE_ARN>\n"
                        f"2. IAM role trust policy includes the EKS OIDC provider:\n"
                        f"   - 'sts:AssumeRoleWithWebIdentity' action is allowed\n"
                        f"   - Condition: StringEquals 'oidc.eks.<region>.amazonaws.com/id/<cluster-id>:sub' = 'system:serviceaccount:<namespace>:<service-account-name>'\n"
                        f"3. Pod is using the correct service account\n"
                        f"4. IAM role has required S3 permissions\n"
                        f"\nCurrent Environment:\n"
                        f"   AWS_ROLE_ARN: {aws_role_arn}\n"
                        f"   AWS_WEB_IDENTITY_TOKEN_FILE: {aws_web_identity_token_file}\n"
                        f"   Token File Status: {token_file_status}{token_info}\n"
                        f"   AWS_REGION: {aws_region}\n"
                        f"\nError Details: {error_message}\n"
                        f"Full Error: {str(e)}\n"
                        f"\nTroubleshooting Steps:\n"
                        f"1. Check IAM role trust policy for: {aws_role_arn}\n"
                        f"2. Verify EKS OIDC provider is configured\n"
                        f"3. Ensure role trust policy condition matches the token subject above"
                    )
                else:
                    logger.error(
                        f"AWS authentication failed with error {error_code}. If using IRSA, check that:\n"
                        f"1. IAM role has necessary S3 permissions (s3:ListBucket, s3:GetObject)\n"
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

    @lru_cache()
    def _is_directory(self) -> bool:
        """Check if model_uri is a directory."""
        if self.bucket_path.endswith("/"):
            return True
        # If the exact key exists and there are no child keys under path+"/",
        # we treat it as a file. Otherwise, it's a directory-like prefix.
        key_exists = False
        try:
            self.client.head_object(Bucket=self.bucket_name, Key=self.bucket_path)
            key_exists = True
        except Exception:
            key_exists = False

        # Check for any child under prefix + '/'
        prefix = self.bucket_path.rstrip("/") + "/"
        paginator = self.client.get_paginator("list_objects_v2")
        for page in paginator.paginate(
            Bucket=self.bucket_name, Prefix=prefix, PaginationConfig={"MaxItems": 1}
        ):
            if page.get("KeyCount", 0) > 0 or page.get("Contents"):
                return True
            break

        return not key_exists and True or False

    def _directory_list(self, path: str) -> List[str]:
        # Recursively list all objects under the prefix using paginator
        prefix = path if path.endswith("/") else f"{path}/"
        keys: List[str] = []
        paginator = self.client.get_paginator("list_objects_v2")
        for page in paginator.paginate(Bucket=self.bucket_name, Prefix=prefix):
            for content in page.get("Contents", []) if page else []:
                k = content.get("Key")
                if k is not None:
                    keys.append(k)
        return keys

    def _support_range_download(self) -> bool:
        return True

    def download(
        self,
        local_path: Path,
        bucket_path: str,
        bucket_name: Optional[str] = None,
        enable_range: bool = True,
    ):
        try:
            meta_data = self.client.head_object(Bucket=bucket_name, Key=bucket_path)
        except Exception as e:
            raise ValueError(f"S3 bucket path {bucket_path} not exist for {e}.")

        _file_name = bucket_path.split("/")[-1]
        local_file = local_path.joinpath(_file_name).absolute()
        # Write to a temporary file to ensure atomic finalize
        tmp_file = (
            local_file.with_suffix(local_file.suffix + ".part")
            if local_file.suffix
            else Path(str(local_file) + ".part")
        )

        # check if file exist
        etag = meta_data.get("ETag", "")
        file_size = meta_data.get("ContentLength", 0)
        meta_data_file = meta_file(
            local_path=local_path, file_name=_file_name, source=self._source.value
        )

        if not need_to_download(local_file, meta_data_file, file_size, etag):
            return

        # construct TransferConfig
        config_kwargs = {
            "max_concurrency": self.download_extra_config.num_threads
            or envs.DOWNLOADER_NUM_THREADS,
            "use_threads": enable_range,
            "max_io_queue": self.download_extra_config.max_io_queue
            or envs.DOWNLOADER_S3_MAX_IO_QUEUE,
            "io_chunksize": self.download_extra_config.io_chunksize
            or envs.DOWNLOADER_S3_IO_CHUNKSIZE,
            "multipart_threshold": self.download_extra_config.part_threshold
            or envs.DOWNLOADER_PART_THRESHOLD,
            "multipart_chunksize": self.download_extra_config.part_chunksize
            or envs.DOWNLOADER_PART_CHUNKSIZE,
        }

        config = TransferConfig(**config_kwargs)

        # download file
        total_length = int(meta_data.get("ContentLength", 0))
        with (
            tqdm(desc=_file_name, total=total_length, unit="b", unit_scale=True)
            if self.enable_progress_bar
            else nullcontext()
        ) as pbar:

            def download_progress(bytes_transferred):
                pbar.update(bytes_transferred)

            download_file = get_local_download_paths(
                local_path, _file_name, self._source
            )
            with download_file.download_lock():
                self.client.download_file(
                    Bucket=bucket_name,
                    Key=bucket_path,
                    Filename=str(
                        tmp_file
                    ),  # S3 client does not support Path, convert it to str
                    Config=config,
                    Callback=download_progress if self.enable_progress_bar else None,
                )
                # Atomically move into place then write metadata
                tmp_file.replace(local_file)
                save_meta_data(meta_data_file, etag)


class S3Downloader(S3BaseDownloader):
    _source: ClassVar[RemoteSource] = RemoteSource.S3

    def __init__(
        self,
        model_uri,
        model_name: Optional[str] = None,
        download_extra_config: DownloadExtraConfig = DEFAULT_DOWNLOADER_EXTRA_CONFIG,
        enable_progress_bar: bool = False,
    ):
        super().__init__(
            scheme="s3",
            model_uri=model_uri,
            model_name=model_name,
            download_extra_config=download_extra_config,
            enable_progress_bar=enable_progress_bar,
        )  # type: ignore

    def _get_auth_config(self) -> Dict[str, Optional[str]]:
        ak, sk = (
            self.download_extra_config.ak or envs.DOWNLOADER_AWS_ACCESS_KEY_ID,
            self.download_extra_config.sk or envs.DOWNLOADER_AWS_SECRET_ACCESS_KEY,
        )

        auth_config: Dict[str, Optional[str]] = {}
        region = self.download_extra_config.region or envs.DOWNLOADER_AWS_REGION
        if region:
            auth_config["region_name"] = region

        endpoint = (
            self.download_extra_config.endpoint or envs.DOWNLOADER_AWS_ENDPOINT_URL
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
                arg_name="sk", arg_source="--download-extra-config"
            )
        else:  # not ak
            raise ArgNotCongiuredError(
                arg_name="ak", arg_source="--download-extra-config"
            )

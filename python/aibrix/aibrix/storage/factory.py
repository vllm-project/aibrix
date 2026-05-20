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
from typing import Dict, Optional, Union

from aibrix import envs
from aibrix.storage.base import BaseStorage, StorageConfig
from aibrix.storage.local import LocalStorage
from aibrix.storage.redis import RedisStorage
from aibrix.storage.s3 import S3Storage
from aibrix.storage.tos import TOSStorage
from aibrix.storage.types import StorageType


def create_storage(
    storage_type: Union[StorageType, str],
    config: Optional[StorageConfig] = None,
    **kwargs,
) -> BaseStorage:
    """Factory function to create storage instances.

    Args:
        storage_type: Type of storage to create
        config: Storage configuration
        **kwargs: Storage-specific parameters

    Returns:
        Storage instance

    Raises:
        ValueError: If storage type is not supported or required parameters are missing
    """
    if isinstance(storage_type, str):
        try:
            storage_type = StorageType(storage_type.lower())
        except ValueError:
            raise ValueError(f"Unsupported storage type: {storage_type}")

    if config is None:
        config = StorageConfig()

    if storage_type == StorageType.AUTO:
        return create_storage_from_env()

    if storage_type == StorageType.LOCAL:
        # Honor STORAGE_LOCAL_PATH from the environment when no explicit
        # ``base_path`` kwarg is provided. LocalStorage itself only reads
        # the env when constructed with ``base_path=None``, so without
        # this fallback the env var never reaches it via the factory.
        base_path = (
            kwargs.get("base_path")
            or os.environ.get("STORAGE_LOCAL_PATH")
            or ".storage"
        )
        return LocalStorage(base_path=base_path, config=config)

    elif storage_type == StorageType.S3:
        bucket_name = kwargs.get("bucket_name") or envs.STORAGE_AWS_BUCKET
        if not bucket_name:
            raise ValueError("bucket_name is required for S3 storage")

        return S3Storage(
            bucket_name=bucket_name,
            region_name=kwargs.get("region_name") or envs.STORAGE_AWS_REGION,
            endpoint_url=kwargs.get("endpoint_url") or envs.STORAGE_AWS_ENDPOINT_URL,
            aws_access_key_id=kwargs.get("aws_access_key_id")
            or envs.STORAGE_AWS_ACCESS_KEY_ID,
            aws_secret_access_key=kwargs.get("aws_secret_access_key")
            or envs.STORAGE_AWS_SECRET_ACCESS_KEY,
            config=config,
        )

    elif storage_type == StorageType.TOS:
        bucket_name = kwargs.get("bucket_name") or envs.STORAGE_TOS_BUCKET
        access_key = kwargs.get("access_key") or envs.STORAGE_TOS_ACCESS_KEY
        secret_key = kwargs.get("secret_key") or envs.STORAGE_TOS_SECRET_KEY
        endpoint = kwargs.get("endpoint") or envs.STORAGE_TOS_ENDPOINT
        region = kwargs.get("region") or envs.STORAGE_TOS_REGION

        if not bucket_name:
            raise ValueError("bucket_name is required for TOS storage")
        if not access_key:
            raise ValueError("access_key is required for TOS storage")
        if not secret_key:
            raise ValueError("secret_key is required for TOS storage")
        if not endpoint:
            raise ValueError("endpoint is required for TOS storage")
        if not region:
            raise ValueError("region is required for TOS storage")

        return TOSStorage(
            bucket_name=bucket_name,
            access_key=access_key,
            secret_key=secret_key,
            endpoint=endpoint,
            region=region,
            config=config,
        )

    elif storage_type == StorageType.REDIS:
        host = kwargs.get("host") or envs.STORAGE_REDIS_HOST or "localhost"
        port = kwargs.get("port") or envs.STORAGE_REDIS_PORT or 6379
        db = kwargs.get("db", 0) or envs.STORAGE_REDIS_DB
        password = kwargs.get("password") or envs.STORAGE_REDIS_PASSWORD

        return RedisStorage(
            host=host,
            port=port,
            db=db,
            password=password,
            config=config,
        )

    else:
        raise ValueError(f"Unsupported storage type: {storage_type}")


def create_storage_from_env() -> BaseStorage:
    """Create storage instance from environment variables.

    Determines storage type and configuration from environment variables.
    Priority order: TOS > S3 > Local (default)

    Args:
        bucket_name: Optional bucket name for S3/TOS storage

    Returns:
        Storage instance
    """
    explicit_storage_type = os.environ.get("STORAGE_TYPE")
    if (
        explicit_storage_type
        and explicit_storage_type.lower() != StorageType.AUTO.value
    ):
        normalized_type = explicit_storage_type.lower()
        if normalized_type == "minio":
            normalized_type = StorageType.S3.value
        kwargs: Dict[str, str] = {}
        if normalized_type == StorageType.LOCAL.value:
            kwargs["base_path"] = os.environ.get("STORAGE_LOCAL_PATH") or ".storage"
        return create_storage(normalized_type, config=None, **kwargs)

    # Default to local storage; honor STORAGE_LOCAL_PATH if set, else
    # fall back to a cwd-relative ``.storage`` folder.
    storage_type = StorageType.LOCAL
    kwargs = {"base_path": os.environ.get("STORAGE_LOCAL_PATH") or ".storage"}

    # Check if S3 credentials are available
    if envs.STORAGE_AWS_ACCESS_KEY_ID and envs.STORAGE_AWS_SECRET_ACCESS_KEY:
        storage_type = StorageType.S3
        kwargs = {}

    # Check if TOS credentials are available (higher priority than S3)
    if (
        envs.STORAGE_TOS_ACCESS_KEY
        and envs.STORAGE_TOS_SECRET_KEY
        and envs.STORAGE_TOS_ENDPOINT
        and envs.STORAGE_TOS_REGION
    ):
        storage_type = StorageType.TOS
        kwargs = {}

    return create_storage(storage_type, config=None, **kwargs)

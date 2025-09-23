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

from typing import Optional, Tuple

from aibrix.metadata.logger import init_logger
from aibrix.storage import BaseStorage, StorageType, create_storage

logger = init_logger(__name__)

p_metastore: Optional[BaseStorage] = None
NUM_REQUESTS_PER_READ = 1024


def initialize_batch_metastore(storage_type=StorageType.AUTO, params={}):
    """Initialize storage type. Now it supports local files, S3, and TOS.

    For some storage type, user needs to pass in other parameters to params.

    Args:
        storage_type: Legacy storage type enum
        params: Storage-specific parameters
    """
    global p_metastore

    # Create new storage instance and wrap with adapter
    try:
        p_metastore = create_storage(storage_type, base_path=".metastore", **params)
        logger.info(f"Initialized batch metastore with type: {storage_type}")
    except Exception as e:
        logger.error(f"Failed to initialize storage: {e}")
        raise


async def set_metadata(key: str, value: str) -> None:
    """Set metadata to metastore.

    Args:
        key: Metadata key
        value: Metadata value
    """
    assert p_metastore is not None
    await p_metastore.put_object(key, value)


async def get_metadata(key: str) -> Tuple[str, bool]:
    """Get metadata from metastore.

    Args:
        key: Metadata key

    Returns:
        Metadata value
    """
    assert p_metastore is not None

    try:
        data = await p_metastore.get_object(key)
        return data.decode("utf-8"), True
    except FileNotFoundError:
        return "", False


async def delete_metadata(key: str) -> None:
    """Delete metadata from metastore.

    Args:
        key: Metadata key
    """
    assert p_metastore is not None
    await p_metastore.delete_object(key)

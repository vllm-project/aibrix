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
from concurrent.futures import Executor
from pathlib import Path
from typing import Sequence

import torch

from ... import envs
from ...common import AsyncBase
from ...common.absl_logging import getLogger
from ...memory import MemoryRegion
from ...status import Status, StatusCodes
from ...utils import ensure_dir_exist
from . import Connector, ConnectorFeature

logger = getLogger(__name__)


@AsyncBase.async_wrap(
    exists="_exists", get="_get", put="_put", delete="_delete"
)
class SHFSConnector(Connector[bytes, torch.Tensor], AsyncBase):
    """Shared File System (SHFS) connector for KVCache L2 storage.

    This connector stores KVCache blocks as files in a shared file system.
    All prefiller and decoder vllm engines can access the same shared
    directory to store and retrieve KVCache blocks.
    """

    def __init__(
        self,
        root_path: str,
        executor: Executor,
    ):
        super().__init__(executor)
        self.root_path = Path(root_path)
        self.conn_id: str | None = None  # Will be set in from_envs

    @classmethod
    def from_envs(
        cls, conn_id: str, executor: Executor, **kwargs
    ) -> "SHFSConnector":
        """Create a connector from environment variables."""
        root = envs.AIBRIX_KV_CACHE_OL_SHFS_ROOT

        # Create full path: root/conn_id
        full_path = os.path.join(os.path.expanduser(root), conn_id)

        instance = cls(full_path, executor)
        instance.conn_id = conn_id
        return instance

    @property
    def name(self) -> str:
        return "SHFS"

    @property
    def feature(self) -> ConnectorFeature:
        """SHFS connector features."""
        feature = ConnectorFeature(mput_mget=True)
        return feature

    def __del__(self) -> None:
        self.close()

    def _key_to_filepath(self, key: bytes) -> Path:
        """Convert a key (bytes) to a file path.

        Args:
            key: The cache key as bytes.

        Returns:
            Path object for the file.
        """
        key_hex = key.hex()
        # Create a two-level directory structure to avoid too many files in one
        # directory. Use first 2 chars and next 2 chars as subdirectories
        if len(key_hex) >= 4:
            subdir1 = key_hex[:2]
            subdir2 = key_hex[2:4]
            filepath = self.root_path / subdir1 / subdir2 / key_hex
        else:
            # Fallback for very short keys
            filepath = self.root_path / key_hex

        return filepath

    @Status.capture_exception
    def open(self) -> Status:
        """Open a connection by ensuring the root directory exists."""
        try:
            ensure_dir_exist(str(self.root_path))
            return Status.ok()
        except Exception as e:
            logger.error(f"SHFS open() failed: {e}")
            return Status(
                StatusCodes.ERROR, f"Failed to create root directory: {e}"
            )

    @Status.capture_exception
    def close(self) -> Status:
        """Close a connection."""
        return Status.ok()

    def get_batches(
        self,
        keys: Sequence[bytes],
        mrs: Sequence[MemoryRegion | Sequence[MemoryRegion]],
        batch_size: int,
    ) -> Sequence[
        Sequence[tuple[bytes, MemoryRegion | Sequence[MemoryRegion]]]
    ]:
        """Get batches for mput/mget operations."""
        batches = []
        current_batch = []

        for key, mr in zip(keys, mrs):
            current_batch.append((key, mr))
            if len(current_batch) >= batch_size:
                batches.append(current_batch)
                current_batch = []

        if current_batch:
            batches.append(current_batch)

        return batches

    @Status.capture_exception
    async def mget(
        self,
        keys: Sequence[bytes],
        mrs: Sequence[MemoryRegion | Sequence[MemoryRegion]],
    ) -> Sequence[Status]:
        """MGet a list of values."""
        statuses = []

        for i, (key, mr) in enumerate(zip(keys, mrs)):
            status = await self.get(key, mr)
            statuses.append(status)
            if not status.is_ok() and not status.is_not_found():
                logger.error(f"SHFS mget[{i}] failed: {status}")

        return statuses

    @Status.capture_exception
    async def mput(
        self,
        keys: Sequence[bytes],
        mrs: Sequence[MemoryRegion | Sequence[MemoryRegion]],
    ) -> Sequence[Status]:
        """MPut a list of key value pairs."""
        statuses = []

        for i, (key, mr) in enumerate(zip(keys, mrs)):
            status = await self.put(key, mr)
            statuses.append(status)
            if not status.is_ok():
                logger.error(f"SHFS mput[{i}] failed: {status}")

        return statuses

    @Status.capture_exception
    def _exists(self, key: bytes) -> Status:
        """Check if key is in the store."""
        filepath = self._key_to_filepath(key)

        try:
            if filepath.exists():
                return Status.ok()
            else:
                return Status(StatusCodes.NOT_FOUND)
        except Exception as e:
            logger.error(f"SHFS exists failed: {e}")
            return Status(StatusCodes.ERROR, f"Failed to check existence: {e}")

    @Status.capture_exception
    def _get(
        self,
        key: bytes,
        mr: MemoryRegion | Sequence[MemoryRegion],
    ) -> Status:
        """Get a value."""
        if isinstance(mr, Sequence):
            # For sequence of MRs, we need to handle differently
            # For now, assume single MR
            if len(mr) != 1:
                logger.error(
                    f"SHFS get: Sequence MR with {len(mr)} elements unsupported"
                )
                return Status(
                    StatusCodes.ERROR,
                    "Sequence MR with multiple elements unsupported",
                )
            mr = mr[0]

        filepath = self._key_to_filepath(key)

        try:
            if not filepath.exists():
                return Status(StatusCodes.NOT_FOUND)

            file_size = filepath.stat().st_size

            if file_size != mr.length:
                logger.error(
                    f"SHFS get: file size mismatch: {file_size} != {mr.length}"
                )
                return Status(
                    StatusCodes.ERROR,
                    f"File size mismatch: {file_size} != {mr.length}",
                )

            with open(filepath, "rb") as f:
                data = f.read()

            if len(data) != mr.length:
                logger.error(
                    f"SHFS get: data size mismatch: {len(data)} != {mr.length}"
                )
                return Status(
                    StatusCodes.ERROR,
                    f"Data size mismatch: {len(data)} != {mr.length}",
                )

            mr.fill(data)
            return Status.ok()

        except Exception as e:
            logger.error(f"SHFS get failed: {e}")
            return Status(StatusCodes.ERROR, f"Failed to get value: {e}")

    @Status.capture_exception
    def _put(
        self,
        key: bytes,
        mr: MemoryRegion | Sequence[MemoryRegion],
    ) -> Status:
        """Put a key value pair."""
        if isinstance(mr, Sequence):
            # For sequence of MRs, we need to handle differently
            # For now, assume single MR
            if len(mr) != 1:
                logger.error(
                    f"SHFS put: Sequence MR with {len(mr)} elements unsupported"
                )
                return Status(
                    StatusCodes.ERROR,
                    "Sequence MR with multiple elements unsupported",
                )
            mr = mr[0]

        filepath = self._key_to_filepath(key)

        try:
            filepath.parent.mkdir(parents=True, exist_ok=True)

            data = mr.tobytes()

            # Write atomically: write to temp file first, then rename
            temp_filepath = filepath.with_suffix(filepath.suffix + ".tmp")

            with open(temp_filepath, "wb") as f:
                bytes_written = f.write(data)

            if bytes_written != len(data):
                msg = f"Incomplete write: {bytes_written} != {len(data)}"
                logger.error(f"SHFS put: {msg.lower()}")
                temp_filepath.unlink(missing_ok=True)
                return Status(StatusCodes.ERROR, msg)

            temp_filepath.replace(filepath)

            if filepath.exists():
                actual_size = filepath.stat().st_size
                if actual_size != len(data):
                    msg = (
                        f"File size mismatch after write: "
                        f"{actual_size} != {len(data)}"
                    )
                    logger.error(f"SHFS put: {msg.lower()}")
                    return Status(StatusCodes.ERROR, msg)

            return Status.ok()

        except Exception as e:
            logger.error(f"SHFS put failed: {e}")
            temp_filepath = filepath.with_suffix(filepath.suffix + ".tmp")
            temp_filepath.unlink(missing_ok=True)
            return Status(StatusCodes.ERROR, f"Failed to put value: {e}")

    @Status.capture_exception
    def _delete(self, key: bytes) -> Status:
        """Delete a key."""
        filepath = self._key_to_filepath(key)

        try:
            if filepath.exists():
                filepath.unlink()
                return Status.ok()
            else:
                return Status(StatusCodes.NOT_FOUND)
        except Exception as e:
            logger.error(f"SHFS delete failed: {e}")
            return Status(StatusCodes.ERROR, f"Failed to delete key: {e}")

    def register_slabs(self, slabs: list[torch.Tensor]) -> Status:
        """Register slabs with backend-specific register function.

        SHFS doesn't need to register slabs since it uses file I/O.
        """
        # No-op for SHFS
        return Status.ok()

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

import rocksdict
import torch

from ... import envs
from ...common import AsyncBase
from ...memory import MemoryRegion
from ...status import Status, StatusCodes
from ...utils import ensure_dir_exist
from . import Connector, ConnectorFeature


@AsyncBase.async_wrap(
    exists="_exists", get="_get", put="_put", delete="_delete"
)
class RocksDBConnector(Connector[bytes, torch.Tensor], AsyncBase):
    """RocksDB connector."""

    def __init__(
        self,
        path: str,
        opts: rocksdict.Options,
        access_type: rocksdict.AccessType,
        executor: Executor,
    ):
        super().__init__(executor)
        self.path = path
        self.opts = opts
        self.access_type = access_type
        self.store: rocksdict.Rdict | None = None

    @classmethod
    def from_envs(
        cls, conn_id: str, executor: Executor, **kwargs
    ) -> "RocksDBConnector":
        """Create a connector from environment variables."""
        assert len(kwargs) == 0, "rocksdb connector does not support kwargs"
        root = envs.AIBRIX_KV_CACHE_OL_ROCKSDB_ROOT
        root = os.path.join(os.path.expanduser(root), conn_id)
        opts = rocksdict.Options(raw_mode=True)
        opts.create_if_missing(True)
        opts.create_missing_column_families(True)
        opts.set_write_buffer_size(
            envs.AIBRIX_KV_CACHE_OL_ROCKSDB_WRITE_BUFFER_SIZE
        )
        opts.set_target_file_size_base(
            envs.AIBRIX_KV_CACHE_OL_ROCKSDB_TARGET_FILE_SIZE_BASE
        )
        opts.set_max_write_buffer_number(
            envs.AIBRIX_KV_CACHE_OL_ROCKSDB_MAX_WRITE_BUFFER_NUMBER
        )
        opts.set_max_background_jobs(
            envs.AIBRIX_KV_CACHE_OL_ROCKSDB_MAX_BACKGROUND_JOBS
        )
        opts.set_max_total_wal_size(
            envs.AIBRIX_KV_CACHE_OL_ROCKSDB_MAX_TOTAL_WAL_SIZE
        )
        opts.set_wal_dir(os.path.join(os.path.expanduser(root), "wal"))
        opts.set_db_log_dir(os.path.join(os.path.expanduser(root), "db"))
        access_type = rocksdict.AccessType.read_write()
        # use TTL to manage the life cycle of the data in the KV cache
        access_type = access_type.with_ttl(
            envs.AIBRIX_KV_CACHE_OL_ROCKSDB_TTL_S
        )
        return cls(root, opts, access_type, executor)

    @property
    def name(self) -> str:
        return "RocksDB"

    @property
    def feature(self) -> ConnectorFeature:
        return ConnectorFeature()

    def __del__(self) -> None:
        self.close()

    @Status.capture_exception
    def open(self) -> Status:
        """Open a connection."""
        if self.store is None:
            ensure_dir_exist(self.path)
            self.store = rocksdict.Rdict(
                self.path, self.opts, access_type=self.access_type
            )
        return Status.ok()

    @Status.capture_exception
    def close(self) -> Status:
        """Close a connection."""
        if self.store is not None:
            self.store.close()
            self.store = None
        return Status.ok()

    @Status.capture_exception
    def _exists(self, key: bytes) -> Status:
        """Check if key is in the store."""
        assert self.store is not None
        if key in self.store:
            return Status.ok()
        return Status(StatusCodes.NOT_FOUND)

    @Status.capture_exception
    def _get(self, key: bytes, mr: MemoryRegion) -> Status:
        """Get a value."""
        assert self.store is not None
        val = self.store.get(key)
        if val is None:
            return Status(StatusCodes.NOT_FOUND)
        mr.fill(val)
        return Status.ok()

    @Status.capture_exception
    def _put(self, key: bytes, mr: MemoryRegion) -> Status:
        """Put a key value pair"""
        assert self.store is not None
        self.store.put(key, mr.tobytes())
        return Status.ok()

    @Status.capture_exception
    def _delete(self, key: bytes) -> Status:
        """Delete a key."""
        assert self.store is not None
        self.store.delete(key)
        return Status.ok()

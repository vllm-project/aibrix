# Copyright 2026 The Aibrix Team.
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
"""Distributed ownership leases for multi-replica batch scheduling."""

from __future__ import annotations

import os
import socket
import uuid
from dataclasses import dataclass
from typing import Optional, Protocol, runtime_checkable

import aibrix.batch.constant as constant
from aibrix.batch.storage import batch_metastore
from aibrix.storage.redis import RedisStorage

JOB_LEASE_KEY_PREFIX = "batchjob_lease"


@dataclass(frozen=True)
class LeaseAcquisition:
    acquired: bool
    retry_after_seconds: float = 0.0


@runtime_checkable
class JobLease(Protocol):
    ttl_seconds: float
    renew_interval_seconds: float

    async def acquire(self, job_id: str) -> LeaseAcquisition: ...

    async def renew(self, job_id: str) -> bool: ...

    async def release(self, job_id: str) -> bool: ...


class RedisJobLease:
    """Per-scheduler Redis lease with owner-safe renewal and release."""

    def __init__(
        self,
        storage: RedisStorage,
        *,
        ttl_seconds: float = constant.JOB_LEASE_TTL_SECONDS,
        renew_interval_seconds: float = constant.JOB_LEASE_RENEW_INTERVAL_SECONDS,
        owner_token: Optional[str] = None,
    ) -> None:
        self._storage = storage
        self.ttl_seconds = ttl_seconds
        self.renew_interval_seconds = renew_interval_seconds
        instance_name = (
            os.getenv("POD_NAME") or os.getenv("HOSTNAME") or socket.gethostname()
        )
        self._owner_token = owner_token or f"{instance_name}:{uuid.uuid4().hex}"

    @staticmethod
    def _key(job_id: str) -> str:
        return f"{JOB_LEASE_KEY_PREFIX}:{job_id}"

    async def acquire(self, job_id: str) -> LeaseAcquisition:
        acquired, retry_after_seconds = await self._storage.acquire_lease(
            self._key(job_id),
            self._owner_token,
            self.ttl_seconds,
        )
        return LeaseAcquisition(acquired, retry_after_seconds)

    async def renew(self, job_id: str) -> bool:
        return await self._storage.renew_lease(
            self._key(job_id),
            self._owner_token,
            self.ttl_seconds,
        )

    async def release(self, job_id: str) -> bool:
        return await self._storage.release_lease(
            self._key(job_id),
            self._owner_token,
        )


def create_job_lease() -> Optional[JobLease]:
    """Enable distributed scheduling only for the shared Redis metastore."""
    metastore = batch_metastore.p_metastore
    if not isinstance(metastore, RedisStorage):
        return None
    return RedisJobLease(metastore)

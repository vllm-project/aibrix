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

"""Unit tests for the metadata API batch resolve helper.

Exercises ``aibrix.metadata.api.v1.batch._resolve_batch_job`` directly
with stubbed app state, so the test does not need a running FastAPI
TestClient or a real ``BatchDriver``. The helper now resolves through
``BatchDriver`` so GET can trigger on-demand republishing of unfinished
jobs that were missed during restart recovery.
"""

import os
import uuid
from datetime import datetime, timezone
from types import SimpleNamespace
from typing import Optional

# Set required environment variable before importing
os.environ.setdefault("SECRET_KEY", "test-secret-key-for-testing")

import pytest

from aibrix.batch.job_entity.batch_job import (
    BatchJob,
    BatchJobEndpoint,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    CompletionWindow,
    ObjectMeta,
    RequestCountStats,
    TypeMeta,
)
from aibrix.metadata.api.v1.batch import _resolve_batch_job


def _make_job(batch_id: str) -> BatchJob:
    return BatchJob(
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
        metadata=ObjectMeta(
            name="test-job",
            namespace="default",
            uid=str(uuid.uuid4()),
            creationTimestamp=datetime.now(timezone.utc),
            resourceVersion="1",
            deletionTimestamp=None,
        ),
        spec=BatchJobSpec(
            input_file_id="file-input",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
        ),
        status=BatchJobStatus(
            jobID=batch_id,
            state=BatchJobState.IN_PROGRESS,
            createdAt=datetime.now(timezone.utc),
            requestCounts=RequestCountStats(
                total=10, launched=0, completed=2, failed=0
            ),
        ),
    )


class _StubJobManager:
    def __init__(self, jobs: dict):
        self._jobs = jobs

    async def get_job(self, batch_id: str) -> Optional[BatchJob]:
        return self._jobs.get(batch_id)


class _StubBatchDriver:
    """Mimics the BatchDriver facade used by the helper's fallback path.

    The real ``get_job`` marshals onto the driver's event loop; for a unit
    test the stub manager can be queried inline.
    """

    def __init__(self, jobs: dict):
        self._job_manager = _StubJobManager(jobs)

    async def get_job(self, batch_id: str):
        return await self._job_manager.get_job(batch_id)


def _make_request(*, manager_jobs: Optional[dict] = None):
    state = SimpleNamespace(
        batch_driver=_StubBatchDriver(manager_jobs or {}),
    )
    return SimpleNamespace(app=SimpleNamespace(state=state))


@pytest.mark.asyncio
async def test_driver_hit_returns_driver_document():
    job = _make_job("batch-driver-hit")
    request = _make_request(manager_jobs={"batch-driver-hit": job})

    result = await _resolve_batch_job(request, "batch-driver-hit")

    assert result is not None
    assert result.status.job_id == "batch-driver-hit"


@pytest.mark.asyncio
async def test_metastore_miss_falls_back_to_job_manager():
    """Covers the K8s submit -> kopf ADDED gap where the metadata service
    has already seeded its BatchManager pool synchronously on POST."""
    job = _make_job("batch-gap")
    request = _make_request(manager_jobs={"batch-gap": job})

    result = await _resolve_batch_job(request, "batch-gap")
    assert result is not None
    assert result.status.job_id == "batch-gap"


@pytest.mark.asyncio
async def test_both_miss_returns_none():
    request = _make_request(manager_jobs={})

    result = await _resolve_batch_job(request, "batch-missing")
    assert result is None

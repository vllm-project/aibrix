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

"""Unit tests for the metadata API read-flip helper.

Exercises ``aibrix.metadata.api.v1.batch._resolve_batch_job`` directly
with stubbed app state, so the test does not need a running FastAPI
TestClient or a real ``BatchDriver``. The point is to pin down the
metastore-first-with-JobManager-fallback contract that closes the
submit -> kopf ADDED window.
"""

import os
import uuid
from datetime import datetime, timezone
from types import SimpleNamespace
from typing import Optional
from unittest.mock import patch

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
    """Mimics enough of BatchDriver for the helper's fallback path.

    ``run_coroutine`` on the real driver schedules the coroutine on a
    dedicated event loop; for a unit test the same coroutine can be
    awaited inline.
    """

    def __init__(self, jobs: dict):
        self.job_manager = _StubJobManager(jobs)

    async def run_coroutine(self, coro):
        return await coro


def _make_request(*, manager_jobs: Optional[dict] = None):
    state = SimpleNamespace(
        batch_driver=_StubBatchDriver(manager_jobs or {}),
    )
    return SimpleNamespace(app=SimpleNamespace(state=state))


@pytest.mark.asyncio
async def test_metastore_hit_returns_metastore_document():
    job = _make_job("batch-store-hit")

    # JobManager has nothing — proves we did not fall through.
    request = _make_request(manager_jobs={})

    async def fake_get(batch_id: str) -> Optional[BatchJob]:
        return job if batch_id == "batch-store-hit" else None

    with patch("aibrix.metadata.api.v1.batch.get_batch_job", side_effect=fake_get):
        result = await _resolve_batch_job(request, "batch-store-hit")
    assert result is not None
    assert result.status.job_id == "batch-store-hit"


@pytest.mark.asyncio
async def test_metastore_miss_falls_back_to_job_manager():
    """Covers the K8s submit -> kopf ADDED gap where the metastore has
    not yet observed the new job but the metadata service already
    seeded its JobManager pool synchronously on POST."""
    job = _make_job("batch-gap")
    request = _make_request(manager_jobs={"batch-gap": job})

    async def fake_get(batch_id: str) -> Optional[BatchJob]:
        return None

    with patch("aibrix.metadata.api.v1.batch.get_batch_job", side_effect=fake_get):
        result = await _resolve_batch_job(request, "batch-gap")
    assert result is not None
    assert result.status.job_id == "batch-gap"


@pytest.mark.asyncio
async def test_both_miss_returns_none():
    request = _make_request(manager_jobs={})

    async def fake_get(batch_id: str) -> Optional[BatchJob]:
        return None

    with patch("aibrix.metadata.api.v1.batch.get_batch_job", side_effect=fake_get):
        result = await _resolve_batch_job(request, "batch-missing")
    assert result is None


@pytest.mark.asyncio
async def test_metastore_takes_precedence_over_job_manager():
    """If the metastore has an updated copy and JobManager has stale
    state, the read returns the metastore's view — that is the whole
    point of the read flip."""
    fresh = _make_job("batch-fresh")
    fresh.status.state = BatchJobState.FINALIZED
    fresh.status.request_counts.completed = 10

    stale = _make_job("batch-fresh")
    stale.status.state = BatchJobState.IN_PROGRESS
    stale.status.request_counts.completed = 5
    request = _make_request(manager_jobs={"batch-fresh": stale})

    async def fake_get(batch_id: str) -> Optional[BatchJob]:
        return fresh if batch_id == "batch-fresh" else None

    with patch("aibrix.metadata.api.v1.batch.get_batch_job", side_effect=fake_get):
        result = await _resolve_batch_job(request, "batch-fresh")
    assert result is not None
    assert result.status.state == BatchJobState.FINALIZED
    assert result.status.request_counts.completed == 10

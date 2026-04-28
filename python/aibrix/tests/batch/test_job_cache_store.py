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

"""Unit tests for JobCache integration with the BatchJobStore.

These tests exercise ``JobCache._put_to_store`` and ``_delete_from_store``
directly so we can pin the store contract (LWW write, idempotent
delete, no-op when the store is unconfigured, error swallowing on
write) without standing up a kubernetes client mock. The higher-level
flow tests for ``update_job_status`` and the kopf monotonicity rule
live further below in this file.
"""

import os
import uuid
from datetime import datetime, timezone
from pathlib import Path
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
from aibrix.batch.store import BatchJobStore, InMemoryBatchJobStore
from aibrix.batch.template import local_profile_registry, local_template_registry
from aibrix.metadata.cache.job import JobCache

_FIXTURE = Path(__file__).parent / "testdata" / "template_configmaps_unittest.yaml"


def _make_job(batch_id: str = "batch-1") -> BatchJob:
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


def _make_cache(store: Optional[BatchJobStore]) -> JobCache:
    template_registry = local_template_registry(_FIXTURE)
    profile_registry = local_profile_registry(_FIXTURE)
    template_registry.reload()
    profile_registry.reload()
    return JobCache(
        template_registry=template_registry,
        profile_registry=profile_registry,
        batch_job_store=store,
    )


@pytest.mark.asyncio
async def test_put_to_store_writes_when_store_configured():
    store = InMemoryBatchJobStore()
    cache = _make_cache(store)

    job = _make_job("batch-1")
    await cache._put_to_store(job, op="update_job_status")

    fetched = await store.get("batch-1")
    assert fetched is not None
    assert fetched.status.state == BatchJobState.IN_PROGRESS
    assert fetched.status.request_counts.completed == 2


@pytest.mark.asyncio
async def test_put_to_store_is_noop_when_store_is_none():
    cache = _make_cache(None)
    # Must not raise, must not error.
    await cache._put_to_store(_make_job(), op="update_job_status")


@pytest.mark.asyncio
async def test_delete_from_store_removes_document():
    store = InMemoryBatchJobStore()
    cache = _make_cache(store)

    job = _make_job("batch-del")
    await cache._put_to_store(job, op="update_job_ready")
    assert await store.get("batch-del") is not None

    await cache._delete_from_store(job)
    assert await store.get("batch-del") is None


@pytest.mark.asyncio
async def test_delete_from_store_is_noop_when_store_is_none():
    cache = _make_cache(None)
    await cache._delete_from_store(_make_job())


class _FailingStore(BatchJobStore):
    """BatchJobStore that fails every write/delete, used to pin error
    semantics on the JobCache helpers."""

    async def get(self, batch_id):
        return None

    async def put(self, batch_id, job):
        raise RuntimeError("simulated store outage")

    async def delete(self, batch_id):
        raise RuntimeError("simulated store outage")


@pytest.mark.asyncio
async def test_put_to_store_default_swallows_failures(caplog):
    """Default behavior (no ``propagate`` flag) swallows store errors.
    Reserved for the kopf seed write where the next event will retry;
    status-mutation callers must opt into propagation explicitly."""
    cache = _make_cache(_FailingStore())
    await cache._put_to_store(_make_job("batch-fail"), op="job_created")


@pytest.mark.asyncio
async def test_put_to_store_propagates_when_requested():
    """Status-mutation callers (update_job_status / update_job_ready /
    cancel_job) pass ``propagate=True`` because the store is the only
    persistent record; a swallowed failure would leave active_jobs
    ahead of disk and surface as stale reads after restart."""
    cache = _make_cache(_FailingStore())
    with pytest.raises(RuntimeError, match="simulated store outage"):
        await cache._put_to_store(
            _make_job("batch-fail"), op="update_job_status", propagate=True
        )


@pytest.mark.asyncio
async def test_delete_from_store_swallows_failures():
    """Delete is best-effort: the K8s ``delete_namespaced_job`` op is
    the authoritative deletion signal, so a store delete failure leaks
    a stale document at worst."""
    cache = _make_cache(_FailingStore())
    await cache._delete_from_store(_make_job("batch-fail"))


@pytest.mark.asyncio
async def test_subsequent_puts_overwrite_via_lww():
    store = InMemoryBatchJobStore()
    cache = _make_cache(store)

    job = _make_job("batch-lww")
    await cache._put_to_store(job, op="update_job_status")

    job.status.request_counts.completed = 9
    job.status.state = BatchJobState.FINALIZED
    await cache._put_to_store(job, op="update_job_status")

    fetched = await store.get("batch-lww")
    assert fetched is not None
    assert fetched.status.state == BatchJobState.FINALIZED
    assert fetched.status.request_counts.completed == 9


@pytest.mark.asyncio
async def test_update_job_status_writes_store_and_cache_no_k8s():
    """As of PR4, update_job_status no longer issues a K8s patch. The
    BatchJobStore is the source of truth and active_jobs is updated
    directly so the JobManager pool stays in sync without waiting for a
    kopf MODIFIED echo."""
    store = InMemoryBatchJobStore()
    cache = _make_cache(store)
    # Replace the K8s client with one that fails on any call so the test
    # asserts no K8s round-trip is attempted.

    class _ExplodingApi:
        def patch_namespaced_job(self, *args, **kwargs):
            raise AssertionError("update_job_status must not call K8s")

    cache.batch_v1_api = _ExplodingApi()

    job = _make_job("batch-update")
    await cache.update_job_status(job)

    assert (await store.get("batch-update")) is not None
    assert "batch-update" in cache.active_jobs
    assert cache.active_jobs["batch-update"].status.state == BatchJobState.IN_PROGRESS


@pytest.mark.asyncio
async def test_update_job_status_fires_job_updated_callback():
    """The callback is how JobManager moves a job between its pending /
    in_progress / done pools. Without a kopf MODIFIED to trigger it
    after we dropped the annotation patch, JobCache must invoke it."""
    store = InMemoryBatchJobStore()
    cache = _make_cache(store)

    received: list = []

    async def _on_updated(old, new):
        received.append((old.status.state, new.status.state))
        return True

    cache.on_job_updated(_on_updated)

    initial = _make_job("batch-cb")
    initial.status.state = BatchJobState.IN_PROGRESS
    cache.active_jobs["batch-cb"] = initial

    advanced = _make_job("batch-cb")
    advanced.status.state = BatchJobState.FINALIZED

    await cache.update_job_status(advanced)

    assert received == [(BatchJobState.IN_PROGRESS, BatchJobState.FINALIZED)]


@pytest.mark.asyncio
async def test_update_job_status_raises_and_does_not_advance_cache_on_store_failure():
    """When the store is unreachable, update_job_status must raise and
    leave ``active_jobs`` at its previous value. Otherwise the
    in-memory view would advance past the persistent record and the
    next API read (which prefers the store) would silently regress."""
    cache = _make_cache(_FailingStore())

    initial = _make_job("batch-fail")
    initial.status.state = BatchJobState.IN_PROGRESS
    cache.active_jobs["batch-fail"] = initial

    advanced = _make_job("batch-fail")
    advanced.status.state = BatchJobState.FINALIZED

    with pytest.raises(RuntimeError, match="simulated store outage"):
        await cache.update_job_status(advanced)

    # Cache must still hold the pre-failure view.
    assert cache.active_jobs["batch-fail"].status.state == BatchJobState.IN_PROGRESS


@pytest.mark.asyncio
async def test_update_job_status_first_seen_skips_callback():
    """First-time observation has no ``old`` to diff against; the
    callback is reserved for genuine transitions."""
    store = InMemoryBatchJobStore()
    cache = _make_cache(store)

    received: list = []

    async def _on_updated(old, new):  # pragma: no cover - asserted not called
        received.append((old, new))
        return True

    cache.on_job_updated(_on_updated)

    job = _make_job("batch-first")
    await cache.update_job_status(job)

    assert received == []
    assert "batch-first" in cache.active_jobs


# ---------------------------------------------------------------------------
# Kopf monotonicity rule.
#
# K8s annotations are no longer authoritative for status. A non-status
# K8s patch (e.g. ``update_job_ready`` flipping ``spec.suspend = False``)
# still fires kopf MODIFIED, which re-runs the transformer with absent
# JOB_STATE annotations and produces a CREATED-state BatchJob. The
# kopf-driven cache update must drop such echoes so it never regresses
# below the state JobCache wrote directly.
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_kopf_update_skips_when_cached_state_is_more_advanced(monkeypatch):
    from aibrix.metadata.cache import job as job_module

    captured = {"updated": False}

    class _StubMeta:
        name = "n"
        namespace = "default"
        resource_version = "1"

    class _StubStatus:
        def __init__(self, state):
            self.state = state
            self.job_id = "uid-skip"

    class _StubJob:
        def __init__(self, state):
            self.status = _StubStatus(state)
            self.metadata = _StubMeta()

    class _StubCache:
        def __init__(self):
            self.active_jobs = {"uid-skip": _StubJob(BatchJobState.IN_PROGRESS)}

        async def job_updated(self, old, new):  # pragma: no cover - asserted not called
            captured["updated"] = True
            return True

    stub = _StubCache()
    monkeypatch.setattr(job_module, "get_global_job_cache", lambda: stub)
    monkeypatch.setattr(
        job_module, "k8s_job_to_batch_job", lambda body: _StubJob(BatchJobState.CREATED)
    )

    body = type("B", (), {"metadata": type("M", (), {"uid": "uid-skip"})()})()
    await job_module.job_updated_handler(body)

    assert stub.active_jobs["uid-skip"].status.state == BatchJobState.IN_PROGRESS
    assert captured["updated"] is False


@pytest.mark.asyncio
async def test_kopf_update_propagates_when_state_advances(monkeypatch):
    """A FINALIZING (or higher) view derived from K8s native conditions
    must propagate; that is the whole reason the kopf path stays alive
    after we stopped writing status annotations."""
    from aibrix.metadata.cache import job as job_module

    received: list = []

    class _StubMeta:
        name = "n"
        namespace = "default"
        resource_version = "1"

    class _StubStatus:
        def __init__(self, state):
            self.state = state
            self.job_id = "uid-adv"

    class _StubJob:
        def __init__(self, state):
            self.status = _StubStatus(state)
            self.metadata = _StubMeta()

    cached_job = _StubJob(BatchJobState.IN_PROGRESS)
    new_job = _StubJob(BatchJobState.FINALIZING)

    class _StubCache:
        def __init__(self):
            self.active_jobs = {"uid-adv": cached_job}

        async def job_updated(self, old, new):
            received.append((old.status.state, new.status.state))
            return True

    stub = _StubCache()
    monkeypatch.setattr(job_module, "get_global_job_cache", lambda: stub)
    monkeypatch.setattr(job_module, "k8s_job_to_batch_job", lambda body: new_job)

    body = type("B", (), {"metadata": type("M", (), {"uid": "uid-adv"})()})()
    await job_module.job_updated_handler(body)

    assert received == [(BatchJobState.IN_PROGRESS, BatchJobState.FINALIZING)]
    assert stub.active_jobs["uid-adv"].status.state == BatchJobState.FINALIZING

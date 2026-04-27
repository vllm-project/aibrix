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

import uuid
from datetime import datetime, timezone

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
from aibrix.batch.runtime_updater import (
    JobStateRuntimeUpdater,
    NoOpJobStateRuntimeUpdater,
)


def _make_job() -> BatchJob:
    return BatchJob(
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
        metadata=ObjectMeta(
            name="test-job",
            namespace="default",
            uid=str(uuid.uuid4()),
            creationTimestamp=datetime.now(timezone.utc),
            resourceVersion=None,
            deletionTimestamp=None,
        ),
        spec=BatchJobSpec(
            input_file_id="file-input",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
        ),
        status=BatchJobStatus(
            jobID="job-1",
            state=BatchJobState.IN_PROGRESS,
            createdAt=datetime.now(timezone.utc),
            requestCounts=RequestCountStats(total=1, launched=0, completed=0, failed=0),
        ),
    )


@pytest.mark.asyncio
async def test_noop_updater_swallows_calls():
    updater = NoOpJobStateRuntimeUpdater()
    await updater.update(_make_job())  # must not raise


@pytest.mark.asyncio
async def test_runtime_updater_is_abstract():
    with pytest.raises(TypeError):
        JobStateRuntimeUpdater()  # type: ignore[abstract]


@pytest.mark.asyncio
async def test_custom_updater_receives_job():
    seen: list[BatchJob] = []

    class _Recording(JobStateRuntimeUpdater):
        async def update(self, job: BatchJob) -> None:
            seen.append(job)

    updater = _Recording()
    job = _make_job()
    await updater.update(job)
    assert seen == [job]

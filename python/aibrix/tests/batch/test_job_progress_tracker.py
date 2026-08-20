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

from datetime import datetime

from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    ObjectMeta,
    RequestCountStats,
    TypeMeta,
)
from aibrix.batch.state import JobMetaInfo


def _make_meta(total: int) -> JobMetaInfo:
    job = BatchJob(
        typeMeta=TypeMeta(apiVersion="v1", kind="BatchJob"),
        metadata=ObjectMeta(
            resourceVersion="1",
            creationTimestamp=datetime.now(),
            deletionTimestamp=None,
        ),
        spec=BatchJobSpec(
            input_file_id="input-1",
            endpoint="/v1/chat/completions",
            completion_window=86400,
        ),
        status=BatchJobStatus(
            jobID="job-1",
            state=BatchJobState.IN_PROGRESS,
            createdAt=datetime.now(),
            requestCounts=RequestCountStats(total=total),
        ),
    )
    return JobMetaInfo(job)


def test_complete_one_request_allows_out_of_order_completion():
    meta = _make_meta(total=3)

    meta.complete_one_request(1)
    assert meta.status.request_counts.completed == 1
    assert meta.status.state == BatchJobState.IN_PROGRESS
    meta.complete_one_request(0)
    assert meta.status.state == BatchJobState.IN_PROGRESS
    meta.complete_one_request(2)

    assert meta.status.request_counts.completed == 3
    assert meta.status.request_counts.failed == 0
    assert meta.status.state == BatchJobState.FINALIZING


def test_complete_one_request_is_idempotent_for_counts():
    meta = _make_meta(total=2)

    meta.complete_one_request(0)
    meta.complete_one_request(0)

    assert meta.status.request_counts.completed == 1
    assert meta.status.state == BatchJobState.IN_PROGRESS


def test_failed_completion_counts_toward_finalizing():
    meta = _make_meta(total=2)

    meta.complete_one_request(0, failed=True)
    meta.complete_one_request(1)

    assert meta.status.request_counts.failed == 1
    assert meta.status.request_counts.completed == 1
    assert meta.status.state == BatchJobState.FINALIZING

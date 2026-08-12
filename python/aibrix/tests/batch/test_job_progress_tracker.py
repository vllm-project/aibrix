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

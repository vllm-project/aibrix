from datetime import datetime

from aibrix.batch.job_driver.error_injection import (
    JobDriverErrorInjectionEvent,
    JobDriverErrorInjector,
)
from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    ObjectMeta,
    TypeMeta,
)
from aibrix.batch.metrics import tags_from_job
from aibrix.metadata.core.metrics import Emitter, metrics_names


def _make_job() -> BatchJob:
    return BatchJob(
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
        ),
    )


def test_tags_from_job_defaults_when_job_is_missing():
    assert tuple(tag.value for tag in tags_from_job(None)) == (
        "none",
        "none",
        "none",
        "none",
    )


def test_log_event_emits_default_job_tags_when_job_is_missing(monkeypatch):
    metric_calls: list[tuple[str, float, tuple[str, ...]]] = []

    def _record_counter(name, value, *tags):
        metric_calls.append((name, value, tuple(tag.value for tag in tags)))

    monkeypatch.setattr(Emitter, "counter", _record_counter)
    injector = JobDriverErrorInjector(None)
    event = JobDriverErrorInjectionEvent(
        opt_key="fail_init_runtime",
        breakpoint="runtime_initialization",
        action="raise",
    )

    injector._log_event(event)

    assert metric_calls == [
        (
            metrics_names.METRIC_METADATA_BATCH_DRIVER_FAILURE_INJECTION,
            1,
            (
                "none",
                "none",
                "none",
                "none",
                event.opt_key,
                event.breakpoint,
                event.action,
            ),
        )
    ]


def test_tags_from_job_keeps_job_specific_values():
    job = _make_job()

    assert tuple(tag.value for tag in tags_from_job(job)) == (
        job.spec.endpoint,
        str(job.spec.completion_window),
        job.job_id,
        "none",
    )

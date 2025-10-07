"""Unit tests for job status persistence functionality."""

import json
from datetime import datetime, timezone

from aibrix.batch.job_entity import (
    BatchJobError,
    BatchJobErrorCode,
    BatchJobState,
    BatchJobStatus,
    BatchJobTransformer,
    Condition,
    ConditionStatus,
    ConditionType,
    JobAnnotationKey,
    RequestCountStats,
)


def test_create_status_annotations():
    """Test that create_status_annotations correctly handles all possible fields."""
    # Create timestamps
    base_time = datetime(2024, 1, 1, 12, 0, 0, tzinfo=timezone.utc)
    in_progress_time = datetime(2024, 1, 1, 12, 5, 0, tzinfo=timezone.utc)
    cancelling_time = datetime(2024, 1, 1, 12, 10, 0, tzinfo=timezone.utc)
    completed_time = datetime(2024, 1, 1, 12, 15, 0, tzinfo=timezone.utc)
    finalizing_time = datetime(2024, 1, 1, 12, 16, 0, tzinfo=timezone.utc)
    finalized_time = datetime(2024, 1, 1, 12, 17, 0, tzinfo=timezone.utc)

    # Create comprehensive conditions - one of each type
    conditions = [
        Condition(
            type=ConditionType.COMPLETED,
            status=ConditionStatus.TRUE,
            lastTransitionTime=completed_time,
            reason="AllCompleted",
            message="All requests completed successfully",
        ),
        Condition(
            type=ConditionType.FAILED,
            status=ConditionStatus.TRUE,
            lastTransitionTime=completed_time,
            reason="ProcessingFailed",
            message="Some requests failed",
        ),
        Condition(
            type=ConditionType.CANCELLED,
            status=ConditionStatus.TRUE,
            lastTransitionTime=cancelling_time,
            reason="UserCancelled",
            message="Job cancelled by user",
        ),
    ]

    # Create errors list that would be persisted
    errors = [
        BatchJobError(
            code=BatchJobErrorCode.INVALID_INPUT_FILE,
            message="Input file contains invalid JSON",
            param="input_file",
            line=42,
        ),
        BatchJobError(
            code=BatchJobErrorCode.INFERENCE_FAILED,
            message="Model inference timeout",
            param="timeout",
        ),
    ]

    # Create status with all possible fields that create_status_annotations touches
    status = BatchJobStatus(
        jobID="test-job-id",
        state=BatchJobState.FINALIZED,  # Required for finalized state annotation
        createdAt=base_time,
        inProgressAt=in_progress_time,  # Touches IN_PROGRESS_AT
        cancellingAt=cancelling_time,  # Touches CANCELLING_AT
        completedAt=completed_time,  # Touches COMPLETED_AT
        finalizingAt=finalizing_time,  # Touches FINALIZING_AT
        finalizedAt=finalized_time,  # Touches FINALIZED_AT
        conditions=conditions,  # Touches CONDITION (priority: cancelled > failed, completed not persisted)
        errors=errors,  # Touches ERRORS
        requestCounts=RequestCountStats(  # Touches REQUEST_COUNTS
            total=100,
            launched=95,
            completed=85,
            failed=10,
        ),
    )

    # Call create_status_annotations
    annotations = BatchJobTransformer.create_status_annotations(status)
    assert json.dumps(annotations)

    # Verify all expected annotations are created

    # 1. Finalized state annotation (only for FINALIZED state)
    assert (
        annotations[JobAnnotationKey.JOB_STATE.value] == BatchJobState.FINALIZED.value
    )

    # 2. Condition annotation (should prioritize CANCELLED over FAILED; COMPLETED not persisted)
    assert (
        annotations[JobAnnotationKey.CONDITION.value] == ConditionType.CANCELLED.value
    )

    # 3. Errors annotation (only if errors exist)
    errors_json = annotations[JobAnnotationKey.ERRORS.value]
    errors_data = json.loads(errors_json)
    assert len(errors_data) == 2
    assert errors_data[0]["code"] == BatchJobErrorCode.INVALID_INPUT_FILE.value
    assert errors_data[0]["message"] == "Input file contains invalid JSON"
    assert errors_data[0]["param"] == "input_file"
    assert errors_data[0]["line"] == 42
    assert errors_data[1]["code"] == BatchJobErrorCode.INFERENCE_FAILED.value
    assert errors_data[1]["message"] == "Model inference timeout"
    assert errors_data[1]["param"] == "timeout"
    assert errors_data[1]["line"] is None

    # 4. Request counts annotation (only if total > 0)
    request_counts_json = annotations[JobAnnotationKey.REQUEST_COUNTS.value]
    request_counts_data = json.loads(request_counts_json)
    assert request_counts_data == {
        "total": 100,
        "launched": 95,
        "completed": 85,
        "failed": 10,
    }

    # 5. Timestamp annotations (only if timestamps exist)
    assert (
        annotations[JobAnnotationKey.IN_PROGRESS_AT.value]
        == in_progress_time.isoformat()
    )
    assert (
        annotations[JobAnnotationKey.FINALIZING_AT.value] == finalizing_time.isoformat()
    )
    assert (
        annotations[JobAnnotationKey.FINALIZED_AT.value] == finalized_time.isoformat()
    )
    assert (
        annotations[JobAnnotationKey.CANCELLING_AT.value] == cancelling_time.isoformat()
    )


def test_create_status_annotations_condition_priority():
    """Test that condition annotation respects priority: cancelled > failed (completed not persisted)."""
    base_time = datetime.now(timezone.utc)

    # Test priority: when multiple conditions exist, CANCELLED takes precedence
    conditions_with_cancelled = [
        Condition(
            type=ConditionType.COMPLETED,
            status=ConditionStatus.TRUE,
            lastTransitionTime=base_time,
        ),
        Condition(
            type=ConditionType.CANCELLED,
            status=ConditionStatus.TRUE,
            lastTransitionTime=base_time,
        ),
        Condition(
            type=ConditionType.FAILED,
            status=ConditionStatus.TRUE,
            lastTransitionTime=base_time,
        ),
    ]

    status = BatchJobStatus(
        jobID="test-job-id",
        state=BatchJobState.FINALIZED,
        createdAt=base_time,
        conditions=conditions_with_cancelled,
        requestCounts=RequestCountStats(total=10),
    )

    annotations = BatchJobTransformer.create_status_annotations(status)
    assert json.dumps(annotations)
    assert (
        annotations[JobAnnotationKey.CONDITION.value] == ConditionType.CANCELLED.value
    )

    # Test priority: when no CANCELLED, FAILED takes precedence over COMPLETED
    conditions_failed_completed = [
        Condition(
            type=ConditionType.COMPLETED,
            status=ConditionStatus.TRUE,
            lastTransitionTime=base_time,
        ),
        Condition(
            type=ConditionType.FAILED,
            status=ConditionStatus.TRUE,
            lastTransitionTime=base_time,
        ),
    ]

    status.conditions = conditions_failed_completed
    annotations = BatchJobTransformer.create_status_annotations(status)
    assert annotations[JobAnnotationKey.CONDITION.value] == ConditionType.FAILED.value

    # Test: when only COMPLETED exists, no condition annotation is created
    conditions_only_completed = [
        Condition(
            type=ConditionType.COMPLETED,
            status=ConditionStatus.TRUE,
            lastTransitionTime=base_time,
        ),
    ]

    status.conditions = conditions_only_completed
    annotations = BatchJobTransformer.create_status_annotations(status)
    assert (
        JobAnnotationKey.CONDITION.value not in annotations
    )  # COMPLETED is not persisted


def test_create_status_annotations_errors():
    """Test that create_status_annotations correctly handles errors field."""
    base_time = datetime.now(timezone.utc)

    # Test with errors present
    errors = [
        BatchJobError(
            code=BatchJobErrorCode.AUTHENTICATION_ERROR,
            message="Invalid API key",
            param="api_key",
        ),
    ]

    status = BatchJobStatus(
        jobID="test-job-id",
        state=BatchJobState.FINALIZED,
        createdAt=base_time,
        errors=errors,
        requestCounts=RequestCountStats(total=0),
    )

    annotations = BatchJobTransformer.create_status_annotations(status)
    assert json.dumps(annotations)

    # Should persist errors
    assert JobAnnotationKey.ERRORS.value in annotations
    errors_json = annotations[JobAnnotationKey.ERRORS.value]
    errors_data = json.loads(errors_json)
    assert len(errors_data) == 1
    assert errors_data[0]["code"] == BatchJobErrorCode.AUTHENTICATION_ERROR.value
    assert errors_data[0]["message"] == "Invalid API key"
    assert errors_data[0]["param"] == "api_key"
    assert errors_data[0]["line"] is None

    # Test with None errors
    status.errors = None
    annotations = BatchJobTransformer.create_status_annotations(status)
    assert JobAnnotationKey.ERRORS.value not in annotations

    # Test with empty errors list
    status.errors = []
    annotations = BatchJobTransformer.create_status_annotations(status)
    assert JobAnnotationKey.ERRORS.value not in annotations


def test_create_status_annotations_edge_cases():
    """Test edge cases for create_status_annotations."""
    base_time = datetime.now(timezone.utc)

    # Test with empty conditions list
    status = BatchJobStatus(
        jobID="test-job-id",
        state=BatchJobState.FINALIZED,
        createdAt=base_time,
        conditions=[],  # Empty list
        requestCounts=RequestCountStats(total=0),
    )

    annotations = BatchJobTransformer.create_status_annotations(status)
    assert json.dumps(annotations)

    # Should persist finalized state but no condition
    assert (
        annotations[JobAnnotationKey.JOB_STATE.value] == BatchJobState.FINALIZED.value
    )
    assert JobAnnotationKey.CONDITION.value not in annotations
    assert JobAnnotationKey.REQUEST_COUNTS.value not in annotations  # total=0

    # Test with None conditions
    status.conditions = None
    annotations = BatchJobTransformer.create_status_annotations(status)

    assert (
        annotations[JobAnnotationKey.JOB_STATE.value] == BatchJobState.FINALIZED.value
    )
    assert JobAnnotationKey.CONDITION.value not in annotations

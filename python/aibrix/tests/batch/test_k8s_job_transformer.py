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
from types import SimpleNamespace
from typing import Any

import kopf
import pytest

from aibrix.batch.job_entity import (
    BatchJobEndpoint,
    BatchJobErrorCode,
    BatchJobState,
    CompletionWindow,
    ConditionStatus,
    ConditionType,
    k8s_job_to_batch_job,
)
from aibrix.metadata.cache.utils import merge_yaml_object


class MockK8sJob:
    """Mock Kubernetes Job object for testing."""

    def __init__(
        self,
        metadata=None,
        annotations=None,
        status=None,
        api_version="batch/v1",
        kind="Job",
    ):
        self.metadata = metadata or {}
        self.status = status or {}
        self.spec = {"template": {"metadata": {"annotations": annotations or {}}}}
        self.api_version = api_version
        self.kind = kind


creation_time = datetime.fromisoformat("2025-08-05T05:26:10+00:00")
start_time = datetime.fromisoformat("2025-08-05T05:26:13+00:00")
cancel_time = datetime.fromisoformat("2025-08-05T05:26:20+00:00")
update_time = datetime.fromisoformat("2025-08-05T05:26:25+00:00")
end_time = datetime.fromisoformat("2025-08-05T05:26:30+00:00")


def _get_job_base_obj():
    return {
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": {
            "name": "test-batch-job",
            "namespace": "default",
            "uid": "test-uid-123",
            "creationTimestamp": creation_time.isoformat(),
        },
        "spec": {
            "template": {
                "metadata": {
                    "annotations": {
                        "batch.job.aibrix.ai/input-file-id": "file-123",
                        "batch.job.aibrix.ai/endpoint": "/v1/chat/completions",
                        "batch.job.aibrix.ai/metadata.priority": "high",
                        "batch.job.aibrix.ai/metadata.customer": "test-customer",
                    },
                },
            },
            "activeDeadlineSeconds": 86400,
        },
        "status": {
            "startTime": creation_time.isoformat(),
            "active": 0,
            "terminating": 0,
            "uncountedTerminatedPods": {},
            "ready": 0,
        },
    }


def _get_job_created_obj():
    return merge_yaml_object(
        _get_job_base_obj(),
        {
            "spec": {
                "suspend": True,
            },
        },
    )


def _get_job_in_progress_obj():
    return merge_yaml_object(
        _get_job_base_obj(),
        {
            "metadata": {
                "annotations": {
                    "batch.job.aibrix.ai/state": "in_progress",
                    "batch.job.aibrix.ai/in-progress-at": start_time.isoformat(),
                },
            },
            "spec": {
                "template": {
                    "metadata": {
                        "annotations": {
                            "batch.job.aibrix.ai/output-file-id": "output-123",
                            "batch.job.aibrix.ai/error-file-id": "error-123",
                            "batch.job.aibrix.ai/temp-output-file-id": "temp-output-123",
                            "batch.job.aibrix.ai/temp-error-file-id": "temp-error-123",
                        },
                    },
                },
            },
            "status": {
                "conditions": None,
                "active": 1,
            },
        },
    )


def _get_job_succees_with_finalizing_obj():
    return merge_yaml_object(
        _get_job_in_progress_obj(),
        {
            "status": {
                "conditions": [
                    {
                        "type": "SuccessCriteriaMet",
                        "status": "True",
                        "lastProbeTime": update_time.isoformat(),
                        "lastTransitionTime": update_time.isoformat(),
                        "reason": "CompletionsReached",
                        "message": "Reached expected number of succeeded pods",
                    },
                    {
                        "type": "Complete",
                        "status": "True",
                        "lastProbeTime": update_time.isoformat(),
                        "lastTransitionTime": update_time.isoformat(),
                        "reason": "CompletionsReached",
                        "message": "Reached expected number of succeeded pods",
                    },
                ],
                "completionTime": update_time.isoformat(),
                "succeeded": 1,
            },
        },
    )


def _get_job_failed_with_finalizing_obj():
    return merge_yaml_object(
        _get_job_in_progress_obj(),
        {
            "status": {
                "conditions": [
                    {
                        "type": "Failed",
                        "status": "True",
                        "lastTransitionTime": update_time.isoformat(),
                        "reason": "BackoffLimitExceeded",
                        "message": "Job has reached the specified backoff limit",
                    },
                ],
                "completionTime": update_time.isoformat(),
                "failed": 1,
            },
        },
    )


def _get_job_exprired_with_finalizing_obj():
    return merge_yaml_object(
        _get_job_in_progress_obj(),
        {
            "status": {
                "conditions": [
                    {
                        "type": "Failed",
                        "status": "True",
                        "lastProbeTime": update_time.isoformat(),
                        "lastTransitionTime": update_time.isoformat(),
                        "reason": "DeadlineExceeded",
                        "message": "Job was active longer than specified deadline",
                    },
                ],
                "completionTime": update_time.isoformat(),
                "terminating": 1,
            },
        },
    )


def _get_job_cancelled_with_finalizing_obj():
    return merge_yaml_object(
        _get_job_in_progress_obj(),
        {
            "metadata": {
                "annotations": {
                    "batch.job.aibrix.ai/state": "finalizing",
                    "batch.job.aibrix.ai/condition": "cancelled",
                    "batch.job.aibrix.ai/cancelling-at": cancel_time.isoformat(),
                },
            },
            "spec": {
                "suspend": True,
            },
            "status": {
                "conditions": [
                    {
                        "type": "Suspended",
                        "status": "True",
                        "lastProbeTime": update_time.isoformat(),
                        "lastTransitionTime": update_time.isoformat(),
                        "reason": "JobSuspended",
                        "message": "Job suspended",
                    },
                ],
            },
        },
    )


def _get_job_validation_failed_obj():
    return merge_yaml_object(
        _get_job_created_obj(),
        {
            "metadata": {
                "annotations": {
                    "batch.job.aibrix.ai/state": "finalized",
                    "batch.job.aibrix.ai/condition": "failed",
                    "batch.job.aibrix.ai/errors": '[{"code": "authentication_error", "message": "Simulated authentication failure", "param": "authentication", "line": null}]',
                    "batch.job.aibrix.ai/finalized-at": end_time.isoformat(),
                },
            },
            "spec": {
                "suspend": True,
            },
            "status": {
                "conditions": [
                    {
                        "type": "Suspended",
                        "status": "True",
                        "lastProbeTime": start_time.isoformat(),
                        "lastTransitionTime": start_time.isoformat(),
                        "reason": "JobSuspended",
                        "message": "Job suspended",
                    },
                ],
            },
        },
    )


def _get_job_validation_cancelled_obj():
    return merge_yaml_object(
        _get_job_created_obj(),
        {
            "metadata": {
                "annotations": {
                    "batch.job.aibrix.ai/state": "finalized",
                    "batch.job.aibrix.ai/condition": "cancelled",
                    "batch.job.aibrix.ai/cancelling-at": cancel_time.isoformat(),
                    "batch.job.aibrix.ai/finalized-at": end_time.isoformat(),
                },
            },
            "spec": {
                "suspend": True,
            },
            "status": {
                "conditions": [
                    {
                        "type": "Suspended",
                        "status": "True",
                        "lastProbeTime": start_time.isoformat(),
                        "lastTransitionTime": start_time.isoformat(),
                        "reason": "JobSuspended",
                        "message": "Job suspended",
                    },
                ],
            },
        },
    )


def _get_job_completed_obj():
    return merge_yaml_object(
        _get_job_succees_with_finalizing_obj(),
        {
            "metadata": {
                "annotations": {
                    "batch.job.aibrix.ai/state": "finalized",
                    "batch.job.aibrix.ai/finalizing-at": update_time.isoformat(),
                    "batch.job.aibrix.ai/finalized-at": end_time.isoformat(),
                    "batch.job.aibrix.ai/request-counts": '{"total":10, "completed":10, "failed":0}',
                },
            },
        },
    )


def _get_job_failed_obj():
    return merge_yaml_object(
        _get_job_failed_with_finalizing_obj(),
        {
            "metadata": {
                "annotations": {
                    "batch.job.aibrix.ai/state": "finalized",
                    "batch.job.aibrix.ai/finalizing-at": update_time.isoformat(),
                    "batch.job.aibrix.ai/finalized-at": end_time.isoformat(),
                    "batch.job.aibrix.ai/request-counts": '{"total":10, "completed":0, "failed":10}',
                },
            },
        },
    )


def _get_job_failed_during_finalizing_obj():
    return merge_yaml_object(
        _get_job_succees_with_finalizing_obj(),
        {
            "metadata": {
                "annotations": {
                    "batch.job.aibrix.ai/state": "finalized",
                    "batch.job.aibrix.ai/condition": "failed",
                    "batch.job.aibrix.ai/finalizing-at": update_time.isoformat(),
                    "batch.job.aibrix.ai/finalized-at": end_time.isoformat(),
                    "batch.job.aibrix.ai/request-counts": '{"total":10, "completed":0, "failed":10}',
                },
            },
        },
    )


def _get_job_expired_obj():
    return merge_yaml_object(
        _get_job_exprired_with_finalizing_obj(),
        {
            "metadata": {
                "annotations": {
                    "batch.job.aibrix.ai/state": "finalized",
                    "batch.job.aibrix.ai/finalizing-at": update_time.isoformat(),
                    "batch.job.aibrix.ai/finalized-at": end_time.isoformat(),
                    "batch.job.aibrix.ai/request-counts": '{"total":10, "completed":9, "failed":1}',
                },
            },
        },
    )


def _get_job_cancelled_obj():
    return merge_yaml_object(
        _get_job_cancelled_with_finalizing_obj(),
        {
            "metadata": {
                "annotations": {
                    "batch.job.aibrix.ai/state": "finalized",
                    "batch.job.aibrix.ai/finalizing-at": update_time.isoformat(),
                    "batch.job.aibrix.ai/finalized-at": end_time.isoformat(),
                    "batch.job.aibrix.ai/request-counts": '{"total":10, "completed":5, "failed":0}',
                },
            },
        },
    )


def dict_to_obj(d: dict) -> Any:
    """Recursively converts a dictionary to a multi-level object."""
    # Convert nested dictionaries recursively
    for key, value in d.items():
        if isinstance(value, dict) and key != "annotations":
            d[key] = dict_to_obj(value)

    # Convert the top-level dictionary to a SimpleNamespace object
    return SimpleNamespace(**d)


def test_k8s_job_created():
    """Test successful transformation of Kubernetes job to BatchJob."""
    # Create mock Kubernetes job with required annotations
    k8s_job = _get_job_created_obj()
    batch_job = k8s_job_to_batch_job(k8s_job)

    # Verify transformation
    assert batch_job.type_meta.api_version == "batch/v1"
    assert batch_job.type_meta.kind == "Job"

    assert batch_job.metadata.name == "test-batch-job"
    assert batch_job.metadata.namespace == "default"
    assert batch_job.metadata.uid == "test-uid-123"

    assert batch_job.spec.input_file_id == "file-123"
    assert batch_job.spec.endpoint == BatchJobEndpoint.CHAT_COMPLETIONS
    assert (
        batch_job.spec.completion_window
        == CompletionWindow.TWENTY_FOUR_HOURS.expires_at()
    )
    assert batch_job.spec.metadata == {"priority": "high", "customer": "test-customer"}

    assert batch_job.status.job_id == "test-uid-123"
    assert batch_job.status.state == BatchJobState.CREATED

    assert batch_job.status.created_at == creation_time
    assert batch_job.status.in_progress_at is None
    assert batch_job.status.finalizing_at is None
    assert batch_job.status.completed_at is None
    assert batch_job.status.failed_at is None
    assert batch_job.status.expired_at is None
    assert batch_job.status.cancelling_at is None
    assert batch_job.status.cancelled_at is None

    assert not batch_job.status.finished
    assert batch_job.status.condition is None


def test_k8s_job_missing_required_annotation():
    """Test transformation fails when required annotation is missing."""
    k8s_job = MockK8sJob(
        metadata={
            "name": "test-batch-job",
        },
        annotations={
            # Missing required input-file-id annotation
            "batch.job.aibrix.ai/endpoint": "/v1/chat/completions"
        },
    )

    with pytest.raises(
        ValueError, match="Required annotation.*input-file-id.*not found"
    ):
        k8s_job_to_batch_job(k8s_job)


def test_k8s_job_invalid_endpoint():
    """Test transformation fails with invalid endpoint."""
    k8s_job = MockK8sJob(
        annotations={
            "batch.job.aibrix.ai/input-file-id": "file-123",
            "batch.job.aibrix.ai/endpoint": "/invalid/endpoint",
        }
    )

    # We don't check validity of k8s job obj
    batch_job = k8s_job_to_batch_job(k8s_job)
    assert batch_job.spec.endpoint == "/invalid/endpoint"


def test_k8s_job_in_progress():
    """Test successful transformation of Kubernetes job to BatchJob."""
    # Create mock Kubernetes job with required annotations
    k8s_job = _get_job_in_progress_obj()
    batch_job = k8s_job_to_batch_job(k8s_job)

    # Verify transformation
    assert batch_job.type_meta.api_version == "batch/v1"
    assert batch_job.type_meta.kind == "Job"

    assert batch_job.metadata.name == "test-batch-job"
    assert batch_job.metadata.namespace == "default"
    assert batch_job.metadata.uid == "test-uid-123"

    assert batch_job.spec.input_file_id == "file-123"
    assert batch_job.spec.endpoint == BatchJobEndpoint.CHAT_COMPLETIONS
    assert (
        batch_job.spec.completion_window
        == CompletionWindow.TWENTY_FOUR_HOURS.expires_at()
    )
    assert batch_job.spec.metadata == {"priority": "high", "customer": "test-customer"}

    assert batch_job.status.job_id == "test-uid-123"
    assert batch_job.status.state == BatchJobState.IN_PROGRESS

    assert batch_job.status.created_at == creation_time
    assert batch_job.status.in_progress_at == start_time
    assert batch_job.status.finalizing_at is None
    assert batch_job.status.completed_at is None
    assert batch_job.status.failed_at is None
    assert batch_job.status.expired_at is None
    assert batch_job.status.cancelling_at is None
    assert batch_job.status.cancelled_at is None

    assert not batch_job.status.finished
    assert batch_job.status.condition is None


def test_k8s_job_success_with_finalizing():
    """Test successful transformation of Kubernetes job to BatchJob."""
    # Create mock Kubernetes job with required annotations
    k8s_job = _get_job_succees_with_finalizing_obj()
    batch_job = k8s_job_to_batch_job(k8s_job)

    # Skip type_meta, metadata, and spec testing

    assert batch_job.status.job_id == "test-uid-123"
    assert batch_job.status.state == BatchJobState.FINALIZING

    assert batch_job.status.created_at == creation_time
    assert batch_job.status.in_progress_at == start_time
    assert batch_job.status.finalizing_at == update_time
    assert batch_job.status.completed_at is None
    assert batch_job.status.failed_at is None
    assert batch_job.status.expired_at is None
    assert batch_job.status.cancelling_at is None
    assert batch_job.status.cancelled_at is None

    assert not batch_job.status.finished
    assert not batch_job.status.completed
    assert batch_job.status.condition == ConditionType.COMPLETED


def test_k8s_job_completed():
    """Test transformation of successfully completed and finalized job."""
    batch_job = k8s_job_to_batch_job(_get_job_completed_obj())

    # Skip type_meta, metadata, and spec testing

    # Should be FINALIZED based on annotations
    assert batch_job.status.state == BatchJobState.FINALIZED

    # Should have completed condition
    assert batch_job.status.conditions is not None
    assert len(batch_job.status.conditions) > 0
    assert batch_job.status.finished
    assert batch_job.status.completed
    assert not batch_job.status.cancelled
    assert not batch_job.status.failed
    assert not batch_job.status.expired
    assert batch_job.status.condition == ConditionType.COMPLETED

    # Verify timestamp requirements
    assert batch_job.status.created_at == creation_time
    assert batch_job.status.in_progress_at == start_time
    assert batch_job.status.finalizing_at == update_time
    assert batch_job.status.completed_at == end_time
    assert batch_job.status.failed_at is None
    assert batch_job.status.expired_at is None
    assert batch_job.status.cancelling_at is None
    assert batch_job.status.cancelled_at is None


def test_k8s_job_validation_failed():
    """Test transformation of validation failed job."""
    batch_job = k8s_job_to_batch_job(_get_job_validation_failed_obj())
    import json

    print(json.dumps(_get_job_validation_failed_obj(), indent=2))

    # Skip type_meta, metadata, and spec testing

    # Should be FINALIZED based on annotations
    assert batch_job.status.state == BatchJobState.FINALIZED

    # Should have failed condition
    assert batch_job.status.conditions is not None
    assert len(batch_job.status.conditions) > 0
    assert batch_job.status.finished
    assert not batch_job.status.completed
    assert not batch_job.status.cancelled
    assert batch_job.status.failed
    assert not batch_job.status.expired
    assert batch_job.status.condition == ConditionType.FAILED
    assert batch_job.status.errors is not None
    assert len(batch_job.status.errors) > 0
    assert batch_job.status.errors[0].code == BatchJobErrorCode.AUTHENTICATION_ERROR

    # Verify timestamp requirements
    assert batch_job.status.created_at == creation_time
    assert batch_job.status.in_progress_at is None
    assert batch_job.status.finalizing_at is None
    assert batch_job.status.completed_at is None
    assert batch_job.status.failed_at == end_time
    assert batch_job.status.expired_at is None
    assert batch_job.status.cancelling_at is None
    assert batch_job.status.cancelled_at is None


def test_k8s_job_failed_with_finalizing():
    """Test successful transformation of Kubernetes job to BatchJob."""
    # Create mock Kubernetes job with required annotations
    k8s_job = _get_job_failed_with_finalizing_obj()
    batch_job = k8s_job_to_batch_job(k8s_job)

    # Skip type_meta, metadata, and spec testing

    assert batch_job.status.job_id == "test-uid-123"
    assert batch_job.status.state == BatchJobState.FINALIZING

    # Should have failed condition
    assert batch_job.status.conditions is not None
    assert len(batch_job.status.conditions) > 0
    assert not batch_job.status.finished
    assert not batch_job.status.completed
    assert not batch_job.status.cancelled
    assert not batch_job.status.failed
    assert not batch_job.status.expired
    assert batch_job.status.condition == ConditionType.FAILED

    # Verify timestamp requirements
    assert batch_job.status.created_at == creation_time
    assert batch_job.status.in_progress_at == start_time
    assert batch_job.status.finalizing_at == update_time
    assert batch_job.status.completed_at is None
    assert batch_job.status.failed_at is None
    assert batch_job.status.expired_at is None
    assert batch_job.status.cancelling_at is None
    assert batch_job.status.cancelled_at is None


def test_k8s_job_failed():
    """Test transformation of successfully completed and finalized job."""
    batch_job = k8s_job_to_batch_job(_get_job_failed_obj())

    # Skip type_meta, metadata, and spec testing

    # Should be FINALIZED based on annotations
    assert batch_job.status.state == BatchJobState.FINALIZED

    # Should have completed condition
    assert batch_job.status.conditions is not None
    assert len(batch_job.status.conditions) > 0
    assert batch_job.status.finished
    assert not batch_job.status.completed
    assert not batch_job.status.cancelled
    assert batch_job.status.failed
    assert not batch_job.status.expired
    assert batch_job.status.condition == ConditionType.FAILED

    # Verify timestamp requirements
    assert batch_job.status.created_at == creation_time
    assert batch_job.status.in_progress_at == start_time
    assert batch_job.status.finalizing_at == update_time
    assert batch_job.status.completed_at is None
    assert batch_job.status.failed_at == end_time
    assert batch_job.status.expired_at is None
    assert batch_job.status.cancelling_at is None
    assert batch_job.status.cancelled_at is None


def test_k8s_job_failed_during_finalizing():
    """Test transformation of successfully completed and finalized job."""
    batch_job = k8s_job_to_batch_job(_get_job_failed_during_finalizing_obj())

    # Skip type_meta, metadata, and spec testing
    print(str(batch_job.status.dict()))

    # Should be FINALIZED based on annotations
    assert batch_job.status.state == BatchJobState.FINALIZED

    # Should have completed condition
    assert batch_job.status.conditions is not None
    assert len(batch_job.status.conditions) > 0
    assert batch_job.status.finished
    assert batch_job.status.completed
    assert not batch_job.status.cancelled
    assert batch_job.status.failed
    assert not batch_job.status.expired
    assert batch_job.status.condition == ConditionType.FAILED

    # Verify timestamp requirements
    assert batch_job.status.created_at == creation_time
    assert batch_job.status.in_progress_at == start_time
    assert batch_job.status.finalizing_at == update_time
    assert batch_job.status.completed_at is None
    assert batch_job.status.failed_at == end_time
    assert batch_job.status.expired_at is None
    assert batch_job.status.cancelling_at is None
    assert batch_job.status.cancelled_at is None


def test_k8s_job_expired_with_finalizing():
    """Test successful transformation of Kubernetes job to BatchJob."""
    # Create mock Kubernetes job with required annotations
    k8s_job = _get_job_exprired_with_finalizing_obj()
    batch_job = k8s_job_to_batch_job(k8s_job)

    # Skip type_meta, metadata, and spec testing

    assert batch_job.status.job_id == "test-uid-123"
    assert batch_job.status.state == BatchJobState.FINALIZING

    # Should have expired condition
    assert batch_job.status.conditions is not None
    assert len(batch_job.status.conditions) > 0
    assert not batch_job.status.finished
    assert not batch_job.status.completed
    assert not batch_job.status.cancelled
    assert not batch_job.status.failed
    assert not batch_job.status.expired
    assert batch_job.status.condition == ConditionType.EXPIRED

    # Verify timestamp requirements
    assert batch_job.status.created_at == creation_time
    assert batch_job.status.in_progress_at == start_time
    assert batch_job.status.finalizing_at == update_time
    assert batch_job.status.completed_at is None
    assert batch_job.status.failed_at is None
    assert batch_job.status.expired_at is None
    assert batch_job.status.cancelling_at is None
    assert batch_job.status.cancelled_at is None


def test_k8s_job_expired():
    """Test transformation of successfully completed and finalized job."""
    batch_job = k8s_job_to_batch_job(_get_job_expired_obj())

    # Skip type_meta, metadata, and spec testing

    # Should be FINALIZED based on annotations
    assert batch_job.status.state == BatchJobState.FINALIZED

    # Should have completed condition
    assert batch_job.status.conditions is not None
    assert len(batch_job.status.conditions) > 0
    assert batch_job.status.finished
    assert not batch_job.status.completed
    assert not batch_job.status.cancelled
    assert not batch_job.status.failed
    assert batch_job.status.expired
    assert batch_job.status.condition == ConditionType.EXPIRED

    # Verify timestamp requirements
    assert batch_job.status.created_at == creation_time
    assert batch_job.status.in_progress_at == start_time
    assert batch_job.status.finalizing_at == update_time
    assert batch_job.status.completed_at is None
    assert batch_job.status.failed_at is None
    assert batch_job.status.expired_at == end_time
    assert batch_job.status.cancelling_at is None
    assert batch_job.status.cancelled_at is None


def test_k8s_job_validation_cancelled():
    """Test transformation of cancelled job in finalized state."""
    batch_job = k8s_job_to_batch_job(_get_job_validation_cancelled_obj())

    # Skip type_meta, metadata, and spec testing

    # Should be FINALIZED based on annotations
    assert batch_job.status.state == BatchJobState.FINALIZED

    # Should have cancelled condition
    assert batch_job.status.conditions is not None
    assert len(batch_job.status.conditions) > 0
    assert batch_job.status.finished
    assert not batch_job.status.completed
    assert batch_job.status.cancelled
    assert not batch_job.status.failed
    assert not batch_job.status.expired
    assert batch_job.status.condition == ConditionType.CANCELLED

    # Verify timestamp requirements
    assert batch_job.status.created_at == creation_time
    assert batch_job.status.in_progress_at is None
    assert batch_job.status.finalizing_at is None
    assert batch_job.status.completed_at is None
    assert batch_job.status.failed_at is None
    assert batch_job.status.expired_at is None
    assert batch_job.status.cancelling_at == cancel_time
    assert batch_job.status.cancelled_at == end_time


def test_k8s_job_cancelled_with_finalizing():
    """Test transformation of cancelled job in finalizing state."""
    batch_job = k8s_job_to_batch_job(_get_job_cancelled_with_finalizing_obj())

    # Skip type_meta, metadata, and spec testing

    # Should be FINALIZING since it has conditions
    assert batch_job.status.state == BatchJobState.FINALIZING

    # Should have cancelled condition
    assert batch_job.status.conditions is not None
    assert len(batch_job.status.conditions) > 0
    assert not batch_job.status.finished
    assert not batch_job.status.completed
    assert not batch_job.status.cancelled
    assert not batch_job.status.failed
    assert not batch_job.status.expired
    assert batch_job.status.condition == ConditionType.CANCELLED

    # Verify timestamp requirements
    assert batch_job.status.created_at == creation_time
    assert batch_job.status.in_progress_at == start_time
    assert batch_job.status.finalizing_at == update_time
    assert batch_job.status.completed_at is None
    assert batch_job.status.failed_at is None
    assert batch_job.status.expired_at is None
    assert batch_job.status.cancelling_at == cancel_time
    assert batch_job.status.cancelled_at is None


def test_k8s_job_cancelled():
    """Test transformation of cancelled job in finalizing state."""
    batch_job = k8s_job_to_batch_job(_get_job_cancelled_obj())

    # Skip type_meta, metadata, and spec testing

    # Should be FINALIZING since it has conditions
    assert batch_job.status.state == BatchJobState.FINALIZED

    # Should have cancelled condition
    assert batch_job.status.conditions is not None
    assert len(batch_job.status.conditions) > 0
    assert batch_job.status.finished
    assert not batch_job.status.completed
    assert batch_job.status.cancelled
    assert not batch_job.status.failed
    assert not batch_job.status.expired
    assert batch_job.status.condition == ConditionType.CANCELLED

    # Verify timestamp requirements
    assert batch_job.status.created_at == creation_time
    assert batch_job.status.in_progress_at == start_time
    assert batch_job.status.finalizing_at == update_time
    assert batch_job.status.completed_at is None
    assert batch_job.status.failed_at is None
    assert batch_job.status.expired_at is None
    assert batch_job.status.cancelling_at == cancel_time
    assert batch_job.status.cancelled_at == end_time


def test_k8s_job_obj_access():
    """Test transformer works with object-style access."""
    obj = dict_to_obj(_get_job_created_obj())
    batch_job = k8s_job_to_batch_job(obj)

    assert batch_job.type_meta.api_version == "batch/v1"
    assert batch_job.type_meta.kind == "Job"
    assert batch_job.metadata.name == "test-batch-job"
    assert batch_job.spec.endpoint == BatchJobEndpoint.CHAT_COMPLETIONS
    assert batch_job.status.state == BatchJobState.CREATED
    assert not batch_job.status.finished
    assert batch_job.status.condition is None

    obj = dict_to_obj(_get_job_in_progress_obj())
    batch_job = k8s_job_to_batch_job(obj)

    assert batch_job.type_meta.api_version == "batch/v1"
    assert batch_job.type_meta.kind == "Job"
    assert batch_job.metadata.name == "test-batch-job"
    assert batch_job.spec.endpoint == BatchJobEndpoint.CHAT_COMPLETIONS
    assert batch_job.status.state == BatchJobState.IN_PROGRESS
    assert not batch_job.status.finished
    assert batch_job.status.condition is None

    obj = dict_to_obj(_get_job_succees_with_finalizing_obj())
    batch_job = k8s_job_to_batch_job(obj)

    assert batch_job.type_meta.api_version == "batch/v1"
    assert batch_job.type_meta.kind == "Job"
    assert batch_job.metadata.name == "test-batch-job"
    assert batch_job.spec.endpoint == BatchJobEndpoint.CHAT_COMPLETIONS
    assert batch_job.status.state == BatchJobState.FINALIZING
    assert not batch_job.status.finished
    assert not batch_job.status.completed
    assert batch_job.status.condition == ConditionType.COMPLETED

    obj = dict_to_obj(_get_job_exprired_with_finalizing_obj())
    batch_job = k8s_job_to_batch_job(obj)
    batch_job.status.state == BatchJobState.FINALIZING
    assert not batch_job.status.finished
    assert not batch_job.status.expired
    assert batch_job.status.condition == ConditionType.EXPIRED


def test_k8s_job_kopf_access():
    """Test transformer works with dict-style access (e.g., from kopf)."""

    kopf_body = kopf.Body(_get_job_created_obj())
    batch_job = k8s_job_to_batch_job(kopf_body)

    assert batch_job.type_meta.api_version == "batch/v1"
    assert batch_job.type_meta.kind == "Job"
    assert batch_job.metadata.name == "test-batch-job"
    assert batch_job.spec.endpoint == BatchJobEndpoint.CHAT_COMPLETIONS
    assert batch_job.status.state == BatchJobState.CREATED
    assert not batch_job.status.finished
    assert batch_job.status.condition is None

    kopf_body = kopf.Body(_get_job_in_progress_obj())
    batch_job = k8s_job_to_batch_job(kopf_body)

    assert batch_job.type_meta.api_version == "batch/v1"
    assert batch_job.type_meta.kind == "Job"
    assert batch_job.metadata.name == "test-batch-job"
    assert batch_job.spec.endpoint == BatchJobEndpoint.CHAT_COMPLETIONS
    assert batch_job.status.state == BatchJobState.IN_PROGRESS
    assert not batch_job.status.finished
    assert batch_job.status.condition is None

    kopf_body = kopf.Body(_get_job_succees_with_finalizing_obj())
    batch_job = k8s_job_to_batch_job(kopf_body)

    assert batch_job.type_meta.api_version == "batch/v1"
    assert batch_job.type_meta.kind == "Job"
    assert batch_job.metadata.name == "test-batch-job"
    assert batch_job.spec.endpoint == BatchJobEndpoint.CHAT_COMPLETIONS
    assert batch_job.status.state == BatchJobState.FINALIZING
    assert not batch_job.status.finished
    assert not batch_job.status.completed
    assert batch_job.status.condition == ConditionType.COMPLETED

    kopf_body = kopf.Body(_get_job_exprired_with_finalizing_obj())
    batch_job = k8s_job_to_batch_job(kopf_body)
    batch_job.status.state == BatchJobState.FINALIZING
    assert not batch_job.status.finished
    assert not batch_job.status.expired
    assert batch_job.status.condition == ConditionType.EXPIRED


def test_k8s_job_s3_integration_case():
    """Test transformer with real S3 integration job object structure."""
    k8s_job = {
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": {
            "name": "s3-batch-job",
            "namespace": "default",
            "uid": "b08af167-8d56-41e9-92b6-efebe4a859ab",
            "creationTimestamp": "2025-07-28T21:13:41Z",
            "resourceVersion": "767483",
            "generation": 1,
            "labels": {
                "app": "aibrix-batch",
                "component": "batch-processor",
            },
        },
        "spec": {
            "activeDeadlineSeconds": 3600,
            "backoffLimit": 3,
            "completions": 1,
            "parallelism": 1,
            "selector": {
                "matchLabels": {
                    "batch.kubernetes.io/controller-uid": "b08af167-8d56-41e9-92b6-efebe4a859ab"
                }
            },
            "template": {
                "metadata": {
                    "labels": {
                        "app": "aibrix-batch",
                        "component": "batch-processor",
                        "batch.kubernetes.io/controller-uid": "b08af167-8d56-41e9-92b6-efebe4a859ab",
                        "batch.kubernetes.io/job-name": "s3-batch-job",
                        "controller-uid": "b08af167-8d56-41e9-92b6-efebe4a859ab",
                        "job-name": "s3-batch-job",
                    },
                    "annotations": {
                        "batch.job.aibrix.ai/endpoint": "/v1/chat/completions",
                        "batch.job.aibrix.ai/input-file-id": "s3-test-input-db5ada19.jsonl",
                    },
                },
                "spec": {
                    "restartPolicy": "OnFailure",
                    "serviceAccountName": "job-reader-sa",
                    "automountServiceAccountToken": True,
                    "containers": [
                        {
                            "name": "batch-worker",
                            "image": "aibrix/runtime:nightly",
                            "command": ["aibrix_batch_worker"],
                            "env": [
                                {
                                    "name": "JOB_NAME",
                                    "valueFrom": {
                                        "fieldRef": {
                                            "apiVersion": "v1",
                                            "fieldPath": "metadata.labels['job-name']",
                                        }
                                    },
                                },
                                {
                                    "name": "JOB_NAMESPACE",
                                    "valueFrom": {
                                        "fieldRef": {
                                            "apiVersion": "v1",
                                            "fieldPath": "metadata.namespace",
                                        }
                                    },
                                },
                                {
                                    "name": "STORAGE_AWS_REGION",
                                    "value": "us-west-1",
                                },
                                {
                                    "name": "STORAGE_AWS_BUCKET",
                                    "value": "tianium.aibrix",
                                },
                                {
                                    "name": "REDIS_HOST",
                                    "value": "aibrix-redis-master.aibrix-system.svc.cluster.local",
                                },
                            ],
                        },
                        {
                            "name": "llm-engine",
                            "image": "aibrix/vllm-mock:nightly",
                            "ports": [{"containerPort": 8000, "protocol": "TCP"}],
                            "readinessProbe": {
                                "httpGet": {
                                    "path": "/ready",
                                    "port": 8000,
                                    "scheme": "HTTP",
                                },
                                "periodSeconds": 5,
                                "timeoutSeconds": 1,
                                "successThreshold": 1,
                                "failureThreshold": 3,
                            },
                        },
                    ],
                },
            },
        },
        "status": {
            "active": 1,
            "ready": 0,
            "startTime": "2025-07-28T21:13:41Z",
            "terminating": 0,
        },
    }

    batch_job = k8s_job_to_batch_job(k8s_job)

    # Verify transformation results
    assert batch_job.type_meta.api_version == "batch/v1"
    assert batch_job.type_meta.kind == "Job"

    assert batch_job.metadata.name == "s3-batch-job"
    assert batch_job.metadata.namespace == "default"
    assert batch_job.metadata.uid == "b08af167-8d56-41e9-92b6-efebe4a859ab"
    assert batch_job.metadata.resource_version == "767483"
    assert batch_job.metadata.generation == 1

    assert batch_job.spec.input_file_id == "s3-test-input-db5ada19.jsonl"
    assert batch_job.spec.endpoint == BatchJobEndpoint.CHAT_COMPLETIONS
    assert batch_job.spec.completion_window == 3600

    assert batch_job.status.job_id == "b08af167-8d56-41e9-92b6-efebe4a859ab"
    assert batch_job.status.state == BatchJobState.CREATED


def test_condition_mapping_completed():
    """Test that K8s Complete condition maps to ConditionType.COMPLETED."""
    batch_job = k8s_job_to_batch_job(_get_job_succees_with_finalizing_obj())

    # Should be FINALIZING since conditions exist
    assert batch_job.status.state == BatchJobState.FINALIZING

    # Should have conditions mapped
    assert batch_job.status.conditions is not None
    assert (
        len(batch_job.status.conditions) == 1
    )  # Only Complete maps, SuccessCriteriaMet doesn't

    condition = batch_job.status.conditions[0]
    assert condition.type == ConditionType.COMPLETED


def test_condition_mapping_expired():
    """Test that K8s Failed condition with DeadlineExceeded maps to ConditionType.EXPIRED."""
    batch_job = k8s_job_to_batch_job(_get_job_exprired_with_finalizing_obj())

    # Should be FINALIZING since conditions exist
    assert batch_job.status.state == BatchJobState.FINALIZING

    # Should have conditions mapped
    assert batch_job.status.conditions is not None
    assert len(batch_job.status.conditions) == 1

    condition = batch_job.status.conditions[0]
    assert condition.type == ConditionType.EXPIRED
    assert condition.status == ConditionStatus.TRUE
    assert condition.reason == "DeadlineExceeded"
    assert condition.last_transition_time is not None


def test_condition_mapping_failed():
    """Test that K8s Failed condition (non-deadline) maps to ConditionType.FAILED."""
    batch_job = k8s_job_to_batch_job(_get_job_failed_with_finalizing_obj())

    # Should be FINALIZING since conditions exist
    assert batch_job.status.state == BatchJobState.FINALIZING

    # Should have conditions mapped
    assert batch_job.status.conditions is not None
    assert len(batch_job.status.conditions) == 1

    condition = batch_job.status.conditions[0]
    assert condition.type == ConditionType.FAILED
    assert condition.status == ConditionStatus.TRUE
    assert condition.reason == "BackoffLimitExceeded"
    assert condition.last_transition_time is not None


def test_no_conditions_legacy_behavior():
    """Test that jobs without conditions use legacy state mapping."""
    batch_job = k8s_job_to_batch_job(_get_job_in_progress_obj())

    # Should be IN_PROGRESS (legacy mapping) since no conditions exist
    assert batch_job.status.state == BatchJobState.IN_PROGRESS

    # Should have no conditions
    assert batch_job.status.conditions is None


def test_unknown_conditions_ignored():
    """Test that unknown K8s condition types are ignored."""
    k8s_job = {
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": {
            "name": "test-batch-job",
            "namespace": "default",
            "uid": "test-uid-123",
            "creationTimestamp": "2024-01-01T12:00:00Z",
            "annotations": {
                "batch.job.aibrix.ai/state": "in_progress",
                "batch.job.aibrix.ai/in-progress-at": start_time.isoformat(),
            },
        },
        "spec": {
            "template": {
                "metadata": {
                    "annotations": {
                        "batch.job.aibrix.ai/input-file-id": "file-123",
                        "batch.job.aibrix.ai/endpoint": "/v1/embeddings",
                        "batch.job.aibrix.ai/output-file-id": "output-123",
                        "batch.job.aibrix.ai/error-file-id": "error-123",
                        "batch.job.aibrix.ai/temp-output-file-id": "temp-output-123",
                        "batch.job.aibrix.ai/temp-error-file-id": "temp-error-123",
                    },
                },
            },
        },
        "status": {
            "conditions": [
                {
                    "type": "ProgressDeadlineExceeded",  # Unknown condition type
                    "status": "True",
                    "lastTransitionTime": "2025-08-05T05:26:25Z",
                    "reason": "UnknownReason",
                    "message": "Unknown condition message",
                },
                {
                    "type": "Complete",  # Known condition type
                    "status": "True",
                    "lastTransitionTime": "2025-08-05T05:26:25Z",
                    "reason": "CompletionsReached",
                    "message": "Job completed successfully",
                },
            ],
        },
    }

    batch_job = k8s_job_to_batch_job(k8s_job)

    # Should be FINALIZING since valid conditions exist
    assert batch_job.status.state == BatchJobState.FINALIZING

    # Should have only the known condition mapped
    assert batch_job.status.conditions is not None
    assert len(batch_job.status.conditions) == 1

    condition = batch_job.status.conditions[0]
    assert condition.type == ConditionType.COMPLETED

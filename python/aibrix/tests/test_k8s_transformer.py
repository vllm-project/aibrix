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


import pytest

from aibrix.batch.job_entity import (
    BatchJobEndpoint,
    BatchJobState,
    CompletionWindow,
    k8s_job_to_batch_job,
)


class MockK8sJob:
    """Mock Kubernetes Job object for testing."""

    def __init__(self, metadata=None, status=None, api_version="batch/v1", kind="Job"):
        self.metadata = metadata or {}
        self.status = status or {}
        self.api_version = api_version
        self.kind = kind


def test_k8s_job_to_batch_job_success():
    """Test successful transformation of Kubernetes job to BatchJob."""
    # Create mock Kubernetes job with required annotations
    k8s_job = MockK8sJob(
        metadata={
            "name": "test-batch-job",
            "namespace": "default",
            "uid": "test-uid-123",
            "creation_timestamp": "2024-01-01T12:00:00Z",
            "annotations": {
                "batch.job.aibrix.ai/input-file-id": "file-123",
                "batch.job.aibrix.ai/endpoint": "/v1/chat/completions",
                "batch.job.aibrix.ai/completion-window": "24h",
                "batch.job.aibrix.ai/metadata.priority": "high",
                "batch.job.aibrix.ai/metadata.customer": "test-customer",
            },
        },
        status={"active": 1, "succeeded": 0, "failed": 0},
    )

    # Transform to BatchJob
    batch_job = k8s_job_to_batch_job(k8s_job)

    # Verify transformation
    assert batch_job.type_meta.api_version == "batch/v1"
    assert batch_job.type_meta.kind == "Job"

    assert batch_job.metadata.name == "test-batch-job"
    assert batch_job.metadata.namespace == "default"
    assert batch_job.metadata.uid == "test-uid-123"

    assert batch_job.spec.input_file_id == "file-123"
    assert batch_job.spec.endpoint == BatchJobEndpoint.CHAT_COMPLETIONS
    assert batch_job.spec.completion_window == CompletionWindow.TWENTY_FOUR_HOURS
    assert batch_job.spec.metadata == {"priority": "high", "customer": "test-customer"}

    assert batch_job.status.job_id == "test-uid-123"
    assert batch_job.status.state == BatchJobState.IN_PROGRESS


def test_k8s_job_to_batch_job_missing_required_annotation():
    """Test transformation fails when required annotation is missing."""
    k8s_job = MockK8sJob(
        metadata={
            "name": "test-batch-job",
            "annotations": {
                # Missing required input-file-id annotation
                "batch.job.aibrix.ai/endpoint": "/v1/chat/completions"
            },
        }
    )

    with pytest.raises(
        ValueError, match="Required annotation.*input-file-id.*not found"
    ):
        k8s_job_to_batch_job(k8s_job)


def test_k8s_job_to_batch_job_invalid_endpoint():
    """Test transformation fails with invalid endpoint."""
    k8s_job = MockK8sJob(
        metadata={
            "annotations": {
                "batch.job.aibrix.ai/input-file-id": "file-123",
                "batch.job.aibrix.ai/endpoint": "/invalid/endpoint",
            }
        }
    )

    with pytest.raises(ValueError, match="Invalid endpoint"):
        k8s_job_to_batch_job(k8s_job)


def test_k8s_job_dict_access():
    """Test transformer works with dict-style access (e.g., from kopf)."""
    k8s_job = {
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": {
            "name": "test-batch-job",
            "namespace": "default",
            "uid": "test-uid-123",
            "creationTimestamp": "2024-01-01T12:00:00Z",
            "annotations": {
                "batch.job.aibrix.ai/input-file-id": "file-123",
                "batch.job.aibrix.ai/endpoint": "/v1/embeddings",
            },
        },
        "status": {"succeeded": 1, "failed": 0},
    }

    batch_job = k8s_job_to_batch_job(k8s_job)

    assert batch_job.type_meta.api_version == "batch/v1"
    assert batch_job.type_meta.kind == "Job"
    assert batch_job.metadata.name == "test-batch-job"
    assert batch_job.spec.endpoint == BatchJobEndpoint.EMBEDDINGS
    assert batch_job.status.state == BatchJobState.FINALIZING

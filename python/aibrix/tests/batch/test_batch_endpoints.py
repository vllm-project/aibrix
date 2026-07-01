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

"""Unit tests for batch API endpoint support and body validation."""

from datetime import datetime, timezone

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from pydantic import ValidationError

from aibrix.batch.job_entity import (
    AibrixMetadata,
    BatchJob,
    BatchJobEndpoint,
    BatchJobError,
    BatchJobErrorCode,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    BatchProfileRef,
    Condition,
    ConditionStatus,
    ConditionType,
    ModelTemplateRef,
    ObjectMeta,
    RequestCountStats,
    ResourceAllocation,
    ResourceDetail,
    RuntimeSpec,
    TypeMeta,
)
from aibrix.batch.storage.input_validation import validate_request_body_for_endpoint
from aibrix.metadata.api.v1.batch import (
    BatchSpec,
    _batch_job_to_openai_response,
)
from aibrix.metadata.api.v1.batch import (
    router as batch_router,
)


def test_chat_completions_endpoint_supported():
    """Test that /v1/chat/completions endpoint is supported."""
    endpoint = "/v1/chat/completions"
    assert BatchJobEndpoint(endpoint) == BatchJobEndpoint.CHAT_COMPLETIONS


def test_completions_endpoint_supported():
    """Test that /v1/completions endpoint is supported."""
    endpoint = "/v1/completions"
    assert BatchJobEndpoint(endpoint) == BatchJobEndpoint.COMPLETIONS


def test_embeddings_endpoint_supported():
    """Test that /v1/embeddings endpoint is supported."""
    endpoint = "/v1/embeddings"
    assert BatchJobEndpoint(endpoint) == BatchJobEndpoint.EMBEDDINGS


def test_rerank_endpoint_supported():
    """Test that /v1/rerank endpoint is supported."""
    endpoint = "/v1/rerank"
    assert BatchJobEndpoint(endpoint) == BatchJobEndpoint.RERANK


def test_all_supported_endpoints():
    """Test that all four OpenAI-compatible endpoints are supported."""
    supported_endpoints = [
        "/v1/chat/completions",
        "/v1/completions",
        "/v1/embeddings",
        "/v1/rerank",
    ]

    for endpoint in supported_endpoints:
        # Should not raise ValueError
        result = BatchJobEndpoint(endpoint)
        assert result.value == endpoint


def test_unsupported_endpoint_raises_error():
    """Test that unsupported endpoints raise ValueError."""
    unsupported_endpoints = [
        "/v1/models",
        "/v1/moderations",  # Not supported (vLLM doesn't support it)
        "/v1/images/generations",
        "/v1/audio/transcriptions",
        "/invalid/endpoint",
    ]

    for endpoint in unsupported_endpoints:
        with pytest.raises(ValueError):
            BatchJobEndpoint(endpoint)


def test_endpoint_enum_values():
    """Test that endpoint enum values match expected strings."""
    assert BatchJobEndpoint.CHAT_COMPLETIONS.value == "/v1/chat/completions"
    assert BatchJobEndpoint.COMPLETIONS.value == "/v1/completions"
    assert BatchJobEndpoint.EMBEDDINGS.value == "/v1/embeddings"


def test_endpoint_count():
    """Test that we have exactly 4 supported endpoints."""
    endpoints = list(BatchJobEndpoint)
    assert len(endpoints) == 4


# ---- Endpoint-specific body validation tests ----


class TestEndpointBodyValidation:
    """Tests for endpoint-specific request body validation."""

    def test_chat_completions_valid_body(self):
        """Test valid chat completions body passes validation."""
        body = {
            "model": "gpt-3.5-turbo",
            "messages": [{"role": "user", "content": "Hello"}],
        }
        result = validate_request_body_for_endpoint(body, "/v1/chat/completions", 1)
        assert result is None

    def test_chat_completions_missing_messages(self):
        """Test chat completions body without messages fails."""
        body = {"model": "gpt-3.5-turbo"}
        result = validate_request_body_for_endpoint(body, "/v1/chat/completions", 1)
        assert result is not None
        assert "messages" in result

    def test_chat_completions_messages_not_array(self):
        """Test chat completions body with non-array messages fails."""
        body = {"model": "gpt-3.5-turbo", "messages": "not an array"}
        result = validate_request_body_for_endpoint(body, "/v1/chat/completions", 1)
        assert result is not None
        assert "messages" in result
        assert "list" in result

    def test_completions_valid_body_string_prompt(self):
        """Test valid completions body with string prompt passes."""
        body = {"model": "gpt-3.5-turbo", "prompt": "Hello world"}
        result = validate_request_body_for_endpoint(body, "/v1/completions", 1)
        assert result is None

    def test_completions_valid_body_array_prompt(self):
        """Test valid completions body with array prompt passes."""
        body = {"model": "gpt-3.5-turbo", "prompt": ["Hello", "World"]}
        result = validate_request_body_for_endpoint(body, "/v1/completions", 1)
        assert result is None

    def test_completions_missing_prompt(self):
        """Test completions body without prompt fails."""
        body = {"model": "gpt-3.5-turbo"}
        result = validate_request_body_for_endpoint(body, "/v1/completions", 1)
        assert result is not None
        assert "prompt" in result

    def test_completions_invalid_prompt_type(self):
        """Test completions body with invalid prompt type fails."""
        body = {"model": "gpt-3.5-turbo", "prompt": 123}
        result = validate_request_body_for_endpoint(body, "/v1/completions", 1)
        assert result is not None
        assert "prompt" in result

    def test_embeddings_valid_body_string_input(self):
        """Test valid embeddings body with string input passes."""
        body = {"model": "text-embedding-ada-002", "input": "Hello world"}
        result = validate_request_body_for_endpoint(body, "/v1/embeddings", 1)
        assert result is None

    def test_embeddings_valid_body_array_input(self):
        """Test valid embeddings body with array input passes."""
        body = {"model": "text-embedding-ada-002", "input": ["Hello", "World"]}
        result = validate_request_body_for_endpoint(body, "/v1/embeddings", 1)
        assert result is None

    def test_embeddings_missing_input(self):
        """Test embeddings body without input fails."""
        body = {"model": "text-embedding-ada-002"}
        result = validate_request_body_for_endpoint(body, "/v1/embeddings", 1)
        assert result is not None
        assert "input" in result

    def test_embeddings_missing_model(self):
        """Test embeddings body without model fails."""
        body = {"input": "Hello world"}
        result = validate_request_body_for_endpoint(body, "/v1/embeddings", 1)
        assert result is not None
        assert "model" in result

    def test_rerank_valid_body(self):
        """Test valid rerank body passes validation."""
        body = {
            "model": "reranker-v1",
            "query": "What is AI?",
            "documents": ["AI is...", "Machine learning is..."],
        }
        result = validate_request_body_for_endpoint(body, "/v1/rerank", 1)
        assert result is None

    def test_rerank_missing_query(self):
        """Test rerank body without query fails."""
        body = {
            "model": "reranker-v1",
            "documents": ["doc1", "doc2"],
        }
        result = validate_request_body_for_endpoint(body, "/v1/rerank", 1)
        assert result is not None
        assert "query" in result

    def test_rerank_missing_documents(self):
        """Test rerank body without documents fails."""
        body = {
            "model": "reranker-v1",
            "query": "What is AI?",
        }
        result = validate_request_body_for_endpoint(body, "/v1/rerank", 1)
        assert result is not None
        assert "documents" in result

    def test_rerank_invalid_documents_type(self):
        """Test rerank body with non-array documents fails."""
        body = {
            "model": "reranker-v1",
            "query": "What is AI?",
            "documents": "not an array",
        }
        result = validate_request_body_for_endpoint(body, "/v1/rerank", 1)
        assert result is not None
        assert "documents" in result
        assert "list" in result

    def test_unknown_endpoint_passes(self):
        """Test that unknown endpoints skip body validation."""
        body = {"anything": "goes"}
        result = validate_request_body_for_endpoint(body, "/v1/unknown", 1)
        assert result is None


def test_batch_spec_accepts_aibrix_metadata():
    spec = BatchSpec.model_validate(
        {
            "input_file_id": "file-123",
            "endpoint": "/v1/chat/completions",
            "completion_window": "24h",
            "aibrix": {
                "job_id": "planner-job-1",
                "runtime": {"target": "Kubernetes"},
                "resource_allocation": {
                    "provision_id": "reservation-1",
                    "provision_resource_deadline": 123,
                    "resource_details": [
                        {
                            "endpoint_cluster": "cluster-a",
                            "gpu_type": "H100",
                            "replica": 4,
                        }
                    ],
                },
                "model_template": {
                    "name": "echo-template",
                    "version": "v1.0.0",
                    "overrides": {
                        "engine_args": {
                            "max_num_seqs": "128",
                        }
                    },
                },
                "profile": {
                    "name": "default-profile",
                    "overrides": {
                        "scheduling": {
                            "max_concurrency": 4,
                        }
                    },
                },
            },
        }
    )

    batch_job_spec = BatchSpec.newBatchJobSpec(spec)

    assert batch_job_spec.aibrix is not None
    assert batch_job_spec.aibrix.job_id == "planner-job-1"
    assert batch_job_spec.aibrix.resource_allocation is not None
    assert batch_job_spec.aibrix.resource_allocation.provision_id == "reservation-1"
    assert batch_job_spec.aibrix.resource_allocation.provision_resource_deadline == 123
    assert len(batch_job_spec.aibrix.resource_allocation.resource_details or []) == 1
    resource = batch_job_spec.aibrix.resource_allocation.resource_details[0]
    assert resource is not None
    assert resource.endpoint_cluster == "cluster-a"
    assert resource.gpu_type == "H100"
    assert resource.replica == 4
    assert batch_job_spec.aibrix.runtime is not None
    assert batch_job_spec.aibrix.runtime.target == "Kubernetes"
    assert batch_job_spec.aibrix.runtime_target == "Kubernetes"
    assert batch_job_spec.aibrix.model_template is not None
    assert batch_job_spec.aibrix.model_template.name == "echo-template"
    assert batch_job_spec.aibrix.model_template.overrides == {
        "engine_args": {"max_num_seqs": "128"}
    }
    assert batch_job_spec.aibrix.profile is not None
    assert batch_job_spec.aibrix.profile.name == "default-profile"
    assert batch_job_spec.aibrix.profile.overrides == {
        "scheduling": {"max_concurrency": 4}
    }


def test_batch_spec_accepts_client_config():
    spec = BatchSpec.model_validate(
        {
            "input_file_id": "file-123",
            "endpoint": "/v1/chat/completions",
            "completion_window": "24h",
            "aibrix": {
                "client": {
                    "max_concurrency": 256,
                    "adaptive_concurrency": True,
                    "adaptive_max_factor": 16,
                    "retry_policy": {
                        "max_retries": 5,
                        "base_delay_seconds": 2,
                        "max_delay_seconds": 10,
                        "no_endpoint_max_retries": 7,
                    },
                }
            },
        }
    )

    batch_job_spec = BatchSpec.newBatchJobSpec(spec)

    assert batch_job_spec.aibrix is not None
    assert batch_job_spec.aibrix.client is not None
    assert batch_job_spec.aibrix.client.max_concurrency == 256
    assert batch_job_spec.aibrix.client.adaptive_concurrency is True
    assert batch_job_spec.aibrix.client.adaptive_max_factor == 16
    retry = batch_job_spec.aibrix.client.retry_policy
    assert retry is not None
    assert retry.max_retries == 5
    assert retry.base_delay_seconds == 2
    assert retry.max_delay_seconds == 10
    assert retry.no_endpoint_max_retries == 7


@pytest.mark.parametrize(
    "client",
    [
        {"max_concurrency": 0},
        {"max_concurrency": 257},
        {"adaptive_max_factor": 0.5},
        {"retry_policy": {"max_retries": -1}},
        {"retry_policy": {"base_delay_seconds": -0.1}},
    ],
)
def test_batch_spec_rejects_invalid_client_config(client):
    with pytest.raises(ValidationError):
        BatchSpec.model_validate(
            {
                "input_file_id": "file-123",
                "endpoint": "/v1/chat/completions",
                "completion_window": "24h",
                "aibrix": {"client": client},
            }
        )


def test_batch_spec_accepts_full_template_and_profile_objects():
    spec = BatchSpec.model_validate(
        {
            "input_file_id": "file-123",
            "endpoint": "/v1/chat/completions",
            "completion_window": "24h",
            "aibrix": {
                "model_template": {
                    "name": "echo-template",
                    "version": "v1.0.0",
                    "status": "active",
                    "spec": {
                        "engine": {
                            "type": "mock",
                            "version": "0.1.0",
                            "image": "aibrix/mock:latest",
                        },
                        "model_source": {
                            "type": "local",
                            "uri": "/models/echo",
                        },
                        "accelerator": {"type": "cpu", "count": 1},
                        "parallelism": {"tp": 1},
                        "supported_endpoints": ["/v1/chat/completions"],
                    },
                },
                "profile": {
                    "name": "inline-profile",
                    "spec": {
                        "scheduling": {
                            "completion_window": "24h",
                        }
                    },
                },
            },
        }
    )

    batch_job_spec = BatchSpec.newBatchJobSpec(spec)

    assert batch_job_spec.aibrix is not None
    assert batch_job_spec.aibrix.model_template is not None
    assert batch_job_spec.aibrix.model_template.name == "echo-template"
    assert batch_job_spec.aibrix.model_template.version == "v1.0.0"
    assert batch_job_spec.aibrix.model_template.spec is not None
    assert batch_job_spec.aibrix.model_template.spec["engine"]["type"] == "mock"
    assert batch_job_spec.aibrix.profile is not None
    assert batch_job_spec.aibrix.profile.name == "inline-profile"
    assert batch_job_spec.aibrix.profile.spec is not None
    assert (
        batch_job_spec.aibrix.profile.spec["scheduling"]["completion_window"] == "24h"
    )


def test_batch_response_includes_input_aibrix_metadata():
    created_at = datetime.now(timezone.utc)
    batch_job = BatchJob(
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="BatchJob"),
        metadata=ObjectMeta(name="test-batch", namespace="default"),
        spec=BatchJobSpec(
            input_file_id="file-123",
            endpoint="/v1/chat/completions",
            completion_window=86400,
            aibrix=AibrixMetadata(
                job_id="planner-job-1",
                runtime=RuntimeSpec(target="Kubernetes"),
                resource_allocation=ResourceAllocation(
                    provision_id="reservation-1",
                    provision_resource_deadline=123,
                    resource_details=[
                        ResourceDetail(
                            endpoint_cluster="cluster-a",
                            gpu_type="H100",
                            replica=4,
                        )
                    ],
                ),
                model_template=ModelTemplateRef(
                    name="echo-template",
                    version="v1.0.0",
                    overrides={"engine_args": {"max_num_seqs": "128"}},
                ),
                profile=BatchProfileRef(
                    name="default-profile",
                    overrides={"scheduling": {"max_concurrency": 4}},
                ),
                client={
                    "max_concurrency": 64,
                    "adaptive_concurrency": True,
                    "adaptive_max_factor": 8,
                    "retry_policy": {"max_retries": 5},
                },
            ),
        ),
        status=BatchJobStatus(
            jobID="job-123",
            state=BatchJobState.CREATED,
            createdAt=created_at,
        ),
    )

    response = _batch_job_to_openai_response(batch_job)

    assert response.aibrix is not None
    assert response.aibrix.job_id == "planner-job-1"
    assert response.aibrix.resource_allocation is not None
    assert response.aibrix.resource_allocation.provision_id == "reservation-1"
    assert response.aibrix.resource_allocation.resource_details is not None
    assert len(response.aibrix.resource_allocation.resource_details) == 1
    assert response.aibrix.resource_allocation.resource_details[0].gpu_type == "H100"
    assert response.aibrix.runtime is not None
    assert response.aibrix.runtime.target == "Kubernetes"
    assert response.aibrix.model_template is not None
    assert response.aibrix.model_template.name == "echo-template"
    assert response.aibrix.model_template.overrides == {
        "engine_args": {"max_num_seqs": "128"}
    }
    assert response.aibrix.profile is not None
    assert response.aibrix.profile.name == "default-profile"
    assert response.aibrix.profile.overrides == {"scheduling": {"max_concurrency": 4}}
    assert response.aibrix.client is not None
    assert response.aibrix.client.max_concurrency == 64
    assert response.aibrix.client.retry_policy is not None
    assert response.aibrix.client.retry_policy.max_retries == 5
    assert response.model == "echo-template"


def _minimal_batch_job(status: BatchJobStatus) -> BatchJob:
    return BatchJob(
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="BatchJob"),
        metadata=ObjectMeta(
            name="test-batch",
            namespace="default",
            resourceVersion=None,
            creationTimestamp=None,
            deletionTimestamp=None,
        ),
        spec=BatchJobSpec(
            input_file_id="file-123",
            endpoint="/v1/chat/completions",
            completion_window=86400,
        ),
        status=status,
    )


def test_output_file_ids_hidden_until_finished():
    """Mirror OpenAI: file ids stay null until the batch reaches a terminal state.

    The ids are allocated upfront (so the worker can write into them), but the
    underlying multipart upload is not downloadable until finalization, so we must
    not surface them while the batch is still running.
    """
    created_at = datetime.now(timezone.utc)
    for state in (
        BatchJobState.CREATED,
        BatchJobState.VALIDATING,
        BatchJobState.IN_PROGRESS,
        BatchJobState.FINALIZING,
    ):
        batch_job = _minimal_batch_job(
            BatchJobStatus(
                jobID="job-123",
                state=state,
                createdAt=created_at,
                # Allocated upfront, but must not be exposed before termination.
                outputFileID="out-file-1",
                errorFileID="err-file-1",
                tempOutputFileID="temp-out-1",
                tempErrorFileID="temp-err-1",
                requestCounts=RequestCountStats(total=2, completed=1, failed=1),
            )
        )
        response = _batch_job_to_openai_response(batch_job)
        assert response.output_file_id is None, f"leaked output_file_id in {state}"
        assert response.error_file_id is None, f"leaked error_file_id in {state}"


def _finalized_batch_job(*, completed: int, failed: int) -> BatchJob:
    now = datetime.now(timezone.utc)
    return _minimal_batch_job(
        BatchJobStatus(
            jobID="job-123",
            state=BatchJobState.FINALIZED,
            createdAt=now,
            outputFileID="out-file-1",
            errorFileID="err-file-1",
            requestCounts=RequestCountStats(
                total=completed + failed, completed=completed, failed=failed
            ),
            conditions=[
                Condition(
                    type=ConditionType.COMPLETED,
                    status=ConditionStatus.TRUE,
                    lastTransitionTime=now,
                )
            ],
        )
    )


def test_output_file_ids_exposed_when_finished():
    """A finalized job with both successes and failures surfaces both ids."""
    response = _batch_job_to_openai_response(
        _finalized_batch_job(completed=2, failed=1)
    )
    assert response.status == "completed"
    assert response.output_file_id == "out-file-1"
    assert response.error_file_id == "err-file-1"


def test_error_file_id_hidden_when_no_failures():
    """Mirror OpenAI: error_file_id stays null when nothing errored."""
    response = _batch_job_to_openai_response(
        _finalized_batch_job(completed=2, failed=0)
    )
    assert response.output_file_id == "out-file-1"
    assert response.error_file_id is None


def test_output_file_id_hidden_when_no_successes():
    """No successful results means no output file to download."""
    response = _batch_job_to_openai_response(
        _finalized_batch_job(completed=0, failed=2)
    )
    assert response.output_file_id is None
    assert response.error_file_id == "err-file-1"


def test_get_batch_response_omits_none_fields_from_json(monkeypatch):
    created_at = datetime.now(timezone.utc)
    batch_job = BatchJob(
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="BatchJob"),
        metadata=ObjectMeta(name="test-batch", namespace="default"),
        spec=BatchJobSpec(
            input_file_id="file-123",
            endpoint="/v1/chat/completions",
            completion_window=86400,
            aibrix=AibrixMetadata(
                job_id="planner-job-1",
                resource_allocation=ResourceAllocation(
                    provision_id="reservation-1",
                    provision_resource_deadline=123,
                    resource_details=[
                        ResourceDetail(
                            endpoint_cluster="cluster-a",
                            replica=1,
                        )
                    ],
                ),
            ),
        ),
        status=BatchJobStatus(
            jobID="job-123",
            state=BatchJobState.CREATED,
            createdAt=created_at,
            failedAt=created_at,
            errors=[
                BatchJobError(
                    code=BatchJobErrorCode.RESOURCE_CREATION_ERROR,
                    message="workload already exists",
                    param=None,
                    line=None,
                )
            ],
        ),
    )

    async def fake_resolve_batch_job(request, batch_id):
        assert batch_id == "job-123"
        return batch_job

    monkeypatch.setattr(
        "aibrix.metadata.api.v1.batch._resolve_batch_job",
        fake_resolve_batch_job,
    )

    app = FastAPI()
    app.include_router(batch_router, prefix="/v1/batches")

    with TestClient(app) as client:
        payload = client.get("/v1/batches/job-123").json()

    assert "output_file_id" not in payload
    assert "error_file_id" not in payload
    assert "usage" not in payload

    error = payload["errors"]["data"][0]
    assert error == {
        "code": BatchJobErrorCode.RESOURCE_CREATION_ERROR.value,
        "message": "workload already exists",
    }

    resource = payload["aibrix"]["resource_allocation"]["resource_details"][0]
    assert resource == {"endpoint_cluster": "cluster-a", "replica": 1}


@pytest.mark.parametrize(
    "endpoint,body",
    [
        (
            "/v1/chat/completions",
            {
                "model": "gpt-3.5-turbo",
                "messages": [{"role": "user", "content": "Hi"}],
            },
        ),
        (
            "/v1/completions",
            {"model": "gpt-3.5-turbo", "prompt": "Hi"},
        ),
        (
            "/v1/embeddings",
            {"model": "text-embedding-ada-002", "input": "Hi"},
        ),
        (
            "/v1/rerank",
            {
                "model": "reranker-v1",
                "query": "Hi",
                "documents": ["doc1"],
            },
        ),
    ],
    ids=["chat_completions", "completions", "embeddings", "rerank"],
)
def test_all_endpoints_accept_valid_bodies(endpoint, body):
    """Parametrized test: all endpoints accept their valid bodies."""
    result = validate_request_body_for_endpoint(body, endpoint, 1)
    assert result is None, f"Unexpected error for {endpoint}: {result}"


def test_validate_aibrix_extension_rejects_unknown_runtime_target():
    from fastapi import HTTPException

    from aibrix.metadata.api.v1.batch import (
        AibrixExtension,
        _validate_aibrix_extension,
    )

    ext = AibrixExtension(runtime={"target": "kubernetes"})  # lowercase typo
    with pytest.raises(HTTPException) as excinfo:
        _validate_aibrix_extension(None, ext)
    assert excinfo.value.status_code == 400


def test_validate_aibrix_extension_accepts_known_runtime_target():
    from aibrix.metadata.api.v1.batch import (
        AibrixExtension,
        _validate_aibrix_extension,
    )

    # Known runtime target + no model_template returns cleanly (no request access).
    ext = AibrixExtension(runtime={"target": "Kubernetes"})
    _validate_aibrix_extension(None, ext)

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

from aibrix.batch.job_entity import (
    AibrixMetadata,
    BatchJob,
    BatchJobEndpoint,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    BatchProfileRef,
    ModelTemplateRef,
    ObjectMeta,
    PlannerDecision,
    ResourceDetail,
    TypeMeta,
)
from aibrix.metadata.api.v1.batch import (
    BatchSpec,
    _batch_job_to_openai_response,
    _validate_request_body_for_endpoint,
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
        result = _validate_request_body_for_endpoint(body, "/v1/chat/completions", 1)
        assert result is None

    def test_chat_completions_missing_messages(self):
        """Test chat completions body without messages fails."""
        body = {"model": "gpt-3.5-turbo"}
        result = _validate_request_body_for_endpoint(body, "/v1/chat/completions", 1)
        assert result is not None
        assert "messages" in result

    def test_chat_completions_messages_not_array(self):
        """Test chat completions body with non-array messages fails."""
        body = {"model": "gpt-3.5-turbo", "messages": "not an array"}
        result = _validate_request_body_for_endpoint(body, "/v1/chat/completions", 1)
        assert result is not None
        assert "messages" in result
        assert "list" in result

    def test_completions_valid_body_string_prompt(self):
        """Test valid completions body with string prompt passes."""
        body = {"model": "gpt-3.5-turbo", "prompt": "Hello world"}
        result = _validate_request_body_for_endpoint(body, "/v1/completions", 1)
        assert result is None

    def test_completions_valid_body_array_prompt(self):
        """Test valid completions body with array prompt passes."""
        body = {"model": "gpt-3.5-turbo", "prompt": ["Hello", "World"]}
        result = _validate_request_body_for_endpoint(body, "/v1/completions", 1)
        assert result is None

    def test_completions_missing_prompt(self):
        """Test completions body without prompt fails."""
        body = {"model": "gpt-3.5-turbo"}
        result = _validate_request_body_for_endpoint(body, "/v1/completions", 1)
        assert result is not None
        assert "prompt" in result

    def test_completions_invalid_prompt_type(self):
        """Test completions body with invalid prompt type fails."""
        body = {"model": "gpt-3.5-turbo", "prompt": 123}
        result = _validate_request_body_for_endpoint(body, "/v1/completions", 1)
        assert result is not None
        assert "prompt" in result

    def test_embeddings_valid_body_string_input(self):
        """Test valid embeddings body with string input passes."""
        body = {"model": "text-embedding-ada-002", "input": "Hello world"}
        result = _validate_request_body_for_endpoint(body, "/v1/embeddings", 1)
        assert result is None

    def test_embeddings_valid_body_array_input(self):
        """Test valid embeddings body with array input passes."""
        body = {"model": "text-embedding-ada-002", "input": ["Hello", "World"]}
        result = _validate_request_body_for_endpoint(body, "/v1/embeddings", 1)
        assert result is None

    def test_embeddings_missing_input(self):
        """Test embeddings body without input fails."""
        body = {"model": "text-embedding-ada-002"}
        result = _validate_request_body_for_endpoint(body, "/v1/embeddings", 1)
        assert result is not None
        assert "input" in result

    def test_embeddings_missing_model(self):
        """Test embeddings body without model fails."""
        body = {"input": "Hello world"}
        result = _validate_request_body_for_endpoint(body, "/v1/embeddings", 1)
        assert result is not None
        assert "model" in result

    def test_rerank_valid_body(self):
        """Test valid rerank body passes validation."""
        body = {
            "model": "reranker-v1",
            "query": "What is AI?",
            "documents": ["AI is...", "Machine learning is..."],
        }
        result = _validate_request_body_for_endpoint(body, "/v1/rerank", 1)
        assert result is None

    def test_rerank_missing_query(self):
        """Test rerank body without query fails."""
        body = {
            "model": "reranker-v1",
            "documents": ["doc1", "doc2"],
        }
        result = _validate_request_body_for_endpoint(body, "/v1/rerank", 1)
        assert result is not None
        assert "query" in result

    def test_rerank_missing_documents(self):
        """Test rerank body without documents fails."""
        body = {
            "model": "reranker-v1",
            "query": "What is AI?",
        }
        result = _validate_request_body_for_endpoint(body, "/v1/rerank", 1)
        assert result is not None
        assert "documents" in result

    def test_rerank_invalid_documents_type(self):
        """Test rerank body with non-array documents fails."""
        body = {
            "model": "reranker-v1",
            "query": "What is AI?",
            "documents": "not an array",
        }
        result = _validate_request_body_for_endpoint(body, "/v1/rerank", 1)
        assert result is not None
        assert "documents" in result
        assert "list" in result

    def test_unknown_endpoint_passes(self):
        """Test that unknown endpoints skip body validation."""
        body = {"anything": "goes"}
        result = _validate_request_body_for_endpoint(body, "/v1/unknown", 1)
        assert result is None


def test_batch_spec_accepts_aibrix_metadata():
    spec = BatchSpec.model_validate(
        {
            "input_file_id": "file-123",
            "endpoint": "/v1/chat/completions",
            "completion_window": "24h",
            "aibrix": {
                "job_id": "planner-job-1",
                "planner_decision": {
                    "provision_id": "reservation-1",
                    "provision_resource_deadline": 123,
                    "resource_details": [
                        {
                            "resource_type": "openai",
                            "endpoint_cluster": "cluster-a",
                            "gpu_type": "H100",
                            "worker_num": 4,
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
    assert batch_job_spec.aibrix.planner_decision is not None
    assert batch_job_spec.aibrix.planner_decision.provision_id == "reservation-1"
    assert batch_job_spec.aibrix.planner_decision.provision_resource_deadline == 123
    assert len(batch_job_spec.aibrix.planner_decision.resource_details or []) == 1
    resource = batch_job_spec.aibrix.planner_decision.resource_details[0]
    assert resource is not None
    assert resource.resource_type == "openai"
    assert resource.endpoint_cluster == "cluster-a"
    assert resource.gpu_type == "H100"
    assert resource.worker_num == 4
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
                planner_decision=PlannerDecision(
                    provision_id="reservation-1",
                    provision_resource_deadline=123,
                    resource_details=[
                        ResourceDetail(
                            resource_type="openai",
                            endpoint_cluster="cluster-a",
                            gpu_type="H100",
                            worker_num=4,
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
    assert response.aibrix.planner_decision is not None
    assert response.aibrix.planner_decision.provision_id == "reservation-1"
    assert response.aibrix.planner_decision.resource_details is not None
    assert len(response.aibrix.planner_decision.resource_details) == 1
    assert response.aibrix.planner_decision.resource_details[0].gpu_type == "H100"
    assert response.aibrix.model_template is not None
    assert response.aibrix.model_template.name == "echo-template"
    assert response.aibrix.model_template.overrides == {
        "engine_args": {"max_num_seqs": "128"}
    }
    assert response.aibrix.profile is not None
    assert response.aibrix.profile.name == "default-profile"
    assert response.aibrix.profile.overrides == {"scheduling": {"max_concurrency": 4}}
    assert response.model == "echo-template"


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
    result = _validate_request_body_for_endpoint(body, endpoint, 1)
    assert result is None, f"Unexpected error for {endpoint}: {result}"

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

import pytest

from aibrix.batch.job_entity import BatchJobEndpoint
from aibrix.metadata.api.v1.batch import _validate_request_body_for_endpoint


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
        result = _validate_request_body_for_endpoint(
            body, "/v1/chat/completions", 1
        )
        assert result is None

    def test_chat_completions_missing_messages(self):
        """Test chat completions body without messages fails."""
        body = {"model": "gpt-3.5-turbo"}
        result = _validate_request_body_for_endpoint(
            body, "/v1/chat/completions", 1
        )
        assert result is not None
        assert "messages" in result

    def test_chat_completions_messages_not_array(self):
        """Test chat completions body with non-array messages fails."""
        body = {"model": "gpt-3.5-turbo", "messages": "not an array"}
        result = _validate_request_body_for_endpoint(
            body, "/v1/chat/completions", 1
        )
        assert result is not None
        assert "messages" in result
        assert "array" in result

    def test_completions_valid_body_string_prompt(self):
        """Test valid completions body with string prompt passes."""
        body = {"model": "gpt-3.5-turbo", "prompt": "Hello world"}
        result = _validate_request_body_for_endpoint(
            body, "/v1/completions", 1
        )
        assert result is None

    def test_completions_valid_body_array_prompt(self):
        """Test valid completions body with array prompt passes."""
        body = {"model": "gpt-3.5-turbo", "prompt": ["Hello", "World"]}
        result = _validate_request_body_for_endpoint(
            body, "/v1/completions", 1
        )
        assert result is None

    def test_completions_missing_prompt(self):
        """Test completions body without prompt fails."""
        body = {"model": "gpt-3.5-turbo"}
        result = _validate_request_body_for_endpoint(
            body, "/v1/completions", 1
        )
        assert result is not None
        assert "prompt" in result

    def test_completions_invalid_prompt_type(self):
        """Test completions body with invalid prompt type fails."""
        body = {"model": "gpt-3.5-turbo", "prompt": 123}
        result = _validate_request_body_for_endpoint(
            body, "/v1/completions", 1
        )
        assert result is not None
        assert "prompt" in result

    def test_embeddings_valid_body_string_input(self):
        """Test valid embeddings body with string input passes."""
        body = {"model": "text-embedding-ada-002", "input": "Hello world"}
        result = _validate_request_body_for_endpoint(
            body, "/v1/embeddings", 1
        )
        assert result is None

    def test_embeddings_valid_body_array_input(self):
        """Test valid embeddings body with array input passes."""
        body = {"model": "text-embedding-ada-002", "input": ["Hello", "World"]}
        result = _validate_request_body_for_endpoint(
            body, "/v1/embeddings", 1
        )
        assert result is None

    def test_embeddings_missing_input(self):
        """Test embeddings body without input fails."""
        body = {"model": "text-embedding-ada-002"}
        result = _validate_request_body_for_endpoint(
            body, "/v1/embeddings", 1
        )
        assert result is not None
        assert "input" in result

    def test_embeddings_missing_model(self):
        """Test embeddings body without model fails."""
        body = {"input": "Hello world"}
        result = _validate_request_body_for_endpoint(
            body, "/v1/embeddings", 1
        )
        assert result is not None
        assert "model" in result

    def test_rerank_valid_body(self):
        """Test valid rerank body passes validation."""
        body = {
            "model": "reranker-v1",
            "query": "What is AI?",
            "documents": ["AI is...", "Machine learning is..."],
        }
        result = _validate_request_body_for_endpoint(
            body, "/v1/rerank", 1
        )
        assert result is None

    def test_rerank_missing_query(self):
        """Test rerank body without query fails."""
        body = {
            "model": "reranker-v1",
            "documents": ["doc1", "doc2"],
        }
        result = _validate_request_body_for_endpoint(
            body, "/v1/rerank", 1
        )
        assert result is not None
        assert "query" in result

    def test_rerank_missing_documents(self):
        """Test rerank body without documents fails."""
        body = {
            "model": "reranker-v1",
            "query": "What is AI?",
        }
        result = _validate_request_body_for_endpoint(
            body, "/v1/rerank", 1
        )
        assert result is not None
        assert "documents" in result

    def test_rerank_invalid_documents_type(self):
        """Test rerank body with non-array documents fails."""
        body = {
            "model": "reranker-v1",
            "query": "What is AI?",
            "documents": "not an array",
        }
        result = _validate_request_body_for_endpoint(
            body, "/v1/rerank", 1
        )
        assert result is not None
        assert "documents" in result
        assert "array" in result

    def test_unknown_endpoint_passes(self):
        """Test that unknown endpoints skip body validation."""
        body = {"anything": "goes"}
        result = _validate_request_body_for_endpoint(
            body, "/v1/unknown", 1
        )
        assert result is None

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
    def test_all_endpoints_accept_valid_bodies(self, endpoint, body):
        """Parametrized test: all endpoints accept their valid bodies."""
        result = _validate_request_body_for_endpoint(body, endpoint, 1)
        assert result is None, f"Unexpected error for {endpoint}: {result}"

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

"""Unit tests for batch API endpoint support."""

import pytest

from aibrix.batch.job_entity import BatchJobEndpoint


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

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

from http import HTTPStatus
from unittest.mock import AsyncMock, MagicMock, patch

import httpx
import pytest
from fastapi.testclient import TestClient

from aibrix.app import build_app
from aibrix.openapi.engine.vllm import VLLMInferenceEngine
from aibrix.openapi.protocol import (
    EmbeddingData,
    EmbeddingRequest,
    EmbeddingResponse,
    EmbeddingUsage,
    ErrorResponse,
)


class TestVLLMInferenceEngineEmbeddings:
    def setup_method(self):
        """Set up test fixtures."""
        self.engine = VLLMInferenceEngine(
            name="vllm",
            version="0.6.1",
            endpoint="http://localhost:8000",
        )

    @pytest.mark.asyncio
    async def test_create_embeddings_success(self):
        """Test successful embeddings creation."""
        # Mock the VLLM response
        mock_response = {
            "object": "list",
            "data": [
                {
                    "object": "embedding",
                    "embedding": [0.1, 0.2, 0.3, 0.4],
                    "index": 0,
                }
            ],
            "model": "text-embedding-ada-002",
            "usage": {"prompt_tokens": 4, "total_tokens": 4},
        }

        mock_http_response = MagicMock()
        mock_http_response.status_code = HTTPStatus.OK
        mock_http_response.json.return_value = mock_response

        with patch.object(self.engine.client, "post", new_callable=AsyncMock) as mock_post:
            mock_post.return_value = mock_http_response

            request = EmbeddingRequest(
                input="Hello world",
                model="text-embedding-ada-002",
            )

            result = await self.engine.create_embeddings(request)

            # Verify the result
            assert isinstance(result, EmbeddingResponse)
            assert result.object == "list"
            assert len(result.data) == 1
            assert result.data[0].embedding == [0.1, 0.2, 0.3, 0.4]
            assert result.model == "text-embedding-ada-002"
            assert result.usage.prompt_tokens == 4

            # Verify the HTTP call
            mock_post.assert_called_once_with(
                "http://localhost:8000/v1/embeddings",
                json=request.model_dump(),
                headers=self.engine.headers,
            )

    @pytest.mark.asyncio
    async def test_create_embeddings_batch_input(self):
        """Test embeddings creation with batch input."""
        mock_response = {
            "object": "list",
            "data": [
                {
                    "object": "embedding",
                    "embedding": [0.1, 0.2, 0.3],
                    "index": 0,
                },
                {
                    "object": "embedding",
                    "embedding": [0.4, 0.5, 0.6],
                    "index": 1,
                },
            ],
            "model": "text-embedding-ada-002",
            "usage": {"prompt_tokens": 8, "total_tokens": 8},
        }

        mock_http_response = MagicMock()
        mock_http_response.status_code = HTTPStatus.OK
        mock_http_response.json.return_value = mock_response

        with patch.object(self.engine.client, "post", new_callable=AsyncMock) as mock_post:
            mock_post.return_value = mock_http_response

            request = EmbeddingRequest(
                input=["Hello", "World"],
                model="text-embedding-ada-002",
            )

            result = await self.engine.create_embeddings(request)

            assert isinstance(result, EmbeddingResponse)
            assert len(result.data) == 2
            assert result.data[0].index == 0
            assert result.data[1].index == 1

    @pytest.mark.asyncio
    async def test_create_embeddings_http_error(self):
        """Test embeddings creation with HTTP error."""
        mock_http_response = MagicMock()
        mock_http_response.status_code = HTTPStatus.BAD_REQUEST
        mock_http_response.text = "Invalid model"

        with patch.object(self.engine.client, "post", new_callable=AsyncMock) as mock_post:
            mock_post.return_value = mock_http_response

            request = EmbeddingRequest(
                input="Hello world",
                model="invalid-model",
            )

            result = await self.engine.create_embeddings(request)

            assert isinstance(result, ErrorResponse)
            assert result.type == "ServerError"
            assert "Failed to create embeddings: Invalid model" in result.message
            assert result.code == HTTPStatus.BAD_REQUEST

    @pytest.mark.asyncio
    async def test_create_embeddings_network_error(self):
        """Test embeddings creation with network error."""
        with patch.object(
            self.engine.client, "post", new_callable=AsyncMock
        ) as mock_post:
            mock_post.side_effect = httpx.ConnectError("Connection failed")

            request = EmbeddingRequest(
                input="Hello world",
                model="text-embedding-ada-002",
            )

            result = await self.engine.create_embeddings(request)

            assert isinstance(result, ErrorResponse)
            assert result.type == "ServerError"
            assert result.message == "Failed to create embeddings"
            assert result.code == HTTPStatus.INTERNAL_SERVER_ERROR

    @pytest.mark.asyncio
    async def test_create_embeddings_with_api_key(self):
        """Test embeddings creation with API key authentication."""
        engine_with_key = VLLMInferenceEngine(
            name="vllm",
            version="0.6.1",
            endpoint="http://localhost:8000",
            api_key="test-api-key",
        )

        mock_response = {
            "object": "list",
            "data": [
                {
                    "object": "embedding",
                    "embedding": [0.1, 0.2],
                    "index": 0,
                }
            ],
            "model": "text-embedding-ada-002",
            "usage": {"prompt_tokens": 2, "total_tokens": 2},
        }

        mock_http_response = MagicMock()
        mock_http_response.status_code = HTTPStatus.OK
        mock_http_response.json.return_value = mock_response

        with patch.object(
            engine_with_key.client, "post", new_callable=AsyncMock
        ) as mock_post:
            mock_post.return_value = mock_http_response

            request = EmbeddingRequest(
                input="test",
                model="text-embedding-ada-002",
            )

            result = await engine_with_key.create_embeddings(request)

            assert isinstance(result, EmbeddingResponse)
            # Verify that the client was created with the Authorization header
            assert "Authorization" in engine_with_key.client.headers
            assert engine_with_key.client.headers["Authorization"] == "Bearer test-api-key"


class TestEmbeddingsAPIEndpoint:
    @pytest.fixture
    def app(self):
        """Create a test FastAPI app."""
        import argparse

        args = argparse.Namespace(enable_fastapi_docs=True)
        return build_app(args)

    @pytest.fixture
    def client(self, app):
        """Create a test client."""
        return TestClient(app)

    def test_embeddings_endpoint_success(self, client):
        """Test the /v1/embeddings endpoint with successful response."""
        # Mock the inference engine
        mock_response = EmbeddingResponse(
            data=[
                EmbeddingData(embedding=[0.1, 0.2, 0.3], index=0),
            ],
            model="text-embedding-ada-002",
            usage=EmbeddingUsage(prompt_tokens=3, total_tokens=3),
        )

        with patch("aibrix.app.inference_engine") as mock_inference_engine:
            mock_engine = MagicMock()
            mock_engine.create_embeddings = AsyncMock(return_value=mock_response)
            mock_inference_engine.return_value = mock_engine

            response = client.post(
                "/v1/embeddings",
                json={
                    "input": "Hello world",
                    "model": "text-embedding-ada-002",
                },
            )

            assert response.status_code == 200
            data = response.json()
            assert data["object"] == "list"
            assert len(data["data"]) == 1
            assert data["data"][0]["embedding"] == [0.1, 0.2, 0.3]
            assert data["model"] == "text-embedding-ada-002"

    def test_embeddings_endpoint_error(self, client):
        """Test the /v1/embeddings endpoint with error response."""
        mock_error = ErrorResponse(
            message="Model not found",
            type="NotFoundError",
            code=404,
        )

        with patch("aibrix.app.inference_engine") as mock_inference_engine:
            mock_engine = MagicMock()
            mock_engine.create_embeddings = AsyncMock(return_value=mock_error)
            mock_inference_engine.return_value = mock_engine

            response = client.post(
                "/v1/embeddings",
                json={
                    "input": "Hello world",
                    "model": "non-existent-model",
                },
            )

            assert response.status_code == 404
            data = response.json()
            assert data["message"] == "Model not found"
            assert data["type"] == "NotFoundError"

    def test_embeddings_endpoint_validation_error(self, client):
        """Test the /v1/embeddings endpoint with validation error."""
        response = client.post(
            "/v1/embeddings",
            json={
                "input": "Hello world",
                # Missing required 'model' field
            },
        )

        assert response.status_code == 422  # Validation error
        data = response.json()
        assert "detail" in data

    def test_embeddings_endpoint_different_input_types(self, client):
        """Test the endpoint with different input types."""
        mock_response = EmbeddingResponse(
            data=[
                EmbeddingData(embedding=[0.1, 0.2], index=0),
                EmbeddingData(embedding=[0.3, 0.4], index=1),
            ],
            model="test-model",
            usage=EmbeddingUsage(prompt_tokens=4, total_tokens=4),
        )

        with patch("aibrix.app.inference_engine") as mock_inference_engine:
            mock_engine = MagicMock()
            mock_engine.create_embeddings = AsyncMock(return_value=mock_response)
            mock_inference_engine.return_value = mock_engine

            # Test with string array
            response = client.post(
                "/v1/embeddings",
                json={
                    "input": ["Hello", "World"],
                    "model": "test-model",
                },
            )
            assert response.status_code == 200

            # Test with token array
            response = client.post(
                "/v1/embeddings",
                json={
                    "input": [101, 102, 103],
                    "model": "test-model",
                },
            )
            assert response.status_code == 200

            # Test with nested token array
            response = client.post(
                "/v1/embeddings",
                json={
                    "input": [[101, 102], [103, 104]],
                    "model": "test-model",
                },
            )
            assert response.status_code == 200

    def test_embeddings_endpoint_optional_parameters(self, client):
        """Test the endpoint with optional parameters."""
        mock_response = EmbeddingResponse(
            data=[
                EmbeddingData(embedding="base64encodedstring", index=0),
            ],
            model="test-model",
            usage=EmbeddingUsage(prompt_tokens=3, total_tokens=3),
        )

        with patch("aibrix.app.inference_engine") as mock_inference_engine:
            mock_engine = MagicMock()
            mock_engine.create_embeddings = AsyncMock(return_value=mock_response)
            mock_inference_engine.return_value = mock_engine

            response = client.post(
                "/v1/embeddings",
                json={
                    "input": "Hello world",
                    "model": "test-model",
                    "encoding_format": "base64",
                    "dimensions": 256,
                    "user": "test-user",
                },
            )

            assert response.status_code == 200
            data = response.json()
            assert data["data"][0]["embedding"] == "base64encodedstring"

            # Verify the request was passed correctly
            call_args = mock_engine.create_embeddings.call_args[0][0]
            assert call_args.encoding_format == "base64"
            assert call_args.dimensions == 256
            assert call_args.user == "test-user"


class TestBaseInferenceEngineEmbeddings:
    def test_base_engine_not_implemented(self):
        """Test that base inference engine returns NotImplementedError."""
        from aibrix.openapi.engine.base import InferenceEngine

        engine = InferenceEngine(
            name="test",
            version="1.0",
            endpoint="http://localhost:8000",
        )

        request = EmbeddingRequest(
            input="test",
            model="test-model",
        )

        import asyncio

        result = asyncio.run(engine.create_embeddings(request))

        assert isinstance(result, ErrorResponse)
        assert result.type == "NotImplementedError"
        assert result.code == HTTPStatus.NOT_IMPLEMENTED
        assert "not support embeddings" in result.message
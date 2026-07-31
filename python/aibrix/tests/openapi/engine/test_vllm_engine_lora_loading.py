# Copyright 2025 The Aibrix Team.
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
from unittest.mock import AsyncMock, Mock, patch

import httpx
import pytest
from tenacity import RetryError

from aibrix.openapi.engine.vllm import VLLMInferenceEngine
from aibrix.openapi.protocol import (
    ErrorResponse,
    LoadLoraAdapterRequest,
    UnloadLoraAdapterRequest,
)


class TestVLLMInferenceEngine:
    """Test robust LoRA implementation in VLLMInferenceEngine."""

    @pytest.fixture
    def engine(self):
        """Create VLLMInferenceEngine instance for testing."""
        return VLLMInferenceEngine("vllm", "0.10.0", "http://localhost:8000")

    @pytest.fixture
    def engine_with_auth(self):
        """Create VLLMInferenceEngine instance with auth for testing."""
        return VLLMInferenceEngine(
            "vllm", "0.10.0", "http://localhost:8000", api_key="test-key"
        )

    def test_initialization(self, engine):
        """Test engine initialization sets up robust HTTP client."""
        assert engine.name == "vllm"
        assert engine.version == "0.10.0"
        assert engine.endpoint == "http://localhost:8000"
        assert engine.client is not None

        # Check timeout configuration
        assert engine.client.timeout.connect == 10.0
        assert engine.client.timeout.read == 30.0
        assert engine.client.timeout.write == 10.0

        # Check that client was configured (we can't directly access private attributes)
        assert hasattr(engine.client, "timeout")
        assert hasattr(engine.client, "follow_redirects")

    def test_initialization_with_auth(self, engine_with_auth):
        """Test engine initialization with API key."""
        # Check that auth header would be set
        auth_header = engine_with_auth.client.headers.get("authorization")
        assert auth_header == "Bearer test-key"

    def test_validate_lora_request_valid(self, engine):
        """Test LoRA request validation with valid inputs."""
        error = engine._validate_lora_request("test-lora", "/path/to/lora")
        assert error is None

    def test_validate_lora_request_empty_name(self, engine):
        """Test LoRA request validation with empty name."""
        error = engine._validate_lora_request("", "/path/to/lora")
        assert "LoRA name cannot be empty" in error

        error = engine._validate_lora_request("   ", "/path/to/lora")
        assert "LoRA name cannot be empty" in error

    def test_validate_lora_request_empty_path(self, engine):
        """Test LoRA request validation with empty path."""
        error = engine._validate_lora_request("test-lora", "")
        assert "LoRA path cannot be empty" in error

    def test_validate_lora_request_name_too_long(self, engine):
        """Test LoRA request validation with name too long."""
        long_name = "a" * 101
        error = engine._validate_lora_request(long_name, "/path/to/lora")
        assert "LoRA name too long" in error

    def test_validate_lora_request_invalid_characters(self, engine):
        """Test LoRA request validation with invalid characters."""
        error = engine._validate_lora_request("test lora!", "/path/to/lora")
        assert "must contain only alphanumeric characters" in error

    def test_validate_lora_request_valid_characters(self, engine):
        """Test LoRA request validation with valid characters."""
        valid_names = ["test-lora", "test_lora", "TestLora123", "lora-v1_2"]
        for name in valid_names:
            error = engine._validate_lora_request(name, "/path/to/lora")
            assert error is None, f"Name '{name}' should be valid"

    def test_validate_lora_request_name_only_valid(self, engine):
        """Test LoRA request validation with only name (no path)."""
        error = engine._validate_lora_request("test-lora")
        assert error is None

    def test_validate_lora_request_name_only_empty(self, engine):
        """Test LoRA request validation with empty name and no path."""
        error = engine._validate_lora_request("")
        assert "LoRA name cannot be empty" in error

    def test_validate_lora_request_name_only_too_long(self, engine):
        """Test LoRA request validation with long name and no path."""
        long_name = "a" * 101
        error = engine._validate_lora_request(long_name)
        assert "LoRA name too long" in error

    def test_validate_lora_request_name_only_invalid_characters(self, engine):
        """Test LoRA request validation with invalid characters and no path."""
        error = engine._validate_lora_request("test lora!")
        assert "must contain only alphanumeric characters" in error


class TestLoraAdapterLoading:
    """Test LoRA adapter loading with robust error handling."""

    @pytest.fixture
    def engine(self):
        return VLLMInferenceEngine("vllm", "0.10.0", "http://localhost:8000")

    @pytest.fixture
    def valid_request(self):
        return LoadLoraAdapterRequest(lora_name="test-lora", lora_path="/path/to/lora")

    @pytest.mark.asyncio
    async def test_load_lora_success(self, engine, valid_request):
        """Test successful LoRA loading."""
        mock_response = Mock()
        mock_response.status_code = HTTPStatus.OK

        with patch.object(
            engine, "_make_request", new_callable=AsyncMock
        ) as mock_make_request:
            mock_make_request.return_value = mock_response

            result = await engine.load_lora_adapter(valid_request)

            assert isinstance(result, str)
            assert "Success" in result
            assert "test-lora" in result
            assert "loaded successfully" in result

    @pytest.mark.asyncio
    async def test_load_lora_validation_error(self, engine):
        """Test LoRA loading with validation error."""
        invalid_request = LoadLoraAdapterRequest(
            lora_name="", lora_path="/path/to/lora"
        )

        result = await engine.load_lora_adapter(invalid_request)

        assert isinstance(result, ErrorResponse)
        assert result.type == "ValidationError"
        assert result.code == HTTPStatus.BAD_REQUEST
        assert "empty" in result.message

    @pytest.mark.asyncio
    async def test_load_lora_timeout_error(self, engine, valid_request):
        """Test LoRA loading with timeout error."""
        with patch.object(
            engine, "_make_request", new_callable=AsyncMock
        ) as mock_make_request:
            mock_make_request.side_effect = httpx.TimeoutException("Request timeout")

            result = await engine.load_lora_adapter(valid_request)

            assert isinstance(result, ErrorResponse)
            assert result.type == "TimeoutError"
            assert result.code == HTTPStatus.REQUEST_TIMEOUT
            assert "took too long" in result.message

    @pytest.mark.asyncio
    async def test_load_lora_connection_error(self, engine, valid_request):
        """Test LoRA loading with connection error."""
        with patch.object(
            engine, "_make_request", new_callable=AsyncMock
        ) as mock_make_request:
            mock_make_request.side_effect = httpx.ConnectError("Connection refused")

            result = await engine.load_lora_adapter(valid_request)

            assert isinstance(result, ErrorResponse)
            assert result.type == "ConnectionError"
            assert result.code == HTTPStatus.BAD_GATEWAY
            assert "Cannot connect" in result.message

    @pytest.mark.asyncio
    async def test_load_lora_request_error(self, engine, valid_request):
        """Test LoRA loading with general request error."""
        with patch.object(
            engine, "_make_request", new_callable=AsyncMock
        ) as mock_make_request:
            mock_make_request.side_effect = httpx.RequestError("Network error")

            result = await engine.load_lora_adapter(valid_request)

            assert isinstance(result, ErrorResponse)
            assert result.type == "RequestError"
            assert result.code == HTTPStatus.INTERNAL_SERVER_ERROR

    @pytest.mark.asyncio
    async def test_load_lora_conflict_error(self, engine, valid_request):
        """Test LoRA loading with conflict (already exists) error."""
        mock_response = Mock()
        mock_response.status_code = HTTPStatus.CONFLICT
        mock_response.text = "LoRA already exists"

        with patch.object(
            engine, "_make_request", new_callable=AsyncMock
        ) as mock_make_request:
            mock_make_request.return_value = mock_response

            result = await engine.load_lora_adapter(valid_request)

            assert isinstance(result, ErrorResponse)
            assert result.type == "ConflictError"
            assert result.code == HTTPStatus.CONFLICT
            assert "already exists" in result.message

    @pytest.mark.asyncio
    async def test_load_lora_not_found_error(self, engine, valid_request):
        """Test LoRA loading with not found error."""
        mock_response = Mock()
        mock_response.status_code = HTTPStatus.NOT_FOUND
        mock_response.text = "LoRA path not found"

        with patch.object(
            engine, "_make_request", new_callable=AsyncMock
        ) as mock_make_request:
            mock_make_request.return_value = mock_response

            result = await engine.load_lora_adapter(valid_request)

            assert isinstance(result, ErrorResponse)
            assert result.type == "NotFoundError"
            assert result.code == HTTPStatus.NOT_FOUND
            assert "not found" in result.message


class TestLoraAdapterUnloading:
    """Test LoRA adapter unloading with robust error handling."""

    @pytest.fixture
    def engine(self):
        return VLLMInferenceEngine("vllm", "0.10.0", "http://localhost:8000")

    @pytest.fixture
    def valid_request(self):
        return UnloadLoraAdapterRequest(lora_name="test-lora")

    @pytest.mark.asyncio
    async def test_unload_lora_success(self, engine, valid_request):
        """Test successful LoRA unloading."""
        mock_response = Mock()
        mock_response.status_code = HTTPStatus.OK

        with patch.object(
            engine, "_make_request", new_callable=AsyncMock
        ) as mock_make_request:
            mock_make_request.return_value = mock_response

            result = await engine.unload_lora_adapter(valid_request)

            assert isinstance(result, str)
            assert "Success" in result
            assert "test-lora" in result
            assert "unloaded successfully" in result

    @pytest.mark.asyncio
    async def test_unload_lora_empty_name(self, engine):
        """Test LoRA unloading with empty name."""
        invalid_request = UnloadLoraAdapterRequest(lora_name="")

        result = await engine.unload_lora_adapter(invalid_request)

        assert isinstance(result, ErrorResponse)
        assert result.type == "ValidationError"
        assert result.code == HTTPStatus.BAD_REQUEST

    @pytest.mark.asyncio
    async def test_unload_lora_name_too_long(self, engine):
        """Test LoRA unloading with name too long (now validated)."""
        long_name = "a" * 101
        invalid_request = UnloadLoraAdapterRequest(lora_name=long_name)

        result = await engine.unload_lora_adapter(invalid_request)

        assert isinstance(result, ErrorResponse)
        assert result.type == "ValidationError"
        assert result.code == HTTPStatus.BAD_REQUEST
        assert "too long" in result.message

    @pytest.mark.asyncio
    async def test_unload_lora_invalid_characters(self, engine):
        """Test LoRA unloading with invalid characters (now validated)."""
        invalid_request = UnloadLoraAdapterRequest(lora_name="test lora!")

        result = await engine.unload_lora_adapter(invalid_request)

        assert isinstance(result, ErrorResponse)
        assert result.type == "ValidationError"
        assert result.code == HTTPStatus.BAD_REQUEST
        assert "alphanumeric characters" in result.message

    @pytest.mark.asyncio
    async def test_unload_lora_not_found_error(self, engine, valid_request):
        """Test LoRA unloading with not found error."""
        mock_response = Mock()
        mock_response.status_code = HTTPStatus.NOT_FOUND
        mock_response.text = "LoRA not found"

        with patch.object(
            engine, "_make_request", new_callable=AsyncMock
        ) as mock_make_request:
            mock_make_request.return_value = mock_response

            result = await engine.unload_lora_adapter(valid_request)

            assert isinstance(result, ErrorResponse)
            assert result.type == "NotFoundError"
            assert result.code == HTTPStatus.NOT_FOUND


class TestMakeRequestRetry:
    """Test the retry logic in _make_request method."""

    @pytest.fixture
    def engine(self):
        return VLLMInferenceEngine("vllm", "0.10.0", "http://localhost:8000")

    @pytest.mark.asyncio
    async def test_make_request_success_first_try(self, engine):
        """Test successful request on first attempt."""
        mock_response = Mock()
        mock_response.status_code = HTTPStatus.OK

        with patch.object(engine.client, "post", new_callable=AsyncMock) as mock_post:
            mock_post.return_value = mock_response

            result = await engine._make_request("post", "http://test.com", json={})

            assert result == mock_response
            assert mock_post.call_count == 1

    @pytest.mark.asyncio
    async def test_make_request_retry_on_timeout(self, engine):
        """Test retry logic on timeout errors."""
        mock_response = Mock()
        mock_response.status_code = HTTPStatus.OK

        with patch.object(engine.client, "post", new_callable=AsyncMock) as mock_post:
            # First two calls timeout, third succeeds
            mock_post.side_effect = [
                httpx.TimeoutException("Timeout 1"),
                httpx.TimeoutException("Timeout 2"),
                mock_response,
            ]

            result = await engine._make_request("post", "http://test.com", json={})

            assert result == mock_response
            assert mock_post.call_count == 3

    @pytest.mark.asyncio
    async def test_make_request_retry_exhausted(self, engine):
        """Test retry logic when all attempts fail."""
        with patch.object(engine.client, "post", new_callable=AsyncMock) as mock_post:
            mock_post.side_effect = httpx.TimeoutException("Persistent timeout")

            with pytest.raises(RetryError):
                await engine._make_request("post", "http://test.com", json={})

            # Should try 3 times (initial + 2 retries)
            assert mock_post.call_count == 3

    @pytest.mark.asyncio
    async def test_make_request_server_error_retry(self, engine):
        """Test retry on server errors (5xx)."""
        # First response: server error that triggers retry
        mock_error_response = Mock()
        mock_error_response.status_code = 500
        mock_error_response.raise_for_status.side_effect = httpx.HTTPStatusError(
            "500 Server Error", request=Mock(), response=mock_error_response
        )

        # Second response: success
        mock_success_response = Mock()
        mock_success_response.status_code = 200
        mock_success_response.raise_for_status.return_value = None  # No exception

        with patch.object(engine.client, "post", new_callable=AsyncMock) as mock_post:
            mock_post.side_effect = [mock_error_response, mock_success_response]

            result = await engine._make_request("post", "http://test.com", json={})

            assert result == mock_success_response
            assert mock_post.call_count == 2

    @pytest.mark.asyncio
    async def test_make_request_unsupported_method(self, engine):
        """Test unsupported HTTP method raises ValueError."""
        with pytest.raises(ValueError, match="Unsupported HTTP method"):
            await engine._make_request("PATCH", "http://test.com")


class TestListModels:
    """Test list models functionality with robust error handling."""

    @pytest.fixture
    def engine(self):
        return VLLMInferenceEngine("vllm", "0.10.0", "http://localhost:8000")

    @pytest.mark.asyncio
    async def test_list_models_success(self, engine):
        """Test successful model listing."""
        mock_response = Mock()
        mock_response.status_code = HTTPStatus.OK
        mock_response.json.return_value = {"models": ["model1", "model2"]}

        with patch.object(
            engine, "_make_request", new_callable=AsyncMock
        ) as mock_make_request:
            mock_make_request.return_value = mock_response

            result = await engine.list_models()

            assert result == {"models": ["model1", "model2"]}

    @pytest.mark.asyncio
    async def test_list_models_json_parse_error(self, engine):
        """Test model listing with JSON parse error."""
        mock_response = Mock()
        mock_response.status_code = HTTPStatus.OK
        mock_response.json.side_effect = ValueError("Invalid JSON")

        with patch.object(
            engine, "_make_request", new_callable=AsyncMock
        ) as mock_make_request:
            mock_make_request.return_value = mock_response

            result = await engine.list_models()

            assert isinstance(result, ErrorResponse)
            assert result.type == "ServerError"
            assert "parse" in result.message


class TestContextManager:
    """Test async context manager functionality."""

    @pytest.mark.asyncio
    async def test_context_manager_closes_client(self):
        """Test context manager properly closes HTTP client."""
        engine = VLLMInferenceEngine("vllm", "0.10.0", "http://localhost:8000")

        with patch.object(
            engine.client, "aclose", new_callable=AsyncMock
        ) as mock_close:
            async with engine:
                pass

            mock_close.assert_called_once()

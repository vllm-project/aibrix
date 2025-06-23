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
from pydantic import ValidationError

from aibrix.openapi.protocol import (
    EmbeddingData,
    EmbeddingRequest,
    EmbeddingResponse,
    EmbeddingUsage,
)


class TestEmbeddingRequest:
    def test_valid_string_input(self):
        """Test EmbeddingRequest with valid string input."""
        request = EmbeddingRequest(
            input="Hello, world!",
            model="text-embedding-ada-002",
        )
        assert request.input == "Hello, world!"
        assert request.model == "text-embedding-ada-002"
        assert request.encoding_format == "float"  # default value
        assert request.dimensions is None
        assert request.user is None

    def test_valid_string_array_input(self):
        """Test EmbeddingRequest with string array input."""
        request = EmbeddingRequest(
            input=["Hello", "World"],
            model="text-embedding-ada-002",
        )
        assert request.input == ["Hello", "World"]
        assert request.model == "text-embedding-ada-002"

    def test_valid_token_array_input(self):
        """Test EmbeddingRequest with token array input."""
        request = EmbeddingRequest(
            input=[101, 102, 103],
            model="text-embedding-ada-002",
        )
        assert request.input == [101, 102, 103]

    def test_valid_nested_token_array_input(self):
        """Test EmbeddingRequest with nested token array input."""
        request = EmbeddingRequest(
            input=[[101, 102], [103, 104]],
            model="text-embedding-ada-002",
        )
        assert request.input == [[101, 102], [103, 104]]

    def test_optional_parameters(self):
        """Test EmbeddingRequest with optional parameters."""
        request = EmbeddingRequest(
            input="test",
            model="text-embedding-ada-002",
            encoding_format="base64",
            dimensions=512,
            user="user123",
        )
        assert request.encoding_format == "base64"
        assert request.dimensions == 512
        assert request.user == "user123"

    def test_invalid_encoding_format(self):
        """Test EmbeddingRequest with invalid encoding format."""
        with pytest.raises(ValidationError) as exc_info:
            EmbeddingRequest(
                input="test",
                model="text-embedding-ada-002",
                encoding_format="invalid",
            )
        assert "Input should be 'float' or 'base64'" in str(exc_info.value)

    def test_missing_required_fields(self):
        """Test EmbeddingRequest with missing required fields."""
        with pytest.raises(ValidationError) as exc_info:
            EmbeddingRequest(input="test")
        assert "Field required" in str(exc_info.value)

    def test_extra_fields_not_allowed(self):
        """Test that extra fields are not allowed."""
        with pytest.raises(ValidationError) as exc_info:
            EmbeddingRequest(
                input="test",
                model="text-embedding-ada-002",
                extra_field="not_allowed",
            )
        assert "Extra inputs are not permitted" in str(exc_info.value)

    def test_model_dump(self):
        """Test serialization of EmbeddingRequest."""
        request = EmbeddingRequest(
            input="test",
            model="text-embedding-ada-002",
            dimensions=256,
        )
        data = request.model_dump()
        assert data["input"] == "test"
        assert data["model"] == "text-embedding-ada-002"
        assert data["encoding_format"] == "float"
        assert data["dimensions"] == 256
        assert data["user"] is None


class TestEmbeddingData:
    def test_valid_float_embedding(self):
        """Test EmbeddingData with float array embedding."""
        data = EmbeddingData(
            embedding=[0.1, 0.2, 0.3, 0.4],
            index=0,
        )
        assert data.object == "embedding"
        assert data.embedding == [0.1, 0.2, 0.3, 0.4]
        assert data.index == 0

    def test_valid_base64_embedding(self):
        """Test EmbeddingData with base64 string embedding."""
        data = EmbeddingData(
            embedding="base64encodedstring",
            index=1,
        )
        assert data.object == "embedding"
        assert data.embedding == "base64encodedstring"
        assert data.index == 1

    def test_object_literal_fixed(self):
        """Test that object field is always 'embedding'."""
        data = EmbeddingData(
            embedding=[0.1, 0.2],
            index=0,
        )
        assert data.object == "embedding"
        # Cannot override the literal value

    def test_missing_required_fields(self):
        """Test EmbeddingData with missing required fields."""
        with pytest.raises(ValidationError) as exc_info:
            EmbeddingData(embedding=[0.1, 0.2])
        assert "Field required" in str(exc_info.value)


class TestEmbeddingUsage:
    def test_valid_usage(self):
        """Test EmbeddingUsage with valid token counts."""
        usage = EmbeddingUsage(
            prompt_tokens=10,
            total_tokens=10,
        )
        assert usage.prompt_tokens == 10
        assert usage.total_tokens == 10

    def test_zero_tokens(self):
        """Test EmbeddingUsage with zero tokens."""
        usage = EmbeddingUsage(
            prompt_tokens=0,
            total_tokens=0,
        )
        assert usage.prompt_tokens == 0
        assert usage.total_tokens == 0

    def test_missing_fields(self):
        """Test EmbeddingUsage with missing fields."""
        with pytest.raises(ValidationError) as exc_info:
            EmbeddingUsage(prompt_tokens=10)
        assert "Field required" in str(exc_info.value)


class TestEmbeddingResponse:
    def test_valid_response(self):
        """Test EmbeddingResponse with valid data."""
        response = EmbeddingResponse(
            data=[
                EmbeddingData(embedding=[0.1, 0.2, 0.3], index=0),
                EmbeddingData(embedding=[0.4, 0.5, 0.6], index=1),
            ],
            model="text-embedding-ada-002",
            usage=EmbeddingUsage(prompt_tokens=6, total_tokens=6),
        )
        assert response.object == "list"
        assert len(response.data) == 2
        assert response.model == "text-embedding-ada-002"
        assert response.usage.prompt_tokens == 6
        assert response.usage.total_tokens == 6

    def test_empty_data_list(self):
        """Test EmbeddingResponse with empty data list."""
        response = EmbeddingResponse(
            data=[],
            model="text-embedding-ada-002",
            usage=EmbeddingUsage(prompt_tokens=0, total_tokens=0),
        )
        assert len(response.data) == 0

    def test_model_dump_json(self):
        """Test JSON serialization of EmbeddingResponse."""
        response = EmbeddingResponse(
            data=[
                EmbeddingData(embedding=[0.1, 0.2], index=0),
            ],
            model="text-embedding-ada-002",
            usage=EmbeddingUsage(prompt_tokens=3, total_tokens=3),
        )
        data = response.model_dump()
        assert data["object"] == "list"
        assert len(data["data"]) == 1
        assert data["data"][0]["object"] == "embedding"
        assert data["data"][0]["embedding"] == [0.1, 0.2]
        assert data["data"][0]["index"] == 0
        assert data["model"] == "text-embedding-ada-002"
        assert data["usage"]["prompt_tokens"] == 3
        assert data["usage"]["total_tokens"] == 3

    def test_base64_response(self):
        """Test EmbeddingResponse with base64 encoded embeddings."""
        response = EmbeddingResponse(
            data=[
                EmbeddingData(embedding="base64string1", index=0),
                EmbeddingData(embedding="base64string2", index=1),
            ],
            model="text-embedding-ada-002",
            usage=EmbeddingUsage(prompt_tokens=10, total_tokens=10),
        )
        assert response.data[0].embedding == "base64string1"
        assert response.data[1].embedding == "base64string2"


class TestEmbeddingProtocolIntegration:
    def test_request_response_cycle(self):
        """Test a complete request-response cycle."""
        # Create a request
        request = EmbeddingRequest(
            input=["Hello", "World"],
            model="text-embedding-ada-002",
            encoding_format="float",
        )

        # Simulate processing and create response
        response = EmbeddingResponse(
            data=[
                EmbeddingData(embedding=[0.1, 0.2, 0.3], index=0),
                EmbeddingData(embedding=[0.4, 0.5, 0.6], index=1),
            ],
            model=request.model,
            usage=EmbeddingUsage(prompt_tokens=4, total_tokens=4),
        )

        # Verify the response matches the request
        assert response.model == request.model
        assert len(response.data) == len(request.input)

    def test_mixed_input_types(self):
        """Test that different input types are properly validated."""
        # Valid cases
        valid_inputs = [
            "single string",
            ["multiple", "strings"],
            [1, 2, 3, 4],
            [[1, 2], [3, 4]],
        ]

        for input_val in valid_inputs:
            request = EmbeddingRequest(
                input=input_val,
                model="test-model",
            )
            assert request.input == input_val

        # Invalid cases
        invalid_inputs = [
            {"dict": "not allowed"},
            12.34,  # float not allowed
            True,  # boolean not allowed
        ]

        for input_val in invalid_inputs:
            with pytest.raises(ValidationError):
                EmbeddingRequest(
                    input=input_val,
                    model="test-model",
                )


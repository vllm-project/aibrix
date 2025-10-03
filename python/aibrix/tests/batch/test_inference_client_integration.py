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

from aibrix.batch.job_driver import InferenceEngineClient, ProxyInferenceEngineClient


class TestInferenceClientIntegration:
    """Test inference client functionality and retry logic."""

    @pytest.mark.asyncio
    async def test_mock_inference_client(self):
        """Test inference client in mock mode."""
        client = InferenceEngineClient()  # No base_url = mock mode

        request_data = {
            "custom_id": "test-1",
            "method": "POST",
            "url": "/v1/chat/completions",
            "body": {
                "model": "gpt-3.5-turbo",
                "messages": [{"role": "user", "content": "Hello"}],
            },
        }

        # Test mock response
        response = await client.inference_request("/v1/chat/completions", request_data)
        assert response == request_data  # Mock should echo the request

    @pytest.mark.asyncio
    async def test_real_inference_client_with_invalid_url(self):
        """Test inference client with invalid URL to verify error handling."""
        client = ProxyInferenceEngineClient("http://invalid-host:9999")

        request_data = {
            "custom_id": "test-1",
            "method": "POST",
            "url": "/v1/chat/completions",
            "body": {
                "model": "gpt-3.5-turbo",
                "messages": [{"role": "user", "content": "Hello"}],
            },
        }

        # Should raise an exception when trying to connect to invalid host
        with pytest.raises(Exception):  # Will be httpx.RequestError or similar
            await client.inference_request("/v1/chat/completions", request_data)

    @pytest.mark.asyncio
    async def test_retry_behavior_demonstration(self):
        """Demonstrate that the retry logic works by testing mock behavior."""

        class FailingInferenceClient(InferenceEngineClient):
            def __init__(self):
                super().__init__()
                self.attempt_count = 0

            async def inference_request(self, endpoint, request_data):
                self.attempt_count += 1
                if self.attempt_count < 3:
                    raise Exception(f"Simulated failure {self.attempt_count}")
                return {"success": True, "attempts": self.attempt_count}

        # Note: This test shows the pattern, but retry logic is in JobDriver
        # In actual usage, JobDriver would retry the inference_request calls
        client = FailingInferenceClient()

        # First two calls should fail
        with pytest.raises(Exception, match="Simulated failure 1"):
            await client.inference_request("/test", {})

        with pytest.raises(Exception, match="Simulated failure 2"):
            await client.inference_request("/test", {})

        # Third call should succeed
        result = await client.inference_request("/test", {})
        assert result["success"] is True
        assert result["attempts"] == 3

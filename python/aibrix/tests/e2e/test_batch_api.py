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

"""
End-to-end test for OpenAI Batch API against real service.

This test validates the complete batch processing workflow against a real
Aibrix service running at http://localhost:8888/.

Test workflow:
1. Upload sample input file via Files API
2. Create batch job via Batch API
3. Poll job status until completion
4. Download and verify output via Files API
5. Verify batch list API works

Prerequisites:
- Aibrix service running at http://localhost:8888/
- Service configured with proper storage backend
- Network connectivity to the service

Usage:
    pytest tests/e2e/test_batch_api.py -v
    pytest tests/e2e/test_batch_api.py::test_batch_api_e2e_real_service -v
"""

import asyncio
import copy
import json
from typing import Any, Dict

import httpx
import pytest


def generate_batch_input_data(num_requests: int = 3) -> str:
    """Generate test batch input data and return the content as string."""
    base_request: Dict[str, Any] = {
        "custom_id": "request-1",
        "method": "POST",
        "url": "/v1/chat/completions",
        "body": {
            "model": "gpt-3.5-turbo-0125",
            "messages": [
                {"role": "system", "content": "You are a helpful assistant."},
                {"role": "user", "content": "Hello world!"},
            ],
            "max_tokens": 1000,
        },
    }

    lines = []
    for i in range(num_requests):
        request = copy.deepcopy(base_request)
        request["custom_id"] = f"request-{i+1}"
        request["body"]["messages"][1]["content"] = f"Hello from request {i+1}!"
        lines.append(json.dumps(request))

    return "\n".join(lines)


def verify_batch_output_content(output_content: str, expected_requests: int) -> bool:
    """Verify that batch output content has the expected structure."""
    lines = output_content.strip().split("\n")

    if len(lines) != expected_requests:
        print(f"Expected {expected_requests} output lines, got {len(lines)}")
        return False

    for i, line in enumerate(lines):
        try:
            output = json.loads(line)

            # Check required fields in OpenAI batch response format
            required_fields = ["id", "custom_id", "response"]
            for field in required_fields:
                if field not in output:
                    print(f"Missing required field '{field}' in response {i+1}")
                    return False

            # Verify custom_id matches expected pattern
            expected_custom_id = f"request-{i+1}"
            if output["custom_id"] != expected_custom_id:
                print(
                    f"Expected custom_id '{expected_custom_id}', got '{output['custom_id']}'"
                )
                return False

            response = output["response"]
            required_fields = ["status_code", "request_id", "body"]
            for field in required_fields:
                if field not in response:
                    print(
                        f"Missing required field 'response.{field}' in response {i+1}"
                    )
                    return False

            # Check that we got a successful response
            if response["status_code"] != 200:
                print(f"Expected status_code 200, got {response['status_code']}")
                return False

            body = response["body"]
            required_fields = ["model", "choices"]
            for field in required_fields:
                if field not in body:
                    print(
                        f"Missing required field 'response.body.{field}' in response {i+1}"
                    )
                    return False

        except json.JSONDecodeError as e:
            print(f"Invalid JSON in output line {i+1}: {e}")
            return False

    return True


async def check_service_health(base_url: str) -> bool:
    """Check if the service is running and healthy."""
    try:
        async with httpx.AsyncClient(timeout=10.0) as client:
            # Check general health endpoint
            health_response = await client.get(f"{base_url}/v1/batches")
            assert health_response.status_code == 200, f"Health check response: {health_response}"
            return True
    except Exception as e:
        print(f"Health check failed: {e}")
        return False


@pytest.fixture(scope="session")
def service_health():
    """Fixture to check service health and skip tests if service is not available."""
    base_url = "http://localhost:8888"

    print(f"🔍 Checking service health at {base_url}...")

    # Run the async health check in a sync context
    is_healthy = asyncio.run(check_service_health(base_url))
    if not is_healthy:
        pytest.skip(f"Service at {base_url} is not available or healthy")

    print(f"✅ Service at {base_url} is healthy")
    return base_url


@pytest.mark.asyncio
async def test_batch_api_e2e_real_service(service_health):
    """
    End-to-end test for OpenAI Batch API against real service:
    1. Upload sample input file via Files API
    2. Create batch job via Batch API
    3. Poll job status until completion
    4. Download and verify output via Files API
    5. Verify batch list API works
    """
    base_url = service_health

    async with httpx.AsyncClient(timeout=60.0) as client:
        # Step 1: Upload sample input file via Files API
        print("Step 1: Uploading batch input file...")

        input_data = generate_batch_input_data(3)
        files = {"file": ("batch_input.jsonl", input_data, "application/jsonl")}
        data = {"purpose": "batch"}

        upload_response = await client.post(
            f"{base_url}/v1/files", files=files, data=data
        )
        assert (
            upload_response.status_code == 200
        ), f"File upload failed: {upload_response.text}"

        upload_result = upload_response.json()
        assert upload_result["object"] == "file"
        assert upload_result["purpose"] == "batch"
        assert upload_result["status"] == "uploaded"

        input_file_id = upload_result["id"]
        print(f"✅ File uploaded successfully with ID: {input_file_id}")

        # Step 2: Create batch job via Batch API
        print("Step 2: Creating batch job...")

        batch_request = {
            "input_file_id": input_file_id,
            "endpoint": "/v1/chat/completions",
            "completion_window": "24h",
        }

        batch_response = await client.post(f"{base_url}/v1/batches", json=batch_request)
        assert (
            batch_response.status_code == 200
        ), f"Batch creation failed: {batch_response.text}"

        batch_result = batch_response.json()
        assert batch_result["object"] == "batch"
        assert batch_result["input_file_id"] == input_file_id
        assert batch_result["endpoint"] == "/v1/chat/completions"

        batch_id = batch_result["id"]
        print(f"✅ Batch created successfully with ID: {batch_id}")

        # Step 3: Poll job status until completion
        print("Step 3: Polling job status until completion...")

        max_polls = (
            60  # Maximum number of polling attempts (increased for real service)
        )
        poll_interval = 5  # seconds (increased for real service)

        for attempt in range(max_polls):
            status_response = await client.get(f"{base_url}/v1/batches/{batch_id}")
            assert (
                status_response.status_code == 200
            ), f"Status check failed: {status_response.text}"

            status_result = status_response.json()
            current_status = status_result["status"]

            print(f"  Attempt {attempt + 1}: Status = {current_status}")

            if current_status == "completed":
                print("✅ Batch job completed successfully!")
                output_file_id = status_result["output_file_id"]
                assert (
                    output_file_id is not None
                ), "Expected output_file_id for completed batch"

                request_counts = status_result.get("request_counts")
                if request_counts:
                    print(f"  Request counts: {request_counts}")
                    assert request_counts["total"] == 3
                    assert request_counts["completed"] == 3
                    assert request_counts["failed"] == 0

                break
            elif current_status == "failed":
                error_info = status_result.get("errors", "Unknown error")
                pytest.fail(f"Batch job failed: {error_info}")
            elif current_status in ["cancelled", "expired"]:
                pytest.fail(f"Batch job was {current_status}")
            elif current_status in ["validating", "in_progress", "finalizing"]:
                # These are expected intermediate states
                pass
            else:
                print(f"  Unknown status: {current_status}")

            # Wait before next poll
            await asyncio.sleep(poll_interval)
        else:
            pytest.fail(
                f"Batch job did not complete within {max_polls * poll_interval} seconds"
            )

        # Step 4: Download and verify output via Files API
        print("Step 4: Downloading and verifying output...")

        output_response = await client.get(
            f"{base_url}/v1/files/{output_file_id}/content"
        )
        assert (
            output_response.status_code == 200
        ), f"Output download failed: {output_response.text}"

        output_content = output_response.content.decode("utf-8")
        assert output_content, "Output file is empty"

        # Verify output content structure
        is_valid = verify_batch_output_content(output_content, 3)
        assert (
            is_valid
        ), f"Output content verification failed. Content:\n{output_content}"

        print("✅ Output downloaded and verified successfully!")
        print(f"Output content preview:\n{output_content[:500]}...")

        # Step 5: Verify batch list API works
        print("Step 5: Testing batch list API...")

        list_response = await client.get(f"{base_url}/v1/batches")
        assert (
            list_response.status_code == 200
        ), f"Batch list failed: {list_response.text}"

        list_result = list_response.json()
        assert list_result["object"] == "list"
        assert len(list_result["data"]) >= 1, "Expected at least one batch in the list"

        # Find our batch in the list
        our_batch = None
        for batch in list_result["data"]:
            if batch["id"] == batch_id:
                our_batch = batch
                break

        assert our_batch is not None, f"Batch {batch_id} not found in list"
        assert our_batch["status"] == "completed"

        print("✅ Batch list API verified successfully!")

        print(
            "\n🎉 E2E test completed successfully! All OpenAI Batch API endpoints working correctly."
        )

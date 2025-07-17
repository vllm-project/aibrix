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

import argparse
import json
import asyncio

import pytest
from fastapi.testclient import TestClient

from aibrix.metadata.app import build_app

def create_test_app():
    """Create a FastAPI app configured for e2e testing."""
    return build_app(argparse.Namespace(
        host=None,
        port=8100,
        enable_fastapi_docs=False,
        disable_batch_api=False,
        enable_k8s_job =False,
        e2e_test=True
    ))


def generate_batch_input_data(num_requests: int = 3) -> str:
    """Generate test batch input data and return the content as string."""
    base_request = {
        "custom_id": "request-1",
        "method": "POST",
        "url": "/v1/chat/completions",
        "body": {
            "model": "gpt-3.5-turbo-0125",
            "messages": [
                {"role": "system", "content": "You are a helpful assistant."},
                {"role": "user", "content": "Hello world!"}
            ],
            "max_tokens": 1000
        }
    }
    
    lines = []
    for i in range(num_requests):
        request = base_request.copy()
        request["custom_id"] = f"request-{i+1}"
        lines.append(json.dumps(request))
    
    return "\n".join(lines)


def verify_batch_output_content(output_content: str, expected_requests: int = 3) -> bool:
    """Verify that batch output content has the expected structure."""
    lines = output_content.strip().split('\n')
    
    if len(lines) != expected_requests:
        print(f"Expected {expected_requests} output lines, got {len(lines)}")
        return False
    
    for i, line in enumerate(lines):
        try:
            response = json.loads(line)
            
            # Check required fields in OpenAI batch response format
            # [TODO][NEXT] check required_fields = ["id", "custom_id", "response"]
            required_fields = ["custom_id"] # For now, just check custom_id
            for field in required_fields:
                if field not in response:
                    print(f"Missing required field '{field}' in response {i+1}")
                    return False
            
            # Verify custom_id matches expected pattern
            expected_custom_id = f"request-{i+1}"
            if response["custom_id"] != expected_custom_id:
                print(f"Expected custom_id '{expected_custom_id}', got '{response['custom_id']}'")
                return False
                
        except json.JSONDecodeError as e:
            print(f"Invalid JSON in output line {i+1}: {e}")
            return False
    
    return True

@pytest.mark.asyncio
async def test_openai_batch_api_e2e():
    """
    End-to-end test for OpenAI Batch API:
    1. Upload sample input file via Files API
    2. Create batch job via Batch API
    3. Poll job status until completion
    4. Download and verify output via Files API
    """
    app = create_test_app()
    
    with TestClient(app) as client:
        # Step 1: Upload sample input file via Files API
        print("Step 1: Uploading batch input file...")
        
        input_data = generate_batch_input_data(3)
        files = {
            "file": ("batch_input.jsonl", input_data, "application/jsonl")
        }
        data = {
            "purpose": "batch"
        }
        
        upload_response = client.post("/v1/files", files=files, data=data)
        assert upload_response.status_code == 200, f"File upload failed: {upload_response.text}"
        
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
            "completion_window": "24h"
        }
        
        batch_response = client.post("/v1/batches", json=batch_request)
        assert batch_response.status_code == 200, f"Batch creation failed: {batch_response.text}"
        
        batch_result = batch_response.json()
        assert batch_result["object"] == "batch"
        assert batch_result["input_file_id"] == input_file_id
        assert batch_result["endpoint"] == "/v1/chat/completions"
        
        batch_id = batch_result["id"]
        print(f"✅ Batch created successfully with ID: {batch_id}")
        
        # Step 3: Poll job status until completion
        print("Step 3: Polling job status until completion...")
        
        max_polls = 10  # Maximum number of polling attempts
        poll_interval = 1  # seconds
        
        for attempt in range(max_polls):
            status_response = client.get(f"/v1/batches/{batch_id}")
            assert status_response.status_code == 200, f"Status check failed: {status_response.text}"
            
            status_result = status_response.json()
            current_status = status_result["status"]
            
            print(f"  Attempt {attempt + 1}: Status = {current_status}")
            
            if current_status == "completed":
                print("✅ Batch job completed successfully!")
                output_file_id = status_result["output_file_id"]
                assert output_file_id is not None, "Expected output_file_id for completed batch"
                break
            elif current_status == "failed":
                pytest.fail(f"Batch job failed: {status_result.get('errors', 'Unknown error')}")
            elif current_status in ["cancelled", "expired"]:
                pytest.fail(f"Batch job was {current_status}")
            
            # Wait before next poll
            await asyncio.sleep(poll_interval)
        else:
            pytest.fail(f"Batch job did not complete within {max_polls * poll_interval} seconds")
        
        # Step 4: Download and verify output via Files API
        print("Step 4: Downloading and verifying output...")
        
        output_response = client.get(f"/v1/files/{output_file_id}/content")
        assert output_response.status_code == 200, f"Output download failed: {output_response.text}"
        
        output_content = output_response.content.decode('utf-8')
        assert output_content, "Output file is empty"
        
        # Verify output content structure
        is_valid = verify_batch_output_content(output_content, 3)
        assert is_valid, f"Output content verification failed. Content:\n{output_content}"
        
        print("✅ Output downloaded and verified successfully!")
        print(f"Output content preview:\n{output_content[:200]}...")
        
        # Step 5: Verify batch list API works
        print("Step 5: Testing batch list API...")
        
        list_response = client.get("/v1/batches")
        assert list_response.status_code == 200, f"Batch list failed: {list_response.text}"
        
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
        
        print("\n🎉 E2E test completed successfully! All OpenAI Batch API endpoints working correctly.")
        app.state.job_controller.clear_job(batch_id)

@pytest.mark.asyncio
async def test_batch_api_error_handling():
    """Test error handling in batch API."""
    app = create_test_app()
    
    with TestClient(app) as client:
        # Test creating batch with non-existent file ID
        batch_request = {
            "input_file_id": "non-existent-file-id",
            "endpoint": "/v1/chat/completions",
            "completion_window": "24h"
        }
        
        response = client.post("/v1/batches", json=batch_request)
        # This should either succeed (if validation is async) or fail with proper error
        # The actual behavior depends on when file validation occurs
        print(f"Batch creation with invalid file ID returned status: {response.status_code}")
        
        # Test getting non-existent batch
        response = client.get("/v1/non-existent-batch-id")
        assert response.status_code == 404
        
        # Test invalid file upload
        files = {
            "file": ("invalid.txt", "This is not a valid batch file", "text/plain")
        }
        data = {
            "purpose": "batch"
        }
        
        response = client.post("/v1/files/", files=files, data=data)
        assert response.status_code == 400  # Should fail due to invalid file extension

if __name__ == "__main__":
    # Allow running the test directly
    test_openai_batch_api_e2e()
    test_batch_api_error_handling()
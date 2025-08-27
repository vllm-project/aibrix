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

import asyncio
import json
import os
import tempfile
from pathlib import Path

import pytest

from aibrix.batch.constant import EXPIRE_INTERVAL
from aibrix.batch.driver import BatchDriver
from aibrix.batch.job_entity import BatchJobState


def generate_input_data(num_requests, local_file):
    input_name = Path(os.path.dirname(__file__)) / "sample_job_input.jsonl"
    data = None
    with open(input_name, "r") as file:
        for line in file.readlines():
            data = json.loads(line)
            break

    # In the following tests, we use this custom_id
    # to check if the read and write are exactly the same.
    with open(local_file, "w") as file:
        for i in range(num_requests):
            data["custom_id"] = i
            file.write(json.dumps(data) + "\n")


@pytest.mark.asyncio
async def test_batch_driver_job_creation():
    """Test basic BatchDriver operations without async scheduling."""
    driver = BatchDriver()

    # Test that driver is properly initialized
    assert driver is not None
    assert driver.job_manager is not None

    # Create temporary input file
    with tempfile.NamedTemporaryFile(
        mode="w", suffix=".jsonl", delete=False
    ) as temp_file:
        temp_path = temp_file.name
        generate_input_data(5, temp_path)

    try:
        # Test upload
        upload_id = driver.upload_batch_data(temp_path)
        assert upload_id is not None
        print(f"Upload ID: {upload_id} (type: {type(upload_id)})")

        # Test job creation using job_manager directly
        job_id = await driver.job_manager.create_job(
            session_id="test-session",
            input_file_id=str(upload_id),
            api_endpoint="/v1/chat/completions",
            completion_window="24h",
            meta_data={},
        )
        assert job_id is not None
        print(f"Created job_id: {job_id}")

        # Test status retrieval
        job = driver.job_manager.get_job(job_id)
        assert job is not None
        print(f"Job status: {job.status.state}")
        assert job.status.state == BatchJobState.CREATED

        # Test cleanup
        driver.clear_job(job_id)
    finally:
        # Shutdown driver
        await driver.close()

        # Clean up temporary file
        Path(temp_path).unlink(missing_ok=True)


@pytest.mark.asyncio
async def test_batch_driver_integration():
    """
    Integration test for the batch driver workflow.
    Tests job creation, scheduling, execution, and result retrieval.
    """
    # Initialize driver without job_entity_manager (use local job management)
    driver = BatchDriver()

    # Create temporary input file
    with tempfile.NamedTemporaryFile(
        mode="w", suffix=".jsonl", delete=False
    ) as temp_file:
        temp_path = temp_file.name
        generate_input_data(10, temp_path)

    try:
        # 1. Upload batch data and verify it's stored locally
        upload_id = driver.upload_batch_data(temp_path)
        assert upload_id is not None
        print(f"Upload ID: {upload_id}")

        # 2. Create job and verify initial state
        job_id = await driver.job_manager.create_job(
            session_id="test-session-integration",
            input_file_id=str(upload_id),
            api_endpoint="/v1/chat/completions",
            completion_window="24h",
            meta_data={},
        )
        assert job_id is not None
        print(f"Created job_id: {job_id}")

        job = driver.job_manager.get_job(job_id)
        assert job is not None
        print(f"Initial status: {job.status.state}")
        assert job.status.state == BatchJobState.CREATED

        # 3. Wait for job to be scheduled and start processing
        await asyncio.sleep(5 * EXPIRE_INTERVAL)
        job = driver.job_manager.get_job(job_id)
        assert job is not None
        print(f"Status after scheduling: {job.status.state}")
        assert job.status.state == BatchJobState.IN_PROGRESS
        assert job.status.output_file_id is not None

        # 4. Wait for job to complete
        await asyncio.sleep(6 * EXPIRE_INTERVAL)
        job = driver.job_manager.get_job(job_id)
        assert job is not None
        print(f"Final status: {job.status.state}")
        assert job.status.state == BatchJobState.COMPLETED
        assert job.status.output_file_id is not None

        # 5. Retrieve results and verify they exist
        results = driver.retrieve_job_result(job.status.output_file_id)
        assert results is not None
        assert len(results) == 10  # Should match num_requests
        print(f"Retrieved {len(results)} results")

        # 6. Verify results content
        for i, req_result in enumerate(results):
            print(f"Result {i}: {req_result}")
            assert req_result is not None

        # 7. Clean up the job
        driver.clear_job(job_id)
        print(f"Job {job_id} cleaned up")

    finally:
        # Shutdown driver
        await driver.close()

        # Clean up temporary file
        Path(temp_path).unlink(missing_ok=True)

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

import aibrix.batch.constant as constant
from aibrix.batch.driver import BatchDriver
from aibrix.batch.job_entity import BatchJobErrorCode, BatchJobState, BatchJobStatus
from aibrix.storage import StorageType

constant.EXPIRE_INTERVAL = 0.1


def generate_input_data(num_requests, local_file):
    input_name = Path(os.path.dirname(__file__)) / "testdata" / "sample_job_input.jsonl"
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
    driver = BatchDriver(
        storage_type=StorageType.LOCAL, metastore_type=StorageType.LOCAL
    )
    await driver.start()

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
        upload_id = await driver.upload_job_data(temp_path)
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
        job = await driver.job_manager.get_job(job_id)
        assert job is not None
        print(f"Job status: {job.status.state}")
        assert job.status.state == BatchJobState.CREATED

        # Test cleanup
        await driver.clear_job(job_id)
    finally:
        # Shutdown driver
        await driver.stop()

        # Clean up temporary file
        Path(temp_path).unlink(missing_ok=True)


@pytest.mark.asyncio
async def test_batch_driver_integration():
    """
    Integration test for the batch driver workflow.
    Tests job creation, scheduling, execution, and result retrieval.
    """
    # Initialize driver without job_entity_manager (use local job management)
    driver = BatchDriver(
        storage_type=StorageType.LOCAL, metastore_type=StorageType.LOCAL
    )
    await driver.start()

    # Create temporary input file
    with tempfile.NamedTemporaryFile(
        mode="w", suffix=".jsonl", delete=False
    ) as temp_file:
        temp_path = temp_file.name
        generate_input_data(10, temp_path)

    try:
        # 1. Upload batch data and verify it's stored locally
        upload_id = await driver.upload_job_data(temp_path)
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

        job = await driver.job_manager.get_job(job_id)
        assert job is not None
        print(f"Initial status: {job.status.state}")
        assert job.status.state == BatchJobState.CREATED

        # 3. Wait for job to be scheduled and start processing
        await asyncio.sleep(3 * constant.EXPIRE_INTERVAL)
        job = await driver.job_manager.get_job(job_id)
        assert job is not None
        print(f"Status after scheduling: {job.status.state}")
        assert job.status.state == BatchJobState.IN_PROGRESS
        assert job.status.output_file_id is not None
        assert job.status.error_file_id is not None

        # 4. Wait for job to complete
        while True:
            await asyncio.sleep(1 * constant.EXPIRE_INTERVAL)
            job = await driver.job_manager.get_job(job_id)
            assert job is not None
            print(f"Progressing: {job.status.state}")
            if job.status.finished:
                break

        print(f"Final status: {job.status.state}")
        assert job.status.state == BatchJobState.FINALIZED
        assert job.status.completed
        assert job.status.output_file_id is not None
        assert job.status.error_file_id is not None

        # 5. Retrieve results and verify they exist
        results = await driver.retrieve_job_result(job.status.output_file_id)
        assert results is not None
        assert len(results) == 10  # Should match num_requests
        print(f"Retrieved {len(results)} results")

        # 6. Verify results content
        for i, req_result in enumerate(results):
            print(f"Result {i}: {req_result}")
            assert req_result is not None

        # 7. Clean up the job
        await driver.clear_job(job_id)
        print(f"Job {job_id} cleaned up")

    finally:
        # Shutdown driver
        await driver.stop()

        # Clean up temporary file
        Path(temp_path).unlink(missing_ok=True)


@pytest.mark.asyncio
async def test_batch_driver_resuming():
    """
    Integration test for the batch driver workflow.
    Tests job creation, scheduling, execution, and result retrieval.
    """
    # Initialize driver without job_entity_manager (use local job management)
    driver = BatchDriver(
        storage_type=StorageType.LOCAL, metastore_type=StorageType.LOCAL
    )
    await driver.start()

    # Create temporary input file
    with tempfile.NamedTemporaryFile(
        mode="w", suffix=".jsonl", delete=False
    ) as temp_file:
        temp_path = temp_file.name
        generate_input_data(10, temp_path)

    try:
        # 1. Upload batch data and verify it's stored locally
        upload_id = await driver.upload_job_data(temp_path)
        assert upload_id is not None
        print(f"Upload ID: {upload_id}")

        # 2. Create job and verify initial state
        job_id = await driver.job_manager.create_job(
            session_id="test-session-integration",
            input_file_id=str(upload_id),
            api_endpoint="/v1/chat/completions",
            completion_window="24h",
            meta_data={},
            initial_state=BatchJobState.IN_PROGRESS,
        )
        assert job_id is not None
        print(f"Created job_id: {job_id}")

        job = await driver.job_manager.get_job(job_id)
        assert job is not None
        print(f"Initial status: {job.status.state}")
        assert job.status.state == BatchJobState.IN_PROGRESS

        # 3. Wait for job to be scheduled and start processing
        await asyncio.sleep(3 * constant.EXPIRE_INTERVAL)
        job = await driver.job_manager.get_job(job_id)
        assert job is not None
        print(f"Status after scheduling: {job.status.state}")
        assert job.status.state == BatchJobState.IN_PROGRESS
        assert job.status.output_file_id is not None
        assert job.status.error_file_id is not None

        # 4. Wait for job to complete
        while True:
            await asyncio.sleep(1 * constant.EXPIRE_INTERVAL)
            job = await driver.job_manager.get_job(job_id)
            assert job is not None
            print(f"Progressing: {job.status.state}")
            if job.status.finished:
                break

        print(f"Final status: {job.status.state}")
        assert job.status.state == BatchJobState.FINALIZED
        assert job.status.completed
        assert job.status.output_file_id is not None
        assert job.status.error_file_id is not None

        # 5. Retrieve results and verify they exist
        results = await driver.retrieve_job_result(job.status.output_file_id)
        assert results is not None
        assert len(results) == 10  # Should match num_requests
        print(f"Retrieved {len(results)} results")

        # 6. Verify results content
        for i, req_result in enumerate(results):
            print(f"Result {i}: {req_result}")
            assert req_result is not None

        # 7. Clean up the job
        await driver.clear_job(job_id)
        print(f"Job {job_id} cleaned up")

    finally:
        # Shutdown driver
        await driver.stop()

        # Clean up temporary file
        Path(temp_path).unlink(missing_ok=True)


@pytest.mark.asyncio
async def test_batch_driver_validation_failed() -> None:
    """
    Integration test for the batch driver workflow.
    Tests job creation, scheduling, and validation failed.
    """
    # Initialize driver without job_entity_manager (use local job management)
    driver = BatchDriver(
        storage_type=StorageType.LOCAL, metastore_type=StorageType.LOCAL
    )
    await driver.start()

    try:
        # 1. Create job with non-exist upload_id
        job_id = await driver.job_manager.create_job(
            session_id="test-session-integration",
            input_file_id="non-exist-upload-id",
            api_endpoint="/v1/chat/completions",
            completion_window="24h",
            meta_data={},
        )
        assert job_id is not None
        print(f"Created job_id: {job_id}")

        job = await driver.job_manager.get_job(job_id)
        assert job is not None
        print(f"Initial status: {job.status.state}")
        assert job.status.state == BatchJobState.CREATED

        # 2. Wait for job to be scheduled and validation failed
        await asyncio.sleep(5 * constant.EXPIRE_INTERVAL)
        job = await driver.job_manager.get_job(job_id)
        assert job is not None

        job_status: BatchJobStatus = job.status
        print(f"Status after scheduling: {job_status.state}")
        assert job_status.state == BatchJobState.FINALIZED
        assert job_status.failed
        assert job_status.output_file_id is None
        assert job_status.error_file_id is None
        assert job_status.errors is not None
        assert len(job_status.errors) > 0
        assert job_status.errors[0].code == BatchJobErrorCode.INVALID_INPUT_FILE

        # 7. Clean up the job
        await driver.clear_job(job_id)
        print(f"Job {job_id} cleaned up")

    finally:
        # Shutdown driver
        await driver.stop()


@pytest.mark.asyncio
async def test_batch_driver_stop_raises_exception_with_fail_after_n_requests():
    """Test that BatchDriver.stop() raises RuntimeError when jobs with fail_after_n_requests exist."""

    driver = BatchDriver(
        storage_type=StorageType.LOCAL,
        metastore_type=StorageType.LOCAL,
        stand_alone=False,
    )

    # Create a temporary file for job input
    temp_file_descriptor, temp_path = tempfile.mkstemp(suffix=".jsonl")
    try:
        # Generate test data
        generate_input_data(3, temp_path)

        # Start the driver
        await driver.start()

        # 1. Upload input data
        input_file_id = await driver.upload_job_data(temp_path)
        print(f"Input file uploaded: {input_file_id}")

        # 2. Create a job with fail_after_n_requests opts
        from aibrix.batch.job_entity import BatchJobSpec

        job_spec = BatchJobSpec.from_strings(
            input_file_id=input_file_id,
            endpoint="/v1/chat/completions",
            completion_window="24h",
            metadata={"test": "metadata"},
            opts={
                constant.BATCH_OPTS_FAIL_AFTER_N_REQUESTS: "2"
            },  # This should trigger the stop() exception
        )

        job_id = await driver.job_manager.create_job_with_spec(
            session_id="test-session", job_spec=job_spec
        )
        print(f"Job created with fail_after_n_requests: {job_id}")

        # 3. Verify the job was created successfully
        job = await driver.job_manager.get_job(job_id)
        assert job is not None
        assert job.spec.opts is not None
        assert constant.BATCH_OPTS_FAIL_AFTER_N_REQUESTS in job.spec.opts

        # 4. Wait for job to complete
        waited = 0
        while True:
            await asyncio.sleep(1 * constant.EXPIRE_INTERVAL)
            waited += 1
            job = await driver.job_manager.get_job(job_id)
            assert job is not None
            print(f"Progressing: {job.status.state}")
            if job.status.finished:
                break
            if waited > 10:
                assert False, "job timeout"

        print(f"Final status: {job.status.state}")
        assert job.status.state == BatchJobState.FINALIZED
        assert job.status.failed
        assert job.status.output_file_id is not None
        assert job.status.error_file_id is not None

        # wait for exception reach driver.
        await asyncio.sleep(3.0)

        # 5. Attempt to stop the driver - this should raise RuntimeError
        with pytest.raises(RuntimeError, match="Artificial failure.*"):
            await driver.stop()

        print(
            "✅ BatchDriver.stop() correctly raised RuntimeError for job with fail_after_n_requests"
        )

        # 6. Clean up the job to allow proper shutdown
        await driver.clear_job(job_id)

    finally:
        # Clean up temporary file
        Path(temp_path).unlink(missing_ok=True)

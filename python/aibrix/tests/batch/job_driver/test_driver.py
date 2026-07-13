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
import tempfile
from pathlib import Path

import pytest

import aibrix.batch.constant as constant
from aibrix.batch.client import NoopEndpointSource
from aibrix.batch.driver import BatchDriver
from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobErrorCode,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
)
from aibrix.batch.state import JobEntityManager
from aibrix.context import InfrastructureContext
from aibrix.storage import StorageType

constant.EXPIRE_INTERVAL = 0.1
# Idle poll is now a separate knob from the expiry cadence; speed it up too so
# the scheduler picks jobs up promptly in these timing-sensitive tests.
constant.SCHEDULE_IDLE_INTERVAL = 0.1


class LifecycleEntityManager(JobEntityManager):
    def __init__(self):
        super().__init__()
        self.started = False
        self.stopped = False

    async def start(self) -> None:
        self.started = True

    async def stop(self) -> None:
        self.stopped = True

    async def submit_job(
        self,
        session_id: str,
        job_spec: BatchJobSpec,
        request_count: int = 0,
    ):
        raise NotImplementedError

    async def update_job_ready(self, job: BatchJob):
        raise NotImplementedError

    async def update_job_status(self, job: BatchJob):
        raise NotImplementedError

    async def cancel_job(self, job: BatchJob):
        raise NotImplementedError

    async def delete_job(self, job: BatchJob):
        raise NotImplementedError

    async def get_job(self, job_id: str, force_reload: bool = False):
        return None

    async def list_jobs(
        self, after=None, limit=JobEntityManager.DEFAULT_JOB_PAGE_LIMIT
    ):
        return []


def generate_input_data(num_requests, local_file):
    input_name = Path(__file__).parents[1] / "testdata" / "sample_job_input.jsonl"
    data = None
    with open(input_name, "r") as file:
        for line in file.readlines():
            data = json.loads(line)
            break

    # In the following tests, we use this custom_id
    # to check if the read and write are exactly the same.
    with open(local_file, "w") as file:
        for i in range(num_requests):
            data["custom_id"] = str(i)
            file.write(json.dumps(data) + "\n")


@pytest.mark.asyncio
async def test_batch_driver_job_creation():
    """Test basic BatchDriver operations without async scheduling."""
    driver = BatchDriver(
        context=InfrastructureContext(),
        storage_type=StorageType.LOCAL,
        metastore_type=StorageType.LOCAL,
        endpoint_source=NoopEndpointSource(delay=constant.EXPIRE_INTERVAL),
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
async def test_batch_driver_starts_and_stops_entity_manager():
    entity_manager = LifecycleEntityManager()
    driver = BatchDriver(
        context=InfrastructureContext(),
        job_entity_manager=entity_manager,
        storage_type=StorageType.LOCAL,
        metastore_type=StorageType.LOCAL,
    )

    await driver.start()
    await driver.stop()

    assert entity_manager.started is True
    assert entity_manager.stopped is True


@pytest.mark.asyncio
async def test_batch_driver_integration():
    """
    Integration test for the batch driver workflow.
    Tests job creation, scheduling, execution, and result retrieval.
    """
    # Initialize driver without job_entity_manager (use local job management)
    driver = BatchDriver(
        context=InfrastructureContext(),
        storage_type=StorageType.LOCAL,
        metastore_type=StorageType.LOCAL,
        endpoint_source=NoopEndpointSource(delay=constant.EXPIRE_INTERVAL),
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
        context=InfrastructureContext(),
        storage_type=StorageType.LOCAL,
        metastore_type=StorageType.LOCAL,
        endpoint_source=NoopEndpointSource(delay=constant.EXPIRE_INTERVAL),
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
        context=InfrastructureContext(),
        storage_type=StorageType.LOCAL,
        metastore_type=StorageType.LOCAL,
        endpoint_source=NoopEndpointSource(delay=constant.EXPIRE_INTERVAL),
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
async def test_batch_driver_survives_job_failure_with_fail_after_n_requests():
    """A single job failure (fail_after_n_requests) must finalize the job as
    failed without tearing down the scheduler loop, so BatchDriver.stop()
    completes cleanly."""

    driver = BatchDriver(
        context=InfrastructureContext(),
        storage_type=StorageType.LOCAL,
        metastore_type=StorageType.LOCAL,
        endpoint_source=NoopEndpointSource(delay=constant.EXPIRE_INTERVAL),
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
            },  # This triggers an artificial job failure
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

        # wait for the swallowed failure to settle in the scheduler loop.
        await asyncio.sleep(3.0)

        # 5. Stop the driver - the single job failure was swallowed by the
        # scheduler loop, so stop() must complete cleanly without raising.
        await driver.stop()

        print("✅ BatchDriver.stop() completed cleanly after a single job failure")

        # 6. Clean up the job to allow proper shutdown
        await driver.clear_job(job_id)

    finally:
        # Clean up temporary file
        Path(temp_path).unlink(missing_ok=True)

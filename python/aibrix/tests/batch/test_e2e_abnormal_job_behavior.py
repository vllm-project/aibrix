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
End-to-end tests for abnormal job behavior scenarios.

These tests cover various failure modes and edge cases in the batch job lifecycle:
- Validation failures
- Processing failures during different stages
- Job cancellation at various points
- Job expiration scenarios
"""

import asyncio
import json
from typing import Any, Dict, List, Optional, Tuple, TypeVar, Union
from unittest.mock import patch

import pytest
from fastapi.testclient import TestClient

import aibrix.batch.constant as constant
from aibrix.batch.job_entity import (
    BatchJobError,
    BatchJobErrorCode,
    BatchJobSpec,
    BatchJobState,
)
from aibrix.batch.job_manager import JobManager, JobMetaInfo

T = TypeVar("T")


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
                {"role": "user", "content": "Hello world!"},
            ],
            "max_tokens": 1000,
        },
    }

    lines = []
    for i in range(num_requests):
        request = base_request.copy()
        request["custom_id"] = f"request-{i+1}"
        lines.append(json.dumps(request))

    return "\n".join(lines)


async def wait_for_status(
    client: TestClient,
    batch_id: str,
    expected_status: str,
    extra_expected_fields: Optional[Union[str, List[str]]] = None,
    max_polls: int = 20,
    poll_interval: float = 0.5,
) -> Dict[str, Any]:
    """Wait for batch job to reach expected status."""
    for attempt in range(max_polls):
        response = client.get(f"/v1/batches/{batch_id}")
        assert response.status_code == 200, f"Status check failed: {response.text}"

        result = response.json()
        current_status = result["status"]

        if current_status == expected_status:
            if extra_expected_fields is None:
                return result
            elif (
                isinstance(extra_expected_fields, str)
                and result[extra_expected_fields] is not None
            ):
                return result
            else:
                satisfied = True
                # Extra fields must all present.
                for field in extra_expected_fields:
                    if result[extra_expected_fields] is None:
                        satisfied = False
                if satisfied:
                    return result
        elif current_status in ["failed", "cancelled", "expired", "completed"]:
            # Terminal states
            if current_status != expected_status:
                return result  # Return actual final state

        await asyncio.sleep(poll_interval)

    # Return last known status if timeout
    return result


def validate_batch_response(
    response: Dict[str, Any],
    *,
    # Required fields (default checked for not None)
    expected_id: Optional[str] = None,
    expected_object: str = "batch",
    expected_endpoint: Optional[str] = None,
    expected_input_file_id: Optional[str] = None,
    expected_completion_window: str = "24h",
    expected_status: Optional[str] = None,
    # Optional fields (default checked for None)
    expected_errors: Optional[Union[str, List[str]]] = None,  # List of error codes
    expected_output_file_id: Optional[bool] = None,
    expected_error_file_id: Optional[bool] = None,
    expected_in_progress_at: Optional[bool] = None,
    expected_finalizing_at: Optional[bool] = None,
    expected_completed_at: Optional[bool] = None,
    expected_failed_at: Optional[bool] = None,
    expected_expired_at: Optional[bool] = None,
    expected_cancelling_at: Optional[bool] = None,
    expected_cancelled_at: Optional[bool] = None,
    expected_request_counts: Optional[Union[bool, Dict[str, int]]] = None,
    expected_metadata: Optional[Dict[str, str]] = None,
) -> None:
    """Validate batch response fields according to BatchResponse schema.

    This function validates all fields as defined in api/v1/batch.py::BatchResponse.

    Args:
        response: The batch response dict to validate
        expected_*: Expected values or presence checks for each field
                   - For required fields: actual expected value or None to skip check
                   - For optional fields: True = not None, False = None, None = skip check
    """
    # Verify all BatchResponse fields are present
    required_fields = [
        "id",
        "object",
        "endpoint",
        "input_file_id",
        "completion_window",
        "status",
        "created_at",
        "expires_at",
    ]
    optional_fields: List[Tuple[str, Any, type]] = [
        # Pass bool to skip equal test.
        ("errors", True if expected_errors else False, Dict),
        ("output_file_id", expected_output_file_id, str),
        ("error_file_id", expected_error_file_id, str),
        ("in_progress_at", expected_in_progress_at, int),
        ("finalizing_at", expected_finalizing_at, int),
        ("completed_at", expected_completed_at, int),
        ("failed_at", expected_failed_at, int),
        ("expired_at", expected_expired_at, int),
        ("cancelling_at", expected_cancelling_at, int),
        ("cancelled_at", expected_cancelled_at, int),
        ("request_counts", expected_request_counts, Dict),
        ("metadata", expected_metadata, Dict),
    ]

    # Check that all expected fields exist
    all_fields = required_fields + [field[0] for field in optional_fields]
    for field in all_fields:
        assert field in response, f"Field '{field}' missing from response"

    # Validate required fields
    if expected_id is not None:
        assert (
            response["id"] == expected_id
        ), f"Expected id '{expected_id}', got '{response['id']}'"
    else:
        assert response["id"] is not None, "Required field 'id' should not be None"

    assert (
        response["object"] == expected_object
    ), f"Expected object '{expected_object}', got '{response['object']}'"

    if expected_endpoint is not None:
        assert (
            response["endpoint"] == expected_endpoint
        ), f"Expected endpoint '{expected_endpoint}', got '{response['endpoint']}'"
    else:
        assert (
            response["endpoint"] is not None
        ), "Required field 'endpoint' should not be None"

    if expected_input_file_id is not None:
        assert (
            response["input_file_id"] == expected_input_file_id
        ), f"Expected input_file_id '{expected_input_file_id}', got '{response['input_file_id']}'"
    else:
        assert (
            response["input_file_id"] is not None
        ), "Required field 'input_file_id' should not be None"

    assert (
        response["completion_window"] == expected_completion_window
    ), f"Expected completion_window '{expected_completion_window}', got '{response['completion_window']}'"

    if expected_status is not None:
        assert (
            response["status"] == expected_status
        ), f"Expected status '{expected_status}', got '{response['status']}'"
    else:
        assert (
            response["status"] is not None
        ), "Required field 'status' should not be None"

    # created_at is required and should not be None
    assert (
        response["created_at"] is not None
    ), "Required field 'created_at' should not be None"
    assert isinstance(
        response["created_at"], int
    ), "Expected 'created_at' to be unix timestamp (int)"

    # created_at is required and should not be None
    assert (
        response["expires_at"] is not None
    ), "Required field 'expires_at' should not be None"
    assert isinstance(
        response["expires_at"], int
    ), "Expected 'expires_at' to be unix timestamp (int)"
    if expected_completion_window == "24h":
        assert (
            response["expires_at"] == response["created_at"] + 86400
        ), "Expected 'expires_at' to be 'created_at' + 86400"

    # Validate optional fields
    def check_optional_field(
        field_name: str, expected_value: Optional[T], expected_type: type
    ):
        if expected_value is None or expected_value is False:
            assert response[field_name] is None, f"Expected '{field_name}' to be None"
        elif expected_value is True and expected_type is not bool:
            assert (
                response[field_name] is not None
            ), f"Required field '{field_name}' should not be None"
            assert isinstance(
                response[field_name], expected_type
            ), f"Expected '{field_name}' to be type ({expected_type})"
        else:
            assert (
                response[field_name] == expected_value
            ), f"Expected {field_name} '{expected_value}', got '{response[field_name]}'"

    for field_name, expected_value, expected_type in optional_fields:
        check_optional_field(field_name, expected_value, expected_type)

    # Check non-timestamp optional fields
    if expected_errors is not None:
        if isinstance(expected_errors, str):
            expected_errors = [expected_errors]
        assert isinstance(response["errors"], dict), "Expected 'errors' to be dict"
        assert "data" in response["errors"], "Expected 'errors.data' field"
        assert isinstance(
            response["errors"]["data"], list
        ), "Expected 'errors.data' to be list"
        assert len(response["errors"]["data"]) > 0
        errors = {}
        for error in response["errors"]["data"]:
            assert "code" in error, f"No 'code' in error:{error}"
            assert "message" in error, f"No 'message' in error:{error}"
            errors[error["code"]] = error["message"]
        for err_code in expected_errors:
            assert err_code in errors


class FailingJobManager(JobManager):
    """JobManager that can be configured to fail at specific stages."""

    def __init__(
        self,
        fail_validation: bool = False,
        fail_during_processing: bool = False,
        fail_during_finalizing: bool = False,
        stall_validation: Optional[float] = None,
        stall_cancelling: Optional[float] = None,
        fail_after_n_requests: Optional[int] = None,
        expiration: Optional[int] = None,
    ):
        super().__init__()
        self.fail_validation = fail_validation
        self.fail_during_processing = fail_during_processing
        self.fail_during_finalizing = fail_during_finalizing
        self.stall_validation = stall_validation
        self.stall_cancelling = stall_cancelling
        self.fail_after_n_requests = fail_after_n_requests
        self.expiration = expiration
        self._processed_requests = 0

    async def validate_job(self, meta_data: JobMetaInfo):
        """Override to simulate validation failures during job execution start."""
        if self.stall_validation is not None:
            # Prolong validation duration to allow cancellation during validation
            await asyncio.sleep(self.stall_validation)

        if self.fail_validation:
            # Mark job as failed with authentication error
            raise BatchJobError(
                code=BatchJobErrorCode.AUTHENTICATION_ERROR,
                message="Simulated authentication failure",
                param="authentication",
            )

        return await super().validate_job(meta_data)

    async def cancel_job(self, job_id: str) -> bool:
        if self.stall_cancelling is not None:
            await asyncio.sleep(self.stall_cancelling)

        return await super().cancel_job(job_id)

    async def create_job_with_spec(
        self,
        session_id: str,
        job_spec: BatchJobSpec,
        timeout: float = 30.0,
        initial_state: BatchJobState = BatchJobState.CREATED,
    ) -> str:
        """Override create_job to inject fail_after_n_requests in opts."""
        # Create job spec with opts if needed
        if self.fail_after_n_requests is not None:
            opts = job_spec.opts or {}
            opts[constant.BATCH_OPTS_FAIL_AFTER_N_REQUESTS] = str(
                self.fail_after_n_requests
            )
            job_spec.opts = opts
        if self.expiration is not None:
            job_spec.completion_window = self.expiration

        return await super().create_job_with_spec(
            session_id, job_spec, timeout, initial_state
        )


@pytest.mark.asyncio
async def test_job_validation_failure(test_app):
    """Test case 1: Create job, failure during validation."""
    print("Test 1: Job validation failure scenario")

    with TestClient(test_app) as client:
        # Step 1: Skip uploading file

        # Step 2: Inject the FailingJobManager to fail during validation
        original_manager = test_app.state.batch_driver.job_manager
        failing_manager = FailingJobManager(fail_validation=True)
        await test_app.state.batch_driver.run_coroutine(
            failing_manager.set_job_entity_manager(original_manager._job_entity_manager)
        )
        test_app.state.batch_driver._job_manager = failing_manager

        try:
            # Step 3: Create batch job
            batch_request = {
                "input_file_id": "invalid_input_file_id",
                "endpoint": "/v1/chat/completions",
                "completion_window": "24h",
            }

            batch_response = client.post("/v1/batches", json=batch_request)
            assert batch_response.status_code == 200
            batch_id = batch_response.json()["id"]

            # Step 4: Wait for validation to fail
            final_status = await wait_for_status(client, batch_id, "failed")

            # Step 5: Verify failed status using comprehensive validation
            validate_batch_response(
                final_status,
                expected_status="failed",
                expected_failed_at=True,
                expected_errors="authentication_error",
            )

            print("✅ Validation failure test completed successfully")

            await test_app.state.batch_driver.clear_job(batch_id)
        finally:
            # Restore original manager
            test_app.state.batch_driver._job_manager = original_manager


@pytest.mark.asyncio
async def test_job_processing_failure(test_app):
    """Test case 2: Create job, failure during in progress using k8s job worker with fail_after metadata."""
    print("Test 2: Job processing failure scenario using worker fail_after metadata")

    with TestClient(test_app) as client:
        # Step 1: Upload input file
        input_data = generate_batch_input_data(
            10
        )  # Generate more tasks for failure after 3 backoffs.
        files = {"file": ("test_input.jsonl", input_data, "application/jsonl")}
        data = {"purpose": "batch"}

        upload_response = client.post("/v1/files", files=files, data=data)
        assert upload_response.status_code == 200
        input_file_id = upload_response.json()["id"]

        # Step 2: Inject FailingJobManager to add fail_after metadata during job creation
        original_manager = test_app.state.batch_driver.job_manager
        failing_manager = FailingJobManager(
            fail_after_n_requests=1
        )  # Fail after processing 1 request
        await test_app.state.batch_driver.run_coroutine(
            failing_manager.set_job_entity_manager(original_manager._job_entity_manager)
        )
        test_app.state.batch_driver._job_manager = failing_manager

        try:
            # Step 3: Create batch job with failing manager that injects fail_after metadata
            batch_request = {
                "input_file_id": input_file_id,
                "endpoint": "/v1/chat/completions",
                "completion_window": "24h",
            }

            batch_response = client.post("/v1/batches", json=batch_request)
            assert batch_response.status_code == 200
            batch_id = batch_response.json()["id"]

            # Step 4: Wait for job to start processing
            await wait_for_status(client, batch_id, "in_progress", max_polls=10)
            # Step 5: Wait for finalization to complete
            final_status = await wait_for_status(
                client, batch_id, "failed", max_polls=60, poll_interval=1.0
            )  # Wait longer for job retries

            # Step 7: Verify failed status using comprehensive validation
            validate_batch_response(
                final_status,
                expected_status="failed",
                expected_endpoint="/v1/chat/completions",
                expected_in_progress_at=True,  # Should have started processing
                expected_failed_at=True,  # Should have failure timestamp
                expected_finalizing_at=True,  # Should have reached finalizing stage
                expected_output_file_id=True,
                expected_error_file_id=True,
                expected_request_counts={
                    "total": 3,
                    "completed": 3,
                    "failed": 0,
                },
            )

            print(
                "✅ Processing failure test with worker fail_after completed successfully"
            )

            await test_app.state.batch_driver.clear_job(batch_id)
        finally:
            # Restore original manager
            test_app.state.batch_driver._job_manager = original_manager


@pytest.mark.asyncio
async def test_job_finalizing_failure(test_app):
    """Test case 3: Create job, failure during finalizing."""
    print("Test 3: Job finalizing failure scenario")

    with TestClient(test_app) as client:
        # Step 1: Upload input file
        input_data = generate_batch_input_data(2)
        files = {"file": ("test_input.jsonl", input_data, "application/jsonl")}
        data = {"purpose": "batch"}

        upload_response = client.post("/v1/files", files=files, data=data)
        assert upload_response.status_code == 200
        input_file_id = upload_response.json()["id"]

        # Step 2: Inject the exception to the finalize_job_output_data to fail during finalizing
        finalizing_patcher = patch(
            "aibrix.batch.storage.adapter.BatchStorageAdapter.finalize_job_output_data"
        )
        mock_finalize = finalizing_patcher.start()
        mock_finalize.side_effect = Exception("Simulated finalization failure")

        try:
            # Step 3: Create batch job
            batch_request = {
                "input_file_id": input_file_id,
                "endpoint": "/v1/chat/completions",
                "completion_window": "24h",
            }

            batch_response = client.post("/v1/batches", json=batch_request)
            assert batch_response.status_code == 200
            batch_id = batch_response.json()["id"]

            await wait_for_status(client, batch_id, "in_progress")

            await asyncio.sleep(3)

            # Step 4: Wait for job to reach final status
            final_status = await wait_for_status(client, batch_id, "failed")

            # Step 6: Verify failed status due to finalization error using comprehensive validation
            validate_batch_response(
                final_status,
                expected_status="failed",
                expected_in_progress_at=True,  # Should have started processing
                expected_failed_at=True,  # Should have failure timestamp
                expected_finalizing_at=True,  # Should have reached finalizing stage
                expected_errors=BatchJobErrorCode.FINALIZING_ERROR,
                expected_output_file_id=True,  # May or may not have output file
                expected_error_file_id=True,  # May or may not have error file
                expected_request_counts=False,  # May or may not have counts
            )

            print("✅ Finalizing failure test completed successfully")

            await test_app.state.batch_driver.clear_job(batch_id)
        finally:
            finalizing_patcher.stop()


@pytest.mark.asyncio
async def test_job_cancellation_in_validation(test_app):
    """Test case 4: Create job, cancel during validation."""
    print("Test 4: Job cancellation during validation scenario")

    with TestClient(test_app) as client:
        # Step 1: Skip uploading file

        # Step 2: Inject the FailingJobManager to fail during validation
        original_manager = test_app.state.batch_driver.job_manager
        failing_manager = FailingJobManager(stall_validation=3.0)
        await test_app.state.batch_driver.run_coroutine(
            failing_manager.set_job_entity_manager(original_manager._job_entity_manager)
        )
        test_app.state.batch_driver._job_manager = failing_manager

        try:
            # Step 3: Create batch job
            batch_request = {
                "input_file_id": "invalid_input_file_id",
                "endpoint": "/v1/chat/completions",
                "completion_window": "24h",
            }

            batch_response = client.post("/v1/batches", json=batch_request)
            assert batch_response.status_code == 200
            batch_id = batch_response.json()["id"]

            # Step 4: Cancel job during processing
            cancel_response = client.post(f"/v1/batches/{batch_id}/cancel")
            assert cancel_response.status_code == 200

            # Step 5: Wait for validation to fail
            final_status = await wait_for_status(client, batch_id, "cancelled")

            # Step 6: Verify failed status using comprehensive validation
            validate_batch_response(
                final_status,
                expected_status="cancelled",
                expected_cancelling_at=True,
                expected_cancelled_at=True,
            )

            print("✅ Validation failure test completed successfully")

            await test_app.state.batch_driver.clear_job(batch_id)
        finally:
            # Restore original manager
            test_app.state.batch_driver._job_manager = original_manager


@pytest.mark.asyncio
async def test_job_cancellation_in_progress_before_preparation(test_app):
    """Test case 5: Create job, cancel during in progress, finalizing, validate finalized result."""
    print("Test 5: Job cancellation during processing scenario")

    with TestClient(test_app) as client:
        # Step 1: Upload input file
        input_data = generate_batch_input_data(
            10
        )  # More requests for longer processing
        files = {"file": ("test_input.jsonl", input_data, "application/jsonl")}
        data = {"purpose": "batch"}

        upload_response = client.post("/v1/files", files=files, data=data)
        assert upload_response.status_code == 200
        input_file_id = upload_response.json()["id"]

        # Step 2: Create batch job
        batch_request = {
            "input_file_id": input_file_id,
            "endpoint": "/v1/chat/completions",
            "completion_window": "24h",
        }

        batch_response = client.post("/v1/batches", json=batch_request)
        assert batch_response.status_code == 200
        batch_id = batch_response.json()["id"]

        try:
            # Step 3: Wait for job to start processing, ASAP
            await wait_for_status(
                client, batch_id, "in_progress", max_polls=10, poll_interval=0.1
            )

            # Step 4: Cancel job during processing
            cancel_response = client.post(f"/v1/batches/{batch_id}/cancel")
            assert cancel_response.status_code == 200

            # Step 5: Wait for cancellation and finalization
            final_status = await wait_for_status(
                client, batch_id, "cancelled", max_polls=20
            )

            # Step 6: Verify cancelled status using comprehensive validation
            validate_batch_response(
                final_status,
                expected_status="cancelled",
                expected_endpoint="/v1/chat/completions",
                expected_in_progress_at=True,  # Should have started processing
                expected_cancelled_at=True,  # Should have cancellation timestamp
                expected_cancelling_at=True,  # Should have cancelling start timestamp
                expected_finalizing_at=True,  # Should have finalizing timestamp
            )

            print("✅ Processing cancellation test completed successfully")

            await test_app.state.batch_driver.clear_job(batch_id)
        finally:
            pass


@pytest.mark.asyncio
async def test_job_cancellation_in_progress(test_app):
    """Test case 5: Create job, cancel during in progress, finalizing, validate finalized result."""
    print("Test 5: Job cancellation during processing scenario")

    with TestClient(test_app) as client:
        # Step 1: Upload input file
        input_data = generate_batch_input_data(
            10
        )  # More requests for longer processing
        files = {"file": ("test_input.jsonl", input_data, "application/jsonl")}
        data = {"purpose": "batch"}

        upload_response = client.post("/v1/files", files=files, data=data)
        assert upload_response.status_code == 200
        input_file_id = upload_response.json()["id"]

        # Step 2: Create batch job
        batch_request = {
            "input_file_id": input_file_id,
            "endpoint": "/v1/chat/completions",
            "completion_window": "24h",
        }

        batch_response = client.post("/v1/batches", json=batch_request)
        assert batch_response.status_code == 200
        batch_id = batch_response.json()["id"]

        try:
            # Step 3: Wait for job to start processing, ASAP
            await wait_for_status(
                client,
                batch_id,
                "in_progress",
                "output_file_id",
                max_polls=10,
                poll_interval=1,
            )

            await asyncio.sleep(3.0)  # Wait a second for job to make some progress.

            # Step 4: Cancel job during processing
            cancel_response = client.post(f"/v1/batches/{batch_id}/cancel")
            assert cancel_response.status_code == 200

            # Step 5: Wait for cancellation and finalization
            final_status = await wait_for_status(
                client, batch_id, "cancelled", max_polls=20
            )

            # Step 6: Verify cancelled status using comprehensive validation
            validate_batch_response(
                final_status,
                expected_status="cancelled",
                expected_endpoint="/v1/chat/completions",
                expected_in_progress_at=True,  # Should have started processing
                expected_cancelled_at=True,  # Should have cancellation timestamp
                expected_cancelling_at=True,  # Should have cancelling start timestamp
                expected_finalizing_at=True,  # Should have finalizing timestamp
                expected_output_file_id=True,
                expected_error_file_id=True,
                expected_request_counts=True,  # May or may not have counts
            )

            print("✅ Processing cancellation test completed successfully")
        finally:
            await test_app.state.batch_driver.clear_job(batch_id)


@pytest.mark.asyncio
async def test_job_cancellation_in_finalizing(test_app):
    """Test case 6: Create job, cancel during finalizing, report completed."""
    print("Test 6: Job cancellation during finalizing (reports completed) scenario")

    with TestClient(test_app) as client:
        # Step 1: Upload input file
        input_data = generate_batch_input_data(
            10
        )  # Larger file for taking some time to finalizing
        files = {"file": ("test_input.jsonl", input_data, "application/jsonl")}
        data = {"purpose": "batch"}

        upload_response = client.post("/v1/files", files=files, data=data)
        assert upload_response.status_code == 200
        input_file_id = upload_response.json()["id"]

        # Step 2: Inject the FailingJobManager to stall cancellation execution
        original_manager = test_app.state.batch_driver.job_manager
        failing_manager = FailingJobManager(stall_cancelling=2.0)
        await test_app.state.batch_driver.run_coroutine(
            failing_manager.set_job_entity_manager(original_manager._job_entity_manager)
        )
        test_app.state.batch_driver._job_manager = failing_manager

        # Step 3: Create batch job
        batch_request = {
            "input_file_id": input_file_id,
            "endpoint": "/v1/chat/completions",
            "completion_window": "24h",
        }

        batch_response = client.post("/v1/batches", json=batch_request)
        assert batch_response.status_code == 200
        batch_id = batch_response.json()["id"]

        try:
            # Step 4: Wait for processing to reach finalizing
            await wait_for_status(client, batch_id, "finalizing", max_polls=20)

            # Step 5: Try to cancel during or after processing
            client.post(f"/v1/batches/{batch_id}/cancel")
            # Note: Cancel may succeed or be ignored if already finalizing

            # Step 6: Wait for final status
            final_status = await wait_for_status(
                client, batch_id, ["completed", "cancelled"], max_polls=10
            )

            # Step 6: Verify final status using comprehensive validation
            validate_batch_response(
                final_status,
                expected_status="completed",
                expected_endpoint="/v1/chat/completions",
                expected_in_progress_at=True,  # Should have started processing
                expected_completed_at=True,  # Should have completion timestamp
                expected_finalizing_at=True,  # Should have reached finalizing
                expected_output_file_id=True,  # Should have output file
                expected_error_file_id=True,  # Should have error file
                expected_request_counts=True,  # Should have request counts
            )
            print("  Job completed by ignoring cancellation")
        finally:
            # Restore original manager
            test_app.state.batch_driver._job_manager = original_manager
            await test_app.state.batch_driver.clear_job(batch_id)


@pytest.mark.asyncio
async def test_job_expiration_during_validation(test_app):
    """Test case 7: Create job, set expire to 1min and prevent validation, expired and report."""
    print("Test 7: Job expiration during validation scenario")

    pytest.skip("No batch scheduling enabled for k8s job")

    with TestClient(test_app) as client:
        # Step 1: Upload input file
        input_data = generate_batch_input_data(2)
        files = {"file": ("test_input.jsonl", input_data, "application/jsonl")}
        data = {"purpose": "batch"}

        upload_response = client.post("/v1/files", files=files, data=data)
        assert upload_response.status_code == 200
        input_file_id = upload_response.json()["id"]

        # Step 2: Mock job manager to prevent validation
        original_manager = test_app.state.batch_driver.job_manager
        failing_manager = FailingJobManager(prevent_validation=True)
        await test_app.state.batch_driver.run_coroutine(
            failing_manager.set_job_entity_manager(original_manager._job_entity_manager)
        )
        test_app.state.batch_driver.job_manager = failing_manager

        try:
            # Step 3: Create batch job with very short completion window
            batch_request = {
                "input_file_id": input_file_id,
                "endpoint": "/v1/chat/completions",
                "completion_window": "60",  # 60 seconds for test
            }

            # Patch the completion window to be very short for testing
            with patch(
                "aibrix.batch.constant.EXPIRE_INTERVAL", 0.1
            ):  # Check every 100ms
                batch_response = client.post("/v1/batches", json=batch_request)
                assert batch_response.status_code == 200
                batch_id = batch_response.json()["id"]

                # Step 4: Wait longer than completion window for expiration
                await asyncio.sleep(2)  # Wait for expiration to trigger

                # Step 5: Check that job expired
                final_status = await wait_for_status(
                    client, batch_id, "expired", max_polls=10
                )

                # Step 6: Verify expired status using comprehensive validation
                validate_batch_response(
                    final_status,
                    expected_status="expired",
                    expected_endpoint="/v1/chat/completions",
                    expected_in_progress_at=False,  # Should be None (expired before processing)
                    expected_expired_at=True,  # Should have expiration timestamp
                    expected_failed_at=False,  # Should be None (expired, not failed)
                    expected_completed_at=False,  # Should be None (expired, not completed)
                    expected_cancelled_at=False,  # Should be None (not cancelled)
                    expected_cancelling_at=False,  # Should be None (not cancelled)
                    expected_finalizing_at=False,  # Should be None (expired before finalizing)
                    expected_errors=False,  # Should be None (expired during validation)
                    expected_output_file_id=False,  # Should be None (expired before processing)
                    expected_error_file_id=False,  # Should be None (expired before processing)
                    expected_request_counts=False,  # Should be None (expired before processing)
                )

                print("✅ Validation expiration test completed successfully")

        finally:
            # Restore original manager
            test_app.state.batch_driver.job_manager = original_manager
            await test_app.state.batch_driver.clear_job(batch_id)


@pytest.mark.asyncio
async def test_job_expiration_during_processing(test_app):
    """Test case 8: Create job, set expire to 1min, expired during in progress, finalizing, validate result."""
    print("Test 8: Job expiration during processing scenario")

    with TestClient(test_app) as client:
        # Step 1: Upload input file
        input_data = generate_batch_input_data(3)
        files = {"file": ("test_input.jsonl", input_data, "application/jsonl")}
        data = {"purpose": "batch"}

        upload_response = client.post("/v1/files", files=files, data=data)
        assert upload_response.status_code == 200
        input_file_id = upload_response.json()["id"]

        # Step 2: Inject FailingJobManager to add fail_after metadata during job creation
        original_manager = test_app.state.batch_driver.job_manager
        failing_manager = FailingJobManager(expiration=2)  # Expired after 2 seconds
        await test_app.state.batch_driver.run_coroutine(
            failing_manager.set_job_entity_manager(original_manager._job_entity_manager)
        )
        test_app.state.batch_driver._job_manager = failing_manager

        try:
            # Step 3: Create batch job with failing manager that injects fail_after metadata
            batch_request = {
                "input_file_id": input_file_id,
                "endpoint": "/v1/chat/completions",
                "completion_window": "24h",
            }

            batch_response = client.post("/v1/batches", json=batch_request)
            assert batch_response.status_code == 200
            batch_id = batch_response.json()["id"]

            # Step 4: Wait for job to start processing
            await wait_for_status(client, batch_id, "in_progress", max_polls=10)
            # Step 5: Wait for finalization to complete
            final_status = await wait_for_status(
                client, batch_id, "failed", max_polls=60, poll_interval=1.0
            )  # Wait longer for job retries

            # Step 7: Verify failed status using comprehensive validation
            validate_batch_response(
                final_status,
                expected_status="expired",
                expected_endpoint="/v1/chat/completions",
                expected_completion_window="0h",  # overrided to 2s
                expected_in_progress_at=True,  # Should have started processing
                expected_expired_at=True,  # Should have failure timestamp
                expected_finalizing_at=True,  # Should have reached finalizing stage
                expected_output_file_id=True,  # May or may not have output file
                expected_error_file_id=True,  # May or may not have error file
            )

            print(
                "✅ Processing failure test with worker fail_after completed successfully"
            )

            await test_app.state.batch_driver.clear_job(batch_id)
        finally:
            # Restore original manager
            test_app.state.batch_driver._job_manager = original_manager


if __name__ == "__main__":
    # Allow running individual tests
    import sys

    if len(sys.argv) > 1:
        test_name = sys.argv[1]
        if hasattr(sys.modules[__name__], test_name):
            asyncio.run(getattr(sys.modules[__name__], test_name)())
    else:
        print("Available tests:")
        for name in dir(sys.modules[__name__]):
            if name.startswith("test_"):
                print(f"  {name}")

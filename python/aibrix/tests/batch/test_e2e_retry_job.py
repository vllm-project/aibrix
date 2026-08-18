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

"""End-to-end retry-finalize coverage."""

from unittest.mock import patch

import pytest

from aibrix.batch.job_entity import BatchJobErrorCode, ConditionType
from tests.batch.conftest import (
    create_batch_job,
    create_test_client,
    select_e2e_backends,
    upload_batch_input_file,
)
from tests.batch.test_e2e_abnormal_job_behavior import (
    backend_expect_runtime_teardown,
    capture_runtime_debug_state,
    run_follow_up_success_with_same_input,
    validate_batch_response_with_runtime_teardown,
    wait_for_status,
)


def pytest_generate_tests(metafunc):
    select_e2e_backends(metafunc, default_backend="local_metastore_job")


@pytest.mark.asyncio
async def test_retry_finalize_after_finalizing_failure(e2e_test_app, test_backend):
    """A batch that failed in finalization can be retried without reusing runtime."""
    print("Retry finalize: recover batch after finalizing failure")

    with create_test_client(e2e_test_app) as client:
        input_file_id = upload_batch_input_file(client, 2)
        debug_state = capture_runtime_debug_state(e2e_test_app, test_backend)
        storage_cls = e2e_test_app.state.storage.__class__
        batch_id = None
        part_timeout_injected = False
        observed_pre_retry_cleanup = False
        original_get_object = storage_cls.get_object

        async def fail_part_read_once(self, key, range_start=None, range_end=None):
            nonlocal part_timeout_injected, observed_pre_retry_cleanup
            is_multipart_part = key.startswith(".multipart/") and "/part_" in key
            if is_multipart_part and not part_timeout_injected:
                part_timeout_injected = True
                raise TimeoutError("Simulated fine-grained storage timeout")
            if (
                is_multipart_part
                and part_timeout_injected
                and not observed_pre_retry_cleanup
            ):
                persisted = await e2e_test_app.state.batch_driver.get_job(batch_id)
                assert persisted is not None
                assert persisted.status.state.value == "finalizing"
                assert persisted.status.finalizing_at is not None
                assert persisted.status.condition == ConditionType.COMPLETED
                assert persisted.status.failed_at is None
                assert all(
                    error.code != BatchJobErrorCode.FINALIZING_ERROR.value
                    for error in (persisted.status.errors or [])
                )
                observed_pre_retry_cleanup = True
            return await original_get_object(self, key, range_start, range_end)

        try:
            with patch.object(
                storage_cls,
                "get_object",
                new=fail_part_read_once,
            ):
                batch_id = create_batch_job(
                    client, input_file_id, test_backend=test_backend
                )

                await wait_for_status(client, batch_id, "in_progress")
                failed_status = await wait_for_status(
                    client, batch_id, "failed", max_polls=60, poll_interval=0.5
                )

                validate_batch_response_with_runtime_teardown(
                    failed_status,
                    e2e_test_app=e2e_test_app,
                    test_backend=test_backend,
                    batch_id=batch_id,
                    debug_state=debug_state,
                    expect_runtime_teardown=backend_expect_runtime_teardown(
                        test_backend
                    ),
                    expected_status="failed",
                    expected_endpoint="/v1/chat/completions",
                    expected_input_file_id=input_file_id,
                    expected_in_progress_at=True,
                    expected_finalizing_at=True,
                    expected_completed_at=False,
                    expected_failed_at=True,
                    expected_errors=BatchJobErrorCode.FINALIZING_ERROR,
                    expected_output_file_id=True,
                    expected_error_file_id=False,
                    expected_request_counts={"total": 2, "completed": 2, "failed": 0},
                )

                retry_response = client.post(f"/v1/batches/{batch_id}/retry_finalize")
                assert retry_response.status_code == 200, retry_response.text
                retry_body = retry_response.json()
                assert retry_body["status"] == "finalizing"

                final_status = await wait_for_status(
                    client, batch_id, "completed", max_polls=20, poll_interval=0.5
                )
                assert observed_pre_retry_cleanup
                validate_batch_response_with_runtime_teardown(
                    final_status,
                    e2e_test_app=e2e_test_app,
                    test_backend=test_backend,
                    batch_id=batch_id,
                    debug_state=debug_state,
                    expect_runtime_teardown=backend_expect_runtime_teardown(
                        test_backend
                    ),
                    expected_status="completed",
                    expected_endpoint="/v1/chat/completions",
                    expected_input_file_id=input_file_id,
                    expected_in_progress_at=True,
                    expected_finalizing_at=True,
                    expected_completed_at=True,
                    expected_failed_at=False,
                    expected_errors=False,
                    expected_output_file_id=True,
                    expected_error_file_id=False,
                    expected_request_counts={"total": 2, "completed": 2, "failed": 0},
                )

            await run_follow_up_success_with_same_input(
                client, e2e_test_app, test_backend, input_file_id, 2
            )
        finally:
            if batch_id is not None:
                await e2e_test_app.state.batch_driver.clear_job(batch_id)

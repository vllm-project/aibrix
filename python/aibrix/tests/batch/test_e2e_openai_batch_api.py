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
from typing import Any, cast

import pytest
from fastapi.testclient import TestClient

import aibrix.batch.constant as batch_constant
import aibrix.batch.driver as batch_driver_module
from aibrix import envs
from aibrix.batch.client.engine import DispatchEngine
from aibrix.storage import StorageType
from tests.batch.conftest import (
    MockMetadataStore,
    build_batch_request,
    build_e2e_test_app,
    create_test_app,
    e2e_batch_request_kwargs,
    select_e2e_backends,
    upload_batch_input_file,
)

_ORIGINAL_DISPATCH_SEND_ONE: Any = None
_PATCHED_DISPATCH_DELAY_SECONDS = 0.0


def verify_batch_output_content(output_content: str, expected_requests: int) -> bool:
    lines = output_content.strip().split("\n")

    if len(lines) != expected_requests:
        print(f"Expected {expected_requests} output lines, got {len(lines)}")
        return False

    for i, line in enumerate(lines):
        try:
            output = json.loads(line)

            for field in ["id", "custom_id", "response"]:
                if field not in output:
                    print(f"Missing required field '{field}' in response {i + 1}")
                    return False

            expected_custom_id = f"request-{i + 1}"
            if output["custom_id"] != expected_custom_id:
                print(
                    f"Expected custom_id '{expected_custom_id}', got '{output['custom_id']}'"
                )
                return False

            response = output["response"]
            for field in ["status_code", "request_id", "body"]:
                if field not in response:
                    print(
                        f"Missing required field 'response.{field}' in response {i + 1}"
                    )
                    return False

            body = response["body"]
            if "model" not in body:
                print(
                    f"Missing required field 'response.body.model' in response {i + 1}"
                )
                return False

        except json.JSONDecodeError as e:
            print(f"Invalid JSON in output line {i + 1}: {e}")
            return False

    return True


def pytest_generate_tests(metafunc):
    select_e2e_backends(
        metafunc,
        [
            "local_job_using_deployment",
        ],
    )


def backend_batch_request(
    test_backend: str,
    input_file_id: str,
    endpoint: str = "/v1/chat/completions",
) -> dict[str, Any]:
    request = build_batch_request(
        input_file_id,
        endpoint,
        **e2e_batch_request_kwargs(test_backend),
    )
    aibrix = cast(dict[str, Any] | None, request.get("aibrix"))
    if aibrix is not None and "planner_decision" in aibrix:
        planner_decision = cast(dict[str, Any], aibrix.pop("planner_decision"))
        aibrix["runtime"] = {"target": "Kubernetes"}
        aibrix["resource_allocation"] = {
            "provision_id": planner_decision["provision_id"],
            "provision_resource_deadline": planner_decision[
                "provision_resource_deadline"
            ],
            "resource_details": [
                {
                    "endpoint_cluster": "cluster-a",
                    "gpu_type": "H100",
                    "replica": 1,
                }
            ],
        }
    return request


def backend_request_count(test_backend: str) -> int:
    return 10 if test_backend == "k8s_job" else 3


def backend_max_polls(test_backend: str) -> int:
    return 120 if test_backend == "k8s_job" else 20


async def wait_for_completed_batch(
    client: TestClient,
    batch_id: str,
    *,
    max_polls: int,
    poll_interval: float = 1.0,
    inspect_status=None,
) -> dict[str, Any]:
    for _ in range(max_polls):
        status_response = client.get(f"/v1/batches/{batch_id}")
        assert status_response.status_code == 200, status_response.text
        status_result = status_response.json()
        if inspect_status is not None:
            inspect_status(status_result)
        if status_result["status"] == "completed":
            return status_result
        if status_result["status"] in {"failed", "cancelled", "expired"}:
            raise AssertionError(
                f"Batch job {status_result['status']}: {status_result}"
            )
        await asyncio.sleep(poll_interval)
    raise AssertionError(f"Batch job {batch_id} did not complete within polling budget")


async def wait_for_batch_status(
    client: TestClient,
    batch_id: str,
    *,
    expected_statuses: set[str],
    max_polls: int,
    poll_interval: float = 0.5,
) -> dict[str, Any]:
    last_status: dict[str, Any] | None = None
    for _ in range(max_polls):
        status_response = client.get(f"/v1/batches/{batch_id}")
        assert status_response.status_code == 200, status_response.text
        current_status = cast(dict[str, Any], status_response.json())
        last_status = current_status
        if current_status["status"] in expected_statuses:
            return current_status
        if current_status["status"] in {"failed", "cancelled", "expired"}:
            raise AssertionError(
                f"Batch job {current_status['status']}: {current_status}"
            )
        await asyncio.sleep(poll_interval)
    assert last_status is not None
    raise AssertionError(
        f"Batch job {batch_id} did not reach one of {sorted(expected_statuses)}; "
        f"last status: {last_status}"
    )


def assert_completed_batch(
    status_result: dict[str, Any], expected_requests: int
) -> str:
    assert status_result["status"] == "completed"
    output_file_id = status_result["output_file_id"]
    assert output_file_id is not None
    request_counts = status_result.get("request_counts")
    assert request_counts is not None
    assert request_counts["total"] == expected_requests
    assert request_counts["completed"] == expected_requests
    assert request_counts["failed"] == 0
    return output_file_id


async def download_and_verify_output(
    client: TestClient, output_file_id: str, expected_requests: int
) -> str:
    output_response = client.get(f"/v1/files/{output_file_id}/content")
    assert output_response.status_code == 200, output_response.text
    output_content = output_response.content.decode("utf-8")
    assert output_content
    assert verify_batch_output_content(output_content, expected_requests)
    return output_content


def set_dispatch_delay(e2e_test_app, delay_seconds: float) -> float:
    global _ORIGINAL_DISPATCH_SEND_ONE, _PATCHED_DISPATCH_DELAY_SECONDS

    values = e2e_test_app.state.batch_driver._context.values
    original_delay = values.get("endpoint_source_delay_seconds", 0.0)
    values["endpoint_source_delay_seconds"] = delay_seconds

    original_send_one = _ORIGINAL_DISPATCH_SEND_ONE
    if original_send_one is None:
        _ORIGINAL_DISPATCH_SEND_ONE = DispatchEngine.send_one
        original_send_one = _ORIGINAL_DISPATCH_SEND_ONE

    if delay_seconds > 0:
        _PATCHED_DISPATCH_DELAY_SECONDS = delay_seconds
        if DispatchEngine.send_one is original_send_one:

            async def delayed_send_one(self, request):
                await asyncio.sleep(_PATCHED_DISPATCH_DELAY_SECONDS)
                return await original_send_one(self, request)

            cast(Any, DispatchEngine).send_one = delayed_send_one
    else:
        _PATCHED_DISPATCH_DELAY_SECONDS = 0.0
        cast(Any, DispatchEngine).send_one = original_send_one

    for owner in (
        e2e_test_app.state.batch_driver,
        getattr(e2e_test_app.state.batch_driver, "_batch_manager", None),
    ):
        if owner is None:
            continue
        endpoint_source = getattr(owner, "_endpoint_source", None)
        if endpoint_source is not None:
            channel = getattr(endpoint_source, "_channel", None)
            if channel is not None and hasattr(channel, "_delay"):
                channel._delay = delay_seconds
    return original_delay


@pytest.mark.asyncio
async def test_openai_batch_api_success_workflow(e2e_test_app, test_backend):
    app = e2e_test_app
    expected_requests = backend_request_count(test_backend)
    endpoint = "/v1/chat/completions"

    with TestClient(app) as client:
        input_file_id = upload_batch_input_file(
            client,
            num_requests=expected_requests,
            endpoint=endpoint,
            filename=f"{test_backend}-input.jsonl",
        )
        create_response = client.post(
            "/v1/batches",
            json=backend_batch_request(test_backend, input_file_id, endpoint),
        )
        assert create_response.status_code == 200, create_response.text
        batch_result = create_response.json()
        assert batch_result["input_file_id"] == input_file_id
        assert batch_result["endpoint"] == endpoint
        batch_id = batch_result["id"]

        try:
            completed_batch = await wait_for_completed_batch(
                client,
                batch_id,
                max_polls=backend_max_polls(test_backend),
            )
            output_file_id = assert_completed_batch(completed_batch, expected_requests)
            await download_and_verify_output(client, output_file_id, expected_requests)

            if test_backend == "k8s_job":
                assert isinstance(completed_batch["in_progress_at"], int)
                assert isinstance(completed_batch["finalizing_at"], int)
                assert isinstance(completed_batch["completed_at"], int)
        finally:
            await app.state.batch_driver.clear_job(batch_id)


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "endpoint",
    [
        "/v1/chat/completions",
        "/v1/completions",
        "/v1/embeddings",
        "/v1/rerank",
    ],
    ids=["chat_completions", "completions", "embeddings", "rerank"],
)
async def test_openai_batch_api_multi_endpoint(
    e2e_test_app, test_backend, endpoint: str
):
    if test_backend != "local_metastore_job":
        pytest.skip("Multi-endpoint coverage only applies to echo-backed workflows")

    app = e2e_test_app
    expected_requests = 3

    with TestClient(app) as client:
        input_file_id = upload_batch_input_file(
            client,
            num_requests=expected_requests,
            endpoint=endpoint,
            filename=f"{test_backend}-{endpoint.split('/')[-1]}.jsonl",
        )
        create_response = client.post(
            "/v1/batches",
            json=backend_batch_request(test_backend, input_file_id, endpoint),
        )
        assert create_response.status_code == 200, create_response.text
        batch_id = create_response.json()["id"]

        try:
            completed_batch = await wait_for_completed_batch(
                client,
                batch_id,
                max_polls=backend_max_polls(test_backend),
            )
            output_file_id = assert_completed_batch(completed_batch, expected_requests)
            await download_and_verify_output(client, output_file_id, expected_requests)
        finally:
            await app.state.batch_driver.clear_job(batch_id)


@pytest.mark.asyncio
async def test_openai_batch_api_respects_default_job_pool_size(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    monkeypatch.setenv("AIBRIX_BATCH_JOB_POOL_SIZE", "3")
    monkeypatch.setattr(envs, "BATCH_JOB_POOL_SIZE", 3)
    monkeypatch.setattr(batch_constant, "DEFAULT_JOB_POOL_SIZE", 3)
    monkeypatch.setattr(batch_driver_module, "DEFAULT_JOB_POOL_SIZE", 3)

    app = create_test_app(
        storage_type=StorageType.LOCAL,
        metastore_type=StorageType.LOCAL,
        dry_run=True,
    )
    app.state.metadata_store = MockMetadataStore()
    app.state.redis_client = None

    scheduler = app.state.batch_driver._scheduler
    assert scheduler is not None
    assert scheduler._pool_size == 3

    batch_ids: list[str] = []
    original_delay = set_dispatch_delay(app, 0.5)
    with TestClient(app) as client:
        try:
            for index in range(4):
                input_file_id = upload_batch_input_file(
                    client,
                    num_requests=2,
                    endpoint="/v1/chat/completions",
                    filename=f"pool-size-{index}.jsonl",
                )
                create_response = client.post(
                    "/v1/batches",
                    json=backend_batch_request(
                        "local_metastore_job",
                        input_file_id,
                        "/v1/chat/completions",
                    ),
                )
                assert create_response.status_code == 200, create_response.text
                batch_ids.append(create_response.json()["id"])

            observed_running_pool = 0
            for _ in range(40):
                running_pool_size = len(scheduler._job_execution_tasks)
                observed_running_pool = max(observed_running_pool, running_pool_size)
                if observed_running_pool >= 3:
                    break
                await asyncio.sleep(0.2)

            assert observed_running_pool >= 3

            for batch_id in batch_ids:
                completed_batch = await wait_for_completed_batch(
                    client,
                    batch_id,
                    max_polls=40,
                    poll_interval=0.5,
                )
                output_file_id = assert_completed_batch(completed_batch, 2)
                await download_and_verify_output(client, output_file_id, 2)
        finally:
            set_dispatch_delay(app, original_delay)
            for batch_id in batch_ids:
                await app.state.batch_driver.clear_job(batch_id)


@pytest.mark.asyncio
async def test_openai_batch_api_second_job_does_not_stay_validating_when_pool_has_capacity(
    request, tmp_path, monkeypatch
):
    monkeypatch.setenv("AIBRIX_BATCH_JOB_POOL_SIZE", "2")
    monkeypatch.setattr(envs, "BATCH_JOB_POOL_SIZE", 2)
    monkeypatch.setattr(batch_constant, "DEFAULT_JOB_POOL_SIZE", 2)
    monkeypatch.setattr(batch_driver_module, "DEFAULT_JOB_POOL_SIZE", 2)

    app = build_e2e_test_app(
        request,
        "local_metastore_job",
        tmp_path,
        monkeypatch,
    )

    scheduler = app.state.batch_driver._scheduler
    assert scheduler is not None
    assert scheduler._pool_size == 2

    batch_ids: list[str] = []
    original_delay = set_dispatch_delay(app, 0.5)
    with TestClient(app) as client:
        try:
            first_input_file_id = upload_batch_input_file(
                client,
                num_requests=2,
                endpoint="/v1/chat/completions",
                filename="pool-capacity-0.jsonl",
            )
            first_create_response = client.post(
                "/v1/batches",
                json=backend_batch_request(
                    "local_metastore_job",
                    first_input_file_id,
                    "/v1/chat/completions",
                ),
            )
            assert first_create_response.status_code == 200, first_create_response.text
            first_batch_id = first_create_response.json()["id"]
            batch_ids.append(first_batch_id)

            first_status = await wait_for_batch_status(
                client,
                first_batch_id,
                expected_statuses={"in_progress"},
                max_polls=30,
                poll_interval=0.2,
            )
            assert first_status["status"] == "in_progress"

            second_input_file_id = upload_batch_input_file(
                client,
                num_requests=2,
                endpoint="/v1/chat/completions",
                filename="pool-capacity-1.jsonl",
            )
            second_create_response = client.post(
                "/v1/batches",
                json=backend_batch_request(
                    "local_metastore_job",
                    second_input_file_id,
                    "/v1/chat/completions",
                ),
            )
            assert second_create_response.status_code == 200, (
                second_create_response.text
            )
            second_batch_id = second_create_response.json()["id"]
            batch_ids.append(second_batch_id)

            second_status: dict[str, Any] | None = None
            first_status_when_second_progresses: dict[str, Any] | None = None
            for _ in range(30):
                second_response = client.get(f"/v1/batches/{second_batch_id}")
                assert second_response.status_code == 200, second_response.text
                second_status = second_response.json()

                if second_status["status"] in {
                    "in_progress",
                    "finalizing",
                    "completed",
                }:
                    first_response = client.get(f"/v1/batches/{first_batch_id}")
                    assert first_response.status_code == 200, first_response.text
                    first_status_when_second_progresses = first_response.json()
                    break
                assert second_status["status"] == "scheduling", second_status
                await asyncio.sleep(0.2)

            assert second_status is not None
            assert second_status["status"] in {
                "in_progress",
                "finalizing",
                "completed",
            }, second_status
            assert first_status_when_second_progresses is not None
            assert first_status_when_second_progresses["status"] == "in_progress", (
                first_status_when_second_progresses
            )

            for batch_id in batch_ids:
                completed_batch = await wait_for_completed_batch(
                    client,
                    batch_id,
                    max_polls=60,
                    poll_interval=0.5,
                )
                output_file_id = assert_completed_batch(completed_batch, 2)
                output_response = client.get(f"/v1/files/{output_file_id}/content")
                assert output_response.status_code == 200, output_response.text
                output_lines = (
                    output_response.content.decode("utf-8").strip().splitlines()
                )
                assert len(output_lines) == 2
                for line in output_lines:
                    payload = json.loads(line)
                    assert payload["custom_id"].startswith("request-")
                    assert payload["response"]["status_code"] == 200
        finally:
            set_dispatch_delay(app, original_delay)
            for batch_id in batch_ids:
                await app.state.batch_driver.clear_job(batch_id)

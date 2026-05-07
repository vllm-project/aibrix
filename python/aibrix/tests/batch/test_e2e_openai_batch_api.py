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
import copy
import json
import os
import uuid
from typing import Any

import pytest
from fastapi.testclient import TestClient
from kubernetes import client as k8s_client

from aibrix.batch.job_entity import (
    AibrixMetadata,
    BatchJobEndpoint,
    BatchJobSpec,
    BatchProfileRef,
    CompletionWindow,
    ModelTemplateRef,
)
from aibrix.metadata.cache import RedisJobCache
from aibrix.storage import StorageType
from tests.batch.conftest import create_test_app

# ---- Multi-endpoint input data generators ----

# Sample request bodies for each supported batch endpoint
ENDPOINT_SAMPLE_BODIES = {
    "/v1/chat/completions": {
        "model": "gpt-3.5-turbo-0125",
        "messages": [
            {"role": "system", "content": "You are a helpful assistant."},
            {"role": "user", "content": "Hello world!"},
        ],
        "max_tokens": 1000,
    },
    "/v1/completions": {
        "model": "gpt-3.5-turbo-0125",
        "prompt": "Once upon a time",
        "max_tokens": 100,
    },
    "/v1/embeddings": {
        "model": "text-embedding-ada-002",
        "input": "The food was delicious and the waiter was friendly.",
    },
    "/v1/rerank": {
        "model": "reranker-v1",
        "query": "What is deep learning?",
        "documents": [
            "Deep learning is a subset of machine learning.",
            "The weather is nice today.",
            "Neural networks are inspired by the brain.",
        ],
    },
}


def generate_batch_input_data(
    num_requests: int = 3, endpoint: str = "/v1/chat/completions"
) -> str:
    """Generate test batch input data for any supported endpoint.

    Args:
        num_requests: Number of requests to generate
        endpoint: The API endpoint path (e.g., "/v1/chat/completions")

    Returns:
        JSONL string with batch requests
    """
    sample_body = ENDPOINT_SAMPLE_BODIES.get(endpoint)
    if sample_body is None:
        raise ValueError(
            f"No sample body defined for endpoint '{endpoint}'. "
            f"Supported: {list(ENDPOINT_SAMPLE_BODIES.keys())}"
        )

    lines = []
    for i in range(num_requests):
        request = {
            "custom_id": f"request-{i + 1}",
            "method": "POST",
            "url": endpoint,
            "body": copy.deepcopy(sample_body),
        }
        lines.append(json.dumps(request))

    return "\n".join(lines)


def build_batch_request(
    input_file_id: str,
    endpoint: str,
    *,
    aibrix_template: str | None = None,
    aibrix_profile: str | None = None,
    resource_type: str | None = None,
) -> dict[str, Any]:
    request: dict[str, Any] = {
        "input_file_id": input_file_id,
        "endpoint": endpoint,
        "completion_window": "24h",
    }
    aibrix: dict[str, Any] = {}
    if aibrix_template:
        aibrix["model_template"] = {"name": aibrix_template}
    if aibrix_profile:
        aibrix["profile"] = {"name": aibrix_profile}
    if resource_type:
        aibrix["planner_decision"] = {
            "provision_id": "reservation-1",
            "provision_resource_deadline": 3600,
            "resource_details": [
                {
                    "resource_type": resource_type,
                    "endpoint_cluster": "cluster-a",
                    "gpu_type": "H100",
                    "worker_num": 1,
                }
            ],
        }
    if aibrix:
        request["aibrix"] = aibrix
    return request


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
                    print(f"Missing required field '{field}' in response {i + 1}")
                    return False

            # Verify custom_id matches expected pattern
            expected_custom_id = f"request-{i + 1}"
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
                        f"Missing required field 'response.{field}' in response {i + 1}"
                    )
                    return False

            body = response["body"]
            required_fields = ["model"]  # For now, just check model
            for field in required_fields:
                if field not in body:
                    print(
                        f"Missing required field 'response.body.{field}' in response {i + 1}"
                    )
                    return False

        except json.JSONDecodeError as e:
            print(f"Invalid JSON in output line {i + 1}: {e}")
            return False

    return True


@pytest.fixture(scope="function")
def redis_deployment_test_app(
    test_s3_bucket,
    redis_config_available,
    ensure_job_rbac,
    template_configmaps,
    monkeypatch,
):
    monkeypatch.setattr(
        "aibrix.metadata.app.envs.STORAGE_REDIS_HOST",
        os.environ["REDIS_HOST"],
    )
    monkeypatch.setattr(
        "aibrix.metadata.app.envs.STORAGE_REDIS_PORT",
        int(os.environ.get("REDIS_PORT", "6379")),
    )
    monkeypatch.setattr(
        "aibrix.metadata.app.envs.STORAGE_REDIS_DB",
        int(os.environ.get("REDIS_DB", "0")),
    )
    monkeypatch.setattr(
        "aibrix.metadata.app.envs.STORAGE_REDIS_PASSWORD",
        os.environ.get("REDIS_PASSWORD", "unused-for-local-redis-test"),
    )
    monkeypatch.setattr(
        "aibrix.metadata.app.envs.DB_REDIS_PREFIX",
        f"batch-jobs-deployment-{uuid.uuid4().hex}",
    )
    monkeypatch.setattr(
        "aibrix.metadata.cache.redis.RedisJobCache._build_client",
        lambda self, host, port, db, password: __import__("redis").Redis(
            host=host,
            port=port,
            db=db,
            password=os.environ.get("REDIS_PASSWORD"),
            decode_responses=False,
        ),
    )
    monkeypatch.setenv("INFERENCE_ENGINE_ENDPOINT", "http://127.0.0.1:8000")
    return create_test_app(
        enable_redis_job=True,
        storage_type=StorageType.S3,
        metastore_type=StorageType.LOCAL,
        params={"bucket_name": test_s3_bucket},
        dry_run=False,
    )


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
        files = {"file": ("batch_input.jsonl", input_data, "application/jsonl")}
        data = {"purpose": "batch"}

        upload_response = client.post("/v1/files", files=files, data=data)
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

        batch_request = build_batch_request(input_file_id, "/v1/chat/completions")

        batch_response = client.post("/v1/batches", json=batch_request)
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

        max_polls = 10  # Maximum number of polling attempts
        poll_interval = 1  # seconds

        for attempt in range(max_polls):
            status_response = client.get(f"/v1/batches/{batch_id}")
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
                assert request_counts is not None
                assert request_counts["total"] == 3
                assert request_counts["completed"] == 3
                assert request_counts["failed"] == 0

                break
            elif current_status == "failed":
                pytest.fail(
                    f"Batch job failed: {status_result.get('errors', 'Unknown error')}"
                )
            elif current_status in ["cancelled", "expired"]:
                pytest.fail(f"Batch job was {current_status}")

            # Wait before next poll
            await asyncio.sleep(poll_interval)
        else:
            pytest.fail(
                f"Batch job did not complete within {max_polls * poll_interval} seconds"
            )

        # Step 4: Download and verify output via Files API
        print("Step 4: Downloading and verifying output...")

        output_response = client.get(f"/v1/files/{output_file_id}/content")
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
        print(f"Output content preview:\n{output_content[:200]}...")

        # Step 5: Verify batch list API works
        print("Step 5: Testing batch list API...")

        list_response = client.get("/v1/batches")
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
        await app.state.batch_driver.clear_job(batch_id)


@pytest.mark.asyncio
async def test_metadata_server_workflow_renders_worker_env_from_s3_and_cluster_redis(
    test_app,
):
    job_cache = test_app.state.batch_driver._job_entity_manager
    assert job_cache is not None

    spec = BatchJobSpec(
        input_file_id="file-input",
        endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
        completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
        aibrix=AibrixMetadata(
            model_template=ModelTemplateRef(name="mock-vllm"),
            profile=BatchProfileRef(name="unittest"),
        ),
    )

    manifest = job_cache._batch_job_spec_to_k8s_job(
        session_id="test-session",
        job_spec=spec,
        job_name="batch-env-check",
    )
    worker_env = {
        item["name"]: item
        for item in manifest["spec"]["template"]["spec"]["containers"][0]["env"]
    }

    assert worker_env["STORAGE_TYPE"]["value"] == "s3"
    assert "STORAGE_LOCAL_PATH" not in worker_env
    assert "STORAGE_AWS_ACCESS_KEY_ID" in worker_env
    assert "STORAGE_AWS_SECRET_ACCESS_KEY" in worker_env
    assert "STORAGE_AWS_REGION" in worker_env
    assert "STORAGE_AWS_BUCKET" in worker_env
    assert (
        worker_env["REDIS_HOST"]["value"]
        == "aibrix-redis-master.aibrix-system.svc.cluster.local"
    )
    assert worker_env["REDIS_PORT"]["value"] == "6379"


@pytest.mark.asyncio
async def test_openai_batch_api_metadata_server_workflow(test_app):
    """
    End-to-end test for OpenAI Batch API with metadata server workflow:
    1. Upload sample input file via Files API
    2. Create batch job via Batch API (using metadata server mode)
    3. Verify metadata server prepares job output files
    4. Simulate worker execution by checking IN_PROGRESS state
    5. Poll job status until completion
    6. Download and verify output via Files API
    """
    with TestClient(test_app) as client:
        # Step 1: Upload sample input file via Files API
        print("Step 1: Uploading batch input file...")

        input_data = generate_batch_input_data(10)
        files = {
            "file": ("metadata_batch_input.jsonl", input_data, "application/jsonl")
        }
        data = {"purpose": "batch"}

        upload_response = client.post("/v1/files", files=files, data=data)
        assert (
            upload_response.status_code == 200
        ), f"File upload failed: {upload_response.text}"

        upload_result = upload_response.json()
        input_file_id = upload_result["id"]
        print(f"✅ File uploaded successfully with ID: {input_file_id}")

        # Step 2: Create batch job via Batch API (metadata server mode)
        print("Step 2: Creating batch job with metadata server workflow...")

        batch_request = build_batch_request(
            input_file_id,
            "/v1/chat/completions",
            aibrix_template="mock-vllm",
            aibrix_profile="unittest",
        )

        batch_response = client.post("/v1/batches", json=batch_request)
        assert (
            batch_response.status_code == 200
        ), f"Batch creation failed: {batch_response.text}"

        batch_result = batch_response.json()
        assert "id" in batch_result
        assert batch_result["object"] == "batch"
        assert batch_result["input_file_id"] == input_file_id
        assert batch_result["endpoint"] == "/v1/chat/completions"
        assert batch_result["completion_window"] == "24h"
        assert isinstance(batch_result["created_at"], int)

        batch_id = batch_result["id"]
        print(f"✅ Batch created successfully with ID: {batch_id}")

        # Step 3: Verify metadata server prepared job output files
        print("Step 3: Checking if job output files are prepared...")

        # Poll for a short time to see if job moves through states
        preparation_polls = 5
        for attempt in range(preparation_polls):
            status_response = client.get(f"/v1/batches/{batch_id}")
            assert (
                status_response.status_code == 200
            ), f"Status check failed: {status_response.text}"

            status_result = status_response.json()
            current_status = status_result["status"]
            print(f"  Preparation check {attempt + 1}: Status = {current_status}")

            # Check if we have output file IDs which indicates preparation is done
            output_file_id = status_result.get("output_file_id")
            error_file_id = status_result.get("error_file_id")

            if output_file_id and error_file_id:
                print("✅ Job output files prepared by metadata server!")
                break
            elif current_status in ["failed", "cancelled", "expired"]:
                pytest.fail(f"Job failed during preparation: {current_status}")

            await asyncio.sleep(0.5)

        # Step 4: Simulate worker workflow - wait for IN_PROGRESS
        print("Step 4: Waiting for job to reach IN_PROGRESS state...")

        in_progress_polls = 3
        for attempt in range(in_progress_polls):
            status_response = client.get(f"/v1/batches/{batch_id}")
            status_result = status_response.json()
            print(status_result)
            current_status = status_result["status"]

            print(f"  IN_PROGRESS check {attempt + 1}: Status = {current_status}")

            if current_status == "in_progress":
                print("✅ Job reached IN_PROGRESS state - worker can start execution!")

                # Verify status_result
                assert isinstance(status_result["in_progress_at"], int)

                break
            elif current_status in ["failed", "cancelled", "expired"]:
                pytest.fail(f"Job failed before reaching IN_PROGRESS: {current_status}")

            await asyncio.sleep(1)

        # Step 5: Poll job status until completion (metadata server should finalize)
        print("Step 5: Polling job status until completion...")

        max_polls = 20  # Extended for metadata server workflow
        poll_interval = 1

        for attempt in range(max_polls):
            status_response = client.get(f"/v1/batches/{batch_id}")
            status_result = status_response.json()
            print(status_result)
            current_status = status_result["status"]

            print(f"  Completion check {attempt + 1}: Status = {current_status}")

            if current_status == "completed":
                print(
                    "✅ Batch job completed successfully with metadata server workflow!"
                )

                # Verify status_result
                output_file_id = status_result["output_file_id"]
                assert (
                    output_file_id is not None
                ), "Expected output_file_id for completed batch"

                request_counts = status_result.get("request_counts")
                assert request_counts is not None
                assert request_counts["total"] == 10
                assert request_counts["completed"] == 10
                assert request_counts["failed"] == 0

                assert isinstance(status_result["finalizing_at"], int)
                assert isinstance(status_result["completed_at"], int)

                break
            elif current_status == "failed":
                pytest.fail(
                    f"Batch job failed: {status_result.get('errors', 'Unknown error')}"
                )
            elif current_status in ["cancelled", "expired"]:
                pytest.fail(f"Batch job was {current_status}")

            await asyncio.sleep(poll_interval)
        else:
            pytest.fail(
                f"Batch job did not complete within {max_polls * poll_interval} seconds"
            )

        # Step 6: Download and verify output via Files API
        print("Step 6: Downloading and verifying output...")

        output_response = client.get(f"/v1/files/{output_file_id}/content")
        assert (
            output_response.status_code == 200
        ), f"Output download failed: {output_response.text}"

        output_content = output_response.content.decode("utf-8")
        assert output_content, "Output file is empty"

        # Verify output content structure
        is_valid = verify_batch_output_content(output_content, 10)
        assert (
            is_valid
        ), f"Output content verification failed. Content:\n{output_content}"

        print("✅ Output downloaded and verified successfully!")
        print(f"Output content preview:\n{output_content[:200]}...")

        print(
            "\n🎉 Metadata server workflow E2E test completed successfully! "
            "Job preparation, worker coordination, and finalization working correctly."
        )
        await test_app.state.batch_driver.clear_job(batch_id)


@pytest.mark.asyncio
async def test_openai_batch_api_metadata_server_workflow_with_redis_cache_and_deployment_driver(
    redis_deployment_test_app,
    monkeypatch,
):
    monkeypatch.setattr(
        "aibrix.batch.job_driver.deployment_driver._deployment_job_driver",
        None,
    )
    app = redis_deployment_test_app
    assert isinstance(app.state.batch_driver._job_entity_manager, RedisJobCache)
    infrastructure_context = app.state.batch_driver._context
    assert infrastructure_context is not None

    apps_v1_api = infrastructure_context.apps_v1_api
    core_v1_api = infrastructure_context.core_v1_api
    assert apps_v1_api is not None
    assert core_v1_api is not None

    with TestClient(app) as client:
        input_data = generate_batch_input_data(3)
        files = {
            "file": ("deployment-input.jsonl", input_data, "application/jsonl"),
            "purpose": (None, "batch"),
        }

        upload_response = client.post("/v1/files", files=files)
        assert upload_response.status_code == 200
        input_file_id = upload_response.json()["id"]

        batch_request = build_batch_request(
            input_file_id,
            "/v1/chat/completions",
            aibrix_template="mock-vllm",
            aibrix_profile="unittest",
            resource_type="deployment",
        )
        create_response = client.post("/v1/batches", json=batch_request)
        assert create_response.status_code == 200
        batch_id = create_response.json()["id"]
        deployment_name = f"batch-{batch_id[:12]}-engine"
        service_name = f"batch-{batch_id[:12]}-svc"
        model_name = service_name
        saw_deployment = False
        saw_ready_deployment = False
        saw_service = False
        cleanup_runtime = False

        try:
            terminal_batch = None
            for _ in range(120):
                status_response = client.get(f"/v1/batches/{batch_id}")
                assert status_response.status_code == 200
                terminal_batch = status_response.json()
                try:
                    deployment = apps_v1_api.read_namespaced_deployment_status(
                        name=deployment_name,
                        namespace="default",
                    )
                    saw_deployment = True
                    available = deployment.status.available_replicas or 0
                    if available >= 1:
                        saw_ready_deployment = True
                except k8s_client.ApiException as ex:
                    if ex.status != 404:
                        raise
                try:
                    service = core_v1_api.read_namespaced_service(
                        name=service_name,
                        namespace="default",
                    )
                    if service.metadata.name == service_name:
                        saw_service = True
                except k8s_client.ApiException as ex:
                    if ex.status != 404:
                        raise
                if terminal_batch["status"] in {"completed", "failed", "cancelled"}:
                    break
                await asyncio.sleep(1)

            assert terminal_batch is not None
            assert terminal_batch["status"] == "completed"
            assert saw_deployment
            assert saw_ready_deployment
            assert saw_service

            output_file_id = terminal_batch["output_file_id"]
            output_response = client.get(f"/v1/files/{output_file_id}/content")
            assert output_response.status_code == 200
            assert verify_batch_output_content(output_response.text, 3)
            first_line = json.loads(output_response.text.splitlines()[0])
            assert first_line["response"]["body"]["model"] == model_name
            cleanup_runtime = True
        finally:
            if cleanup_runtime:
                try:
                    core_v1_api.delete_namespaced_service(
                        name=service_name,
                        namespace="default",
                    )
                except k8s_client.ApiException as ex:
                    if ex.status != 404:
                        raise
                try:
                    apps_v1_api.delete_namespaced_deployment(
                        name=deployment_name,
                        namespace="default",
                    )
                except k8s_client.ApiException as ex:
                    if ex.status != 404:
                        raise
            await app.state.batch_driver.clear_job(batch_id)


# ---- Multi-endpoint parametrized tests ----


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
async def test_openai_batch_api_multi_endpoint(endpoint: str):
    """
    Test batch API workflow for each supported endpoint type.

    Validates that the batch system correctly accepts, processes,
    and returns results for all supported OpenAI-compatible endpoints:
    - /v1/chat/completions
    - /v1/completions
    - /v1/embeddings
    - /v1/rerank
    """
    app = create_test_app()
    num_requests = 3

    with TestClient(app) as client:
        # Step 1: Upload batch input file for this endpoint
        input_data = generate_batch_input_data(num_requests, endpoint=endpoint)
        files = {
            "file": (
                f"batch_input_{endpoint.split('/')[-1]}.jsonl",
                input_data,
                "application/jsonl",
            )
        }
        data = {"purpose": "batch"}

        upload_response = client.post("/v1/files", files=files, data=data)
        assert (
            upload_response.status_code == 200
        ), f"[{endpoint}] File upload failed: {upload_response.text}"

        input_file_id = upload_response.json()["id"]

        # Step 2: Create batch job for this endpoint
        batch_request = {
            "input_file_id": input_file_id,
            "endpoint": endpoint,
            "completion_window": "24h",
        }

        batch_response = client.post("/v1/batches", json=batch_request)
        assert (
            batch_response.status_code == 200
        ), f"[{endpoint}] Batch creation failed: {batch_response.text}"

        batch_result = batch_response.json()
        assert batch_result["endpoint"] == endpoint
        batch_id = batch_result["id"]

        # Step 3: Poll until completion
        max_polls = 10
        poll_interval = 1

        for attempt in range(max_polls):
            status_response = client.get(f"/v1/batches/{batch_id}")
            assert status_response.status_code == 200
            status_result = status_response.json()
            current_status = status_result["status"]

            if current_status == "completed":
                output_file_id = status_result["output_file_id"]
                assert output_file_id is not None
                request_counts = status_result.get("request_counts")
                assert request_counts is not None
                assert request_counts["total"] == num_requests
                assert request_counts["completed"] == num_requests
                break
            elif current_status in ("failed", "cancelled", "expired"):
                pytest.fail(f"[{endpoint}] Batch job {current_status}")

            await asyncio.sleep(poll_interval)
        else:
            pytest.fail(
                f"[{endpoint}] Batch job did not complete within "
                f"{max_polls * poll_interval}s"
            )

        # Step 4: Download and verify output
        output_response = client.get(f"/v1/files/{output_file_id}/content")
        assert output_response.status_code == 200

        output_content = output_response.content.decode("utf-8")
        assert output_content, f"[{endpoint}] Output file is empty"
        assert verify_batch_output_content(
            output_content, num_requests
        ), f"[{endpoint}] Output verification failed"

        await app.state.batch_driver.clear_job(batch_id)


@pytest.mark.asyncio
async def test_batch_api_error_handling():
    """Test error handling in batch API."""
    app = create_test_app()

    with TestClient(app) as client:
        # Test creating batch with non-existent file ID
        batch_request = {
            "input_file_id": "non-existent-file-id",
            "endpoint": "/v1/chat/completions",
            "completion_window": "24h",
        }

        response = client.post("/v1/batches", json=batch_request)
        # This should either succeed (if validation is async) or fail with proper error
        # The actual behavior depends on when file validation occurs
        print(
            f"Batch creation with invalid file ID returned status: {response.status_code}"
        )

        # Test getting non-existent batch
        response = client.get("/v1/non-existent-batch-id")
        assert response.status_code == 404

        # Test invalid file upload
        files = {
            "file": ("invalid.txt", "This is not a valid batch file", "text/plain")
        }
        data = {"purpose": "batch"}

        response = client.post("/v1/files/", files=files, data=data)
        assert response.status_code == 400  # Should fail due to invalid file extension


if __name__ == "__main__":
    # Allow running the test directly
    test_openai_batch_api_e2e()
    test_batch_api_error_handling()

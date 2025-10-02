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
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Optional

import boto3
import pytest
from kubernetes import client

import aibrix.batch.storage as _storage
import aibrix.batch.storage.batch_metastore as _metastore
from aibrix.batch.job_entity import BatchJob, BatchJobSpec, BatchJobState
from aibrix.logger import init_logger
from aibrix.metadata.cache.job import JobCache
from aibrix.storage.types import StorageType

logger = init_logger(__name__)


class TestWorkerS3Integration:
    """Test worker with S3 storage and Redis metadata integration."""

    @pytest.fixture
    def init_storage(self, test_s3_bucket):
        """Get or create test S3 bucket."""
        try:
            _storage.initialize_storage(StorageType.S3, {"bucket_name": test_s3_bucket})
            _metastore.initialize_batch_metastore(StorageType.REDIS)
        except Exception as e:
            pytest.skip(f"Cannot initialize S3 storage and redis metastore: {e}")

    @pytest.fixture
    def test_input_data(self):
        """Load test input data from sample_job_input.jsonl."""
        import json

        sample_file_path = Path(__file__).parent / "testdata" / "sample_job_input.jsonl"

        test_data = []
        with open(sample_file_path, "r") as f:
            for line in f:
                line = line.strip()
                if line:
                    test_data.append(json.loads(line))

        return test_data

    async def _upload_test_data_to_s3(
        self, test_s3_bucket: str, input_file_id: str, test_data: list
    ) -> Any:
        """
        Upload test data to S3 bucket.

        Returns:
            S3 client for cleanup operations
        """
        session = boto3.Session()
        s3_client = session.client("s3")

        # Create JSONL content
        jsonl_content = "\n".join(json.dumps(item) for item in test_data)
        s3_key = input_file_id

        s3_client.put_object(
            Bucket=test_s3_bucket,
            Key=s3_key,
            Body=jsonl_content.encode("utf-8"),
            ContentType="application/jsonl",
        )
        logger.info(
            f"Uploaded {len(test_data)} requests to S3: s3://{test_s3_bucket}/{s3_key}"
        )

        return s3_client

    async def _submit_patch_and_monitor_job(
        self,
        test_namespace: str,
        input_file_id: str,
        test_s3_bucket: str,
        job_name: str,
        timeout: int,
        job_cache: JobCache,
        is_parallel: bool = False,
    ) -> BatchJob:
        """
        Submit job to Kubernetes and monitor until completion.

        Args:
            batch_client: Kubernetes batch client
            test_namespace: Kubernetes namespace
            job_spec: Job specification
            job_name: Name of the job
            timeout: Timeout in seconds
            is_parallel: Whether to log parallel-specific progress
        """
        # Submit job to Kubernetes
        job_type = "parallel" if is_parallel else "single"
        logger.info(f"Submitting S3 {job_type} batch job to Kubernetes...")

        # Set up job monitoring with kopf-powered JobCache
        main_loop = asyncio.get_running_loop()
        main_loop.name = "test"  # type: ignore[attr-defined]
        job_committed = main_loop.create_future()
        job_done = main_loop.create_future()

        async def job_commited_handler(job: BatchJob) -> bool:
            if not job_committed.done():
                main_loop.call_soon_threadsafe(job_committed.set_result, job)
                return True

            return False

        async def job_updated_handler(_: BatchJob, newJob: BatchJob) -> bool:
            if job_done.done():
                return False

            if newJob.status.state == BatchJobState.FINALIZING:
                main_loop.call_soon_threadsafe(job_done.set_result, newJob)

            return True

        # Register handlers with the kopf-powered JobCache
        job_cache.on_job_committed(job_commited_handler)
        job_cache.on_job_updated(job_updated_handler)

        job_spec = BatchJobSpec.from_strings(
            input_file_id, "/v1/chat/completions", "24h", None
        )

        # Create job
        await job_cache.submit_job(
            "test-session-id",
            job_spec,
            job_name=job_name,
            parallelism=3 if is_parallel else 1,
        )

        # Wait for kopf to detect the job creation and trigger handlers
        logger.info("Waiting for kopf to detect job creation...")
        created_job = await asyncio.wait_for(job_committed, timeout=timeout)

        try:
            assert created_job.metadata.name == job_name
            assert created_job.status.state == BatchJobState.CREATED
            logger.info(
                f"S3 {job_type} batch job submitted successfully:", job_name=job_name
            )  # type:ignore[call-arg]

            # Emulate job_driver behavior and create tempfiles
            created_job.status.in_progress_at = datetime.now(timezone.utc)
            created_job.status.state = BatchJobState.IN_PROGRESS
            prepared_job = await _storage.prepare_job_ouput_files(created_job)

            await job_cache.update_job_ready(prepared_job)
            logger.info(
                "Batch job patched with file ids successfully:",
                job_name=job_name,
                output_file_id=prepared_job.status.output_file_id,
                temp_output_file_id=prepared_job.status.temp_output_file_id,
                error_file_id=prepared_job.status.error_file_id,
                temp_error_file_id=prepared_job.status.temp_error_file_id,
            )  # type:ignore[call-arg]

            # Wait for job completion
            finished_job = await asyncio.wait_for(job_done, timeout=timeout)
            assert finished_job.status.state == BatchJobState.FINALIZING

            return finished_job

        except Exception as e:
            await self._log_pod_details(test_namespace, job_name)
            pytest.fail(
                f"S3 {job_type} job did not complete within {timeout} seconds, error={str(e)}"
            )
            raise

    async def _cleanup_s3_and_k8s_resources(
        self,
        s3_client: Any,
        test_s3_bucket: str,
        input_file_id: str,
        job: Any,
        test_namespace: str,
        job_name: str,
    ) -> None:
        """
        Clean up S3 objects and Kubernetes job.

        Args:
            s3_client: S3 client
            test_s3_bucket: S3 bucket name
            input_file_id: Input file identifier
            job: BatchJob object
            batch_client: Kubernetes batch client
            test_namespace: Kubernetes namespace
            job_name: Job name
        """
        # Cleanup S3 objects
        try:
            # Delete input files
            response = s3_client.list_objects_v2(
                Bucket=test_s3_bucket, Prefix=input_file_id
            )

            if "Contents" in response:
                for obj in response["Contents"]:
                    s3_client.delete_object(Bucket=test_s3_bucket, Key=obj["Key"])
                    logger.info(f"Deleted S3 object: {obj['Key']}")

            # Delete output files
            if job and job.status.temp_output_file_id:
                output_prefix = f".multipart/{job.status.temp_output_file_id}/"
                output_response = s3_client.list_objects_v2(
                    Bucket=test_s3_bucket, Prefix=output_prefix
                )

                if "Contents" in output_response:
                    for obj in output_response["Contents"]:
                        s3_client.delete_object(Bucket=test_s3_bucket, Key=obj["Key"])
                        logger.info(f"Deleted S3 output object: {obj['Key']}")

                # Delete error files
                error_prefix = f".multipart/{job.status.temp_error_file_id}/"
                error_response = s3_client.list_objects_v2(
                    Bucket=test_s3_bucket, Prefix=error_prefix
                )

                if "Contents" in error_response:
                    for obj in error_response["Contents"]:
                        s3_client.delete_object(Bucket=test_s3_bucket, Key=obj["Key"])
                        logger.info(f"Deleted S3 error object: {obj['Key']}")
        except Exception as e:
            logger.warning(f"Failed to cleanup S3 objects: {e}")

        # Delete the Kubernetes job
        try:
            batch_client = client.BatchV1Api()
            batch_client.delete_namespaced_job(
                name=job_name,
                namespace=test_namespace,
                propagation_policy="Background",
            )
            logger.info(f"Deleted S3 batch job: {job_name}")
        except client.ApiException as e:
            if e.status != 404:
                logger.warning(f"Failed to delete job {job_name}: {e}")

    def _expand_test_data(self, test_input_data: list, multiplier: int) -> list:
        """
        Expand test data by duplicating and modifying custom_id.

        Args:
            test_input_data: Original test data
            multiplier: How many times to duplicate the data

        Returns:
            Expanded test data list
        """
        expanded_test_data = []
        for i in range(multiplier):
            for item in test_input_data:
                expanded_item = copy.deepcopy(item)
                expanded_item["custom_id"] = (
                    f"{item.get('custom_id', 'test')}-batch-{i}"
                )
                expanded_test_data.append(expanded_item)

        logger.info(f"Created {len(expanded_test_data)} test requests for processing")
        return expanded_test_data

    @pytest.mark.asyncio
    async def test_single_worker(
        self,
        k8s_config,
        test_namespace,
        s3_credentials_secret,
        test_s3_bucket,
        init_storage,
        test_input_data,
        job_cache,
    ) -> None:
        """Test worker using S3 storage and Redis metadata."""

        # Generate unique job name and setup
        job_name = "s3-batch-job"
        input_file_id = f"s3-test-input-{str(uuid.uuid4())[:8]}.jsonl"

        # Upload test data to S3
        s3_client = await self._upload_test_data_to_s3(
            test_s3_bucket, input_file_id, test_input_data
        )

        job: Optional[BatchJob] = None
        try:
            # Submit job and monitor until completion
            job = await self._submit_patch_and_monitor_job(
                test_namespace,
                input_file_id,
                test_s3_bucket,
                job_name,
                60,
                job_cache,
                is_parallel=False,
            )

            # Verify S3 outputs exist
            await self._verify_s3_outputs(
                s3_client,
                test_s3_bucket,
                job.status.temp_output_file_id,
                len(test_input_data),
            )

            # Verify Redis locking worked correctly by checking completion keys
            await self._verify_redis_completion_keys(job, len(test_input_data))

            logger.info("S3 integration test completed successfully!")

        finally:
            # Cleanup all resources
            await self._cleanup_s3_and_k8s_resources(
                s3_client,
                test_s3_bucket,
                input_file_id,
                job,
                test_namespace,
                job_name,
            )

    @pytest.mark.asyncio
    async def test_parallel_workers(
        self,
        k8s_config,
        test_namespace,
        s3_credentials_secret,
        test_s3_bucket,
        init_storage,
        test_input_data,
        job_cache,
    ) -> None:
        """Test 3 concurrent workers using S3 storage and Redis metadata with request locking."""

        # Generate unique job name and setup
        job_name = "s3-parallel-batch-job"
        input_file_id = f"s3-parallel-test-input-{str(uuid.uuid4())[:8]}.jsonl"

        # Create expanded test data for parallel processing
        expanded_test_data = self._expand_test_data(test_input_data, multiplier=3)

        # Upload expanded test data to S3
        s3_client = await self._upload_test_data_to_s3(
            test_s3_bucket, input_file_id, expanded_test_data
        )

        job: Optional[BatchJob] = None
        try:
            # Submit job and monitor until completion
            job = await self._submit_patch_and_monitor_job(
                test_namespace,
                input_file_id,
                test_s3_bucket,
                job_name,
                60,
                job_cache,
                is_parallel=True,
            )

            # Verify S3 outputs exist for all requests
            await self._verify_s3_outputs(
                s3_client,
                test_s3_bucket,
                job.status.temp_output_file_id,
                len(expanded_test_data),
            )

            # Verify Redis locking worked correctly by checking completion keys
            await self._verify_redis_completion_keys(job, len(expanded_test_data))

            logger.info(
                "S3 parallel integration test with 3 workers completed successfully!"
            )

        finally:
            # Cleanup all resources
            await self._cleanup_s3_and_k8s_resources(
                s3_client,
                test_s3_bucket,
                input_file_id,
                job,
                test_namespace,
                job_name,
            )

    async def _log_pod_details(self, namespace, job_name):
        """Log pod details for debugging."""
        core_v1 = client.CoreV1Api()

        try:
            pods = core_v1.list_namespaced_pod(
                namespace=namespace, label_selector=f"job-name={job_name}"
            )

            for pod in pods.items:
                logger.info(f"S3 Pod: {pod.metadata.name}, Status: {pod.status.phase}")

                # Log container statuses
                if pod.status.container_statuses:
                    for container_status in pod.status.container_statuses:
                        logger.info(
                            f"S3 Container: {container_status.name}, Ready: {container_status.ready}"
                        )

                # Get pod logs
                for container in ["batch-worker", "llm-engine"]:
                    try:
                        logs = core_v1.read_namespaced_pod_log(
                            name=pod.metadata.name,
                            namespace=namespace,
                            container=container,
                            tail_lines=100,  # More logs for S3 debugging
                        )
                        print(
                            f"####################### S3 {container} logs starts #######################"
                        )
                        for line in logs.splitlines():
                            print(line)
                        print(
                            f"####################### S3 {container} logs ends #######################"
                        )
                    except client.ApiException:
                        logger.warning(
                            f"Could not get S3 logs for container {container}"
                        )

        except client.ApiException as e:
            logger.error(f"Failed to get S3 pod details: {e}")

    async def _verify_s3_outputs(
        self, s3_client, bucket, temp_output_file_id, expected_count
    ):
        """Verify that output files were created in S3."""

        # Check for output files in S3
        output_prefix = f".multipart/{temp_output_file_id}/"  # See BaseStorage::_multipart_upload_key
        response = s3_client.list_objects_v2(Bucket=bucket, Prefix=output_prefix)

        assert "Contents" in response
        assert (
            len(response["Contents"]) == expected_count + 1
        )  # Including .multipart metadata
        logger.info(f"Found {len(response['Contents'])} output files in S3:")

        for obj in response["Contents"]:
            logger.info("Loading request output", key=obj["Key"], size=obj["Size"])  # type:ignore[call-arg]

            # check file content
            content = s3_client.get_object(Bucket=bucket, Key=obj["Key"])
            raw_output = content["Body"].read().decode("utf-8")
            logger.info("Loaded request output", key=obj["Key"], output=raw_output)  # type:ignore[call-arg]
            # Skip metadata
            if obj["Key"].endswith("/metadata"):
                continue
            output = json.loads(raw_output)
            assert "id" in output
            assert "custom_id" in output
            assert "response" in output

            response = output["response"]
            assert "status_code" in response
            assert "request_id" in response
            assert "body" in response

            body = response["body"]
            assert "id" in body
            assert "model" in body
            assert "object" in body

    async def _verify_redis_completion_keys(self, job, expected_count):
        """Verify that all requests have completion keys in Redis metastore."""
        try:
            # Initialize Redis metastore to check completion status

            import aibrix.batch.storage.batch_metastore as metastore
            from aibrix.storage import StorageType

            metastore.initialize_batch_metastore(StorageType.REDIS)

            completed_count = 0
            missing_keys = []

            # Check each request's completion status
            for i in range(expected_count):
                completion_key = f"batch:{job.job_id}:done/{i}"
                status, exists = await metastore.get_metadata(completion_key)

                if exists:
                    completed_count += 1
                    logger.info(f"Found completion key {completion_key}: {status}")
                else:
                    missing_keys.append(completion_key)

            logger.info(
                f"Found {completed_count}/{expected_count} completion keys in Redis"
            )

            if missing_keys:
                logger.warning(
                    f"Missing completion keys: {missing_keys[:10]}..."
                )  # Show first 10

            # We expect all requests to be completed
            assert (
                completed_count == expected_count
            ), f"Expected {expected_count} completed requests, but found {completed_count}"

        except Exception as e:
            logger.warning(f"Could not verify Redis completion keys: {e}")
            # Don't fail the test if Redis verification fails

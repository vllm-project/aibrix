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
import uuid
from typing import Any, Optional
from urllib.parse import urljoin

import httpx

import aibrix.batch.constant as constant
import aibrix.batch.storage as storage
from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobError,
    BatchJobErrorCode,
    BatchJobState,
    ConditionType,
)
from aibrix.batch.job_progress_manager import JobProgressManager
from aibrix.logger import init_logger

logger = init_logger(__name__)


class InferenceEngineClient:
    async def inference_request(self, endpoint: str, request_data):
        """Send inference request to the LLM engine."""
        await asyncio.sleep(constant.EXPIRE_INTERVAL)  # Simulate processing time
        return request_data


class ProxyInferenceEngineClient(InferenceEngineClient):
    def __init__(self, base_url: str):
        """
        Initiate client to inference engine.
        """
        self.base_url = base_url

    async def inference_request(self, endpoint: str, request_data):
        """Real inference request to LLM engine."""
        url = urljoin(self.base_url, endpoint)

        logger.debug("requesting inference", url=url, body=request_data)  # type: ignore[call-arg]

        async with httpx.AsyncClient() as client:
            response = await client.post(url, json=request_data, timeout=30.0)
            response.raise_for_status()
            return response.json()


class JobDriver:
    def __init__(
        self,
        progress_manager: JobProgressManager,
        inference_client: Optional[InferenceEngineClient] = None,
    ) -> None:
        """
        JobDriver drives job progress after a job being started. The progress expreiences three phases:
        1. Job preparing: job output file and error file are prepared.
        2. Job executing: tasks in the job are read and executed, possibly in parallel, without order reservation.
        3. Job finalizing: aggregate job outputs and errors.

        Usage:
        * Call execute_job() to execute all phases. This is usually be the case if running in API server with scheduler enabled.
        * Call prepare_job() for Job preparing. API server runs without scheduler will call this to prepara job.
        * Call execute_worker() for Job executing, supporting parallel exeuction. LLM colocated worker will call this.
        * Call finalize_job() for Job finalizing. API server runs without scheduler will call this to aggregate outputs.
        """
        self._progress_manager = progress_manager
        if inference_client is None:
            self._inference_client = InferenceEngineClient()
        else:
            self._inference_client = inference_client

    async def execute_job(self, job_id):
        """
        Execute complete job workflow: prepare -> execute -> finalize.
        This function executes all three steps.
        """
        job = await self._progress_manager.get_job(job_id)
        if job is None:
            logger.warning("Job not found", job_id=job_id)
            return

        # Check if temp file IDs exist to determine if we should skip steps 1 and 3
        has_temp_files = (
            job.status.temp_output_file_id and job.status.temp_error_file_id
        )

        if not has_temp_files:
            # Step 1: Prepare job output files
            logger.debug("Temp files not created, creating...", job_id=job_id)
            job = await storage.prepare_job_ouput_files(job)

        logger.debug(
            "Confirmed temp files",
            job_id=job_id,
            temp_output_file_id=job.status.temp_output_file_id,
            temp_error_file_id=job.status.temp_error_file_id,
        )

        # Step 2: Execute worker (core execution)
        try:
            job = await self.execute_worker(job_id)
        except Exception as ex:
            # Handle exception here, so we can execute finalizing if necessary.
            job = await self._progress_manager.mark_job_failed(
                job_id,
                BatchJobError(code=BatchJobErrorCode.INFERENCE_FAILED, message=str(ex)),
            )

        # Step 3: Aggregate outputs
        if job.status.state == BatchJobState.FINALIZING:
            if not has_temp_files:
                await storage.finalize_job_output_data(job)

            logger.debug("Completed job", job_id=job_id)
            job = await self._sync_job_status(job_id)

        if job.status.failed:
            failed_condition = job.status.get_condition(ConditionType.FAILED)
            if failed_condition is None:
                raise RuntimeError("Job failed but no failure condition was set")
            raise RuntimeError(
                failed_condition.message or "Job failed with an unspecified error"
            )

    async def prepare_job(self, job: BatchJob) -> BatchJob:
        """
        Prepare job output files by creating multipart uploads.
        This is called by metadata server when a new job is committed.
        """
        logger.debug("Preparing job output files")  # type: ignore[call-arg]
        job = await storage.prepare_job_ouput_files(job)
        logger.debug("Job output files prepared")  # type: ignore[call-arg]
        return job

    async def execute_worker(self, job_id) -> BatchJob:
        """
        Execute worker logic: process requests without file preparation or finalization.
        This function only executes step 2 (the core execution loop).
        """
        # Verify job status and get minimum unfinished request id
        job, line_no = await self._get_next_request(job_id)
        if line_no < 0:
            logger.warning(
                "Job has something wrong with metadata in job manager, nothing left to execute",
                job_id=job_id,
            )  # type: ignore[call-arg]
            return job

        # [TODO][NOW] find a quick way to decide where to start testing using metastore
        if line_no == 0:
            logger.debug("Start processing job", job_id=job_id, opts=job.spec.opts)  # type: ignore[call-arg]
        else:
            logger.debug(
                "Resuming job", job_id=job_id, request_id=line_no, opts=job.spec.opts
            )  # type: ignore[call-arg]

        # Check for fail_after_n_requests option
        fail_after_n_requests = None
        if job.spec.opts and constant.BATCH_OPTS_FAIL_AFTER_N_REQUESTS in job.spec.opts:
            try:
                fail_after_n_requests = int(
                    job.spec.opts[constant.BATCH_OPTS_FAIL_AFTER_N_REQUESTS]
                )
                logger.debug(
                    "Detected fail_after_n_requests option",
                    job_id=job_id,
                    fail_after_n_requests=fail_after_n_requests,
                )  # type: ignore[call-arg]
            except (ValueError, TypeError):
                logger.warning(
                    "Invalid fail_after_n_requests value, ignoring",
                    job_id=job_id,
                    value=job.spec.opts["fail_after_n_requests"],
                )  # type: ignore[call-arg]

        # Step 2: Execute requests, resumable.
        processed_requests = 0
        last_line_no = line_no
        while line_no >= 0:
            async for request_input in storage.read_job_next_request(job, line_no):
                # Extract the request index from the locked request
                next_line_no = request_input.pop("_request_index", last_line_no)
                # Valid status of skipped requests.
                while last_line_no < next_line_no:
                    if await storage.is_request_done(job, last_line_no):
                        # Mark the skipped request done
                        logger.debug(
                            "Mark skipped request as done locally",
                            job_id=job_id,
                            request_id=last_line_no,
                        )  # type: ignore[call-arg]
                        job, line_no = await self._sync_job_status_and_get_next_request(
                            job_id, last_line_no
                        )
                    else:
                        # Simply skipped the request and get next request id
                        job, line_no = await self._get_next_request(job_id)
                    logger.debug(
                        "Will test next request",
                        job_id=job_id,
                        next_unexecuted=line_no,
                        next_executable=next_line_no,
                        last_line_no=last_line_no,
                    )  # type: ignore[call-arg]
                    if line_no < last_line_no:
                        # Start next round or stop if no more requests
                        break
                    last_line_no = line_no

                # Start next round or stop if no more requests
                if line_no < last_line_no:
                    break

                if line_no != next_line_no:
                    raise RuntimeError(
                        f"Metastore inconsistency: expected request index {line_no} but got {next_line_no}"
                    )
                # Or global status maintained by metastore is not consistent with local status

                custom_id = request_input.get("custom_id", "")
                logger.debug(
                    "Executing job request",
                    job_id=job_id,
                    line=line_no,
                    request_id=line_no,
                    custom_id=custom_id,
                )  # type: ignore[call-arg]

                # Validate request has required fields
                if "body" not in request_input:
                    raise BatchJobError(
                        code=BatchJobErrorCode.INVALID_INPUT_FILE,
                        message="Request missing 'body' field",
                        line=line_no,
                    )

                # Retry inference request up to 3 times with exponential backoff
                request_output, last_error = await self._retry_inference_request(
                    job.spec.endpoint, request_input["body"], job_id, line_no
                )

                # Build standardized response
                response = self._build_response(
                    custom_id, job_id, line_no, request_output, last_error
                )

                logger.debug(
                    "Got request response",
                    job_id=job_id,
                    request_id=line_no,
                    response=response,
                )  # type: ignore[call-arg]
                # Write single output and unlock the request
                await storage.write_job_output_data(job, line_no, response)

                assert last_line_no == line_no
                logger.debug("Job request executed", job_id=job_id, request_id=line_no)  # type: ignore[call-arg]

                # Check for fail_after_n_requests condition
                if fail_after_n_requests is not None:
                    processed_requests += 1
                    if processed_requests >= fail_after_n_requests:
                        logger.info(
                            "Triggering artificial failure due to fail_after_n_requests",
                            job_id=job_id,
                            processed_requests=processed_requests,
                            fail_after_n_requests=fail_after_n_requests,
                        )  # type: ignore[call-arg]
                        raise RuntimeError(
                            f"Artificial failure triggered after processing {processed_requests} requests "
                            f"(fail_after_n_requests={fail_after_n_requests})"
                        )
                job, line_no = await self._sync_job_status_and_get_next_request(
                    job_id, last_line_no
                )
                logger.debug(
                    "Confirmed next request",
                    job_id=job_id,
                    next_unexecuted=line_no,
                    last_line_no=last_line_no,
                )  # type: ignore[call-arg]
                if line_no < last_line_no:
                    break
                last_line_no = line_no

            # For the first round, this shows we read end of input and we now know the total
            if last_line_no == line_no:
                job = await self._sync_job_status(
                    job_id, total=line_no
                )  # Now that total == request_id
                # We need to confirm that all is execute by try starting next round
                job, line_no = await self._get_next_request(job_id)
                logger.debug(
                    "Confirmed total requests",
                    job_id=job_id,
                    total=job.status.request_counts.total,
                    next_unexecuted=line_no,
                )  # type: ignore[call-arg]

            # Now we'll testing if we really finished, or start another round.

        # Now that all finished.
        logger.debug(
            "Worker completed, job state:",
            job_id=job_id,
            total=job.status.request_counts.total if job else None,
            state=job.status.state.value if job else None,
        )  # type: ignore[call-arg]
        return job

    async def finalize_job(self, job: BatchJob) -> BatchJob:
        """
        Finalize the job by removing all data.
        """
        assert job.status.state == BatchJobState.FINALIZING

        await storage.finalize_job_output_data(job)

        logger.debug("Finalized job", job_id=job.job_id)  # type: ignore[call-arg]
        return await self._sync_job_status(job.job_id)

    async def _retry_inference_request(
        self,
        endpoint: str,
        request_data: dict,
        job_id: str,
        request_id: int,
        max_retries: int = 3,
    ) -> tuple[Any, Optional[Exception]]:
        """
        Retry inference request with exponential backoff.

        Returns:
            tuple: (request_output, last_error) - output on success, error on failure
        """
        request_output = None
        last_error = None

        for attempt in range(max_retries):
            try:
                request_output = await self._inference_client.inference_request(
                    endpoint, request_data
                )
                break  # Success, exit retry loop
            except Exception as e:
                last_error = e
                logger.warning(
                    f"Inference request failed (attempt {attempt + 1}/{max_retries}): {e}",
                    job_id=job_id,
                    request_id=request_id,
                )  # type: ignore[call-arg]
                if attempt < max_retries - 1:  # Don't sleep on last attempt
                    await asyncio.sleep(1 * (attempt + 1))  # Exponential backoff

        return request_output, last_error

    def _build_response(
        self,
        custom_id: str,
        job_id: str,
        request_id: int,
        request_output: Any = None,
        error: Optional[Exception] = None,
    ) -> dict[str, Any]:
        """
        Build a standardized response object for job requests.

        Args:
            custom_id: Custom identifier for the request
            job_id: Job identifier
            request_id: Request identifier
            request_output: Successful response data (if any)
            error: Error that occurred (if any)

        Returns:
            dict: Standardized response object
        """
        response: dict[str, Any] = {
            "id": uuid.uuid4().hex[:5],
            "error": None,
            "response": None,
            "custom_id": custom_id,
        }

        if error is not None:
            logger.error(
                f"All inference attempts failed after retries: {error}",
                job_id=job_id,
                request_id=request_id,
            )  # type: ignore[call-arg]
            response["error"] = BatchJobError(
                code=BatchJobErrorCode.INFERENCE_FAILED, message=str(error)
            )
        else:
            response["response"] = {
                "status_code": 200,
                "request_id": f"{job_id}-{request_id}",
                "body": request_output,
            }

        return response

    async def _sync_job_status(self, job_id, reqeust_id=-1, total=0) -> BatchJob:
        """
        Update job's status back to job manager.
        """
        if total > 0:
            return await self._progress_manager.mark_job_total(job_id, total)
        elif reqeust_id < 0:
            return await self._progress_manager.mark_job_done(job_id)
        else:
            return await self._progress_manager.mark_jobs_progresses(
                job_id, [reqeust_id]
            )

    async def _get_next_request(self, job_id: str) -> tuple[BatchJob, int]:
        """
        Get next request id from job manager.
        """
        return await self._progress_manager.get_job_next_request(job_id)

    async def _sync_job_status_and_get_next_request(
        self, job_id: str, request_id: int
    ) -> tuple[BatchJob, int]:
        """
        Sync job status and get next request, with None checking.
        """
        return await self._progress_manager.mark_job_progress_and_get_next_request(
            job_id, request_id
        )

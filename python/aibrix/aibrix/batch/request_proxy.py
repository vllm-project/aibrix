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

import time
import json

import aibrix.batch.storage as storage
from aibrix.batch.job_entity import BatchJobState, BatchJob
from aibrix.batch.job_manager import JobManager
from aibrix.metadata.logger import init_logger

logger = init_logger(__name__)


class RequestProxy:
    def __init__(self, manager) -> None:
        """ """
        self._job_manager: JobManager = manager
        self._inference_client = InferenceEngineClient()

    async def execute_queries(self, job_id):
        """
        This is the entrance to inference engine.
        This fetches request input from storage and submit request
        to inference engine. Lastly the result is stored back to storage.
        """
        # Verify job status and get minimum unfinished request id
        request_id = self._job_manager.get_job_next_request(job_id)
        if request_id == -1:
            logger.warning(
                "Job has something wrong with metadata in job manager, nothing left to execute",
                job_id=job_id,
            )
            return

        job = self._job_manager.get_job(job_id)

        if request_id == 0:
            logger.debug("Start processing job", job_id=job_id)
        else:
            logger.debug("Resuming job", job_id=job_id, request_id=request_id)

        # Step 1: Prepare job output files.
        await storage.prepare_job_ouput_files(job)

        # Step 2: Execute requests, resumable.
        line_no = request_id
        async for request in storage.read_job_next_request(job, request_id):
            logger.debug(
                "Read job request, checking completion status",
                job_id=job_id,
                line=line_no,
                next_unfinished=request_id,
            )
            # Skip completed requests
            if line_no < request_id:
                continue

            logger.debug("Executing job request", job_id=job_id, request_id=request_id)
            request_output = self._inference_client.inference_request(
                job.spec.endpoint, request
            )
            await storage.write_job_output_data(job, request_id, [request_output])
            # Request next id to avoid state becoming FINALIZING by make total > request_id
            logger.debug(
                "Job request executed",
                job_id=job_id,
                request_id=request_id
            )
            job = self.sync_job_status(job_id, request_id)

            request_id = self._job_manager.get_job_next_request(job_id)
            line_no += 1

        job = self.sync_job_status(job_id, request_id + 1)  # Now that total == request_id
        logger.debug("Finalizing job", job_id=job_id, total=job.status.request_counts.total, state=job.status.state.value)
        assert job.status.state == BatchJobState.FINALIZING

        # Step 3: Aggregate outputs.
        await storage.finalize_job_output_data(job)

        logger.debug("Completed job", job_id=job_id)
        self.sync_job_status(job_id)

    def store_output(self, output_id, request_id, result):
        """
        Write the request result back to storage.
        """
        storage.put_job_results(output_id, request_id, [result])

    def sync_job_status(self, job_id, reqeust_id=-1) -> BatchJob:
        """
        Update job's status back to job manager.
        """
        if reqeust_id < 0:
            return self._job_manager.mark_job_done(job_id)
        else:
            return self._job_manager.mark_job_progress(job_id, [reqeust_id])


class InferenceEngineClient:
    def __init__(self):
        """
        Initiate client to inference engine, such as account
        and its authentication.
        """
        pass

    def inference_request(self, endpoint, prompt_list):
        time.sleep(1)
        return prompt_list

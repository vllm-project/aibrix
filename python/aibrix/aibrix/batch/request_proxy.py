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


class RequestProxy:
    def __init__(self, storage, manager):
        """ """
        self._storage = storage
        self._job_manager = manager
        self._inference_client = InferenceEngineClient()

    async def execute_queries(self, job_id):
        """
        This is the entrance to inference engine.
        This fetches request input from storage and submit request
        to inference engine. Lastly the result is stored back to storage.
        """
        request_id = self._job_manager.get_job_next_request(job_id)
        if request_id == -1:
            print(f"Job {job_id} has something wrong with metadata in job manager.")
            return

        endpoint = self._job_manager.get_job_endpoint(job_id)
        request_input = self.fetch_request_input(job_id, request_id)

        print(f"executing job {job_id} request {request_id}")
        request_output = self._inference_client.inference_request(
            endpoint, request_input
        )
        self.store_output(job_id, request_id, request_output)

        self.sync_job_status(job_id, request_id)

    def fetch_request_input(self, job_id, request_id):
        """
        Read request input from storage. Now it only reads one request.
        Later we can add a list as a batch per call.
        """
        num_request = 1
        requests = self._storage.get_job_input_requests(job_id, request_id, num_request)
        return requests[0]

    def store_output(self, job_id, request_id, result):
        """
        Write the request result back to storage.
        """
        self._storage.put_job_results(job_id, request_id, [result])

    def sync_job_status(self, job_id, reqeust_id):
        """
        Update job's status back to job manager.
        """
        self._job_manager.mark_job_progress(job_id, [reqeust_id])


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

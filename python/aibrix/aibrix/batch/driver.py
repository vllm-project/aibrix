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

import aibrix.batch.storage as _storage
from aibrix.batch.constant import DEFAULT_JOB_POOL_SIZE
from aibrix.batch.job_manager import JobManager
from aibrix.batch.request_proxy import RequestProxy
from aibrix.batch.scheduler import JobScheduler


class BatchDriver:
    def __init__(self):
        """
        This is main entrance to bind all components to serve job requests.
        """
        _storage.initialize_storage()
        self._storage = _storage
        self._job_manager = JobManager()
        self._scheduler = JobScheduler(self._job_manager, DEFAULT_JOB_POOL_SIZE)
        self._proxy = RequestProxy(self._storage, self._job_manager)
        asyncio.create_task(self.jobs_running_loop())

    def upload_batch_data(self, input_file_name):
        job_id = self._storage.submit_job_input(input_file_name)
        return job_id

    def create_job(self, job_id, endpoint, window_due_time):
        self._job_manager.create_job(job_id, endpoint, window_due_time)

        due_time = self._job_manager.get_job_window_due(job_id)
        self._scheduler.append_job(job_id, due_time)

    def get_job_status(self, job_id):
        return self._job_manager.get_job_status(job_id)

    def retrieve_job_result(self, job_id):
        num_requests = _storage.get_job_num_request(job_id)
        req_results = _storage.get_job_results(job_id, 0, num_requests)
        return req_results

    async def jobs_running_loop(self):
        """
        This loop is going through all active jobs in scheduler.
        For now, the executing unit is one request. Later if necessary,
        we can support a batch size of request per execution.
        """
        while True:
            one_job = self._scheduler.round_robin_get_job()
            if one_job:
                await self._proxy.execute_queries(one_job)
            await asyncio.sleep(0)

    def clear_job(self, job_id):
        self._storage.delete_job(job_id)

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
from aibrix.batch.constant import EXPIRE_INTERVAL, DEFAULT_JOB_POOL_SIZE

import aibrix.batch.storage as _storage
from aibrix.batch.scheduler import JobScheduler
from aibrix.batch.job_manager import JobManager
from aibrix.batch.request_proxy import RequestProxy



async def driver_proc():
    """
    This is main driver process on how to call scheduler.
    """
    _scheduler = JobScheduler()
    _scheduler.append_job(1, 2)
    _scheduler.append_job(2, 4)
    _scheduler.append_job(3, 7)
    _scheduler.append_job(4, 15)
    _scheduler.append_job(100, 7)
    await asyncio.sleep(5 * EXPIRE_INTERVAL)
    one_job = _scheduler.schedule_get_job()
    print("###### an active job: ", one_job)
    await asyncio.sleep(5 * EXPIRE_INTERVAL)

    one_job = _scheduler.schedule_get_job()
    print("###### an active job: ", one_job)

class BatchDriver:
    def __init__(self):
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
        
        print("job ", job_id, "manager is done")
        due_time = self._job_manager.get_job_window_due(job_id)
        self._scheduler.append_job(job_id, due_time)
        print(" ", job_id, "_scheduler is done")
    
    def get_job_status(self, job_id):
        return self._job_manager.get_job_status(job_id)
        
    def retrieve_job_result(self, job_id):
        num_requests = _storage.get_job_num_request(job_id)
        print("Try to read results!!!!!!")
        req_results = _storage.get_job_results(job_id, 0, num_requests)
        return req_results

    async def jobs_running_loop(self):
        while True:
            one_job = self._scheduler.round_robin_get_job()
            print("driver running loop: ", one_job)
            if one_job: 
                await self._proxy.execute_queries(one_job)
            await asyncio.sleep(0)

    def clear_job(self, job_id):
        self._storage.delete_job(job_id)

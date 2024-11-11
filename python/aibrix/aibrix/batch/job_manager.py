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
import time
from enum import Enum

import aibrix.batch.storage as _storage


class JobStatus(Enum):
    CREATED = 1
    VALIDATING = 2
    FAILED = 3
    PENDING = 4
    IN_PROGRESS = 5
    EXPIRED = 6
    CANCELLING = 7
    CANCELED = 8
    COMPLETED = 9


class JobMetaInfo:
    def __init__(self, job_id, model_endpoint, completion_window):
        """
        This constructs a full set of metadata for a batch job.
        Later if needed, this can include other extral metadata
        as an easy extention.
        """
        self._job_id = job_id
        self._num_requests = 0
        self._started_time = time.time()
        self._end_time = None

        self._model_endpoint = model_endpoint
        self._completion_window_str = completion_window
        self._completion_window = 0

        self._async_lock = asyncio.Lock()
        self._current_request_id = 0
        self._request_progress_bits = []
        self._succeed_num_requests = 0
        self._job_status = JobStatus.CREATED

        # extral metadata
        self._meta_data = {}

    def set_request_executed(self, req_id):
        # This marks the request successfully executed.
        self._request_progress_bits[req_id] = True

    def get_request_bit(self, req_id):
        return self._request_progress_bits[req_id]

    def set_job_status(self, status):
        self._job_status = status

    def get_job_status(self):
        return self._job_status

    def complete_one_request(self, req_id):
        """
        This is called after an inference call. If all requests
        are done, we need to update its status to be completed.
        """
        if not self._request_progress_bits[req_id]:
            self.set_request_executed(req_id)
            self._succeed_num_requests += 1
            if self._succeed_num_requests == self._num_requests:
                self._job_status = JobStatus.COMPLETED

    def next_request_id(self):
        """
        Returns the next request for inference. Due to the propobility
        that some requests are failed, this returns a request that
        are not marked as executed.
        """
        if self._succeed_num_requests == self._num_requests:
            return -1

        req_id = self._current_request_id
        while self._request_progress_bits[req_id]:
            req_id += 1
            if req_id == self._num_requests:
                req_id = 0

        return req_id

    def validate_job(self):
        """
        This handles all validations before successfully creating a job.
        This is also the place connecting other components
        to check connection and status.
        """
        # 1. this makes sure the job id is consistent with storage side
        input_num = _storage.get_job_num_request(self._job_id)
        self._num_requests = input_num
        self._request_progress_bits = [False] * input_num
        if input_num == 0:
            print("Storage side does not have valid request to process.")
            return False

        # 2. check window time is a valid time string
        completion_time_str = self._completion_window_str
        try:
            # For now, this only supports either minute or hour.
            # A mixed of both h and m, like "4h 40m" needs to be extended later.
            time_unit = completion_time_str[-1]
            assert (
                time_unit == "m" or time_unit == "h"
            ), "Time only supports minutes and hours."
            time_str = completion_time_str[0:-1]
            time_window = int(time_str)
            if time_unit == "h":
                time_window *= 60
            self._completion_window = time_window
        except ValueError:
            print("Completion window is not a valid number!")
            return False
        except AssertionError as e:
            print(e)
            return False

        # 3. model endpoint is valid.
        if not self.check_model_endpoint():
            return False

        # 4. Authenticate job and rate limit
        if not self.job_authentication():
            return False

        return True

    def check_model_endpoint(self):
        # [TODO] Xin
        # Check if endpoint is accessible from model proxy.
        return True

    def job_authentication(self):
        # [TODO] xin
        # Check if the job and account is permitted and rate limit.
        return True


class JobManager:
    def __init__(self):
        """
        This manages jobs in three categorical job pools.
        1. _pending_jobs are jobs that are not scheduled yet
        2. _in_progress_jobs are jobs that are in progress now.
        Theses are the input to the job scheduler.
        3. _done_jobs are inactive jobs. This needs to be updated periodically.
        """
        self._pending_jobs = {}
        self._in_progress_jobs = {}
        self._done_jobs = {}

    def create_job(self, job_id, model_endpoint, completion_window):
        """
        This interface is exposed to users to submit a new job and create
        a job accordingly.
        Before calling this, user needs to submit job input to storage first
        to have job ID ready.
        This will validate a job with multiple checking steps.
        """
        job_meta = JobMetaInfo(job_id, model_endpoint, completion_window)
        job_meta._job_status = JobStatus.VALIDATING

        valid_status = job_meta.validate_job()
        if valid_status:
            job_meta._job_status = JobStatus.PENDING
            self._pending_jobs[job_id] = job_meta
        else:
            job_meta._job_status = JobStatus.FAILED

    def cancel_job(self, job_id):
        """
        This is called by user to cancel a created job.
        if this job is pending or done, we directly remove it.
        if the job is in progress, we also need to remove the requests
        in model proxy.
        """
        if job_id in self._pending_jobs:
            meta_data = self._pending_jobs[job_id]
            del self._pending_jobs[job_id]

            meta_data._job_status = JobStatus.CANCELED
            self._done_jobs[job_id] = meta_data

        elif job_id in self._done_jobs[job_id]:
            print(f"Job {job_id} is already canceled or completed!!!")
            return False

        elif job_id in self._in_progress_jobs:
            meta_data = self._in_progress_jobs[job_id]
            # [TODO] Xin
            # Remove all related requests from scheduler and proxy
            meta_data._job_status = JobStatus.CANCELED
            del self._in_progress_jobs[job_id]
            self._done_jobs[job_id] = meta_data
        else:
            print(f"Job {job_id} not exist, maybe submit a job first!")
            return False

        return True

    def get_job_status(self, job_id):
        """
        This retrieves a job's status to users.
        Job scheduler does not need to check job status. It can directly
        check the job pool for scheduling, such as pending_jobs.
        """
        meta_data = None

        if job_id in self._pending_jobs:
            meta_data = self._pending_jobs[job_id]
        elif job_id in self._in_progress_jobs:
            meta_data = self._in_progress_jobs[job_id]
        elif job_id in self._done_jobs:
            meta_data = self._done_jobs[job_id]

        if not meta_data:
            print(f"Job {job_id} can not be found! Maybe create a job first.")
            return None
        return meta_data._job_status

    def start_execute_job(self, job_id):
        """
        This interface should be called by scheduler.
        User is not allowed to choose a job to be scheduled.
        """
        if job_id not in self._pending_jobs:
            print(f"Job {job_id} does not exist. Maybe create it first?")
            return False
        if job_id in self._in_progress_jobs:
            print(f"Job {job_id} has already been launched.")
            return False

        meta_data = self._pending_jobs[job_id]
        self._in_progress_jobs[job_id] = meta_data
        meta_data.set_job_status(JobStatus.IN_PROGRESS)
        del self._pending_jobs[job_id]
        return True

    def get_job_next_request(self, job_id):
        request_id = -1
        if job_id not in self._in_progress_jobs:
            print(f"Job {job_id} has not been scheduled yet.")
            return request_id
        meta_data = self._in_progress_jobs[job_id]

        return meta_data.next_request_id()

    def get_job_window_due(self, job_id):
        if job_id not in self._pending_jobs:
            print(f"Job {job_id} is not in pending state, its due may change.")
            return -1

        meta_data = self._pending_jobs[job_id]
        return meta_data._completion_window

    def get_job_endpoint(self, job_id):
        if job_id in self._pending_jobs:
            meta_data = self._pending_jobs[job_id]
        elif job_id in self._in_progress_jobs:
            meta_data = self._in_progress_jobs[job_id]
        else:
            print(f"Job {job_id} is discarded.")
            return -1
        return meta_data._model_endpoint

    def mark_job_progress(self, job_id, executed_requests):
        """
        This is used to sync job's progress, called by execution proxy.
        It is guaranteed that each request is executed at least once.
        """
        if job_id not in self._in_progress_jobs:
            print(f"Job {job_id} has not started yet.")
            return False

        meta_data = self._in_progress_jobs[job_id]
        request_len = meta_data._num_requests
        invalid_flag = False

        for req_id in executed_requests:
            if req_id < 0 or req_id >= request_len:
                print(f"makr job {job_id} progress, request index out of boundary!")
                invalid_flag = True
                continue
            meta_data.complete_one_request(req_id)

        status = meta_data.get_job_status()
        if status == JobStatus.COMPLETED:
            # Mark the job to be completed if all requests are finished.
            del self._in_progress_jobs[job_id]
            self._done_jobs[job_id] = meta_data
            print(f"Job {job_id} is completed.")
        else:
            self._in_progress_jobs[job_id] = meta_data

        if invalid_flag:
            return False
        return True

    def expire_job(self, job_id):
        """
        This is called by scheduler. When a job arrives at its
        specified due time, scheduler will mark this expired.
        User can not expire a job, but can cancel a job.
        """

        if job_id in self._pending_jobs:
            meta_data = self._pending_jobs[job_id]
            meta_data.set_job_status(JobStatus.EXPIRED)
            self._done_jobs[job_id] = meta_data
            del self._pending_jobs[job_id]
        elif job_id in self._in_progress_jobs:
            # Now a job can not be expired once it gets scheduled, considering
            # that expiring a partial executed job wastes resources.
            # Later we may apply another policy to force a job to expire
            # regardless of its current progress.
            print(f"Job {job_id} was scheduled and it can not expire")
            return False

        elif job_id in self._done_jobs:
            print(f"Job {job_id} is done and this should not happen.")
            return False

        return True

    def sync_job_to_storage(self, jobId):
        """
        [TODO] Xin
        This is used to serialize everything here to storage to make sure
        that job manager can restart it over from storage once it crashes
        or intentional quit.
        """
        pass

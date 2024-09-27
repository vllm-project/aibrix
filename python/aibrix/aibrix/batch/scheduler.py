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

from enum import Enum
import time
import asyncio
import bisect
import queue

# This is the time interval for the sliding window to check.
EXPIRE_INTERVAL = 1


class SchedulePolicy(Enum):
    FIFO = 1


class JobScheduler:
    def __init__(self, policy=SchedulePolicy.FIFO):
        """
        self._jobs_queue are all the jobs.
        self._due_jobs_list stores all potential jobs that can be marked
        as expired jobs.
        self._inactive_jobs are jobs that are already invalid.
        """
        self.interval = EXPIRE_INTERVAL
        self._jobs_queue = queue.Queue()
        self._inactive_jobs = set()
        self._due_jobs_list = []
        # Start sliding process in an async way
        asyncio.create_task(self.job_cleanup_loop())
        self._policy = policy

    def append_job(self, job_id, due_time_seconds):
        # This submits a job to scheduler. The scheduler will determine
        # which job gets executed.
        self._jobs_queue.put(job_id)

        def key_func(x):
            return x[1]

        current_time = time.time()
        due_time = current_time + due_time_seconds
        item = (job_id, due_time)
        index = bisect.bisect_left(
            [key_func(t) for t in self._due_jobs_list], key_func(item)
        )
        self._due_jobs_list.insert(index, item)

    def schedule_get_job(self):
        # Scheduler outputs a job to be processed following the specified policy.
        job_id = None

        # [TODO] use class abstraction for SchedulingPolicy
        if self._policy == SchedulePolicy.FIFO:
            if not self._jobs_queue.empty():
                job_id = self._jobs_queue.get()

            # Every time when popping a job from queue,
            # we check if this job is in active state.
            while (
                job_id
                and job_id in self._inactive_jobs
                and not self._jobs_queue.empty()
            ):
                job_id = self._jobs_queue.get()

        else:
            print("Unsupported scheduling policy!")

        return job_id

    def get_inactive_jobs(self):
        return self._inactive_jobs

    async def expire_jobs(self):
        # This is to expire jobs based on specified due time per job.
        if self._policy == SchedulePolicy.FIFO:
            current_time = time.time()
            idx = 0
            while (
                idx < len(self._due_jobs_list)
                and self._due_jobs_list[idx][1] <= current_time
            ):
                idx += 1

            print("Number of expired jobs is ", idx)
            for i in range(idx):
                # Update job's status to job manager
                job_id = self._due_jobs_list[i][0]
                self._inactive_jobs.add(job_id)
                print("======> ", job_id, "expired.")
            self._due_jobs_list = self._due_jobs_list[idx:]
        else:
            print("Unsupported scheduling policy!")

    async def job_cleanup_loop(self):
        """
        This is a long-running process to check if jobs have expired or not.
        """
        round_id = 0
        while True:
            start_time = time.time()  # Record start time
            await self.expire_jobs()  # Run the process
            elapsed_time = time.time() - start_time  # Calculate elapsed time
            time_to_next_run = max(
                0, self.interval - elapsed_time
            )  # Calculate remaining time
            print("Sliding, round: ", round_id)
            round_id += 1
            await asyncio.sleep(time_to_next_run)  # Wait for the remaining time

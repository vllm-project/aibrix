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

from aibrix.batch.constant import EXPIRE_INTERVAL, DEFAULT_JOB_POOL_SIZE
from aibrix.batch.job_manager import JobStatus

class SchedulePolicy(Enum):
    FIFO = 1


class JobScheduler:
    def __init__(self, job_manager, pool_size, policy=SchedulePolicy.FIFO,):
        """
        self._jobs_queue are all the jobs.
        self._due_jobs_list stores all potential jobs that can be marked
        as expired jobs.
        self._inactive_jobs are jobs that are already invalid.
        """
        self._job_manager = job_manager
        self.interval = EXPIRE_INTERVAL
        self._jobs_queue = queue.Queue()
        self._inactive_jobs = set()
        self._due_jobs_list = []
        self._running_job_pool = [None] * pool_size
        self._running_job_idx = 0
        self._job_pool_size = pool_size
        # Start sliding process in an async way
        asyncio.create_task(self.job_cleanup_loop())
        self._policy = policy

    def configure_job_pool_size(self, new_pool_size):
        assert(new_pool_size >= 1)
        while new_pool_size > len(self._running_job_pool):
            self._running_job_pool.append(None)
        
        self._job_pool_size = new_pool_size

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

    def schedule_next_job(self):
        # Scheduler outputs a job to be processed following the specified policy.
        job_id = None

        # [TODO] use class abstraction for SchedulingPolicy
        if self._policy == SchedulePolicy.FIFO:
            if self._jobs_queue.empty():
                print("Job scheduler is waiting jobs coming ......")
                time.sleep(1)
                return job_id
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

        self._job_manager.start_execute_job(job_id)
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

    def round_robin_get_job(self):
        self._running_job_idx =  self._running_job_idx % len(self._running_job_pool)
        job_id = self._running_job_pool[self._running_job_idx]
        
        # if found a available slot, shrink job pool size.
        while not job_id and len(self._running_job_pool) > self._job_pool_size:
            del self._running_job_pool[self._running_job_idx]
            temp_id = self._running_job_idx % len(self._running_job_pool)
            self._running_job_idx = temp_id
            job_id = self._running_job_pool[self._running_job_idx]
            print("Shrink job pool size by 1 in JobScheduler!", len(self._running_job_pool), self._job_pool_size)

        num_empty_slots = 0
        # two conditions are:
        # 1. if there is a slot available. schedule a new job in.
        # 2. if there is real job in the slot, we need to check its status
        while not job_id or (job_id and self._job_manager.get_job_status(job_id) == JobStatus.COMPLETED):
            # Schedule a new job and fill in the slot
            job_id = None
            new_job_id = self.schedule_next_job()
            if not new_job_id :
                num_empty_slots += 1
                if num_empty_slots == DEFAULT_JOB_POOL_SIZE:
                    break
                else:
                    continue
            
            self._running_job_pool[self._running_job_idx] = new_job_id
            self._running_job_idx += 1
            self._running_job_idx =  self._running_job_idx % len(self._running_job_pool)
        
            job_id = self._running_job_pool[self._running_job_idx]
        
        if not job_id:
            print("No job is found!")
        return job_id
        
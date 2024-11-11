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
import bisect
import queue
import time
from abc import ABC, abstractmethod
from enum import Enum

from aibrix.batch.constant import DEFAULT_JOB_POOL_SIZE, EXPIRE_INTERVAL
from aibrix.batch.job_manager import JobStatus


class SchedulePolicy(Enum):
    FIFO = 1


class CCInterface(ABC):
    @abstractmethod
    def update_job_pool_size(self, new_pool_size):
        pass

    @abstractmethod
    def shrink_resource(self):
        pass

    @abstractmethod
    def grow_resource(self):
        pass


class BasicCongestionControl(CCInterface):
    def __init__(self, pool_size):
        self._running_job_pool = [None] * pool_size
        self._job_pool_size = pool_size
        self._running_job_idx = 0

    def tighten_jobs(self):
        """
        We are intentional to move all current in-progress jobs
        to the front part of the pool.
        Two advantages of doing this:
        1. it facilitates the following resource adjustment by update the tail of the pool.
        2. it guarantees the jobs' order if necessary.
        """
        current_job_id = self._running_job_pool[self._running_job_idx]

        # Move all jobs to the front part of pool to maitain the order.
        slow_id, fast_id = 0, 0
        while slow_id < len(self._running_job_pool) and fast_id < len(
            self._running_job_pool
        ):
            job_id = self._running_job_pool[fast_id]
            if not job_id:
                fast_id += 1
            else:
                if fast_id != slow_id:
                    self._running_job_pool[slow_id] = job_id
                    self._running_job_pool[fast_id] = None
                fast_id += 1
                slow_id += 1

        # We still need to track the location of current job.
        for i in range(slow_id):
            if self._running_job_pool[i] == current_job_id:
                self._running_job_idx = i
                break

    def update_job_pool_size(self, new_pool_size):
        """
        This is the call exposed to external components.
        [TODO] Spawn a new process to monitor resource and apply
        necessary adjustment from here.
        """
        # This is for the actual resource adjustment.
        assert new_pool_size >= 1
        self._job_pool_size = new_pool_size

        if self._job_pool_size < len(self._running_job_pool):
            self.shrink_resource()

        if self._job_pool_size > len(self._running_job_pool):
            self.grow_resource()

    def shrink_resource(self):
        # Alway move jobs to front.
        self.tighten_jobs()

        while (
            len(self._running_job_pool) > self._job_pool_size
            and not self._running_job_pool[-1]
        ):
            del self._running_job_pool[-1]

        # Note that it is still possible that we can not shrink it
        # when there are jobs in progress
        print(
            "Shrink job pool size in JobScheduler!",
            len(self._running_job_pool),
            self._job_pool_size,
        )

    def grow_resource(self):
        # Alway move jobs to front.
        self.tighten_jobs()

        # This does not influence the relative order of jobs with self._running_job_idx
        while len(self._running_job_pool) < self._job_pool_size:
            self._running_job_pool.append(None)


class JobScheduler:
    def __init__(
        self,
        job_manager,
        pool_size,
        cc_controller=BasicCongestionControl(DEFAULT_JOB_POOL_SIZE),
        policy=SchedulePolicy.FIFO,
    ):
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

        self._CC_controller = cc_controller
        self._current_pool_size = self._CC_controller._job_pool_size
        # Start the loop process in an async way
        asyncio.create_task(self.job_cleanup_loop())
        self._policy = policy

    def configure_job_pool_size(self, new_pool_size):
        # Here it just set the pool size, later when it starts scheduling
        # we will update appropriate slots correspondingly.
        self._current_pool_size = new_pool_size

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
        # Step 1
        # Before scheduling any new jobs, we need to check the status of previous
        # jobs and update it accordingly.
        for i in range(len(self._CC_controller._running_job_pool)):
            if not self._CC_controller._running_job_pool[i]:
                continue
            job_id = self._CC_controller._running_job_pool[i]
            # Do not schedule new job in since we need to adjust capacity
            # based on new pool size representing how much underlying resource.
            if self._job_manager.get_job_status(job_id) == JobStatus.COMPLETED:
                self._CC_controller._running_job_pool[i] = None

        # Step 2, after the jobs' status are updated,
        # we need to iterate over all slots for next job.
        # By default these jobs' priority is higher than new jobs.
        next_job_id = None
        for i in range(len(self._CC_controller._running_job_pool)):
            temp_idx = self._CC_controller._running_job_idx + 1
            temp_idx = temp_idx % len(self._CC_controller._running_job_pool)
            self._CC_controller._running_job_idx = temp_idx
            job_id = self._CC_controller._running_job_pool[temp_idx]
            if not job_id:
                continue
            else:
                next_job_id = job_id
                break
        if not next_job_id:
            self._CC_controller._running_job_idx = 0

        # Step 3, update job pool size with controller.
        self._CC_controller.update_job_pool_size(self._current_pool_size)

        # Step 4, if there is available slot, schedule new job in from queue.
        start_offset = len(self._CC_controller._running_job_pool)
        for i in range(len(self._CC_controller._running_job_pool)):
            if not self._CC_controller._running_job_pool[i]:
                start_offset = i
                break

        while start_offset < len(self._CC_controller._running_job_pool):
            new_job_id = self.schedule_next_job()
            if not new_job_id:
                break

            if not next_job_id:
                next_job_id = new_job_id
            self._CC_controller._running_job_pool[start_offset] = new_job_id
            start_offset += 1

        if not next_job_id:
            print("No job is found!")

        return next_job_id

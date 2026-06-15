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

from typing import Protocol, runtime_checkable

from aibrix.batch.job_driver.runtime import Endpoint
from aibrix.batch.job_entity import BatchJob


@runtime_checkable
class JobDriver(Protocol):
    """One batch job, run through an explicit, multi-phase lifecycle.

    ``execute`` orchestrates the phases inside the runtime's
    provision/teardown session:

        validate_job -> prepare_job -> run_job -> finalize_job

    (with ``provision / wait_ready / connect / teardown`` owned by the Runtime).
    the interface is the lifecycle, ``BaseJobDriver`` implements the common
    behavior; a backend overrides only the phases that differ.
    """

    async def validate_job(self, job: BatchJob) -> None:
        """Pre-flight validation: input file exists.

        Raises:
            BatchJobError: If the job is invalid.
        """
        ...

    async def prepare_job(self, job: BatchJob) -> BatchJob:
        """Prepare the job's output/error multipart uploads.
        provision resources for some specific runtime like kubernetes.
        Normally, the resource provision is done in resource manager(batch's backend)
        """
        ...

    async def run_job(self, job_id: str, endpoint: Endpoint) -> BatchJob:
        """Send job's requests against endpoint once a runtime endpoint
        become ready.

        ``endpoint.source`` is the reachable engine source (None when the
        worker self-hosts inference). Returns the job in its post-execution
        state (FINALIZING when complete).
        """
        ...

    async def finalize_job(self, job: BatchJob) -> BatchJob:
        """Aggregate outputs/errors and mark the job done.
        Terminate resources if the the provision is managed inside runtime.
        """
        ...

    async def execute(self, job_id: str) -> None:
        """Orchestrate the full lifecycle: session(provision/teardown) wraps
        prepare -> run_job -> finalize.

        Raises:
            RuntimeError: If something prevents all jobs from executing.
            BatchJobError: If something prevents one job from executing.
        """
        ...

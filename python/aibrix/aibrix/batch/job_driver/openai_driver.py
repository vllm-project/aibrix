from typing import Optional

from aibrix.batch.job_entity import BatchJob, JobEntityManager, ResourceDetail

from .driver import JobDriver
from .progress_manager import JobProgressManager


class OpenAIJobDriver(JobDriver):
    def __init__(
        self,
        progress_manager: JobProgressManager,
        entity_manager: JobEntityManager,
        job: BatchJob,
        resource: ResourceDetail,
        deadline: Optional[int],
    ) -> None:
        if not resource.endpoint_cluster:
            raise ValueError("OpenAI resource endpoint is required")

        self._progress_manager = progress_manager
        self._entity_manager = entity_manager
        self._job = job
        self._resource = resource
        self._deadline = deadline

    async def execute_job(self, job_id):
        raise NotImplementedError("OpenAIJobDriver execution is not implemented")

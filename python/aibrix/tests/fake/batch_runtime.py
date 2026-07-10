from __future__ import annotations

from typing import Any, Optional

from aibrix.batch.job_driver.runtime.base import RuntimeBase
from aibrix.batch.job_entity import BatchJob, JobRuntimeRef
from aibrix.context import InfrastructureContext


class FakeRuntime(RuntimeBase):
    """Shared fake provisioning runtime for batch job-driver tests."""

    provisions = True

    def __init__(
        self,
        *,
        runtime_key: str = "fake",
        reconnect_handle: Any = "fake-handle",
        reconnect_job_id: Optional[str] = None,
    ) -> None:
        super().__init__(InfrastructureContext())
        self.runtime_key = runtime_key
        self.reconnect_handle = reconnect_handle
        self.reconnect_job_id = reconnect_job_id
        self.cleanup_job_ids: list[str] = []
        self.reconnect_calls: list[tuple[str, JobRuntimeRef]] = []
        self.wait_ready_handles: list[Any] = []
        self.teardown_handles: list[Any] = []

    def _get_runtime_key(self, job: BatchJob) -> str:
        del job
        return self.runtime_key

    async def _reconnect(
        self, job: BatchJob, job_id: str, runtimeRef: JobRuntimeRef
    ) -> Any | None:
        del job
        self.cleanup_job_ids.append(job_id)
        self.reconnect_calls.append((job_id, runtimeRef))
        if self.reconnect_job_id is not None and job_id != self.reconnect_job_id:
            return None
        return self.reconnect_handle

    async def _wait_ready(self, handle: Any, wait_mode: str = "provision") -> None:
        del wait_mode
        self.wait_ready_handles.append(handle)

    async def _teardown(self, handle: Any) -> None:
        self.teardown_handles.append(handle)

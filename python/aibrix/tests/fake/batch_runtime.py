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

    async def _load_handle(
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

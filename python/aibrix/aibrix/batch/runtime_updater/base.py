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

import logging
from abc import ABC, abstractmethod

from aibrix.batch.job_entity.batch_job import BatchJob

logger = logging.getLogger(__name__)


class JobStateRuntimeUpdater(ABC):
    """Update runtime-side metadata to reflect the authoritative BatchJob.

    Implementations write a small projection of BatchJob state onto the
    runtime that executes the job: K8s Job annotations, RunPod tags, EC2
    tags, etc. This view is for operator UX and reconciliation/debug; it
    is never read back as a source of truth — the ``BatchJobStore``
    remains authoritative.

    Failures are best-effort: implementations should log and swallow
    transient errors rather than propagate them, so that a successful
    store write is not rolled back by a failed runtime update. Callers
    are expected to invoke ``update`` *after* a successful store write.

    Note: this abstraction is provisional and may be folded into the
    provisioner layer once that abstraction settles.
    """

    @abstractmethod
    async def update(self, job: BatchJob) -> None:
        """Update runtime-side metadata to match ``job``. Best-effort."""


class NoOpJobStateRuntimeUpdater(JobStateRuntimeUpdater):
    """Updater that drops all writes.

    Useful for tests and providers without runtime-side metadata
    (e.g., bare worker pools).
    """

    async def update(self, job: BatchJob) -> None:
        return None

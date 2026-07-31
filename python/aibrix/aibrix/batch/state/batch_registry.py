# Copyright 2026 The Aibrix Team.
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
"""BatchRegistry: the service-level in-memory job registry.

This is the *collection* of all jobs the process knows about, partitioned into
three pools by lifecycle stage — pending (awaiting scheduling), in-progress
(active; holds JobMetaInfo), and done (terminal). It is the source-of-truth
registry that the API (list/get), the scheduler, and the orchestrator read.

It deliberately holds no policy: *which* pool a job belongs to (which can depend
on whether a scheduler is wired) and *when* to move it are decided by the
orchestrator (BatchManager); the store just holds the pools and answers reads.
Extracting it out of BatchManager makes the service-level "registry" concern a
named component instead of three loose dicts on the god object.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Dict, List, Optional

from aibrix.batch.job_entity import BatchJob

if TYPE_CHECKING:
    from aibrix.batch.state.job_meta_info import JobMetaInfo


class BatchRegistry:
    def __init__(self) -> None:
        # CREATED, awaiting scheduling.
        self._pending: Dict[str, BatchJob] = {}
        # Active; entries are JobMetaInfo (BatchJob + tracker + driver).
        self._in_progress: Dict[str, JobMetaInfo] = {}
        # Terminal (completed / failed / expired / cancelled).
        self._done: Dict[str, BatchJob] = {}

    @property
    def pending(self) -> Dict[str, BatchJob]:
        return self._pending

    @property
    def in_progress(self) -> Dict[str, JobMetaInfo]:
        return self._in_progress

    @property
    def done(self) -> Dict[str, BatchJob]:
        return self._done

    def get(self, job_id: str) -> Optional[BatchJob]:
        """Return the job from whichever pool holds it, or None."""
        if job_id in self._pending:
            return self._pending[job_id]
        if job_id in self._in_progress:
            return self._in_progress[job_id]
        if job_id in self._done:
            return self._done[job_id]
        return None

    def list(self) -> List[BatchJob]:
        """All known jobs across pools (unordered)."""
        jobs: List[BatchJob] = []
        jobs.extend(self._pending.values())
        jobs.extend(self._in_progress.values())
        jobs.extend(self._done.values())
        return jobs

    def reset_runtime_state(self) -> None:
        self._pending.clear()
        self._in_progress.clear()
        self._done.clear()

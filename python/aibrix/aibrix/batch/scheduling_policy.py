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
"""SchedulingPolicy: the order in which queued jobs are picked to run.

This is extracted as a strategy so the BatchScheduler's mechanism
(admission, expiry, the run loop, dispatch) stays fixed while the *ordering*
plugs in. The policy owns only "which job next"; it does not validate, run, or
expire.
"""

from __future__ import annotations

import queue
from typing import Optional, Protocol, runtime_checkable


@runtime_checkable
class SchedulingPolicy(Protocol):
    """Ordering strategy over queued job ids."""

    def add(self, job_id: str) -> None:
        """Enqueue a job for future selection."""
        ...

    def next(self) -> Optional[str]:
        """Pop and return the next job in policy order, or None if empty."""
        ...

    def empty(self) -> bool:
        """Whether there are no queued jobs."""
        ...


class FIFOScheduling:
    """First-in, first-out ordering (the default)."""

    def __init__(self) -> None:
        self._queue: "queue.Queue[str]" = queue.Queue()

    def add(self, job_id: str) -> None:
        self._queue.put(job_id)

    def next(self) -> Optional[str]:
        if self._queue.empty():
            return None
        return self._queue.get()

    def empty(self) -> bool:
        return self._queue.empty()

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
"""JobProgressTracker: the per-request progress ledger for an in-progress job.

Extracted out of the legacy ``JobMetaInfo`` so the persistence model (BatchJob)
and the runtime progress state stop being a single object. The tracker is a
plain component — it does NOT inherit BatchJob; it holds a reference to the job
it tracks and mutates ``job.status.request_counts`` / ``job.status.state``.

The legacy metastore path relies on a serial "launch before complete" streaming
protocol: ``next_request_id`` hands out ids (and discovers ``total`` lazily when
0), and ``complete_one_request`` marks completion and flips the job to
FINALIZING once all requests are accounted for.

The concurrent worker path still uses the same completion bitmap. It does not
need a separate claim ledger because v1 runs one worker per job; in-flight
request ids live in that worker's tasks, and completed requests are recorded
only after output/error has been written.
"""

from __future__ import annotations

from datetime import datetime, timezone

from aibrix.batch.job_entity import BatchJob, BatchJobState


class JobProgressTracker:
    def __init__(self, job: BatchJob):
        self._job = job
        self._next_request_id: int = 0
        # request_id < _min_unexecuted_id are all completed.
        self._min_unexecuted_id: int = 0
        self._no_total: bool = job.status.request_counts.total == 0
        # Initialize progress bits based on total request count.
        self._request_progress_bits: list[bool] = [
            False
        ] * job.status.request_counts.total

    @property
    def _status(self):
        return self._job.status

    def _ensure_capacity(self, size: int) -> None:
        while len(self._request_progress_bits) < size:
            self._request_progress_bits.append(False)

    def set_request_executed(self, req_id):
        self._ensure_capacity(max(req_id + 1, self._status.request_counts.total))
        # This marks the request successfully executed.
        self._ensure_request_capacity(req_id)
        self._request_progress_bits[req_id] = True
        # Check if self._min_unexecuted_id need to be updated
        if req_id != self._min_unexecuted_id:
            return
        # Update self._min_unexecuted_id
        for i in range(self._min_unexecuted_id, self._status.request_counts.total):
            if self._request_progress_bits[i]:
                self._min_unexecuted_id = i + 1
            else:
                break

    def get_request_bit(self, req_id):
        self._ensure_request_capacity(req_id)
        return self._request_progress_bits[req_id]

    def complete_one_request(self, req_id, failed: bool = False):
        """
        This is called after an inference call. If all requests
        are done, we need to update its status to be completed.
        """
        if req_id == self._status.request_counts.total:
            # Fix total count and launched count on total decided.
            self._status.request_counts.total -= 1
            if self._status.request_counts.launched > self._status.request_counts.total:
                self._status.request_counts.launched = self._status.request_counts.total
            self._no_total = False
            self._trim_request_bits(self._status.request_counts.total)
        else:
            self._ensure_request_capacity(req_id)
        if (
            req_id < len(self._request_progress_bits)
            and not self._request_progress_bits[req_id]
        ):
            self.set_request_executed(req_id)
            if failed:
                self._status.request_counts.failed += 1
            else:
                self._status.request_counts.completed += 1

        # Test all done
        if (
            not self._no_total
            and self._status.request_counts.completed
            + self._status.request_counts.failed
            == self._status.request_counts.total
        ):
            self._status.finalizing_at = datetime.now(timezone.utc)
            self._status.state = BatchJobState.FINALIZING

    def next_request_id(self) -> int:
        """
        Returns the next request_id for inference. Due to the propobility
        that some requests are failed, this returns a request that
        are not marked as executed. We used round robin touch all requests
        first and then start another round.

        Returns:
            int: next_request_id or -1 if job is done
        """
        if (
            not self._no_total
            and self._status.request_counts.completed
            + self._status.request_counts.failed
            == self._status.request_counts.total
        ):
            return -1

        req_id = self._next_request_id
        # If total has confirmed and not all request executed, start next round.
        if not self._no_total and req_id == self._status.request_counts.total:
            req_id = self._min_unexecuted_id

        # In case total has not confirmed, expand _request_progress_bits if necessary.
        if req_id >= len(self._request_progress_bits):
            self._ensure_request_capacity(req_id)

        # Skip executed requests.
        while self._request_progress_bits[req_id]:
            req_id += 1
            if not self._no_total and req_id == self._status.request_counts.total:
                req_id = self._min_unexecuted_id
            if req_id >= len(self._request_progress_bits):
                self._ensure_request_capacity(req_id)

        # Update _next_request_id
        self._next_request_id = req_id
        # Update launched request count
        if req_id >= self._status.request_counts.launched:
            self._status.request_counts.launched = req_id + 1
        if req_id >= self._status.request_counts.total:
            self._status.request_counts.total = req_id + 1
        return req_id

    def _ensure_request_capacity(self, req_id: int) -> None:
        while req_id >= len(self._request_progress_bits):
            self._request_progress_bits.append(False)

    def _trim_request_bits(self, total: int) -> None:
        del self._request_progress_bits[total:]

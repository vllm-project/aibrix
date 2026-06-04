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
"""EntityManagerBridge: the impedance adapter between the orchestrator and the
persistence port (JobEntityManager).

Two things here are genuinely "bridge" concerns — an async event source talking
to a synchronous caller, and intent-to-verb translation — so they live apart
from the orchestrator's state machine:

  * ``submit_and_wait`` turns "create a job" (sync, returns the id) into
    "submit + await the committed event that carries the id". ``resolve_creation``
    is the other half, called from the committed handler.
  * ``persist_status`` translates "persist this status" into the right
    entity-manager verb (update vs cancel).

The core persistence/reconciliation logic is moved verbatim; only its home
changed.
"""

from __future__ import annotations

import asyncio
from typing import Callable, Coroutine, Dict, Optional

from aibrix.batch.job_entity import BatchJob, BatchJobSpec, BatchJobState
from aibrix.batch.state.job_entity_manager import JobEntityManager
from aibrix.logger import init_logger

logger = init_logger(__name__)


class EntityManagerBridge:
    def __init__(self) -> None:
        self._em: Optional[JobEntityManager] = None
        # session_id -> future resolved by the committed event with the job id.
        self._creating_jobs: Dict[str, "asyncio.Future[str]"] = {}

    def bind(
        self,
        entity_manager: JobEntityManager,
        on_committed: Callable[[BatchJob], Coroutine],
        on_updated: Callable[[BatchJob, BatchJob], Coroutine],
        on_deleted: Callable[[BatchJob], Coroutine],
    ) -> None:
        """Attach the port and register the orchestrator's lifecycle handlers."""
        self._em = entity_manager
        entity_manager.on_job_committed(on_committed)
        entity_manager.on_job_updated(on_updated)
        entity_manager.on_job_deleted(on_deleted)

    async def submit_and_wait(
        self,
        session_id: str,
        job_spec: BatchJobSpec,
        request_count: int,
        timeout: float,
    ) -> str:
        """Submit a job and wait for the committed event to deliver its id.

        Note: even on timeout the job may still have been created; we do nothing
        to handle that — list_jobs() shows the full set.
        """
        assert self._em is not None
        job_future: "asyncio.Future[str]" = asyncio.Future()
        self._creating_jobs[session_id] = job_future

        try:
            submit_task = asyncio.create_task(
                self._em.submit_job(session_id, job_spec, request_count=request_count)
            )

            # If the submit task fails before the future is resolved (e.g.
            # RenderError on a malformed BatchJobSpec), forward the real
            # exception so wait_for() returns immediately with a useful error
            # instead of stalling for the full timeout. Without this, every
            # render-time rejection looked like a 408 to the client.
            def _propagate_submit_failure(t: "asyncio.Task") -> None:
                if t.cancelled():
                    return
                exc = t.exception()
                if exc is None or job_future.done():
                    return
                job_future.set_exception(exc)

            submit_task.add_done_callback(_propagate_submit_failure)

            try:
                job_id = await asyncio.wait_for(job_future, timeout=timeout)
            except asyncio.TimeoutError:
                logger.error(
                    "Job creation timeout", session_id=session_id, timeout=timeout
                )  # type: ignore[call-arg]
                submit_task.cancel()
                try:
                    await submit_task
                except asyncio.CancelledError:
                    pass
                raise

            await submit_task

            logger.info(
                "Job created successfully", session_id=session_id, job_id=job_id
            )  # type: ignore[call-arg]
            return job_id

        except Exception as e:
            if not isinstance(e, asyncio.TimeoutError):
                logger.error(
                    "Job creation failed",
                    session_id=session_id,
                    error=str(e),
                    exc_info=True,
                )  # type: ignore[call-arg]
            raise
        finally:
            self._creating_jobs.pop(session_id, None)

    def resolve_creation(self, session_id: str, job_id: str) -> bool:
        """Resolve a pending create future with the committed job's id. Returns
        whether the committed handler should proceed (False = no pending future:
        a timed-out or already-created job)."""
        future = self._creating_jobs.get(session_id)
        if future is not None and not future.done():
            future.set_result(job_id)
            logger.debug(
                "Job creation future resolved",
                session_id=session_id,
                job_id=job_id,
            )  # type: ignore[call-arg]
            return True
        logger.warning(
            "Job creation timeout or already created",
            session_id=session_id,
            job_id=job_id,
        )  # type: ignore[call-arg]
        return False

    async def persist_status(self, job: BatchJob, old_job: BatchJob) -> None:
        """Translate 'persist this status' into the right entity-manager verb:
        a finalizing/clean status is an update; a status carrying a cancel/fail
        goes through the cancel channel."""
        assert self._em is not None
        if (
            old_job.status.state == BatchJobState.FINALIZING
            or old_job.status.errors is None
        ):
            await self._em.update_job_status(job)
        else:
            await self._em.cancel_job(job)

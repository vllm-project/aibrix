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
from typing import Any, Callable, Coroutine, Dict, List, Optional

import kopf
from kubernetes import client
from kubernetes.client.rest import ApiException

from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    JobAnnotationKey,
    JobEntityManager,
    k8s_job_to_batch_job,
)
from aibrix.batch.manifest import JobManifestRenderer
from aibrix.batch.storage.batch_metastore import (
    delete_batch_job,
    put_batch_job,
)
from aibrix.batch.template import ProfileRegistry, TemplateRegistry
from aibrix.logger import init_logger

# If you installed kopf[uvloop], kopf will likely set this up.
# Otherwise, you can explicitly set it:
# import uvloop
# asyncio.set_event_loop_policy(uvloop.EventLoopPolicy())


# Global logger for standalone functions
logger = init_logger(__name__)

# Global JobCache instance for kopf handlers
_global_job_cache: Optional["JobCache"] = None


def set_global_job_cache(job_cache: "JobCache") -> None:
    """Set the global job cache instance for kopf handlers."""
    global _global_job_cache
    _global_job_cache = job_cache


def get_global_job_cache() -> Optional["JobCache"]:
    """Get the global job cache instance."""
    return _global_job_cache


class JobCache(JobEntityManager):
    """Kubernetes-based job cache implementing JobEntityManager interface.

    This class uses kopf to watch Kubernetes Job resources and maintains
    an in-memory cache of BatchJob objects. It implements the JobEntityManager
    interface to provide standardized job management capabilities.
    """

    def __init__(
        self,
        template_registry: TemplateRegistry,
        profile_registry: ProfileRegistry,
    ) -> None:
        """Initialize the job cache.

        Args:
            template_registry: Loaded ModelDeploymentTemplate registry.
                Caller must have invoked reload() at least once.
            profile_registry: Loaded BatchProfile registry. Caller must
                have invoked reload() at least once.

        Every status mutation is written via
        ``batch_metastore.put_batch_job`` and the metadata API serves
        point reads from there; K8s Job annotations carry only the
        immutable spec the worker reads via downward API. Status-write
        failures are propagated by ``_put_to_store`` so callers
        (including the synchronous /cancel path) see store outages
        instead of silently leaving the in-memory view ahead of disk.
        The kopf ADDED seed write keeps the default swallow because
        the next event re-emits the document.
        """
        super().__init__()

        # Cache of BatchJob objects keyed by batch ID (K8s UID)
        self.active_jobs: Dict[str, BatchJob] = {}

        # Register this instance as the global job cache for kopf handlers
        set_global_job_cache(self)

        # Callback handlers for job lifecycle events
        self._job_committed_handler: Optional[
            Callable[[BatchJob], Coroutine[Any, Any, bool]]
        ] = None
        self._job_updated_handler: Optional[
            Callable[[BatchJob, BatchJob], Coroutine[Any, Any, bool]]
        ] = None
        self._job_deleted_handler: Optional[
            Callable[[BatchJob], Coroutine[Any, Any, bool]]
        ] = None

        # Both registries are required; pass empty registries explicitly if
        # operating in a degraded / test mode.
        self._template_registry = template_registry
        self._profile_registry = profile_registry
        self._renderer = JobManifestRenderer(template_registry, profile_registry)
        logger.info(
            "JobCache initialized with ConfigMap-driven renderer",
            active_templates=len(template_registry.all_active()),
            profiles=len(profile_registry.all()),
            default_profile=profile_registry.default_name(),
        )  # type: ignore[call-arg]

        self.batch_v1_api = client.BatchV1Api()
        self.core_v1_api = client.CoreV1Api()

    def is_scheduler_enabled(self) -> bool:
        """Check if JobEntityManager has own scheduler enabled."""
        return True

    async def _put_to_store(
        self, job: BatchJob, *, op: str, propagate: bool = False
    ) -> None:
        """Persist a BatchJob status mutation to the batch metastore.

        Status-write callers (``update_job_status``, ``update_job_ready``,
        ``cancel_job``) pass ``propagate=True`` because the metastore is
        the only persistent record of those mutations: a swallowed
        failure would leave ``active_jobs`` ahead of disk and surface
        as stale reads after a restart. Eventual-consistency callers
        (the kopf ADDED handler that seeds the initial document) keep
        the default ``propagate=False`` since a missed write will be
        retried the next time kopf re-emits the event.

        ``op`` tags the originating operation for log correlation.
        """
        batch_id = job.status.job_id
        try:
            await put_batch_job(batch_id, job)
        except Exception as e:
            logger.warning(  # type: ignore[call-arg]
                "BatchJob metastore write failed",
                job_id=batch_id,
                op=op,
                error=str(e),
                error_type=type(e).__name__,
            )
            if propagate:
                raise

    async def _delete_from_store(self, job: BatchJob) -> None:
        """Remove a BatchJob document from the metastore.

        Best-effort cleanup that follows a real K8s ``delete`` op; the
        K8s deletion is the authoritative signal, the metastore entry
        is a leaked artifact at worst. Errors are logged and swallowed.
        """
        batch_id = job.status.job_id
        try:
            await delete_batch_job(batch_id)
        except Exception as e:  # pragma: no cover - defensive
            logger.warning(  # type: ignore[call-arg]
                "BatchJob metastore delete failed",
                job_id=batch_id,
                error=str(e),
                error_type=type(e).__name__,
            )

    # Implementation of JobEntityManager abstract methods
    def get_job(self, job_id: str) -> Optional[BatchJob]:
        """Get cached job detail by batch id.

        Args:
            job_id: Batch id (Kubernetes UID).

        Returns:
            BatchJob: Job detail.

        Raises:
            KeyError: If job with given job_id is not found.
        """
        if job_id not in self.active_jobs:
            return None
        return self.active_jobs[job_id]

    def list_jobs(self) -> List[BatchJob]:
        """List unarchived jobs that cached locally.

        Returns:
            List[BatchJob]: List of jobs.
        """
        return list(self.active_jobs.values())

    async def submit_job(
        self,
        session_id: str,
        job_spec: BatchJobSpec,
        job_name: Optional[str] = None,
        parallelism: Optional[int] = None,
        prepared_job: Optional[BatchJob] = None,
    ) -> None:
        """Submit job by creating a Kubernetes Job.

        Args:
            job_spec: BatchJobSpec to submit to Kubernetes.
            job_name: Optional job name, will generate one if not provided.
            parallelism: Optional parallelism for the job, default to None and follow template settings.
            prepared_job: Optional BatchJob with file IDs to add to pod annotations.

        Raises:
            RuntimeError: If Kubernetes client is not available.
            ApiException: If Kubernetes API call fails.
        """
        if not self.batch_v1_api:
            raise RuntimeError("Kubernetes client not available")

        # Initialize before the try so that any exception raised by
        # _batch_job_spec_to_k8s_job (manifest rendering) doesn't fall
        # into the outer ``except`` with ``namespace`` unbound, which
        # masks the real failure with an UnboundLocalError.
        namespace = "default"
        try:
            # Convert BatchJobSpec to Kubernetes Job manifest
            k8s_job = self._batch_job_spec_to_k8s_job(
                session_id, job_spec, job_name, parallelism, prepared_job
            )

            # Get namespace from k8s_job, use default if not specified
            namespace = k8s_job["metadata"].get("namespace") or "default"

            logger.info(  # type: ignore[call-arg]
                "Submitting job to Kubernetes",
                namespace=namespace,
                input_file_id=job_spec.input_file_id,
                endpoint=job_spec.endpoint,
                opts=job_spec.opts,
                job_name=k8s_job["metadata"]["name"],
            )  # type: ignore[call-arg]

            # Submit job asynchronously
            async_result = await asyncio.to_thread(
                self.batch_v1_api.create_namespaced_job,
                namespace=namespace,
                body=k8s_job,
                async_req=True,
            )

            # Create a task to check job result asynchronously without blocking
            async def check_job_result():
                try:
                    job_result = await asyncio.to_thread(async_result.get)
                    logger.info(  # type: ignore[call-arg]
                        "Job successfully submitted to Kubernetes",
                        namespace=namespace,
                        job_name=job_result.metadata.name,
                        job_uid=job_result.metadata.uid,
                    )  # type: ignore[call-arg]
                    return job_result
                except ApiException as e:
                    logger.error(  # type: ignore[call-arg]
                        "Kubernetes API error during job submission",
                        input_file_id=job_spec.input_file_id,
                        endpoint=job_spec.endpoint,
                        error=str(e),
                        status_code=e.status,
                        reason=e.reason,
                        namespace=namespace,
                        operation="submit_job",
                    )  # type: ignore[call-arg]
                except Exception as e:
                    # This could catch errors from async_result.get() or other unexpected issues
                    error_type = type(e).__name__
                    logger.error(  # type: ignore[call-arg]
                        "Unexpected error during job submission",
                        input_file_id=job_spec.input_file_id,
                        endpoint=job_spec.endpoint,
                        namespace=namespace,
                        error=str(e),
                        error_type=error_type,
                        operation="submit_job",
                    )  # type: ignore[call-arg]

            # Start the job result checking task but don't wait for it
            asyncio.create_task(check_job_result())

        except Exception as e:
            error_type = type(e).__name__
            logger.error(  # type: ignore[call-arg]
                "Unexpected error during job submission",
                input_file_id=job_spec.input_file_id,
                endpoint=job_spec.endpoint,
                namespace=namespace,
                error=str(e),
                error_type=error_type,
                operation="submit_job",
            )  # type: ignore[call-arg]
            # Re-raise so JobManager.create_job_with_spec can fail the
            # waiting future immediately with the real exception (e.g.
            # RenderError → 400) instead of stalling 30s on a timeout.
            raise

    async def update_job_ready(self, job: BatchJob):
        """Update job by marking it ready info in the persist store.
        The job suspend flag will be removed to start the execution.

        Args:
            job (BatchJob): Job to update.
        """
        if not self.batch_v1_api:
            raise RuntimeError("Kubernetes client not available")

        patch_body: Optional[Dict[str, Any]] = None
        try:
            # Get namespace from k8s_job, use default if not specified
            namespace = job.metadata.namespace or "default"

            # Convert BatchJobSpec to Kubernetes Job manifest
            patch_body = self._ready_batch_job_to_k8s_job_patch(job)

            logger.info(  # type: ignore[call-arg]
                "Executing job setting to ready",
                job_name=job.metadata.name,
                namespace=namespace,
                patch=patch_body,
            )  # type: ignore[call-arg]

            for attempt in range(5):
                try:
                    await asyncio.to_thread(
                        self.batch_v1_api.patch_namespaced_job,
                        name=job.metadata.name,
                        namespace=namespace,
                        body=patch_body,
                    )
                    break
                except ApiException as e:
                    if e.status == 404 and attempt < 4:
                        logger.warning(  # type: ignore[call-arg]
                            "Job not visible yet while setting ready; retrying",
                            job_name=job.metadata.name,
                            namespace=namespace,
                            attempt=attempt + 1,
                        )
                        await asyncio.sleep(0.5)
                        continue
                    raise

            await self._put_to_store(job, op="update_job_ready", propagate=True)

        except ApiException as e:
            if e.status == 409:
                logger.warning(  # type: ignore[call-arg]
                    "Job status changed",
                    job=job.metadata.name,
                    namespace=namespace,
                    job_id=job.job_id,
                )
                raise
            else:
                logger.error(  # type: ignore[call-arg]
                    "Failed to set job ready",
                    job_name=job.metadata.name,
                    namespace=namespace,
                    patch=patch_body,
                    error=str(e),
                    status_code=e.status,
                    reason=e.reason,
                )  # type: ignore[call-arg]
                raise
        except Exception as e:
            logger.error(  # type: ignore[call-arg]
                "Unexpected error setting job ready",
                job_name=job.metadata.name,
                namespace=namespace,
                patch=patch_body,
                error=str(e),
                operation="update_job_ready",
            )  # type: ignore[call-arg]
            raise

    async def update_job_status(self, job: BatchJob):
        """Persist a job status mutation.

        Status (state, conditions, request_counts, timestamps, errors,
        usage) is owned by the BatchJobStore as of A.2. K8s annotations
        on the Job object are no longer written for status — they are a
        projection that ``kubectl describe`` no longer sees.

        Because we no longer trigger a K8s MODIFIED via an annotation
        patch, the kopf-driven JobCache refresh path does not fire for
        pure status updates; this method updates ``active_jobs`` and
        invokes ``job_updated`` directly so the JobManager pool stays
        in sync with the new status.
        """
        # Snapshot the previous view for the callback diff before we
        # overwrite the cache. The callback expects (old, new); a None
        # old indicates this is the first time we see the job.
        batch_id = job.status.job_id
        old = self.active_jobs.get(batch_id)

        await self._put_to_store(job, op="update_job_status", propagate=True)
        self.active_jobs[batch_id] = job

        if old is not None:
            await self.job_updated(old, job)

    async def cancel_job(self, job: BatchJob) -> None:
        """Cancel a job by suspending the K8s Job and recording status.

        The K8s side only sees ``spec.suspend = True``; cancellation /
        failure status is written to the BatchJobStore (and reflected in
        ``active_jobs``). Status annotations on the K8s Job are no
        longer written.

        Raises:
            RuntimeError: If Kubernetes client is not available.
            KeyError: If job is not found in cache.
            ApiException: If Kubernetes API call fails.
        """
        if not self.batch_v1_api:
            raise RuntimeError("Kubernetes client not available")

        # Get job from cache to find namespace and name
        assert (
            job.status.state == BatchJobState.FINALIZING
            or job.status.state == BatchJobState.FINALIZED
            or job.status.errors is not None
        )
        namespace = job.metadata.namespace or "default"
        job_name = job.metadata.name

        try:
            suspend_patch = {
                "metadata": {
                    "resourceVersion": job.metadata.resource_version,
                },
                "spec": {
                    "suspend": True  # Suspend the Kubernetes Job (instead of deleting)
                },
            }

            logger.info(  # type: ignore[call-arg]
                "Executing job cancellation",
                job=job_name,
                namespace=namespace,
                job_id=job.job_id,
                patch=suspend_patch,
            )

            await asyncio.to_thread(
                self.batch_v1_api.patch_namespaced_job,
                name=job_name,
                namespace=namespace,
                body=suspend_patch,
            )

            await self._put_to_store(job, op="cancel_job", propagate=True)

        except ApiException as e:
            if e.status == 404:
                logger.warning(  # type: ignore[call-arg]
                    "Job not found in Kubernetes for cancellation",
                    job=job_name,
                    namespace=namespace,
                    job_id=job.job_id,
                )
            elif e.status == 409:
                logger.warning(  # type: ignore[call-arg]
                    "Job status changed",
                    job=job_name,
                    namespace=namespace,
                    job_id=job.job_id,
                )
                raise
            else:
                logger.error(  # type: ignore[call-arg]
                    "Failed to cancel job in Kubernetes",
                    job=job_name,
                    namespace=namespace,
                    job_id=job.job_id,
                    error=str(e),
                    status_code=e.status,
                    reason=e.reason,
                )
                raise
        except Exception as e:
            logger.error(  # type: ignore[call-arg]
                "Unexpected error cancelling job",
                job=job_name,
                namespace=namespace,
                job_id=job.job_id,
                error=str(e),
                patch=str(suspend_patch),
                operation="cancel_job",
            )
            raise

    async def delete_job(self, job: BatchJob) -> None:
        """Cancel job by deleting the Kubernetes Job.

        Args:
            job_id: Job ID (batch ID) to cancel.

        Raises:
            RuntimeError: If Kubernetes client is not available.
            KeyError: If job is not found in cache.
            ApiException: If Kubernetes API call fails.
        """
        if not self.batch_v1_api:
            raise RuntimeError("Kubernetes client not available")

        namespace = job.metadata.namespace or "default"
        job_name = job.metadata.name
        try:
            # Delete the Kubernetes Job
            await asyncio.to_thread(
                self.batch_v1_api.delete_namespaced_job,
                name=job_name,
                namespace=namespace,
                propagation_policy="Foreground",  # Delete pods too
            )

            await self._delete_from_store(job)

            logger.info(  # type: ignore[call-arg]
                "Job deletion requested in Kubernetes",
                job_id=job.job_id,
                job=job_name,
                namespace=namespace,
            )
        except ApiException as e:
            if e.status == 404:
                logger.warning(  # type: ignore[call-arg]
                    "Job not found in Kubernetes for deletion",
                    job=job_name,
                    namespace=namespace,
                )
            else:
                logger.error(  # type: ignore[call-arg]
                    "Failed to delete job in Kubernetes",
                    job_id=job.job_id,
                    job=job_name,
                    namespace=namespace,
                    error=str(e),
                    status_code=e.status,
                    reason=e.reason,
                )
                raise
        except Exception as e:
            logger.error(  # type: ignore[call-arg]
                "Unexpected error deleting job",
                job_id=job.job_id,
                job=job_name,
                namespace=namespace,
                error=str(e),
                operation="delete_job",
            )
            raise

    def _ready_batch_job_to_k8s_job_patch(self, job: BatchJob) -> Dict[str, Any]:
        """Build the K8s Job patch that flips a prepared batch into running.

        The patch carries only what K8s itself needs to act on:

        * ``spec.suspend = False`` to release the Job.
        * Pod template annotations holding the output / error file IDs
          the worker reads via the downward API at startup.

        Job-level status annotations (state, conditions, counts, etc.)
        are owned by the BatchJobStore in A.2 and are no longer mirrored
        here.
        """
        job_status: BatchJobStatus = job.status
        assert (
            job_status.in_progress_at is not None
        ), "AssertError: Job must be set as in progress before setting as ready"

        # No ``metadata.resourceVersion`` here on purpose: this patch is
        # only ever issued by the metadata service immediately after it
        # creates the Job, so there is no concurrent writer to guard
        # against. K8s controller-driven status updates race with our
        # ADDED handler and bump resourceVersion *before* this patch
        # fires, producing a 409 every time the optimistic check is in
        # place. Without it, the strategic-merge patch is naturally
        # idempotent on these fields.
        return {
            "spec": {
                "template": {
                    "metadata": {
                        "annotations": {
                            JobAnnotationKey.OUTPUT_FILE_ID: job_status.output_file_id,
                            JobAnnotationKey.TEMP_OUTPUT_FILE_ID: job_status.temp_output_file_id,
                            JobAnnotationKey.ERROR_FILE_ID: job_status.error_file_id,
                            JobAnnotationKey.TEMP_ERROR_FILE_ID: job_status.temp_error_file_id,
                        },
                    },
                },
                "suspend": False,
            },
        }

    def _batch_job_spec_to_k8s_job(
        self,
        session_id: str,
        job_spec: BatchJobSpec,
        job_name: Optional[str] = None,
        parallelism: Optional[int] = None,
        prepared_job: Optional[BatchJob] = None,
    ) -> Dict[str, Any]:
        """Render the K8s Job manifest from BatchJobSpec via the ConfigMap-driven renderer.

        All manifest layout (system base, engine container, storage env,
        per-batch annotations) is delegated to :class:`JobManifestRenderer`.
        See ``python/aibrix/aibrix/batch/manifest/renderer.py``.

        Args:
            session_id: Session identifier for tracking and annotation persistence.
            job_spec: BatchJobSpec to convert. Must carry ``model_template_name``;
                otherwise the renderer raises (legacy yaml path is removed).
            job_name: Optional job name; renderer generates one if not provided.
            parallelism: Optional override for parallelism / completions.
            prepared_job: Optional BatchJob with file IDs to add to pod annotations.

        Returns:
            Dict ready for ``BatchV1Api.create_namespaced_job(body=...)``.

        Raises:
            RenderError (and subclasses): if template/profile is missing or
                the request violates currently supportable values.
                Callers should surface as 400-class errors.
        """
        return self._renderer.render(
            session_id=session_id,
            spec=job_spec,
            prepared_job=prepared_job,
            parallelism=parallelism,
            job_name=job_name,
        )


logger.info("kopf job handlers imported")


# Monotonic ordering of BatchJob states. Used by the kopf-driven
# ``job_updated_handler`` to decide whether a fresh K8s-extracted view
# may overwrite the in-memory cache: as of PR4 the K8s Job no longer
# carries authoritative status annotations, so a transformer rebuild
# triggered by an unrelated K8s mutation (e.g. ``spec.suspend = False``)
# may yield a lower-state BatchJob. Allowing that through would regress
# the cache and the JobManager pool behind it.
_STATE_RANK: Dict[BatchJobState, int] = {
    BatchJobState.CREATED: 0,
    BatchJobState.VALIDATING: 1,
    BatchJobState.IN_PROGRESS: 2,
    BatchJobState.CANCELLING: 3,
    BatchJobState.FINALIZING: 4,
    BatchJobState.FINALIZED: 5,
}


def _state_rank(state: BatchJobState) -> int:
    return _STATE_RANK.get(state, -1)


# Standalone kopf handlers that work with the global JobCache instance
# Use event handler only to avoid advanced kopf features such as state management,
# which introduces customized annotation.
@kopf.on.event("batch", "v1", "jobs")  # type: ignore[arg-type]
async def job_event_handler(type: str, body: Any, **kwargs: Any) -> None:
    """Handle Kubernetes Job creation events."""
    job_cache = get_global_job_cache()
    if not job_cache:
        logger.warning("No global job cache available for job creation event")
        return

    if type == "ADDED":
        await job_created_handler(body, **kwargs)
    elif type == "MODIFIED":
        job_id = body.get("metadata", {}).get("uid")
        if job_cache.active_jobs.get(job_id) is None:
            await job_created_handler(body, **kwargs)
        else:
            await job_updated_handler(body, **kwargs)
    elif type == "DELETED":
        await job_deleted_handler(body, **kwargs)


# @kopf.on.create("batch", "v1", "jobs")  # type: ignore[arg-type]
async def job_created_handler(body: Any, **kwargs: Any) -> None:
    """Handle Kubernetes Job creation events."""
    job_cache = get_global_job_cache()
    if not job_cache:
        logger.warning("No global job cache available for job creation event")
        return

    try:
        # Transform K8s Job to BatchJob
        batch_job = k8s_job_to_batch_job(body)
        job_id = batch_job.status.job_id if batch_job.status else body.metadata.uid

        logger.info(
            "Job created",
            job_id=job_id,
            name=batch_job.metadata.name,
            namespace=batch_job.metadata.namespace,
            state=batch_job.status.state.value,
            resource_version=batch_job.metadata.resource_version,
        )  # type: ignore[call-arg]

        # Invoke callback if registered
        try:
            committed_ok = await job_cache.job_committed(batch_job)
            if committed_ok:
                # Store in cache
                job_cache.active_jobs[job_id] = batch_job
                # Seed the initial document. The K8s ADDED event is
                # the first time anything outside K8s sees this job, so
                # the store had no prior entry; subsequent status
                # mutations also flow through _put_to_store.
                await job_cache._put_to_store(batch_job, op="job_created")
            else:
                await job_cache.delete_job(batch_job)
        except Exception as e:
            logger.error(
                "Error in job committed handler",
                error=str(e),
                handler="job_committed",
            )  # type: ignore[call-arg]
    except ValueError as ve:
        # For jobs without proper annotations, store basic info for backward compatibility
        job_id = body.metadata.uid
        logger.warning(
            "Failed to process job creation",
            job_id=job_id,
            reason=str(ve),
        )  # type: ignore[call-arg]
    except Exception as e:
        logger.error(
            "Failed to process job creation", error=str(e), operation="job_created"
        )  # type: ignore[call-arg]


# @kopf.on.update("batch", "v1", "jobs")  # type: ignore[arg-type]
async def job_updated_handler(body: Any, **kwargs: Any) -> None:
    """Handle Kubernetes Job update events."""
    job_cache = get_global_job_cache()
    if not job_cache:
        logger.warning("No global job cache available for job update event")
        return

    try:
        # Transform new K8s Job to BatchJob
        new_batch_job = k8s_job_to_batch_job(body)
        job_id = (
            new_batch_job.status.job_id if new_batch_job.status else body.metadata.uid
        )

        # Get old job from cache
        old_batch_job = job_cache.active_jobs.get(job_id)
        if old_batch_job is None:
            logger.warning("Job updating ignored due to job not found", job_id=job_id)  # type: ignore[call-arg]
            return

        # PR4 monotonicity: status annotations are no longer the source
        # of truth, so this kopf event may carry a lower-state view than
        # the cache (e.g. update_job_ready patches only spec.suspend, so
        # the transformer derives state=CREATED from the absent
        # JOB_STATE annotation while the cache already holds
        # IN_PROGRESS). Drop such echo events outright; the internally
        # driven update path (JobCache.update_job_status) keeps both the
        # cache and the BatchJobStore current.
        if _state_rank(new_batch_job.status.state) < _state_rank(
            old_batch_job.status.state
        ):
            logger.debug(  # type: ignore[call-arg]
                "Skipping kopf-driven update; cached state is more advanced",
                job_id=job_id,
                cached_state=old_batch_job.status.state.value,
                k8s_view_state=new_batch_job.status.state.value,
            )
            return

        logger.info(
            "Job updated",
            job_id=job_id,
            name=new_batch_job.metadata.name,
            namespace=new_batch_job.metadata.namespace,
            old_state=old_batch_job.status.state.value if old_batch_job else "unknown",
            new_state=new_batch_job.status.state.value,
            resource_version=new_batch_job.metadata.resource_version,
        )  # type: ignore[call-arg]

        # Invoke callback if registered and we have both old and new jobs
        try:
            if await job_cache.job_updated(old_batch_job, new_batch_job):
                # Update cache
                job_cache.active_jobs[job_id] = new_batch_job
        except Exception as uhe:
            logger.error(
                "Error in job updated handler",
                error=str(uhe),
                handler="job_updated",
            )  # type: ignore[call-arg]
    except Exception as e:
        logger.error(
            "Failed to process job update", error=str(e), operation="job_updated"
        )  # type: ignore[call-arg]


# @kopf.on.field("batch", "v1", "jobs", field="status.conditions")  # type: ignore[arg-type]
async def job_completion_handler(body: Any, **kwargs: Any) -> None:
    """
    This handler triggers ONLY when the 'status.conditions' field of a Job changes.
    """
    if not body:  # The conditions field might be None initially
        return

    await job_updated_handler(body, **kwargs)  # type: ignore[call-arg, misc, arg-type]


# Set optional = True to prevent kopf add the finalizer.
# @kopf.on.delete("batch", "v1", "jobs", optional=True)  # type: ignore[arg-type]
async def job_deleted_handler(body: Any, **kwargs: Any) -> None:
    """Handle Kubernetes Job deletion events."""
    job_cache = get_global_job_cache()
    if not job_cache:
        logger.warning("No global job cache available for job deletion event")
        return

    job_id = body.metadata.uid
    job_name = body.metadata.name
    namespace = body.metadata.namespace

    # Get job from cache before deletion
    deleted_job = job_cache.active_jobs.get(job_id)
    if deleted_job is None:
        logger.info(
            "Job deleted event ignore, no job found",
            job_id=job_id,
            name=job_name,
            namespace=namespace,
        )  # type: ignore[call-arg]
        return

    logger.info(
        "Job deleted",
        job_id=job_id,
        job=deleted_job.metadata.name,
        namespace=deleted_job.metadata.namespace,
        state=deleted_job.status.state.value,
    )  # type: ignore[call-arg]

    # Invoke callback if registered
    try:
        if await job_cache.job_deleted(deleted_job):
            del job_cache.active_jobs[job_id]
    except Exception as e:
        logger.error(
            "Error in job deleted handler",
            error=str(e),
            handler="job_deleted",
        )  # type: ignore[call-arg]

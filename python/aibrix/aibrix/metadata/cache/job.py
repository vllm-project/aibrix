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

from pathlib import Path
from typing import Any, Callable, Dict, List, Optional

import kopf
import yaml
from kubernetes import client, config
from kubernetes.client.rest import ApiException

from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobSpec,
    JobEntityManager,
    k8s_job_to_batch_job,
)
from aibrix.metadata.logger import init_logger

# import asyncio
# If you installed kopf[uvloop], kopf will likely set this up.
# Otherwise, you can explicitly set it:
# import uvloop
# asyncio.set_event_loop_policy(uvloop.EventLoopPolicy())


@kopf.on.startup()
async def configure(logger, **kwargs):
    logger.info("Operator starting up!")


@kopf.on.cleanup()
async def cleanup(logger, **kwargs):
    logger.info("Operator shutting down!")


# Global logger for standalone functions
logger = init_logger(__name__)


class JobCache(JobEntityManager):
    """Kubernetes-based job cache implementing JobEntityManager interface.

    This class uses kopf to watch Kubernetes Job resources and maintains
    an in-memory cache of BatchJob objects. It implements the JobEntityManager
    interface to provide standardized job management capabilities.
    """

    def __init__(self):
        """Initialize the job cache."""
        # Cache of BatchJob objects keyed by batch ID (K8s UID)
        self.active_jobs: Dict[str, BatchJob] = {}

        # Callback handlers for job lifecycle events
        self._job_committed_handler: Optional[Callable[[BatchJob], None]] = None
        self._job_updated_handler: Optional[Callable[[BatchJob, BatchJob], None]] = None
        self._job_deleted_handler: Optional[Callable[[BatchJob], None]] = None

        # Load Kubernetes Job template once at initialization
        template_path = (
            Path(__file__).parent.parent / "setting" / "k8s_job_template.yaml"
        )
        try:
            with open(template_path, "r") as f:
                self.job_template = yaml.safe_load(f)
            logger.info(
                "Kubernetes Job template loaded successfully",
                template_path=str(template_path),
            )
        except FileNotFoundError:
            logger.error(
                "Kubernetes Job template not found",
                template_path=str(template_path),
                operation="__init__",
            )
            raise RuntimeError(f"Job template not found at {template_path}")
        except yaml.YAMLError as e:
            logger.error(
                "Failed to parse Kubernetes Job template",
                template_path=str(template_path),
                error=str(e),
                operation="__init__",
            )
            raise RuntimeError(f"Invalid YAML in job template: {e}")

        # Initialize Kubernetes client
        try:
            # Try to load in-cluster config first (for pod environments)
            config.load_incluster_config()
        except config.ConfigException:
            try:
                # Fall back to local kubeconfig
                config.load_kube_config()
            except config.ConfigException:
                logger.warning(
                    "Failed to load Kubernetes configuration",
                    reason="no_kubeconfig_or_incluster",
                )
                self.batch_v1_api = None
            else:
                self.batch_v1_api = client.BatchV1Api()
        else:
            self.batch_v1_api = client.BatchV1Api()

    def is_scheduler_enabled(self) -> bool:
        """Check if JobEntityManager has own scheduler enabled."""
        return True

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

    def submit_job(self, job_spec: BatchJobSpec) -> None:
        """Submit job by creating a Kubernetes Job.

        Args:
            job_spec: BatchJobSpec to submit to Kubernetes.

        Raises:
            RuntimeError: If Kubernetes client is not available.
            ApiException: If Kubernetes API call fails.
        """
        if not self.batch_v1_api:
            raise RuntimeError("Kubernetes client not available")

        try:
            # Convert BatchJobSpec to Kubernetes Job manifest
            k8s_job = self._batch_job_to_k8s_job(job_spec)

            # Get namespace from k8s_job, use default if not specified
            namespace = k8s_job.metadata.namespace or "default"
            created_job = self.batch_v1_api.create_namespaced_job(
                namespace=namespace, body=k8s_job
            )

            logger.info(  # type: ignore[call-arg]
                "Job submitted to Kubernetes",
                job_name=k8s_job.metadata.name,
                namespace=namespace,
                k8s_uid=created_job.metadata.uid,
                input_file_id=job_spec.input_file_id,
                endpoint=job_spec.endpoint.value,
            )

        except ApiException as e:
            logger.error(  # type: ignore[call-arg]
                "Failed to submit job to Kubernetes",
                input_file_id=job_spec.input_file_id,
                endpoint=job_spec.endpoint.value,
                error=str(e),
                status_code=e.status,
                reason=e.reason,
            )
            raise
        except Exception as e:
            logger.error(  # type: ignore[call-arg]
                "Unexpected error submitting job",
                input_file_id=job_spec.input_file_id,
                endpoint=job_spec.endpoint.value,
                error=str(e),
                operation="submit_job",
            )
            raise

    def cancel_job(self, job_id: str) -> None:
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

        # Get job from cache to find namespace and name
        job = self.get_job(job_id)
        if not job:
            logger.error("Job not found for cancellation", job_id=job_id)  # type: ignore[call-arg]
            raise KeyError(f"Job with ID '{job_id}' not found")

        namespace = job.metadata.namespace or "default"
        job_name = job.metadata.name
        try:
            # Delete the Kubernetes Job
            self.batch_v1_api.delete_namespaced_job(
                name=job_name,
                namespace=namespace,
                propagation_policy="Foreground",  # Delete pods too
            )

            logger.info(  # type: ignore[call-arg]
                "Job deletion requested in Kubernetes",
                job=job_name,
                namespace=namespace,
                job_id=job_id,
            )
        except ApiException as e:
            if e.status == 404:
                logger.warning(  # type: ignore[call-arg]
                    "Job not found in Kubernetes for cancellation",
                    job=job_name,
                    namespace=namespace,
                    job_id=job_id,
                )
            else:
                logger.error(  # type: ignore[call-arg]
                    "Failed to cancel job in Kubernetes",
                    job=job_name,
                    namespace=namespace,
                    job_id=job_id,
                    error=str(e),
                    status_code=e.status,
                    reason=e.reason,
                )
                raise
        except Exception as e:
            logger.error(  # type: ignore[call-arg]
                "Unexpected error canceling job",
                job=job_name,
                namespace=namespace,
                job_id=job_id,
                error=str(e),
                operation="cancel_job",
            )
            raise

    def _batch_job_to_k8s_job(self, job_spec: BatchJobSpec) -> client.V1Job:
        """Convert BatchJobSpec to Kubernetes Job manifest using pre-loaded template.

        Args:
            job_spec: BatchJobSpec to convert.

        Returns:
            Kubernetes V1Job object.
        """
        # Use pre-loaded template (deep copy to avoid modifying the original)
        import copy
        import uuid

        job_template = copy.deepcopy(self.job_template)

        # Create annotations from BatchJobSpec
        batch_annotations = {
            "batch.job.aibrix.ai/input-file-id": job_spec.input_file_id,
            "batch.job.aibrix.ai/endpoint": job_spec.endpoint.value,
            "batch.job.aibrix.ai/completion-window": job_spec.completion_window.value,
        }

        # Add batch metadata as annotations
        if job_spec.metadata:
            for key, value in job_spec.metadata.items():
                batch_annotations[f"batch.job.aibrix.ai/metadata.{key}"] = value

        # Merge template annotations with batch annotations
        template_annotations = job_template.get("metadata", {}).get("annotations") or {}
        merged_annotations = {**template_annotations, **batch_annotations}

        # Generate unique job name
        job_name = f"batch-{uuid.uuid4().hex[:8]}"

        # Update template with BatchJobSpec values
        job_template["metadata"]["name"] = job_name
        job_template["metadata"]["annotations"] = merged_annotations

        # Update environment variables in the container
        containers = job_template["spec"]["template"]["spec"]["containers"]
        if containers:
            env_vars = containers[0].get("env", [])
            for env_var in env_vars:
                if env_var["name"] == "INPUT_FILE_ID":
                    env_var["value"] = job_spec.input_file_id
                elif env_var["name"] == "ENDPOINT":
                    env_var["value"] = job_spec.endpoint.value
                elif env_var["name"] == "COMPLETION_WINDOW":
                    env_var["value"] = job_spec.completion_window.value

        # Convert template dict to Kubernetes V1Job object
        try:
            # Create V1Job object manually from template structure
            job = client.V1Job(
                api_version=job_template.get("apiVersion", "batch/v1"),
                kind=job_template.get("kind", "Job"),
                metadata=client.V1ObjectMeta(
                    name=job_template["metadata"]["name"],
                    namespace=job_template["metadata"].get("namespace", "default"),
                    labels=job_template["metadata"].get("labels", {}),
                    annotations=job_template["metadata"]["annotations"],
                ),
                spec=client.V1JobSpec(
                    template=client.V1PodTemplateSpec(
                        metadata=client.V1ObjectMeta(
                            labels=job_template["spec"]["template"]["metadata"].get(
                                "labels", {}
                            )
                        ),
                        spec=client.V1PodSpec(
                            containers=[
                                client.V1Container(
                                    name=container["name"],
                                    image=container["image"],
                                    env=[
                                        client.V1EnvVar(
                                            name=env["name"], value=env["value"]
                                        )
                                        for env in container.get("env", [])
                                    ],
                                    resources=client.V1ResourceRequirements(
                                        requests=container.get("resources", {}).get(
                                            "requests"
                                        ),
                                        limits=container.get("resources", {}).get(
                                            "limits"
                                        ),
                                    )
                                    if container.get("resources")
                                    else None,
                                )
                                for container in job_template["spec"]["template"][
                                    "spec"
                                ]["containers"]
                            ],
                            restart_policy=job_template["spec"]["template"]["spec"].get(
                                "restartPolicy", "Never"
                            ),
                        ),
                    ),
                    backoff_limit=job_template["spec"].get("backoffLimit", 3),
                    active_deadline_seconds=job_template["spec"].get(
                        "activeDeadlineSeconds"
                    ),
                ),
            )
            return job
        except Exception as e:
            logger.error(  # type: ignore[call-arg]
                "Failed to convert template to Kubernetes Job object",
                error=str(e),
                operation="_batch_job_to_k8s_job",
            )
            raise RuntimeError(f"Failed to create Kubernetes Job object: {e}")

    # Kopf event handlers that delegate to the JobEntityManager interface
    @kopf.on.create("batch", "v1", "jobs")  # type: ignore[arg-type]
    async def job_created(self, logger: Any, body: Any, **kwargs: Any) -> None:
        """Handle Kubernetes Job creation events."""
        try:
            # Transform K8s Job to BatchJob
            batch_job = k8s_job_to_batch_job(body)
            job_id = batch_job.status.job_id if batch_job.status else body.metadata.uid

            # Store in cache
            self.active_jobs[job_id] = batch_job

            # Invoke callback if registered
            if self._job_committed_handler:
                try:
                    self._job_committed_handler(batch_job)
                except Exception as e:
                    logger.error(
                        "Error in job committed handler",
                        error=str(e),
                        handler="job_committed",
                    )

            logger.info(
                "Job created",
                job=batch_job.metadata.name,
                namespace=batch_job.metadata.namespace,
                job_id=job_id,
            )
        except ValueError as ve:
            # For jobs without proper annotations, store basic info for backward compatibility
            job_id = body.metadata.uid
            logger.warning(
                "Failed to process job creation",
                job_id=job_id,
                reason=str(ve),
            )
        except Exception as e:
            logger.error(
                "Failed to process job creation", error=str(e), operation="job_created"
            )

    @kopf.on.update("batch", "v1", "jobs")  # type: ignore[arg-type]
    async def job_updated(
        self, logger: Any, body: Any, old: Any, new: Any, **kwargs: Any
    ) -> None:
        """Handle Kubernetes Job update events."""
        try:
            # Transform new K8s Job to BatchJob
            new_batch_job = k8s_job_to_batch_job(body)
            job_id = (
                new_batch_job.status.job_id
                if new_batch_job.status
                else body.metadata.uid
            )

            # Get old job from cache
            old_batch_job = self.active_jobs.get(job_id)

            # Update cache
            self.active_jobs[job_id] = new_batch_job

            # Invoke callback if registered and we have both old and new jobs
            if self._job_updated_handler and old_batch_job:
                try:
                    self._job_updated_handler(old_batch_job, new_batch_job)
                except Exception as e:
                    logger.error(
                        "Error in job updated handler",
                        error=str(e),
                        handler="job_updated",
                    )

            logger.info(
                "Job updated",
                job=new_batch_job.metadata.name,
                namespace=new_batch_job.metadata.namespace,
                job_id=job_id,
            )
        except Exception as e:
            logger.error(
                "Failed to process job update", error=str(e), operation="job_updated"
            )

    @kopf.on.delete("batch", "v1", "jobs")  # type: ignore[arg-type]
    async def job_deleted(self, logger: Any, body: Any, **kwargs: Any) -> None:
        """Handle Kubernetes Job deletion events."""
        job_id = body.metadata.uid
        job_name = body.metadata.name
        namespace = body.metadata.namespace

        # Get job from cache before deletion
        deleted_job = self.active_jobs.get(job_id)

        if job_id in self.active_jobs:
            del self.active_jobs[job_id]

            # Invoke callback if registered
            if self._job_deleted_handler and deleted_job:
                try:
                    self._job_deleted_handler(deleted_job)
                except Exception as e:
                    logger.error(
                        "Error in job deleted handler",
                        error=str(e),
                        handler="job_deleted",
                    )

            logger.info("Job deleted", job=job_name, namespace=namespace, job_id=job_id)

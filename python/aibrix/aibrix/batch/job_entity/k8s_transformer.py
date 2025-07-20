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

import uuid
from datetime import datetime, timezone
from enum import Enum
from typing import Any, Dict, Optional

from .batch_job import (
    BatchJob,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    CompletionWindow,
    ObjectMeta,
    TypeMeta,
)

# Annotation prefix for batch job specifications
JOB_ANNOTATION_PREFIX = "batch.job.aibrix.ai/"


class JobAnnotationKey(str, Enum):
    """Valid annotation keys for job specifications."""

    SESSION_ID = f"{JOB_ANNOTATION_PREFIX}session-id"
    INPUT_FILE_ID = f"{JOB_ANNOTATION_PREFIX}input-file-id"
    ENDPOINT = f"{JOB_ANNOTATION_PREFIX}endpoint"
    COMPLETION_WINDOW = f"{JOB_ANNOTATION_PREFIX}completion-window"
    METADATA_PREFIX = f"{JOB_ANNOTATION_PREFIX}metadata."


class BatchJobTransformer:
    """Helper class to transform Kubernetes Job objects to BatchJob instances."""

    @classmethod
    def from_k8s_job(cls, k8s_job: Any) -> BatchJob:
        """
        Transform a Kubernetes Job object to a BatchJob instance.

        Args:
            k8s_job: Kubernetes Job object (from kubernetes.client.V1Job or kopf body)

        Returns:
            BatchJob: Internal BatchJob model instance

        Raises:
            ValueError: If required annotations are missing or invalid
        """
        # Extract metadata with null safety
        metadata = cls._safe_get_attr(k8s_job, "metadata", {})
        annotations: Dict[str, str] = cls._safe_get_attr(metadata, "annotations", {})

        # Extract SessionID from annotations
        session_id = annotations.get(JobAnnotationKey.SESSION_ID.value)

        # Extract BatchJobSpec from annotations
        spec = cls._extract_batch_job_spec(annotations)

        # Extract ObjectMeta
        object_meta = cls._extract_object_meta(metadata)

        # Extract TypeMeta from Kubernetes Job
        type_meta = cls._extract_type_meta(k8s_job)

        # Extract or create BatchJobStatus
        status = cls._extract_batch_job_status(k8s_job, spec)

        return BatchJob(
            sessionID=session_id,
            typeMeta=type_meta,
            metadata=object_meta,
            spec=spec,
            status=status,
        )

    @classmethod
    def _extract_batch_job_spec(cls, annotations: Dict[str, str]) -> BatchJobSpec:
        """Extract BatchJobSpec from Kubernetes job annotations."""
        # Extract required fields
        input_file_id = annotations.get(JobAnnotationKey.INPUT_FILE_ID.value)
        if not input_file_id:
            raise ValueError(
                f"Required annotation '{JobAnnotationKey.INPUT_FILE_ID.value}' not found"
            )

        endpoint_str = annotations.get(JobAnnotationKey.ENDPOINT.value)
        if not endpoint_str:
            raise ValueError(
                f"Required annotation '{JobAnnotationKey.ENDPOINT.value}' not found"
            )

        # Extract optional completion window
        completion_window_str = annotations.get(
            JobAnnotationKey.COMPLETION_WINDOW.value,
            CompletionWindow.TWENTY_FOUR_HOURS.value,
        )

        # Extract batch metadata (key-value pairs with prefix)
        batch_metadata = {}
        for key, value in annotations.items():
            if key.startswith(JobAnnotationKey.METADATA_PREFIX.value):
                # Remove prefix to get the actual metadata key
                metadata_key = key[len(JobAnnotationKey.METADATA_PREFIX.value) :]
                batch_metadata[metadata_key] = value

        # Use BatchJobSpec.from_strings for validation and creation
        return BatchJobSpec.from_strings(
            input_file_id=input_file_id,
            endpoint=endpoint_str,
            completion_window=completion_window_str,
            metadata=batch_metadata if batch_metadata else None,
        )

    @classmethod
    def _extract_object_meta(cls, k8s_metadata: Any) -> ObjectMeta:
        """Extract ObjectMeta from Kubernetes metadata."""
        # Handle both attribute access and dict-like access
        name = cls._safe_get_attr(k8s_metadata, "name")
        namespace = cls._safe_get_attr(k8s_metadata, "namespace")
        uid = cls._safe_get_attr(k8s_metadata, "uid")
        resource_version = cls._safe_get_attr(
            k8s_metadata, "resource_version"
        ) or cls._safe_get_attr(k8s_metadata, "resourceVersion")
        generation = cls._safe_get_attr(k8s_metadata, "generation")

        # Handle timestamp conversion
        creation_timestamp = cls._convert_timestamp(
            cls._safe_get_attr(k8s_metadata, "creation_timestamp")
            or cls._safe_get_attr(k8s_metadata, "creationTimestamp")
        )
        deletion_timestamp = cls._convert_timestamp(
            cls._safe_get_attr(k8s_metadata, "deletion_timestamp")
            or cls._safe_get_attr(k8s_metadata, "deletionTimestamp")
        )

        labels = cls._safe_get_attr(k8s_metadata, "labels")
        annotations = cls._safe_get_attr(k8s_metadata, "annotations")

        return ObjectMeta(
            name=name,
            namespace=namespace,
            uid=uid,
            resourceVersion=resource_version,
            generation=generation,
            creationTimestamp=creation_timestamp,
            deletionTimestamp=deletion_timestamp,
            labels=labels,
            annotations=annotations,
        )

    @classmethod
    def _extract_type_meta(cls, k8s_job: Any) -> TypeMeta:
        """Extract TypeMeta from Kubernetes Job."""
        # Extract apiVersion and kind from the Kubernetes job
        api_version = cls._safe_get_attr(k8s_job, "api_version") or cls._safe_get_attr(
            k8s_job, "apiVersion", "batch/v1"
        )
        kind = cls._safe_get_attr(k8s_job, "kind", "Job")

        return TypeMeta(apiVersion=api_version, kind=kind)

    @classmethod
    def _extract_batch_job_status(
        cls, k8s_job: Any, spec: BatchJobSpec
    ) -> BatchJobStatus:
        """Extract or create BatchJobStatus from Kubernetes job."""
        # Extract job status information
        k8s_status = cls._safe_get_attr(k8s_job, "status", {})
        metadata = cls._safe_get_attr(k8s_job, "metadata", {})

        # Generate or extract batch ID
        job_id = cls._safe_get_attr(metadata, "uid") or str(uuid.uuid4())

        # Map Kubernetes job phase to BatchJobState
        state = cls._map_k8s_phase_to_batch_state(k8s_status)

        # Extract creation timestamp
        creation_timestamp = cls._convert_timestamp(
            cls._safe_get_attr(metadata, "creation_timestamp")
            or cls._safe_get_attr(metadata, "creationTimestamp")
        )
        if not creation_timestamp:
            creation_timestamp = datetime.now(timezone.utc)

        # Extract start time for in_process_at
        start_time = cls._convert_timestamp(
            cls._safe_get_attr(k8s_status, "start_time")
            or cls._safe_get_attr(k8s_status, "startTime")
        )

        # Extract completion time
        completion_time = cls._convert_timestamp(
            cls._safe_get_attr(k8s_status, "completion_time")
            or cls._safe_get_attr(k8s_status, "completionTime")
        )

        return BatchJobStatus(
            jobID=job_id,
            state=state,
            createdAt=creation_timestamp,
            inProgressAt=start_time,
            finalizingAt=completion_time,
        )

    @classmethod
    def _map_k8s_phase_to_batch_state(cls, k8s_status: Any) -> BatchJobState:
        """Map Kubernetes job phase to BatchJobState."""
        # Handle both attribute access and dict-like access
        phase = cls._safe_get_attr(k8s_status, "phase")
        conditions = cls._safe_get_attr(k8s_status, "conditions", [])

        # Check if job is active (running)
        active = cls._safe_get_attr(k8s_status, "active", 0)
        succeeded = cls._safe_get_attr(k8s_status, "succeeded", 0)
        failed = cls._safe_get_attr(k8s_status, "failed", 0)

        # Map based on job status
        if succeeded > 0:
            return BatchJobState.FINALIZING
        elif failed > 0:
            return BatchJobState.FAILED
        elif active > 0:
            return BatchJobState.IN_PROGRESS
        elif phase == "Pending":
            return BatchJobState.CREATED
        else:
            # Check conditions for more specific state
            for condition in conditions:
                condition_type = cls._safe_get_attr(condition, "type")
                condition_status = cls._safe_get_attr(condition, "status")

                if condition_type == "Failed" and condition_status == "True":
                    return BatchJobState.FAILED
                elif condition_type == "Complete" and condition_status == "True":
                    return BatchJobState.FINALIZING

            return BatchJobState.CREATED

    @classmethod
    def _safe_get_attr(cls, obj: Any, attr: str, default: Any = None) -> Any:
        """Safely get attribute from object, supporting both attr access and dict access."""
        if obj is None:
            return default

        # Try attribute access first
        if hasattr(obj, attr):
            return getattr(obj, attr)

        # Try dict-like access
        if isinstance(obj, dict):
            return obj.get(attr, default)

        return default

    @classmethod
    def _convert_timestamp(cls, timestamp: Any) -> Optional[datetime]:
        """Convert various timestamp formats to datetime."""
        if timestamp is None:
            return None

        # If already a datetime object
        if isinstance(timestamp, datetime):
            return timestamp

        # If it's a string, try to parse it
        if isinstance(timestamp, str):
            try:
                # Handle ISO format timestamps
                return datetime.fromisoformat(timestamp.replace("Z", "+00:00"))
            except ValueError:
                # Try other common formats
                try:
                    return datetime.strptime(timestamp, "%Y-%m-%dT%H:%M:%SZ")
                except ValueError:
                    return None

        # If it has a timestamp attribute (Kubernetes Time object)
        if hasattr(timestamp, "timestamp"):
            return timestamp.timestamp()

        return None


def k8s_job_to_batch_job(k8s_job: Any) -> BatchJob:
    """
    Convenience function to transform a Kubernetes Job object to a BatchJob.

    Args:
        k8s_job: Kubernetes Job object (from kubernetes.client.V1Job or kopf body)

    Returns:
        BatchJob: Internal BatchJob model instance

    Raises:
        ValueError: If required annotations are missing or invalid
    """
    return BatchJobTransformer.from_k8s_job(k8s_job)

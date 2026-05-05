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

import collections.abc
import json
import uuid
from datetime import datetime, timezone
from enum import Enum
from typing import Any, Dict, List, Optional, Tuple

from aibrix.logger import init_logger

from .aibrix_metadata import AibrixMetadata
from .batch_job import (
    BatchJob,
    BatchJobError,
    BatchJobErrorCode,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    BatchUsage,
    CompletionWindow,
    Condition,
    ConditionStatus,
    ConditionType,
    ObjectMeta,
    RequestCountStats,
    TypeMeta,
)

# Annotation prefix for batch job specifications
JOB_ANNOTATION_PREFIX = "batch.job.aibrix.ai/"

logger = init_logger(__name__)


class JobAnnotationKey(str, Enum):
    """Valid annotation keys for job specifications."""

    SESSION_ID = f"{JOB_ANNOTATION_PREFIX}session-id"
    INPUT_FILE_ID = f"{JOB_ANNOTATION_PREFIX}input-file-id"
    ENDPOINT = f"{JOB_ANNOTATION_PREFIX}endpoint"
    METADATA_PREFIX = f"{JOB_ANNOTATION_PREFIX}metadata."
    OPTS_PREFIX = f"{JOB_ANNOTATION_PREFIX}opts."
    AIBRIX = f"{JOB_ANNOTATION_PREFIX}aibrix"
    OUTPUT_FILE_ID = f"{JOB_ANNOTATION_PREFIX}output-file-id"
    TEMP_OUTPUT_FILE_ID = f"{JOB_ANNOTATION_PREFIX}temp-output-file-id"
    ERROR_FILE_ID = f"{JOB_ANNOTATION_PREFIX}error-file-id"
    TEMP_ERROR_FILE_ID = f"{JOB_ANNOTATION_PREFIX}temp-error-file-id"

    MODEL_TEMPLATE_NAME = f"{JOB_ANNOTATION_PREFIX}model-template-name"
    MODEL_TEMPLATE_VERSION = f"{JOB_ANNOTATION_PREFIX}model-template-version"
    PROFILE_NAME = f"{JOB_ANNOTATION_PREFIX}profile-name"
    TEMPLATE_OVERRIDES = f"{JOB_ANNOTATION_PREFIX}template-overrides"  # JSON-encoded
    PROFILE_OVERRIDES = f"{JOB_ANNOTATION_PREFIX}profile-overrides"  # JSON-encoded

    # Status persistence annotations
    JOB_STATE = f"{JOB_ANNOTATION_PREFIX}state"
    CONDITION = f"{JOB_ANNOTATION_PREFIX}condition"
    REQUEST_COUNTS = f"{JOB_ANNOTATION_PREFIX}request-counts"
    IN_PROGRESS_AT = f"{JOB_ANNOTATION_PREFIX}in-progress-at"
    CANCELLING_AT = f"{JOB_ANNOTATION_PREFIX}cancelling-at"
    FINALIZING_AT = f"{JOB_ANNOTATION_PREFIX}finalizing-at"
    FINALIZED_AT = f"{JOB_ANNOTATION_PREFIX}finalized-at"
    ERRORS = f"{JOB_ANNOTATION_PREFIX}errors"
    USAGE = f"{JOB_ANNOTATION_PREFIX}usage"  # JSON-encoded BatchUsage


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

        # Extract pod annotations from pod template (where we now store the batch job metadata)
        pod_spec = cls._safe_get_attr(k8s_job, "spec", {})
        pod_template = cls._safe_get_attr(pod_spec, "template", {})
        pod_metadata = cls._safe_get_attr(pod_template, "metadata", {})
        pod_annotations: Dict[str, str] = cls._safe_get_attr(
            pod_metadata, "annotations", {}
        )

        # Extract SessionID from pod annotations
        session_id = pod_annotations.get(JobAnnotationKey.SESSION_ID.value)

        # Extract BatchJobSpec from pod annotations
        spec = cls._extract_batch_job_spec(pod_annotations, pod_spec)

        # Extract ObjectMeta from job metadata (not pod metadata)
        object_meta: ObjectMeta = cls._extract_object_meta(metadata)

        # Extract TypeMeta from Kubernetes Job
        type_meta = cls._extract_type_meta(k8s_job)

        # Extract or create BatchJobStatus
        status = cls._extract_batch_job_status(
            k8s_job, object_meta.resource_version, pod_annotations, annotations
        )

        return BatchJob(
            sessionID=session_id,
            typeMeta=type_meta,
            metadata=object_meta,
            spec=spec,
            status=status,
        )

    @classmethod
    def _extract_batch_job_spec(
        cls, annotations: Dict[str, str], pod_spec: Any
    ) -> BatchJobSpec:
        """Extract BatchJobSpec from Kubernetes job annotations."""
        # Extract required fields
        input_file_id = annotations.get(JobAnnotationKey.INPUT_FILE_ID.value)
        if not input_file_id:
            raise ValueError(
                f"Required annotation '{JobAnnotationKey.INPUT_FILE_ID.value}' not found"
            )

        endpoint = annotations.get(JobAnnotationKey.ENDPOINT.value)
        if not endpoint:
            raise ValueError(
                f"Required annotation '{JobAnnotationKey.ENDPOINT.value}' not found"
            )

        # Extract batch metadata (key-value pairs with prefix)
        batch_metadata = {}
        batch_opts = {}
        for key, value in annotations.items():
            if key.startswith(JobAnnotationKey.METADATA_PREFIX.value):
                # Remove prefix to get the actual metadata key
                metadata_key = key[len(JobAnnotationKey.METADATA_PREFIX.value) :]
                batch_metadata[metadata_key] = value
            elif key.startswith(JobAnnotationKey.OPTS_PREFIX.value):
                # Remove prefix to get the actual opts key
                opts_key = key[len(JobAnnotationKey.OPTS_PREFIX.value) :]
                batch_opts[opts_key] = value

        # Template / profile selection. All optional;
        # absence means batch was created before the template feature
        # or via the legacy hardcoded yaml path.
        template_name = annotations.get(JobAnnotationKey.MODEL_TEMPLATE_NAME.value)
        template_version = annotations.get(
            JobAnnotationKey.MODEL_TEMPLATE_VERSION.value
        )
        profile_name = annotations.get(JobAnnotationKey.PROFILE_NAME.value)

        def _decode(key: JobAnnotationKey) -> Optional[Dict[str, Any]]:
            raw = annotations.get(key.value)
            if not raw:
                return None
            try:
                return json.loads(raw)
            except json.JSONDecodeError as e:
                logger.warning(
                    "Failed to parse overrides annotation; treating as None",
                    annotation_key=key.value,
                    error=str(e),
                    annotation_value=raw,
                )  # type: ignore[call-arg]
                return None

        template_overrides = _decode(JobAnnotationKey.TEMPLATE_OVERRIDES)
        profile_overrides = _decode(JobAnnotationKey.PROFILE_OVERRIDES)
        aibrix = (
            AibrixMetadata.model_validate_json(
                annotations[JobAnnotationKey.AIBRIX.value]
            )
            if JobAnnotationKey.AIBRIX.value in annotations
            else None
        )
        # Backward compatible logic, will be upgrade to consolidated aibrix field.
        aibrix_from_spec = AibrixMetadata.from_extension_fields(
            model_template_name=template_name,
            model_template_version=template_version,
            profile_name=profile_name,
            template_overrides=template_overrides,
            profile_overrides=profile_overrides,
        )
        if aibrix is None:
            aibrix = aibrix_from_spec
        elif aibrix_from_spec is not None:
            merged = aibrix.model_dump(exclude_none=True)
            merged.update(aibrix_from_spec.model_dump(exclude_none=True))
            aibrix = AibrixMetadata.model_validate(merged)

        return BatchJobSpec(
            input_file_id=input_file_id,
            endpoint=endpoint,
            completion_window=cls._safe_get_attr(
                pod_spec,
                "activeDeadlineSeconds",
                CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
            ),
            metadata=batch_metadata if batch_metadata else None,
            opts=batch_opts if batch_opts else None,
            aibrix=aibrix,
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
        cls,
        k8s_job: Any,
        resource_version: Optional[str],
        podAnnotations: Dict[str, str],
        annotations: Dict[str, str],
    ) -> BatchJobStatus:
        """Extract or create BatchJobStatus from Kubernetes job."""
        # Extract job status information
        k8s_status = cls._safe_get_attr(k8s_job, "status", {})
        metadata = cls._safe_get_attr(k8s_job, "metadata", {})

        # Generate or extract batch ID
        job_id = cls._safe_get_attr(metadata, "uid") or str(uuid.uuid4())

        # Map file ids
        output_file_id = podAnnotations.get(JobAnnotationKey.OUTPUT_FILE_ID.value)
        temp_output_file_id = podAnnotations.get(
            JobAnnotationKey.TEMP_OUTPUT_FILE_ID.value
        )
        error_file_id = podAnnotations.get(JobAnnotationKey.ERROR_FILE_ID.value)
        temp_error_file_id = podAnnotations.get(
            JobAnnotationKey.TEMP_ERROR_FILE_ID.value
        )

        # Extract conditions from Kubernetes job
        conditions = cls._extract_conditions(k8s_status, annotations)

        # Map Kubernetes job phase to BatchJobState
        state, finalizing_time = cls._map_k8s_phase_to_batch_state(
            annotations, conditions
        )

        # Extract creation timestamp
        creation_timestamp = cls._convert_timestamp(
            cls._safe_get_attr(metadata, "creation_timestamp")
            or cls._safe_get_attr(metadata, "creationTimestamp")
        )
        if not creation_timestamp:
            creation_timestamp = datetime.now(timezone.utc)

        status = BatchJobStatus(
            jobID=job_id,
            state=state,
            outputFileID=output_file_id,
            tempOutputFileID=temp_output_file_id,
            errorFileID=error_file_id,
            tempErrorFileID=temp_error_file_id,
            createdAt=creation_timestamp,
            finalizingAt=finalizing_time,
            conditions=conditions,
        )

        # Update with persisted annotations if available
        status = cls.update_status_from_annotations(status, annotations)

        logger.debug(
            "Extracted batch job status",
            jobID=job_id,
            resource_version=resource_version,
            state=status.state,
            errors=status.errors,
            k8s_status=k8s_status,
            annotations=annotations,
            status=status,
        )  # type:ignore[call-arg]

        return status

    @classmethod
    def _extract_conditions(
        cls, k8s_status: Any, annotations: Dict[str, str]
    ) -> Optional[List[Condition]]:
        """Extract and convert Kubernetes conditions to AIBrix Condition objects."""
        k8s_conditions = cls._safe_get_attr(k8s_status, "conditions")
        if k8s_conditions is None:
            return None

        conditions = []
        has_failure = False
        suspend_condition = annotations.get(JobAnnotationKey.CONDITION.value)
        for k8s_condition in k8s_conditions:
            condition_type = cls._safe_get_attr(k8s_condition, "type")
            condition_status = cls._safe_get_attr(k8s_condition, "status")
            condition_reason = cls._safe_get_attr(k8s_condition, "reason")
            condition_message = cls._safe_get_attr(k8s_condition, "message")

            # Extract and convert timestamp
            last_transition_time = cls._convert_timestamp(
                cls._safe_get_attr(k8s_condition, "lastTransitionTime")
            )
            if not last_transition_time:
                last_transition_time = datetime.now(timezone.utc)

            # Map Kubernetes condition types to AIBrix ConditionType
            aibrix_condition_type = None
            if condition_type == "Complete" and condition_status == "True":
                aibrix_condition_type = ConditionType.COMPLETED
            elif condition_type == "Failed" and condition_status == "True":
                if condition_reason == "DeadlineExceeded":
                    aibrix_condition_type = ConditionType.EXPIRED
                else:
                    aibrix_condition_type = ConditionType.FAILED
                    has_failure = True
            elif (
                condition_type == "Suspended"
                and condition_status == "True"
                and suspend_condition is not None
            ):
                aibrix_condition_type = ConditionType(suspend_condition)
                if aibrix_condition_type == ConditionType.FAILED:
                    has_failure = True

            # Only add conditions that map to our types
            if aibrix_condition_type:
                conditions.append(
                    Condition(
                        type=aibrix_condition_type,
                        status=ConditionStatus.TRUE,  # We only add True conditions
                        lastTransitionTime=last_transition_time,
                        reason=condition_reason,
                        message=condition_message,
                    )
                )

        # Handle failure during finalizing.
        if (
            not has_failure
            and annotations.get(JobAnnotationKey.JOB_STATE.value)
            == BatchJobState.FINALIZED.value
            and suspend_condition == ConditionType.FAILED.value
        ):
            last_transition_time = cls._convert_timestamp(
                annotations.get(JobAnnotationKey.FINALIZED_AT.value)
            )
            if last_transition_time is None:
                last_transition_time = datetime.now(timezone.utc)

            conditions.append(
                Condition(
                    type=ConditionType.FAILED,
                    status=ConditionStatus.TRUE,  # We only add True conditions
                    lastTransitionTime=last_transition_time,
                )
            )

        logger.debug(
            "conditions check", conditions=len(conditions) if conditions else 0
        )  # type: ignore[call-arg]

        return conditions if len(conditions) > 0 else None

    @classmethod
    def _map_k8s_phase_to_batch_state(
        cls, annotations: Dict[str, str], conditions: Optional[List[Condition]]
    ) -> Tuple[BatchJobState, Optional[datetime]]:
        """
        Map Kubernetes job phase to BatchJobState. Most states can be identified using annotation except:
        1. Job first time created, which could created by the 3rd party.
        2. Job previously in progress and finished that need finalizing, which controlled by the 3rd party.
        A special case is cancelling in progress, where state is finalizing, but we need to confirm the
        finalizing time by check the time the job is suspended.

        As of A.2 the JOB_STATE annotation is no longer authoritative
        and is generally absent. When it is missing, fall through to the
        K8s-native condition signal: a terminal condition implies
        FINALIZING; otherwise the Job is CREATED. The kopf-driven
        ``active_jobs`` view is reconciled with the BatchJobStore via a
        monotonicity check in ``job_updated_handler``, so this fallback
        only ever lifts state — it never regresses one.

        Returns:
            state: BatchJobState
            finalizing_time: datetime, optional
        """
        state_value = annotations.get(JobAnnotationKey.JOB_STATE.value)
        if state_value:
            state = BatchJobState(state_value)
            if state not in [BatchJobState.IN_PROGRESS, BatchJobState.FINALIZING]:
                return state, None
            if conditions and len(conditions) > 0:
                return BatchJobState.FINALIZING, conditions[0].last_transition_time
            return BatchJobState.IN_PROGRESS, None

        # JOB_STATE annotation absent (PR4 slim mode): rely solely on
        # K8s-native conditions. A terminal condition (Complete / Failed
        # / Suspended) means the underlying Job has stopped advancing on
        # its own and the AIBrix state machine is at FINALIZING; without
        # any condition we assume CREATED.
        if conditions and len(conditions) > 0:
            return BatchJobState.FINALIZING, conditions[0].last_transition_time
        return BatchJobState.CREATED, None

    @classmethod
    def _safe_get_attr(cls, obj: Any, attr: str, default: Any = None) -> Any:
        """Safely get attribute from object, supporting both attr access and dict access."""
        if obj is None:
            return default

        # Try dict-like access, use collections.abc.Mapping to support kopf.body
        if isinstance(obj, collections.abc.Mapping):
            val = obj.get(attr, None)
        else:
            val = getattr(obj, attr, None)

        return default if val is None else val

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

    @classmethod
    def create_status_annotations(cls, job_status: BatchJobStatus) -> Dict[str, str]:
        """Create pod template annotations from BatchJobStatus for persistence.

        Args:
            job_status: BatchJobStatus to persist

        Returns:
            Dict of annotations to add to pod template
        """
        annotations = {}

        # Persist batch job state
        annotations[JobAnnotationKey.JOB_STATE.value] = job_status.state.value

        # Persist conditions (failed, cancelled)
        if job_status.check_condition(ConditionType.CANCELLED):
            annotations[JobAnnotationKey.CONDITION.value] = (
                ConditionType.CANCELLED.value
            )
        elif job_status.check_condition(ConditionType.FAILED):
            annotations[JobAnnotationKey.CONDITION.value] = ConditionType.FAILED.value

        # Persist errors
        if job_status.errors is not None and len(job_status.errors) > 0:
            annotations[JobAnnotationKey.ERRORS.value] = json.dumps(
                job_status.errors, default=BatchJobError.json_serializer
            )

        # Persist request counts (only if they contain meaningful data)
        if job_status.request_counts.total > 0:
            request_counts_data = {
                "total": job_status.request_counts.total,
                "launched": job_status.request_counts.launched,
                "completed": job_status.request_counts.completed,
                "failed": job_status.request_counts.failed,
            }
            annotations[JobAnnotationKey.REQUEST_COUNTS.value] = json.dumps(
                request_counts_data
            )

        # Persist token usage (mirrors OpenAI Batch usage object). Stored
        # as a single JSON annotation so it can be migrated wholesale to
        # S3 metadata.json later. Only emitted when at least one token
        # has been counted to avoid bloating annotations on empty jobs.
        if job_status.usage is not None and (
            job_status.usage.input_tokens > 0 or job_status.usage.output_tokens > 0
        ):
            annotations[JobAnnotationKey.USAGE.value] = (
                job_status.usage.model_dump_json(exclude_none=True)
            )

        # Persist timestamps (only if they exist)
        timestamp_mappings = [
            (job_status.in_progress_at, JobAnnotationKey.IN_PROGRESS_AT),
            (job_status.finalizing_at, JobAnnotationKey.FINALIZING_AT),
            (job_status.finalized_at, JobAnnotationKey.FINALIZED_AT),
            (job_status.cancelling_at, JobAnnotationKey.CANCELLING_AT),
        ]

        for timestamp, annotation_key in timestamp_mappings:
            if timestamp is not None:
                annotations[annotation_key.value] = timestamp.isoformat()

        return annotations

    @classmethod
    def update_status_from_annotations(
        cls, job_status: BatchJobStatus, annotations: Dict[str, str]
    ) -> BatchJobStatus:
        """Update BatchJobStatus with data from persisted annotations.

        Args:
            job_status: Existing BatchJobStatus to update
            annotations: Pod template annotations containing persisted status

        Returns:
            Updated BatchJobStatus
        """
        # Update errors if persisted
        if (
            persisted_errors := annotations.get(JobAnnotationKey.ERRORS.value)
        ) is not None:
            try:
                errors: list[dict] = json.loads(persisted_errors)
                job_status.errors = []
                for error in errors:
                    job_status.errors.append(
                        BatchJobError(
                            code=BatchJobErrorCode(
                                error.get("code", BatchJobErrorCode.UNKNOWN_ERROR.value)
                            ),
                            message=str(error.get("message")),
                            param=str(error.get("message")),
                            line=error.get("line"),  # type: ignore[arg-type]
                        )
                    )
            except (json.JSONDecodeError, KeyError) as e:
                logger.warning("Failed to parse persisted errors", error=str(e))  # type: ignore[call-arg]

        # Update request counts if persisted
        if (
            persisted_counts := annotations.get(JobAnnotationKey.REQUEST_COUNTS.value)
        ) is not None:
            try:
                counts_data = json.loads(persisted_counts)
                job_status.request_counts = RequestCountStats(
                    total=counts_data.get("total", 0),
                    launched=counts_data.get("launched", 0),
                    completed=counts_data.get("completed", 0),
                    failed=counts_data.get("failed", 0),
                )
            except (json.JSONDecodeError, KeyError) as e:
                logger.warning("Failed to parse persisted request counts", error=str(e))  # type: ignore[call-arg]

        # Update token usage if persisted (mirrors OpenAI Batch `usage` object)
        if (
            persisted_usage := annotations.get(JobAnnotationKey.USAGE.value)
        ) is not None:
            try:
                job_status.usage = BatchUsage.model_validate_json(persisted_usage)
            except Exception as e:
                logger.warning(
                    "Failed to parse persisted usage; treating as None",
                    error=str(e),
                )  # type: ignore[call-arg]

        # Update timestamps if persisted
        timestamp_mappings = [
            (JobAnnotationKey.IN_PROGRESS_AT, "in_progress_at"),
            (JobAnnotationKey.FINALIZING_AT, "finalizing_at"),
            (JobAnnotationKey.FINALIZED_AT, "finalized_at"),
            (JobAnnotationKey.CANCELLING_AT, "cancelling_at"),
        ]

        for annotation_key, attr_name in timestamp_mappings:
            if (
                persisted_timestamp := annotations.get(annotation_key.value)
            ) is not None and (
                converted_timestamp := cls._convert_timestamp(persisted_timestamp)
            ) is not None:
                setattr(job_status, attr_name, converted_timestamp)

        if job_status.state == BatchJobState.FINALIZED:
            if (
                condition := job_status.get_condition(ConditionType.FAILED)
            ) is not None:
                job_status.failed_at = (
                    job_status.finalized_at or condition.last_transition_time
                )
            elif (
                condition := job_status.get_condition(ConditionType.CANCELLED)
            ) is not None:
                job_status.cancelled_at = (
                    job_status.finalized_at or condition.last_transition_time
                )
            elif (
                condition := job_status.get_condition(ConditionType.EXPIRED)
            ) is not None:
                job_status.expired_at = (
                    job_status.finalized_at or condition.last_transition_time
                )
            elif (
                condition := job_status.get_condition(ConditionType.COMPLETED)
            ) is not None:
                job_status.completed_at = (
                    job_status.finalized_at or condition.last_transition_time
                )

        return job_status


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

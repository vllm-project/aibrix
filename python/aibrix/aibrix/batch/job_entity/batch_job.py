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

import copy
import uuid
from datetime import datetime, timezone
from enum import Enum
from typing import Any, Dict, List, Optional

from pydantic import Field
from pydantic_core import core_schema

from .aibrix_metadata import AibrixMetadata
from .base import _Strict


class BatchJobEndpoint(str, Enum):
    """Valid API endpoints for batch jobs."""

    CHAT_COMPLETIONS = "/v1/chat/completions"
    EMBEDDINGS = "/v1/embeddings"
    COMPLETIONS = "/v1/completions"
    RERANK = "/v1/rerank"


class CompletionWindow(str, Enum):
    """Valid completion windows for batch jobs."""

    TWENTY_FOUR_HOURS = "24h"

    def expires_at(self) -> int:
        """Returns the expiration time of the completion window."""
        return 86400  # Return default value


class BatchJobState(str, Enum):
    """Current state of the batch job."""

    CREATED = "created"
    VALIDATING = "validating"
    IN_PROGRESS = "in_progress"
    CANCELLING = "cancelling"
    FINALIZING = "finalizing"
    FINALIZED = "finalized"


class BatchJobErrorCode(str, Enum):
    """Error codes for batch job."""

    INVALID_INPUT_FILE = "invalid_input_file"
    INVALID_ENDPOINT = "invalid_endpoint"
    INVALID_COMPLETION_WINDOW = "invalid_completion_window"
    INVALID_METADATA = "invalid_metadata"
    AUTHENTICATION_ERROR = "authentication_error"
    INFERENCE_FAILED = "inference_failed"
    PREPARE_OUTPUT_ERROR = "prepare_output_failed"
    FINALIZING_ERROR = "finalizing_failed"
    INVALID_DRIVER = "invalid_driver"
    UNKNOWN_ERROR = "unknown_error"


class ConditionType(str, Enum):
    """Types of conditions for batch job status."""

    COMPLETED = "completed"
    EXPIRED = "expired"
    FAILED = "failed"
    CANCELLED = "cancelled"


class ConditionStatus(str, Enum):
    """Status values for conditions."""

    TRUE = "True"
    FALSE = "False"
    UNKNOWN = "Unknown"


class TypeMeta(_Strict):
    """Kubernetes TypeMeta equivalent."""

    api_version: str = Field(alias="apiVersion")
    kind: str


class ObjectMeta(_Strict):
    """Kubernetes ObjectMeta equivalent."""

    name: Optional[str] = None
    namespace: Optional[str] = None
    uid: Optional[str] = None
    resource_version: Optional[str] = Field(None, alias="resourceVersion")
    generation: Optional[int] = None
    creation_timestamp: Optional[datetime] = Field(None, alias="creationTimestamp")
    deletion_timestamp: Optional[datetime] = Field(None, alias="deletionTimestamp")
    labels: Optional[Dict[str, str]] = None
    annotations: Optional[Dict[str, str]] = None


class Condition(_Strict):
    """Kubernetes Condition equivalent."""

    type: ConditionType
    status: ConditionStatus
    last_transition_time: datetime = Field(alias="lastTransitionTime")
    reason: Optional[str] = None
    message: Optional[str] = None


class BatchJobSpec(_Strict):
    """Defines the specification of a Batch job input."""

    input_file_id: str = Field(
        description="The ID of an uploaded file that contains the requests for the batch",
    )
    endpoint: str = Field(
        description="The API endpoint to be used for all requests in the batch"
    )
    completion_window: int = Field(
        default=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
        description="The time window for completion",
    )
    metadata: Optional[Dict[str, str]] = Field(
        default=None,
        description="Set of up to 16 key-value pairs to attach to the batch object",
    )
    opts: Optional[Dict[str, str]] = Field(
        default=None,
        description="System-only options for internal use (e.g., fail_after_n_requests)",
    )
    # Set by Metadata Service when extra_body.aibrix.* is parsed at batch
    # creation. Values are looked up by TemplateRegistry / ProfileRegistry.
    # Stored as raw strings/dicts so this model has no dependency on the
    # template schema package (avoids circular imports). Validation
    # against actual template/profile existence happens upstream; the
    # renderer re-validates override dicts against the typed schemas.
    aibrix: Optional[AibrixMetadata] = Field(
        default=None,
        description="AIBrix-specific metadata attached to the batch job",
    )

    @property
    def model_template_name(self) -> Optional[str]:
        return self.aibrix.model_template_name if self.aibrix else None

    @property
    def model_template_version(self) -> Optional[str]:
        return self.aibrix.model_template_version if self.aibrix else None

    @property
    def profile_name(self) -> Optional[str]:
        return self.aibrix.profile_name if self.aibrix else None

    @property
    def template_overrides(self) -> Optional[Dict[str, Any]]:
        return self.aibrix.template_overrides if self.aibrix else None

    @property
    def profile_overrides(self) -> Optional[Dict[str, Any]]:
        return self.aibrix.profile_overrides if self.aibrix else None

    @classmethod
    def from_strings(
        cls,
        input_file_id: str,
        endpoint: str,
        completion_window: str = CompletionWindow.TWENTY_FOUR_HOURS.value,
        metadata: Optional[Dict[str, str]] = None,
        opts: Optional[Dict[str, str]] = None,
        aibrix: Optional[Dict[str, Any]] = None,
        **kw,
    ) -> "BatchJobSpec":
        """Create BatchJobSpec from string parameters with validation.

        Args:
            input_file_id: The ID of the input file
            endpoint: The API endpoint as string
            completion_window: The completion window as string
            metadata: Optional metadata dictionary
            opts: Optional system options dictionary
            aibrix: Optional structured AIBrix metadata

        Returns:
            BatchJobSpec instance

        Raises:
            ValueError: If parameters are invalid
        """
        # Validate input file ID
        if not input_file_id:
            raise ValueError("Input file ID cannot be empty")

        # Validate and convert endpoint
        validated_endpoint = cls._validate_endpoint(endpoint)

        # Validate and convert completion window
        validated_completion_window = cls._validate_completion_window(completion_window)

        return cls(
            input_file_id=input_file_id,
            endpoint=validated_endpoint.value,
            completion_window=validated_completion_window.expires_at(),
            metadata=metadata,
            opts=opts,
            aibrix=AibrixMetadata(**aibrix)
            if aibrix
            else AibrixMetadata.from_extension_fields(**kw),
        )

    @staticmethod
    def _validate_endpoint(endpoint_str: str) -> BatchJobEndpoint:
        """Validate and convert endpoint string to BatchJobEndpoint.

        Args:
            endpoint_str: String value of the endpoint

        Returns:
            BatchJobEndpoint enum value

        Raises:
            ValueError: If endpoint is invalid
        """
        if not endpoint_str:
            raise ValueError("Endpoint cannot be empty")

        try:
            return BatchJobEndpoint(endpoint_str)
        except ValueError:
            valid_endpoints = [e.value for e in BatchJobEndpoint]
            raise ValueError(
                f"Invalid endpoint '{endpoint_str}'. Valid values: {valid_endpoints}"
            )

    @staticmethod
    def _validate_completion_window(completion_window_str: str) -> CompletionWindow:
        """Validate and convert completion window string to CompletionWindow.

        Args:
            completion_window_str: String value of the completion window

        Returns:
            CompletionWindow enum value

        Raises:
            ValueError: If completion window is invalid
        """
        if not completion_window_str:
            raise ValueError("Completion window cannot be empty")

        try:
            return CompletionWindow(completion_window_str)
        except ValueError:
            valid_windows = [w.value for w in CompletionWindow]
            raise ValueError(
                f"Invalid completion window '{completion_window_str}'. Valid values: {valid_windows}"
            )


class RequestCountStats(_Strict):
    """Holds the statistics on the processing of the batch."""

    total: int = Field(default=0, description="Total number of requests in the batch")
    launched: int = Field(
        default=0, description="Number of requests that have been launched"
    )
    completed: int = Field(
        default=0,
        description="Number of requests that have been successfully completed",
    )
    failed: int = Field(default=0, description="Number of requests that have failed")


class InputTokensDetails(_Strict):
    """Token-count breakdown for the input side of a batch.

    Mirrors the OpenAI Batch API's ``input_tokens_details`` shape;
    ``cached_tokens`` is the count of input tokens served from the
    engine's prefix cache (only meaningful when prefix caching is on).
    """

    cached_tokens: int = Field(default=0, ge=0)


class OutputTokensDetails(_Strict):
    """Token-count breakdown for the output side of a batch.

    ``reasoning_tokens`` are the chain-of-thought tokens emitted by
    reasoning-class models (o1-style). For non-reasoning models this
    stays at zero.
    """

    reasoning_tokens: int = Field(default=0, ge=0)


class BatchUsage(_Strict):
    """Aggregated token usage for a batch.

    Matches the OpenAI Batch API's ``usage`` object (added 2025-09)
    so it can be returned verbatim. Note that the engine's per-request
    response uses the ``prompt_tokens`` / ``completion_tokens`` naming;
    the worker maps those to ``input_tokens`` / ``output_tokens`` when
    accumulating into this object.
    """

    input_tokens: int = Field(default=0, ge=0)
    output_tokens: int = Field(default=0, ge=0)
    total_tokens: int = Field(default=0, ge=0)
    input_tokens_details: InputTokensDetails = Field(default_factory=InputTokensDetails)
    output_tokens_details: OutputTokensDetails = Field(
        default_factory=OutputTokensDetails
    )


class BatchJobError(Exception):
    """Represents an error that occurred during batch job processing."""

    def __init__(
        self,
        code: BatchJobErrorCode,
        message: str,
        param: Optional[str] = None,
        line: Optional[int] = None,
    ):
        # Pass the primary human-readable message to the parent Exception class.
        super().__init__(message)

        # Store the custom error details as instance attributes.
        self.code: str = code.value
        """A machine-readable error code"""

        self.message: str = message
        """A human-readable error message"""

        self.param: Optional[str] = param
        """The parameter that was invalid or caused the error, if applicable"""

        self.line: Optional[int] = line
        """The line number in the input file where the error occurred, if applicable"""

    @classmethod
    def __get_pydantic_core_schema__(cls, source, handler) -> core_schema.CoreSchema:
        """
        Returns the pydantic-core schema for this class, allowing it to be
        used directly within Pydantic models for both validation and serialization.
        """

        # def serialize_batch_job_error(instance: "BatchJobError") -> Dict[str, Any]:
        #     """Custom serializer for BatchJobError."""
        #     return {
        #         "code": instance.code,
        #         "message": instance.message,
        #         "param": instance.param,
        #         "line": instance.line,
        #     }

        def validate_batch_job_error(value) -> "BatchJobError":
            """Custom validator for BatchJobError."""
            if isinstance(value, cls):
                return value
            elif isinstance(value, dict):
                return cls(
                    code=BatchJobErrorCode(value["code"]),
                    message=value["message"],
                    param=value.get("param"),
                    line=value.get("line"),
                )
            else:
                raise ValueError(f"Cannot convert {type(value)} to BatchJobError")

        return core_schema.no_info_plain_validator_function(
            function=validate_batch_job_error,
            serialization=core_schema.plain_serializer_function_ser_schema(
                function=cls.json_serializer,
                return_schema=core_schema.dict_schema(),
            ),
        )

    @classmethod
    def json_serializer(cls, obj: Any):
        """Handles types that the default JSON serializer doesn't know."""
        if isinstance(obj, cls):
            return {
                "code": obj.code,
                "message": obj.message,
                "param": obj.param,
                "line": obj.line,
            }

        return obj

    def __deepcopy__(self, memo):
        """
        Provides a custom implementation for deep copying this object.
        """
        # Create a new instance by calling __init__ with the current object's data.
        # This correctly provides all the required arguments.
        new_copy = self.__class__(
            code=BatchJobErrorCode(self.code),
            message=self.message,
            param=self.param,
            line=self.line,
        )

        # Standard practice: store the new object in the memo dictionary
        # to handle potential circular references during the copy.
        memo[id(self)] = new_copy

        return new_copy


class BatchJobStatus(_Strict):
    """Defines the observed state of BatchJobSpec."""

    job_id: str = Field(
        alias="jobID", description="The unique identifier for the batch job"
    )
    state: BatchJobState = Field(description="The current state of the batch job")

    errors: Optional[List[BatchJobError]] = Field(
        default=None,
        description="List of errors that occurred during the batch job processing",
    )

    temp_output_file_id: Optional[str] = Field(
        default=None,
        alias="tempOutputFileID",
        description="The ID of the file containing the results of successfully completed requests",
    )
    temp_error_file_id: Optional[str] = Field(
        default=None,
        alias="tempErrorFileID",
        description="The ID of the file containing details for any failed requests",
    )

    output_file_id: Optional[str] = Field(
        default=None,
        alias="outputFileID",
        description="The ID of the file containing the results of successfully completed requests",
    )
    error_file_id: Optional[str] = Field(
        default=None,
        alias="errorFileID",
        description="The ID of the file containing details for any failed requests",
    )

    request_counts: RequestCountStats = Field(
        default_factory=RequestCountStats,
        alias="requestCounts",
        description="Statistics on the processing of the batch",
    )

    usage: Optional[BatchUsage] = Field(
        default=None,
        description=(
            "Aggregated token usage. Populated by the worker as it processes "
            "requests; absent until the first progress flush."
        ),
    )

    # Timestamps
    created_at: datetime = Field(
        alias="createdAt", description="Timestamp of when the batch job was created"
    )
    in_progress_at: Optional[datetime] = Field(
        default=None,
        alias="inProgressAt",
        description="Timestamp of when the batch job started processing",
    )
    finalizing_at: Optional[datetime] = Field(
        default=None,
        alias="finalizingAt",
        description="Timestamp of when the batch job started finalizing",
    )
    finalized_at: Optional[datetime] = Field(
        default=None,
        alias="finalizedAt",
        description="Timestamp of when the batch job was finalized, will be copied to completed_at, failed_at, expired_at, and cancelled_at based on condition",
    )
    completed_at: Optional[datetime] = Field(
        default=None,
        alias="completedAt",
        description="Timestamp of when the batch job was completed",
    )
    failed_at: Optional[datetime] = Field(
        default=None,
        alias="failedAt",
        description="Timestamp of when the batch job failed",
    )
    expired_at: Optional[datetime] = Field(
        default=None,
        alias="expiredAt",
        description="Timestamp of when the batch job expired",
    )
    cancelling_at: Optional[datetime] = Field(
        default=None,
        alias="cancellingAt",
        description="Timestamp of when the batch job start cancelling",
    )
    cancelled_at: Optional[datetime] = Field(
        default=None,
        alias="cancelledAt",
        description="Timestamp of when the batch job get cancelled",
    )

    conditions: Optional[List[Condition]] = Field(
        default=None,
        description="Conditions represent the latest available observations of the batch job's state",
    )

    @property
    def finished(self) -> bool:
        return self.state == BatchJobState.FINALIZED

    @property
    def completed(self) -> bool:
        return self.finished and self.check_condition(ConditionType.COMPLETED)

    @property
    def failed(self) -> bool:
        return (
            self.finished
            and self.check_condition(ConditionType.FAILED)
            and not self.check_condition(ConditionType.EXPIRED)
        )

    @property
    def expired(self) -> bool:
        return self.finished and self.check_condition(ConditionType.EXPIRED)

    @property
    def cancelled(self) -> bool:
        return self.finished and self.check_condition(ConditionType.CANCELLED)

    @property
    def condition(self) -> Optional[ConditionType]:
        """If mutiple conditions exists, expired > failed > cancelled > completed"""
        if self.conditions is None:
            return None
        elif self.check_condition(ConditionType.EXPIRED):
            return ConditionType.EXPIRED
        elif self.check_condition(ConditionType.FAILED):
            return ConditionType.FAILED
        elif self.check_condition(ConditionType.CANCELLED):
            return ConditionType.CANCELLED
        elif self.check_condition(ConditionType.COMPLETED):
            return ConditionType.COMPLETED
        else:
            return None

    def check_condition(self, type: ConditionType) -> bool:
        if self.conditions is None:
            return False

        for condition in self.conditions:
            if condition.type == type:
                return True

        return False

    def get_condition(self, type: ConditionType) -> Optional[Condition]:
        if self.conditions is None:
            return None

        for condition in self.conditions:
            if condition.type == type:
                return condition

        return None

    def add_condition(self, condition: Condition):
        if self.conditions is None:
            self.conditions = []
        self.conditions.append(condition)


class BatchJob(_Strict):
    """Schema for the BatchJob API - Kubernetes Custom Resource equivalent."""

    session_id: Optional[str] = Field(
        default=None,
        alias="sessionID",
        description="Session ID used to track job creation",
    )
    type_meta: TypeMeta = Field(alias="typeMeta", description="Kubernetes TypeMeta")
    metadata: ObjectMeta = Field(description="Kubernetes ObjectMeta")
    spec: BatchJobSpec = Field(description="Desired state of the batch job")
    status: BatchJobStatus = Field(description="Observed state of the batch job")

    def copy(self):
        return BatchJob(
            sessionID=self.session_id,
            typeMeta=self.type_meta,
            metadata=self.metadata,
            spec=self.spec,
            status=copy.deepcopy(self.status),
        )

    @classmethod
    def new(
        cls,
        name: str,
        namespace: str,
        input_file_id: str,
        endpoint: BatchJobEndpoint,
        completion_window: CompletionWindow = CompletionWindow.TWENTY_FOUR_HOURS,
        metadata: Optional[Dict[str, str]] = None,
    ) -> "BatchJob":
        """Create a new BatchJob with default values."""
        return cls.new_from_spec(
            name,
            namespace,
            spec=BatchJobSpec(
                input_file_id=input_file_id,
                endpoint=endpoint.value,
                completion_window=completion_window.expires_at(),
                metadata=metadata,
            ),
        )

    @classmethod
    def new_from_spec(
        cls,
        name: str,
        namespace: str,
        spec: BatchJobSpec,
    ) -> "BatchJob":
        return cls(
            typeMeta=TypeMeta(apiVersion="batch.aibrix.ai/v1alpha1", kind="BatchJob"),
            metadata=ObjectMeta(
                name=name,
                namespace=namespace,
                creationTimestamp=datetime.now(timezone.utc),
                resourceVersion=None,
                deletionTimestamp=None,
            ),
            spec=spec,
            status=BatchJobStatus(
                jobID=str(uuid.uuid4()),
                state=BatchJobState.CREATED,
                createdAt=datetime.now(timezone.utc),
            ),
        )

    @classmethod
    def new_local(
        cls,
        spec: BatchJobSpec,
        request_count: int = 0,
    ) -> "BatchJob":
        # Pre-seed request_counts.total from the validated input line
        # count so it is fixed at job creation, matching OpenAI Batch
        # API semantics. When 0 (caller didn't validate upfront), the
        # JobMetaInfo falls back to the legacy "discover total while
        # streaming" behavior.
        request_counts = (
            RequestCountStats(total=request_count) if request_count > 0 else None
        )
        status_kwargs: Dict[str, Any] = {
            "jobID": str(uuid.uuid4()),
            "state": BatchJobState.CREATED,
            "createdAt": datetime.now(timezone.utc),
        }
        if request_counts is not None:
            status_kwargs["requestCounts"] = request_counts
        return cls(
            typeMeta=TypeMeta(apiVersion="", kind="LocalBatchJob"),
            metadata=ObjectMeta(
                creationTimestamp=datetime.now(timezone.utc),
                resourceVersion=None,
                deletionTimestamp=None,
            ),
            spec=spec,
            status=BatchJobStatus(**status_kwargs),
        )

    @property
    def job_id(self) -> Optional[str]:
        """Get the job ID."""
        return self.status.job_id if self.status else None

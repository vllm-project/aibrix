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

from datetime import datetime, timezone
from enum import Enum
from typing import Dict, List, Optional

from pydantic import BaseModel, ConfigDict, Field
from pydantic_core import core_schema


class NoExtraBaseModel(BaseModel):
    """Base model that forbids extra fields."""

    model_config = ConfigDict(extra="forbid")


class BatchJobEndpoint(str, Enum):
    """Valid API endpoints for batch jobs."""

    CHAT_COMPLETIONS = "/v1/chat/completions"
    EMBEDDINGS = "/v1/embeddings"
    COMPLETIONS = "/v1/completions"


class CompletionWindow(str, Enum):
    """Valid completion windows for batch jobs."""

    TWENTY_FOUR_HOURS = "24h"

    def expires_at(self) -> float:
        """Returns the expiration time of the completion window."""
        return 86400.0  # Return default value


class BatchJobState(str, Enum):
    """Current state of the batch job."""

    CREATED = "created"
    VALIDATING = "validating"
    IN_PROGRESS = "in_progress"
    FINALIZING = "finalizing"
    COMPLETED = "completed"
    EXPIRED = "expired"
    FAILED = "failed"
    CANCELLING = "cancelling"
    CANCELED = "canceled"


class BatchJobErrorCode(str, Enum):
    """Error codes for batch job."""

    INVALID_INPUT_FILE = "invalid_input_file"
    INVALID_ENDPOINT = "invalid_endpoint"
    INVALID_COMPLETION_WINDOW = "invalid_completion_window"
    INVALID_METADATA = "invalid_metadata"
    AUTHENTICATION_ERROR = "authentication_error"
    UNKNOWN_ERROR = "unknown_error"


class ConditionType(str, Enum):
    """Types of conditions for batch job status."""

    READY = "Ready"
    PROCESSING = "Processing"
    FAILED = "Failed"


class ConditionStatus(str, Enum):
    """Status values for conditions."""

    TRUE = "True"
    FALSE = "False"
    UNKNOWN = "Unknown"


class TypeMeta(NoExtraBaseModel):
    """Kubernetes TypeMeta equivalent."""

    api_version: str = Field(alias="apiVersion")
    kind: str


class ObjectMeta(NoExtraBaseModel):
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


class Condition(NoExtraBaseModel):
    """Kubernetes Condition equivalent."""

    type: ConditionType
    status: ConditionStatus
    last_transition_time: datetime = Field(alias="lastTransitionTime")
    reason: Optional[str] = None
    message: Optional[str] = None


class BatchJobSpec(NoExtraBaseModel):
    """Defines the desired state of a Batch job, which is OpenAI batch compatible."""

    input_file_id: str = Field(
        alias="inputFileID",
        description="The ID of an uploaded file that contains the requests for the batch",
    )
    endpoint: BatchJobEndpoint = Field(
        description="The API endpoint to be used for all requests in the batch"
    )
    completion_window: CompletionWindow = Field(
        default=CompletionWindow.TWENTY_FOUR_HOURS,
        alias="completionWindow",
        description="The time window for completion",
    )
    metadata: Optional[Dict[str, str]] = Field(
        default=None,
        description="Set of up to 16 key-value pairs to attach to the batch object",
        max_length=16,
    )

    @classmethod
    def from_strings(
        cls,
        input_file_id: str,
        endpoint: str,
        completion_window: str = CompletionWindow.TWENTY_FOUR_HOURS.value,
        metadata: Optional[Dict[str, str]] = None,
    ) -> "BatchJobSpec":
        """Create BatchJobSpec from string parameters with validation.

        Args:
            input_file_id: The ID of the input file
            endpoint: The API endpoint as string
            completion_window: The completion window as string
            metadata: Optional metadata dictionary

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
            inputFileID=input_file_id,
            endpoint=validated_endpoint,
            completionWindow=validated_completion_window,
            metadata=metadata,
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


class RequestCountStats(NoExtraBaseModel):
    """Holds the statistics on the processing of the batch."""

    total: Optional[int] = Field(
        default=0, description="Total number of requests in the batch"
    )
    completed: Optional[int] = Field(
        default=0,
        description="Number of requests that have been successfully completed",
    )
    failed: Optional[int] = Field(
        default=0, description="Number of requests that have failed"
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
    def __get_pydantic_core_schema__(
        cls, source, handler
    ) -> core_schema.CoreSchema:
        """
        Returns the pydantic-core schema for this class, allowing it to be
        used directly within Pydantic models.
        """
        # This defines the schema for the arguments to __init__
        arguments_schema = core_schema.model_fields_schema({
            'code': core_schema.model_field(core_schema.str_schema()),
            'message': core_schema.model_field(core_schema.str_schema()),
            'param': core_schema.model_field(core_schema.nullable_schema(core_schema.str_schema())),
            'line': core_schema.model_field(core_schema.nullable_schema(core_schema.str_schema())),
        })
        return core_schema.call_schema(
            arguments_schema,
            function=cls,
        )

class BatchJobStatus(NoExtraBaseModel):
    """Defines the observed state of BatchJobSpec."""

    job_id: str = Field(
        alias="jobID", description="The unique identifier for the batch job"
    )
    state: BatchJobState = Field(description="The current state of the batch job")

    errors: Optional[List[BatchJobError]] = Field(
        default=None,
        description="List of errors that occurred during the batch job processing",
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

    request_counts: Optional[RequestCountStats] = Field(
        default=None,
        alias="requestCounts",
        description="Statistics on the processing of the batch",
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

    conditions: Optional[Condition] = Field(
        default=None,
        description="Conditions represent the latest available observations of the batch job's state",
    )


class BatchJob(NoExtraBaseModel):
    """Schema for the BatchJob API - Kubernetes Custom Resource equivalent."""

    type_meta: TypeMeta = Field(alias="typeMeta", description="Kubernetes TypeMeta")
    metadata: ObjectMeta = Field(description="Kubernetes ObjectMeta")
    spec: BatchJobSpec = Field(description="Desired state of the batch job")
    status: Optional[BatchJobStatus] = Field(
        default=None, description="Observed state of the batch job"
    )

    @classmethod
    def create_new(
        cls,
        name: str,
        namespace: str,
        input_file_id: str,
        endpoint: BatchJobEndpoint,
        completion_window: CompletionWindow = CompletionWindow.TWENTY_FOUR_HOURS,
        metadata: Optional[Dict[str, str]] = None,
    ) -> "BatchJob":
        """Create a new BatchJob with default values."""
        return cls(
            typeMeta=TypeMeta(apiVersion="batch.aibrix.ai/v1alpha1", kind="BatchJob"),
            metadata=ObjectMeta(
                name=name,
                namespace=namespace,
                creationTimestamp=datetime.now(timezone.utc),
                resourceVersion=None,
                deletionTimestamp=None,
            ),
            spec=BatchJobSpec(
                inputFileID=input_file_id,
                endpoint=endpoint,
                completionWindow=completion_window,
                metadata=metadata,
            ),
        )

    @property
    def job_id(self) -> Optional[str]:
        """Get the job ID."""
        return self.status.job_id if self.status else None

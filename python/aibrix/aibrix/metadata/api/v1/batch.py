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
import json
import traceback
import uuid
from datetime import datetime, timedelta
from typing import Dict, List, Optional

from fastapi import APIRouter, HTTPException, Query, Request
from pydantic import BaseModel, Field

from aibrix.batch import BatchDriver
from aibrix.batch.job_entity import (
    AibrixMetadata,
    BatchJob,
    BatchJobEndpoint,
    BatchJobError,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    BatchProfileRef,
    BatchUsage,
    CompletionWindow,
    ModelTemplateRef,
    PlannerDecision,
)
from aibrix.batch.manifest import RenderError
from aibrix.batch.storage.batch_metastore import get_batch_job
from aibrix.batch.template import (
    ProfileRegistry,
    TemplateRegistry,
)
from aibrix.logger import init_logger
from aibrix.storage.base import BaseStorage

logger = init_logger(__name__)

router = APIRouter()

# Batch input limits
MAX_BATCH_REQUESTS = 50000  # Maximum number of requests per batch

# Constants for validation (defined outside loop for efficiency)
REQUIRED_FIELDS = ["custom_id", "method", "url", "body"]
VALID_HTTP_METHODS = {"GET", "POST", "PUT", "DELETE", "PATCH"}

# Endpoint-specific required body fields
# These define the minimum required fields in the request body for each endpoint type
ENDPOINT_REQUIRED_BODY_FIELDS: dict[str, list[str]] = {
    BatchJobEndpoint.CHAT_COMPLETIONS.value: ["model", "messages"],
    BatchJobEndpoint.COMPLETIONS.value: ["model", "prompt"],
    BatchJobEndpoint.EMBEDDINGS.value: ["model", "input"],
    BatchJobEndpoint.RERANK.value: ["model", "query", "documents"],
}


def _validate_request_body_for_endpoint(
    body: dict, endpoint: str, line_num: int
) -> Optional[str]:
    """Validate request body fields are appropriate for the given endpoint.

    Args:
        body: The request body dictionary
        endpoint: The API endpoint string (e.g., "/v1/chat/completions")
        line_num: Line number for error reporting

    Returns:
        Error message string if validation fails, None if valid
    """
    required_fields = ENDPOINT_REQUIRED_BODY_FIELDS.get(endpoint)
    if required_fields is None:
        # Unknown endpoint, skip body validation
        return None

    for field in required_fields:
        if field not in body:
            return (
                f"Line {line_num}: Request body for endpoint '{endpoint}' "
                f"is missing required field '{field}'"
            )

    # Endpoint-specific type validation
    if endpoint == BatchJobEndpoint.CHAT_COMPLETIONS.value:
        if not isinstance(body.get("messages"), list):
            return f"Line {line_num}: 'messages' must be a list for {endpoint}"
    elif endpoint == BatchJobEndpoint.COMPLETIONS.value:
        prompt = body.get("prompt")
        if not isinstance(prompt, (str, list)):
            return f"Line {line_num}: 'prompt' must be a string or list for {endpoint}"
    elif endpoint == BatchJobEndpoint.EMBEDDINGS.value:
        input_val = body.get("input")
        if not isinstance(input_val, (str, list)):
            return f"Line {line_num}: 'input' must be a string or list for {endpoint}"
    elif endpoint == BatchJobEndpoint.RERANK.value:
        if not isinstance(body.get("query"), str):
            return f"Line {line_num}: 'query' must be a string for {endpoint}"
        if not isinstance(body.get("documents"), list):
            return f"Line {line_num}: 'documents' must be a list for {endpoint}"

    return None


async def _validate_batch_input_file(
    storage: BaseStorage, file_id: str, endpoint: str
) -> tuple[int, Optional[str]]:
    """Validate batch input file format and content.

    Args:
        storage: Storage backend instance
        file_id: ID of the input file to validate
        endpoint: Expected endpoint for requests (validated against request URLs)

    Returns:
        Tuple of (request_count, error_message)
        - request_count: Number of valid non-empty requests
        - error_message: None if validation passes, error string otherwise

    Validates:
        - File exists and is readable
        - JSONL format (valid JSON on each line)
        - Required fields present: custom_id, method, url, body
        - Field types are correct
        - HTTP methods are valid
        - Request count doesn't exceed MAX_BATCH_REQUESTS
        - Endpoint matches the batch endpoint (if provided)

    Note:
        Uses streaming to avoid loading entire file into memory.
        Only counts non-empty lines toward the request limit.
    """
    try:
        request_count = 0
        line_num = 0

        # Stream file line by line to avoid memory issues with large files
        async for line in storage.readline_iter(file_id):
            line_num += 1
            line_stripped = line.strip()

            # Skip empty lines (don't count toward request limit)
            if not line_stripped:
                continue

            request_count += 1

            # Check request limit before processing
            if request_count > MAX_BATCH_REQUESTS:
                return (
                    request_count,
                    f"Batch input contains more than {MAX_BATCH_REQUESTS} requests",
                )

            # Validate JSON format
            try:
                request = json.loads(line_stripped)
            except json.JSONDecodeError as e:
                return 0, f"Line {line_num}: Invalid JSON - {str(e)}"

            # Validate required fields
            for field in REQUIRED_FIELDS:
                if field not in request:
                    return 0, f"Line {line_num}: Missing required field '{field}'"

            # Validate field types
            if not isinstance(request.get("custom_id"), str):
                return 0, f"Line {line_num}: 'custom_id' must be a string"

            if not isinstance(request.get("method"), str):
                return 0, f"Line {line_num}: 'method' must be a string"

            if not isinstance(request.get("url"), str):
                return 0, f"Line {line_num}: 'url' must be a string"

            if not isinstance(request.get("body"), dict):
                return 0, f"Line {line_num}: 'body' must be an object"

            # Validate HTTP method
            if request["method"].upper() not in VALID_HTTP_METHODS:
                return (
                    0,
                    f"Line {line_num}: Invalid HTTP method '{request['method']}'",
                )

            # Validate endpoint matches if provided
            request_url = request["url"]
            if endpoint and not request_url.endswith(endpoint):
                return (
                    0,
                    f"Line {line_num}: Request URL '{request_url}' does not match "
                    f"batch endpoint '{endpoint}'",
                )

            # Validate request body has required fields for the endpoint
            # Use the canonical `endpoint` (not the per-line `request_url`)
            # to ensure validation always matches a known endpoint key.
            body_error = _validate_request_body_for_endpoint(
                request["body"], endpoint, line_num
            )
            if body_error:
                return 0, body_error

        # Check if file was empty
        if request_count == 0:
            return 0, "Batch input file is empty or contains only empty lines"

        return request_count, None

    except FileNotFoundError:
        return 0, f"Input file '{file_id}' not found"
    except Exception as e:
        logger.error("Error validating batch input", file_id=file_id, error=str(e))  # type: ignore[call-arg]
        return 0, f"Failed to validate input file: {str(e)}"


# OpenAI Batch API request/response models
class Decision(PlannerDecision):
    """Decision from planner

    Wire shape (under ``extra_body.aibrix.planner_decision``)::

        {
            "provision_id": "xxxxxxxx",
            "provision_resource_deadline": 1422443902,  # optional; 0 / null = will not expire
            "resource_details": [
                {
                    "resource_type": "xxxxxxxx",
                    "endpoint_cluster": 1422443902,  # optional; 0 / null = will not expire
                    "gpu_type": "A100-SM-80G",  # optional
                    "worker_num": 2,  # optional; 1 / null = 1
                }
            ],
        }
    """


class TemplateRef(ModelTemplateRef):
    """Reference to a ModelDeploymentTemplate registered via ConfigMap.

    Wire shape (under ``extra_body.aibrix.model_template``)::

        {
            "name": "llama3-70b-prod",
            "version": "v1.3.0",  # optional; "" / null = latest active
            "overrides": {  # optional, allowlisted
                "engine_args": {"max_num_seqs": "512"}
            },
        }
    """


class ProfileRef(BatchProfileRef):
    """Reference to a BatchProfile registered via ConfigMap.

    Wire shape (under ``extra_body.aibrix.profile``)::

        {
            "name": "prod-24h",
            "overrides": {  # optional, allowlisted
                "scheduling": {"max_concurrency": 32}
            },
        }
    """


class AibrixExtension(BaseModel):
    """AIBrix extension fields carried under extra_body.aibrix.

    Authoritative shape is documented in
    apps/console/api/proto/console/v1/console.proto under
    "Batch SDK contract (forward reference)". Keep these two in sync.

    OpenAI SDK users transmit this via::

        client.batches.create(
            input_file_id="...",
            endpoint="/v1/chat/completions",
            extra_body={
                "aibrix": {
                    "model_template": {"name": "llama3-70b-prod"},
                    "profile": {"name": "prod-24h"},
                }
            },
        )

    A request omitting ``aibrix.model_template`` is rejected at render
    time as a 400 ('model_template_name is required'). The legacy
    hardcoded yaml fallback was removed in this same release; admins
    must register at least one ModelDeploymentTemplate via the
    ConfigMap to accept any batch. See
    docs/source/features/batch-templates.rst.

    Inline ``model_template_spec`` is intentionally NOT supported.
    Templates are the curated security/cost gate; bypassing them via
    inline spec would leak image / GPU SKU / namespace control to
    users and shatter audit by template name.
    """

    model_config = {"extra": "allow"}

    job_id: Optional[str] = None
    planner_decision: Optional[PlannerDecision] = None
    model_template: Optional[TemplateRef] = Field(
        default=None,
        description="ModelDeploymentTemplate reference (name + optional version + overrides)",
    )
    profile: Optional[ProfileRef] = Field(
        default=None,
        description="BatchProfile reference; falls back to registry default if omitted",
    )


class BatchSpec(BaseModel):
    """Defines the specification of a Batch job input, which is OpenAI batch compatible."""

    input_file_id: str = Field(
        description="The ID of an uploaded file that contains the requests for the batch",
    )
    endpoint: BatchJobEndpoint = Field(
        description="The API endpoint to be used for all requests in the batch"
    )
    completion_window: CompletionWindow = Field(
        default=CompletionWindow.TWENTY_FOUR_HOURS,
        description="The time window for completion",
    )
    metadata: Optional[Dict[str, str]] = Field(
        default=None,
        description="Set of up to 16 key-value pairs to attach to the batch object",
        max_length=16,
    )
    aibrix: Optional[AibrixExtension] = Field(
        default=None,
        description=(
            "AIBrix extension namespace. Carries ConfigMap-driven model "
            "template selection, profile, and per-batch overrides. "
            "Absent block routes to the legacy yaml path."
        ),
    )

    @classmethod
    def newBatchJobSpec(cls, spec: "BatchSpec") -> BatchJobSpec:
        aibrix: Optional[AibrixMetadata] = None
        if spec.aibrix is not None:
            aibrix = AibrixMetadata(
                job_id=spec.aibrix.job_id,
                planner_decision=spec.aibrix.planner_decision,
                model_template=spec.aibrix.model_template,
                profile=spec.aibrix.profile,
            )
        return BatchJobSpec(
            input_file_id=spec.input_file_id,
            endpoint=spec.endpoint.value,
            completion_window=spec.completion_window.expires_at(),
            metadata=spec.metadata,
            aibrix=aibrix,
        )


def _validate_aibrix_extension(
    request: Request, extension: Optional[AibrixExtension]
) -> None:
    """Reject the request early if the named template / profile is unknown.

    Looks up registries on app.state if present. When registries are
    not yet wired (e.g. during rollout where some deployments
    have not yet attached registries), this is a no-op and validation
    falls back to render time with a less-friendly error path.
    """
    if extension is None or extension.model_template is None:
        return

    template_registry: Optional[TemplateRegistry] = getattr(
        request.app.state, "template_registry", None
    )
    profile_registry: Optional[ProfileRegistry] = getattr(
        request.app.state, "profile_registry", None
    )

    if template_registry is None:
        return  # registries not yet configured; defer to renderer

    tref = extension.model_template
    if tref.version:
        # Pin: must match exactly
        resolved = template_registry.get_by_version(tref.name, tref.version)
        if resolved is None:
            available = template_registry.names()
            raise HTTPException(
                status_code=400,
                detail=(
                    f"aibrix.model_template '{tref.name}@{tref.version}' not found. "
                    f"Templates with at least one active version: {available}"
                ),
            )
    else:
        if template_registry.get(tref.name) is None:
            available = template_registry.names()
            raise HTTPException(
                status_code=400,
                detail=(
                    f"aibrix.model_template '{tref.name}' has no active version. "
                    f"Templates with at least one active version: {available}"
                ),
            )

    if extension.profile is not None and profile_registry is not None:
        pref = extension.profile
        if profile_registry.get(pref.name) is None:
            available = profile_registry.names()
            raise HTTPException(
                status_code=400,
                detail=(
                    f"aibrix.profile '{pref.name}' not found. "
                    f"Available profiles: {available}"
                ),
            )


class BatchRequestCounts(BaseModel):
    """Request counts for OpenAI batch API."""

    total: int = Field(description="Total number of requests in the batch")
    completed: int = Field(
        description="Number of requests that have been successfully completed"
    )
    failed: int = Field(description="Number of requests that have failed")


class BatchErrors(BaseModel):
    """Error model for batch operations."""

    data: List[BatchJobError] = Field(
        description="List of errors that occurred during processing"
    )
    object: str = Field(
        default="list", description="The object type, which is always `list`"
    )


class BatchResponse(BaseModel):
    """Response model for batch operations."""

    id: str = Field(description="The unique identifier for the batch")
    object: str = Field(default="batch", description="The object type")
    endpoint: str = Field(description="The API endpoint used for the batch")
    model: Optional[str] = Field(
        default=None,
        description=(
            "Model identifier the batch was submitted with. Mirrors the "
            "OpenAI Batch object's optional ``model`` field. AIBrix "
            "reports the resolved ModelDeploymentTemplate name (e.g. "
            "'llama3-70b-prod')."
        ),
    )
    errors: Optional[BatchErrors] = Field(
        default=None, description="List of errors that occurred during processing"
    )
    input_file_id: str = Field(description="The ID of the input file")
    completion_window: str = Field(description="The completion window")
    status: str = Field(description="Current status of the batch")
    output_file_id: Optional[str] = Field(
        default=None, description="The ID of the file containing the results"
    )
    error_file_id: Optional[str] = Field(
        default=None, description="The ID of the file containing error details"
    )
    created_at: int = Field(description="Unix timestamp of when the batch was created")
    in_progress_at: Optional[int] = Field(
        default=None, description="Unix timestamp of when the batch started processing"
    )
    expires_at: int = Field(description="Unix timestamp of when the batch expires")
    finalizing_at: Optional[int] = Field(
        default=None, description="Unix timestamp of when the batch started finalizing"
    )
    completed_at: Optional[int] = Field(
        default=None, description="Unix timestamp of when the batch was completed"
    )
    failed_at: Optional[int] = Field(
        default=None, description="Unix timestamp of when the batch failed"
    )
    expired_at: Optional[int] = Field(
        default=None, description="Unix timestamp of when the batch expired"
    )
    cancelling_at: Optional[int] = Field(
        default=None, description="Unix timestamp of when the batch started cancelling"
    )
    cancelled_at: Optional[int] = Field(
        default=None, description="Unix timestamp of when the batch was cancelled"
    )
    request_counts: Optional[BatchRequestCounts] = Field(
        default=None, description="Statistics on the processing of the batch"
    )
    usage: Optional[BatchUsage] = Field(
        default=None,
        description=(
            "Aggregated token usage. Mirrors the OpenAI Batch API field "
            "added in 2025-09. Absent until the worker has flushed at "
            "least one progress checkpoint."
        ),
    )
    metadata: Optional[Dict[str, str]] = Field(
        default=None, description="Batch metadata"
    )
    aibrix: Optional[AibrixMetadata] = Field(
        default=None,
        description="AIBrix-specific batch metadata",
    )


class BatchListResponse(BaseModel):
    """Response model for listing batches."""

    object: str = Field(default="list", description="The object type")
    data: List[BatchResponse] = Field(description="List of batch objects")
    first_id: Optional[str] = Field(default=None, description="First ID in the list")
    last_id: Optional[str] = Field(default=None, description="Last ID in the list")
    has_more: bool = Field(
        default=False, description="Whether there are more results available"
    )


def _batch_job_to_openai_response(batch_job: BatchJob) -> BatchResponse:
    """Convert BatchJob to OpenAI batch response format."""
    status: BatchJobStatus = batch_job.status
    spec: BatchJobSpec = batch_job.spec

    def dt_to_unix(dt: Optional[datetime]) -> Optional[int]:
        """Convert datetime to unix timestamp."""
        return int(dt.timestamp()) if dt else None

    # Convert request counts
    request_counts = None
    if status.request_counts and status.request_counts.total > 0:
        request_counts = BatchRequestCounts(
            total=status.request_counts.total,
            completed=status.request_counts.completed,
            failed=status.request_counts.failed,
        )

    created_at_unix = dt_to_unix(status.created_at)
    assert created_at_unix is not None

    delta = timedelta(seconds=spec.completion_window)
    total_hours = delta.total_seconds() / 3600
    completion_window = f"{int(total_hours)}h"

    # Map the internal state machine to OpenAI's 8-state enum:
    #   {validating, failed, in_progress, finalizing, completed, expired,
    #    cancelling, cancelled}
    # Two internal states have no direct OpenAI counterpart:
    #   - CREATED: batch just created, validation not yet started — surfaced
    #     to clients as 'validating' since they cannot distinguish it from
    #     in-flight validation.
    #   - FINALIZED: terminal umbrella state; the actual outcome lives in
    #     `status.condition` (completed / failed / expired / cancelled).
    if status.finished:
        condition = status.condition
        if condition is None:
            logger.error(
                "Unexpected job finalized without condition",
                job_id=batch_job.job_id,
                state=status.state.value,
                conditions=status.conditions,
            )  # type:ignore[call-arg]
            raise ValueError("job finalized without condition")
        state = condition.value
    elif status.state == BatchJobState.CREATED:
        state = BatchJobState.VALIDATING.value
    else:
        state = status.state.value

    return BatchResponse(
        id=status.job_id,
        endpoint=spec.endpoint,
        model=spec.model_template_name,
        errors=BatchErrors(data=status.errors) if status.errors else None,
        input_file_id=spec.input_file_id,
        completion_window=completion_window,
        status=state,
        output_file_id=status.output_file_id,
        error_file_id=status.error_file_id,
        created_at=created_at_unix,
        in_progress_at=dt_to_unix(status.in_progress_at),
        expires_at=created_at_unix + spec.completion_window,
        finalizing_at=dt_to_unix(status.finalizing_at),
        completed_at=dt_to_unix(status.completed_at),
        failed_at=dt_to_unix(status.failed_at),
        expired_at=dt_to_unix(status.expired_at),
        cancelling_at=dt_to_unix(status.cancelling_at),
        cancelled_at=dt_to_unix(status.cancelled_at),
        request_counts=request_counts,
        usage=status.usage,
        metadata=spec.metadata,
        aibrix=spec.aibrix,
    )


@router.post("/", include_in_schema=False)
@router.post("")
async def create_batch(request: Request, batch_spec: BatchSpec) -> BatchResponse:
    """Create a new batch.

    Creates a new batch for processing multiple requests. The batch will be
    processed asynchronously and can be monitored using the batch ID.
    """
    try:
        # Validate aibrix extension (template / profile existence) before
        # deeper validation. Surfaces a friendly 400 with the list of
        # available templates / profiles when the user picks an unknown
        # name, instead of letting the renderer fail later.
        _validate_aibrix_extension(request, batch_spec.aibrix)

        # Convert OpenAI-shaped request body into the internal job spec.
        batch_request: BatchJobSpec = BatchSpec.newBatchJobSpec(batch_spec)

        # Get job controller from app state
        batch_driver: BatchDriver = request.app.state.batch_driver

        # Generate session ID for tracking
        session_id = str(uuid.uuid4())

        logger.info(
            "Creating batch",
            input_file_id=batch_request.input_file_id,
            endpoint=batch_request.endpoint,
            completion_window=batch_request.completion_window,
            session_id=session_id,
        )  # type: ignore[call-arg]

        # Validate input file format
        storage = request.app.state.storage
        request_count, validation_error = await _validate_batch_input_file(
            storage, batch_request.input_file_id, batch_request.endpoint
        )

        if validation_error:
            logger.error(
                "Batch input validation failed",
                input_file_id=batch_request.input_file_id,
                error=validation_error,
            )  # type: ignore[call-arg]
            raise HTTPException(status_code=400, detail=validation_error)

        logger.info(
            "Batch input validated",
            input_file_id=batch_request.input_file_id,
            request_count=request_count,
        )  # type: ignore[call-arg]

        # Create job using JobManager. Pass the validated input line
        # count so request_counts.total is fixed at creation, matching
        # OpenAI Batch API semantics.
        job_id = await batch_driver.run_coroutine(
            batch_driver.job_manager.create_job_with_spec(
                session_id=session_id,
                job_spec=batch_request,
                request_count=request_count,
            )
        )

        # Retrieve the created job
        job = await batch_driver.run_coroutine(batch_driver.job_manager.get_job(job_id))
        if not job:
            logger.error("Created job not found", job_id=job_id)  # type: ignore[call-arg]
            raise HTTPException(status_code=500, detail="Created batch not found")

        logger.info("Batch created successfully", job_id=job_id, session_id=session_id)  # type: ignore[call-arg]

        return _batch_job_to_openai_response(job)

    except asyncio.TimeoutError:
        logger.error("Batch creation timed out")  # type: ignore[call-arg]
        raise HTTPException(status_code=408, detail="Batch creation timed out")
    except RenderError as e:
        # Template / profile lookup failures, override allowlist
        # violations, and unsupported template features all
        # surface here as user-fixable 400s, not server faults.
        logger.error("Batch rendering rejected", error=str(e))  # type: ignore[call-arg]
        raise HTTPException(status_code=400, detail=str(e))
    except ValueError as e:
        logger.error("Invalid batch request", error=str(e))  # type: ignore[call-arg]
        raise HTTPException(status_code=400, detail=str(e))
    except HTTPException:
        raise
    except Exception as e:
        logger.error("Unexpected error creating batch", error=str(e))  # type: ignore[call-arg]
        raise HTTPException(status_code=500, detail="Internal server error")


async def _resolve_batch_job(request: Request, batch_id: str) -> Optional[BatchJob]:
    """Resolve a BatchJob by id, metastore-first with JobManager fallback.

    The batch metastore is the source of truth. The fallback to
    ``JobManager.get_job`` covers the brief window between
    ``create_namespaced_job`` returning and the kopf ADDED handler
    persisting the document, since the metadata service seeds the
    JobManager pool synchronously on POST. Standalone mode (no kopf,
    no K8s) also relies on the JobManager fallback because no
    metastore document is ever written.
    """
    job = await get_batch_job(batch_id)
    if job is not None:
        return job

    batch_driver: BatchDriver = request.app.state.batch_driver
    return await batch_driver.run_coroutine(batch_driver.job_manager.get_job(batch_id))


@router.get("/{batch_id}")
async def get_batch(request: Request, batch_id: str) -> BatchResponse:
    """Retrieve a batch by ID.

    Returns the details of a specific batch including its current status,
    request counts, and timestamps.
    """
    try:
        logger.debug("Retrieving batch", batch_id=batch_id)  # type: ignore[call-arg]

        job = await _resolve_batch_job(request, batch_id)
        if not job:
            logger.warning("Batch not found", batch_id=batch_id)  # type: ignore[call-arg]
            raise HTTPException(status_code=404, detail="Batch not found")

        return _batch_job_to_openai_response(job)
    except HTTPException:
        raise
    except Exception as e:
        logger.error(
            "Unexpected error retrieving batch", batch_id=batch_id, error=str(e)
        )  # type: ignore[call-arg]
        logger.error(f"Stack trace: {traceback.format_exc()}")
        raise HTTPException(status_code=500, detail="Internal server error")


@router.post("/{batch_id}/cancel")
async def cancel_batch(request: Request, batch_id: str) -> BatchResponse:
    """Cancel a batch.

    Cancels an in-progress batch. Once cancelled, the batch cannot be resumed.
    Any completed requests in the batch will still be available in the output.
    """
    try:
        # Get job controller from app state
        batch_driver: BatchDriver = request.app.state.batch_driver

        logger.info("Cancelling batch", batch_id=batch_id)  # type: ignore[call-arg]

        # Check if job exists (store-first read).
        job = await _resolve_batch_job(request, batch_id)
        if not job:
            logger.warning("Batch not found for cancellation", batch_id=batch_id)  # type: ignore[call-arg]
            raise HTTPException(status_code=404, detail="Batch not found")

        # Cancel the job. JobManager.cancel_job drives the K8s suspend
        # patch and the status write to the BatchJobStore via
        # JobCache._put_to_store.
        success = await batch_driver.run_coroutine(
            batch_driver.job_manager.cancel_job(batch_id)
        )
        if not success:
            logger.warning("Failed to cancel batch", batch_id=batch_id)  # type: ignore[call-arg]
            raise HTTPException(status_code=400, detail="Batch cannot be cancelled")

        # Get updated job status (store-first again).
        updated_job = await _resolve_batch_job(request, batch_id)
        if not updated_job:
            logger.error("Job not found after cancellation", batch_id=batch_id)  # type: ignore[call-arg]
            raise HTTPException(status_code=500, detail="Internal server error")

        logger.info("Batch cancelled successfully", batch_id=batch_id)  # type: ignore[call-arg]

        return _batch_job_to_openai_response(updated_job)

    except HTTPException:
        raise
    except Exception as e:
        logger.error(
            "Unexpected error cancelling batch", batch_id=batch_id, error=str(e)
        )  # type: ignore[call-arg]
        raise HTTPException(status_code=500, detail="Internal server error")


@router.get("/", include_in_schema=False)
@router.get("")
async def list_batches(
    request: Request,
    after: Optional[str] = Query(None, description="Cursor for pagination"),
    limit: int = Query(20, ge=1, le=100, description="Number of batches to return"),
) -> BatchListResponse:
    """List batches.

    Returns a list of your organization's batches. Supports pagination
    using cursor-based pagination with the 'after' parameter.
    """
    try:
        # Get job controller from app state
        batch_driver: BatchDriver = request.app.state.batch_driver

        logger.debug("Listing batches", after=after, limit=limit)  # type: ignore[call-arg]

        # List still goes through JobManager. Adding a list_by_prefix to
        # BatchJobStore would require either an eagerly-maintained index
        # (Redis ZSET keyed by created_at) or an S3 list-objects walk;
        # the latter does not fit the current cursor-based pagination
        # cheaply. Point reads already serve from the store, so the
        # tradeoff only hurts the rare list call.
        # TODO(A.2 follow-up): wire list to BatchJobStore via an index.
        all_jobs: List[BatchJob] = await batch_driver.run_coroutine(
            batch_driver.job_manager.list_jobs()
        )

        # Apply cursor-based pagination
        if after:
            # Find the index of the job with the 'after' ID
            after_index = -1
            for i, job in enumerate(all_jobs):
                if job.status.job_id == after:
                    after_index = i
                    break

            if after_index >= 0:
                # Start after the found job
                all_jobs = all_jobs[after_index + 1 :]
            else:
                # If 'after' job not found, return empty list
                all_jobs = []

        # Apply limit
        jobs_page = all_jobs[:limit]

        # Convert to OpenAI format
        batch_responses = [_batch_job_to_openai_response(job) for job in jobs_page]

        # Calculate pagination info
        first_id = batch_responses[0].id if batch_responses else None
        last_id = batch_responses[-1].id if batch_responses else None
        has_more = len(all_jobs) > limit

        logger.debug(
            "Listed batches",
            count=len(batch_responses),
            has_more=has_more,
            first_id=first_id,
            last_id=last_id,
        )  # type: ignore[call-arg]

        return BatchListResponse(
            data=batch_responses, first_id=first_id, last_id=last_id, has_more=has_more
        )

    except Exception as e:
        logger.error("Unexpected error listing batches", error=str(e))  # type: ignore[call-arg]
        raise HTTPException(status_code=500, detail="Internal server error")

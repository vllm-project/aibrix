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
import traceback
import uuid
from datetime import datetime, timedelta
from typing import Any, Dict, List, Optional

from fastapi import APIRouter, HTTPException, Query, Request
from pydantic import BaseModel, Field, field_validator

from aibrix.batch import BatchDriver
from aibrix.batch.job_entity import (
    AibrixMetadata,
    BatchJob,
    BatchJobEndpoint,
    BatchJobError,
    BatchJobErrorCode,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    BatchProfileRef,
    BatchUsage,
    ClientConfig,
    CompletionWindow,
    ModelTemplateRef,
    ResourceAllocation,
    RuntimeSpec,
)
from aibrix.batch.manifest import RenderError
from aibrix.batch.storage.batch_metastore import get_batch_job
from aibrix.batch.template import (
    BatchProfile,
    ModelDeploymentTemplate,
    ProfileRegistry,
    TemplateRegistry,
)
from aibrix.logger import init_logger

logger = init_logger(__name__)

router = APIRouter()


# OpenAI Batch API request/response models
class ResourceAllocationRef(ResourceAllocation):
    """Resource allocation metadata from the planner / resource manager.

    Wire shape (under ``extra_body.aibrix.resource_allocation``)::

        {
            "provision_id": "xxxxxxxx",
            "provision_resource_deadline": 1422443902,  # optional; 0 / null = will not expire
            "resource_details": [
                {
                    "endpoint_cluster": "cluster-a",  # optional
                    "gpu_type": "A100-SM-80G",  # optional
                    "replica": 2,  # optional; 1 / null = 1
                }
            ],
        }

    Runtime selection lives under ``extra_body.aibrix.runtime.target``.
    """


class RuntimeRef(RuntimeSpec):
    """Selects which Runtime the batch job runs on.

    Wire shape (under ``extra_body.aibrix.runtime``)::

        {
            "target": "Kubernetes",  # one registered runtime target
            "options": {"namespace": "default"},  # free-form runtime options
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
                    "runtime": {
                        "target": "Kubernetes",
                        "options": {"namespace": "default"},
                    },
                    "resource_allocation": {"provision_id": "reservation-1"},
                    "model_template": {"name": "llama3-70b-prod"},
                    "profile": {"name": "prod-24h"},
                    "client": {
                        "max_concurrency": 256,
                        "adaptive_concurrency": True,
                        "adaptive_max_factor": 16,
                        "retry_policy": {
                            "max_retries": 5,
                            "base_delay_seconds": 2,
                            "max_delay_seconds": 10,
                            "no_endpoint_max_retries": 5,
                        },
                    },
                }
            },
        )

    A request omitting ``aibrix.model_template`` is rejected at render
    time as a 400 ('model_template_name is required'). The legacy
    hardcoded yaml fallback was removed in this same release; admins
    must register at least one ModelDeploymentTemplate via the
    ConfigMap to accept any batch. See
    docs/source/features/batch-templates.rst.

    Inline ``model_template.spec`` IS supported (Pydantic ``TemplateRef``
    accepts a ``spec`` field). It is intended for cross-cluster deployments
    where MDS cannot share a registry with the upstream Console: Console
    resolves the template against its own DB and pushes the resolved spec
    inline so MDS skips its local registry lookup. The audit trail still
    keys off ``name`` + ``version``.
    """

    model_config = {"extra": "forbid"}

    job_id: Optional[str] = None
    model: Optional[str] = Field(
        default=None,
        description=("Serving identifier the batch's requests carry in body.model"),
    )
    resource_allocation: Optional[ResourceAllocationRef] = None
    runtime: Optional[RuntimeRef] = Field(
        default=None,
        description=(
            "Runtime target selection. Absent routes to the injected "
            "endpoint-source / standalone path."
        ),
    )
    model_template: Optional[TemplateRef] = Field(
        default=None,
        description="ModelDeploymentTemplate reference (name + optional version + overrides)",
    )
    profile: Optional[ProfileRef] = Field(
        default=None,
        description="BatchProfile reference; falls back to registry default if omitted",
    )
    client: Optional[ClientConfig] = Field(
        default=None,
        description="Per-job smart-client concurrency and retry controls",
    )

    @field_validator("model_template", mode="before")
    @classmethod
    def normalize_model_template(cls, value: Any) -> Any:
        if isinstance(value, ModelDeploymentTemplate):
            value = value.model_dump(exclude_none=True)
        if isinstance(value, dict) and "status" in value:
            value = dict(value)
            value.pop("status")
        return value

    @field_validator("profile", mode="before")
    @classmethod
    def normalize_profile(cls, value: Any) -> Any:
        if isinstance(value, BatchProfile):
            return value.model_dump(exclude_none=True)
        return value


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
                model=spec.aibrix.model,
                resource_allocation=spec.aibrix.resource_allocation,
                runtime=spec.aibrix.runtime,
                model_template=spec.aibrix.model_template,
                profile=spec.aibrix.profile,
                client=spec.aibrix.client,
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
    if extension is None:
        return

    # runtime.target is a free-form string on the wire (RuntimeSpec is
    # lenient, for downstream runtimes); reject an unknown one here with a 400
    # instead of falling through to the standalone path / a late INVALID_DRIVER.
    # Validate against the live runtime registry (not the static enum) so
    # downstream/custom registered runtimes are accepted.
    if extension.runtime is not None:
        from aibrix.batch.job_driver.runtime import registered_runtimes

        valid_targets = set(registered_runtimes())
        if extension.runtime.target not in valid_targets:
            raise HTTPException(
                status_code=400,
                detail=(
                    f"aibrix.runtime.target '{extension.runtime.target}' is "
                    f"not a known runtime target. Valid: {sorted(valid_targets)}"
                ),
            )

    if extension.model_template is None:
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
    # Inline spec bypasses registry lookup: trusted caller (e.g. Console)
    # already resolved the template; renderer will consume the inline spec.
    if tref.spec is None:
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
        if pref.spec is None and profile_registry.get(pref.name) is None:
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

    # Map the internal state machine to the client-facing status:
    #   - CREATED: accepted and waiting in the scheduler's pending pool for
    #     admission (resource bring-up) — surfaced as 'scheduling'. NOTE: this
    #     deviates from the OpenAI batch enum (no 'scheduling' state); we accept
    #     the break because our platform spends real time here and 'scheduling'
    #     is more accurate than OpenAI's 'validating' (which on our side is
    #     near-instant: VALIDATING flips to IN_PROGRESS inside admit()).
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
        state = BatchJobState.SCHEDULING.value
    else:
        state = status.state.value

    # Mirror OpenAI's batch object contract for the result file ids. We allocate
    # them upfront so the worker can stream into them, but they must not surface
    # until they are real, downloadable files:
    #   - both stay null through validating/in_progress/finalizing (the multipart
    #     upload is only completed during finalization), and
    #   - output_file_id is exposed only when there are successful results, and
    #     error_file_id only when at least one request failed (OpenAI leaves the
    #     error file null when nothing errored).
    # rc is non-Optional (default_factory), but guard defensively to match the
    # check at the request_counts conversion above and stay safe if it ever
    # becomes nullable.
    rc = status.request_counts
    output_file_id = (
        status.output_file_id if status.finished and rc and rc.completed > 0 else None
    )
    error_file_id = (
        status.error_file_id if status.finished and rc and rc.failed > 0 else None
    )

    return BatchResponse(
        id=status.job_id,
        endpoint=spec.endpoint,
        model=spec.model or spec.model_template_name,
        errors=BatchErrors(data=status.errors) if status.errors else None,
        input_file_id=spec.input_file_id,
        completion_window=completion_window,
        status=state,
        output_file_id=output_file_id,
        error_file_id=error_file_id,
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


@router.post("/", include_in_schema=False, response_model_exclude_none=True)
@router.post("", response_model_exclude_none=True)
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
        job_id = await batch_driver.create_job(
            session_id=session_id,
            job_spec=batch_request,
        )

        # Retrieve the created job
        job = await batch_driver.get_job(job_id)
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
    """Resolve a BatchJob by id, metastore-first with BatchManager fallback.

    The batch metastore is the source of truth. The fallback to the driver's
    in-memory pool covers the brief window after POST before the store write
    is observable, since the metadata service seeds the BatchManager pool
    synchronously on create.
    """
    job = await get_batch_job(batch_id)
    if job is not None:
        return job

    batch_driver: BatchDriver = request.app.state.batch_driver
    return await batch_driver.get_job(batch_id)


@router.get("/{batch_id}", response_model_exclude_none=True)
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


@router.post("/{batch_id}/cancel", response_model_exclude_none=True)
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

        # Cancel the job. BatchManager.cancel_job signals the entity manager
        # or live driver to persist the cancellation and stop execution.
        terminate_result = await batch_driver.cancel_job(batch_id)

        # Get updated job status (store-first again).
        updated_job = await _resolve_batch_job(request, batch_id)
        if not updated_job:
            logger.error("Job not found after cancellation", batch_id=batch_id)  # type: ignore[call-arg]
            raise HTTPException(status_code=500, detail="Internal server error")

        if terminate_result.value == "rejected":
            if updated_job.status.errors is None:
                updated_job.status.errors = []
            updated_job.status.errors.append(
                BatchJobError(
                    code=BatchJobErrorCode.CANCEL_REJECTED_ERROR,
                    message=(
                        "Batch cannot be cancelled in current state "
                        f"'{updated_job.status.state.value}'"
                    ),
                    param="status",
                )
            )
            logger.info(  # type: ignore[call-arg]
                "Batch cancel request rejected by current state",
                batch_id=batch_id,
                state=updated_job.status.state,
            )
        else:
            logger.info("Batch cancelled successfully", batch_id=batch_id)  # type: ignore[call-arg]

        return _batch_job_to_openai_response(updated_job)

    except HTTPException:
        raise
    except Exception as e:
        logger.error(
            "Unexpected error cancelling batch", batch_id=batch_id, error=str(e)
        )  # type: ignore[call-arg]
        raise HTTPException(status_code=500, detail="Internal server error")


@router.get("/", include_in_schema=False, response_model_exclude_none=True)
@router.get("", response_model_exclude_none=True)
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

        all_jobs: List[BatchJob] = await batch_driver.list_jobs(
            after=after, limit=limit + 1
        )
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

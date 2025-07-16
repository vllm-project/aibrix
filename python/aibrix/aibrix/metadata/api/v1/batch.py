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
import uuid
from datetime import datetime
from typing import Dict, List, Optional

from fastapi import APIRouter, HTTPException, Query, Request
from pydantic import BaseModel, Field

from aibrix.batch.job_entity import BatchJob, BatchJobError, BatchJobSpec
from aibrix.batch.job_manager import JobManager
from aibrix.metadata.logger import init_logger

logger = init_logger(__name__)

router = APIRouter()


# OpenAI Batch API request/response models


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
    expires_at: Optional[int] = Field(
        default=None, description="Unix timestamp of when the batch expires"
    )
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
    metadata: Optional[Dict[str, str]] = Field(
        default=None, description="Batch metadata"
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
    status = batch_job.status
    spec = batch_job.spec

    def dt_to_unix(dt: Optional[datetime]) -> Optional[int]:
        """Convert datetime to unix timestamp."""
        return int(dt.timestamp()) if dt else None

    # Convert request counts
    request_counts = None
    if status.request_counts:
        request_counts = BatchRequestCounts(
            total=status.request_counts.total,
            completed=status.request_counts.completed,
            failed=status.request_counts.failed,
        )

    created_at_unix = dt_to_unix(status.created_at)
    if created_at_unix is None:
        created_at_unix = int(datetime.now().timestamp())

    return BatchResponse(
        id=status.job_id,
        endpoint=spec.endpoint.value,
        errors=BatchErrors(data=status.errors) if status.errors else None,
        input_file_id=spec.input_file_id,
        completion_window=spec.completion_window.value,
        status=status.state.value,
        output_file_id=status.output_file_id,
        error_file_id=status.error_file_id,
        created_at=created_at_unix,
        in_progress_at=dt_to_unix(status.in_progress_at),
        expires_at=created_at_unix + int(spec.completion_window.expires_at()),
        finalizing_at=dt_to_unix(status.finalizing_at),
        completed_at=dt_to_unix(status.completed_at),
        failed_at=dt_to_unix(status.failed_at),
        expired_at=dt_to_unix(status.expired_at),
        cancelling_at=dt_to_unix(status.cancelling_at),
        cancelled_at=dt_to_unix(status.cancelled_at),
        request_counts=request_counts,
        metadata=spec.metadata,
    )


@router.post("/")
async def create_batch(request: Request, batch_request: BatchJobSpec) -> BatchResponse:
    """Create a new batch.

    Creates a new batch for processing multiple requests. The batch will be
    processed asynchronously and can be monitored using the batch ID.
    """
    try:
        # Get job controller from app state
        job_manager: JobManager = request.app.state.job_controller.job_manager

        # Generate session ID for tracking
        session_id = str(uuid.uuid4())

        logger.info(
            "Creating batch",
            input_file_id=batch_request.input_file_id,
            endpoint=batch_request.endpoint,
            completion_window=batch_request.completion_window,
            session_id=session_id,
        )  # type: ignore[call-arg]

        # Create job using JobManager
        job_id = await job_manager.create_job_with_spec(
            session_id=session_id,
            job_spec=batch_request,
        )

        # Retrieve the created job
        job = job_manager.get_job(job_id)
        if not job:
            logger.error("Created job not found", job_id=job_id)  # type: ignore[call-arg]
            raise HTTPException(status_code=500, detail="Created batch not found")

        logger.info("Batch created successfully", job_id=job_id, session_id=session_id)  # type: ignore[call-arg]

        return _batch_job_to_openai_response(job)

    except asyncio.TimeoutError:
        logger.error("Batch creation timed out")  # type: ignore[call-arg]
        raise HTTPException(status_code=408, detail="Batch creation timed out")
    except ValueError as e:
        logger.error("Invalid batch request", error=str(e))  # type: ignore[call-arg]
        raise HTTPException(status_code=400, detail=str(e))
    except HTTPException:
        raise
    except Exception as e:
        logger.error("Unexpected error creating batch", error=str(e))  # type: ignore[call-arg]
        raise HTTPException(status_code=500, detail="Internal server error")


@router.get("/{batch_id}")
async def get_batch(request: Request, batch_id: str) -> BatchResponse:
    """Retrieve a batch by ID.

    Returns the details of a specific batch including its current status,
    request counts, and timestamps.
    """
    try:
        # Get job controller from app state
        job_manager: JobManager = request.app.state.job_controller.job_manager

        logger.debug("Retrieving batch", batch_id=batch_id)  # type: ignore[call-arg]

        # Get job from manager
        job = job_manager.get_job(batch_id)
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
        raise HTTPException(status_code=500, detail="Internal server error")


@router.post("/{batch_id}/cancel")
async def cancel_batch(request: Request, batch_id: str) -> BatchResponse:
    """Cancel a batch.

    Cancels an in-progress batch. Once cancelled, the batch cannot be resumed.
    Any completed requests in the batch will still be available in the output.
    """
    try:
        # Get job controller from app state
        job_manager: JobManager = request.app.state.job_controller.job_manager

        logger.info("Cancelling batch", batch_id=batch_id)  # type: ignore[call-arg]

        # Check if job exists
        job = job_manager.get_job(batch_id)
        if not job:
            logger.warning("Batch not found for cancellation", batch_id=batch_id)  # type: ignore[call-arg]
            raise HTTPException(status_code=404, detail="Batch not found")

        # Cancel the job
        success = job_manager.cancel_job(batch_id)
        if not success:
            logger.warning("Failed to cancel batch", batch_id=batch_id)  # type: ignore[call-arg]
            raise HTTPException(status_code=400, detail="Batch cannot be cancelled")

        # Get updated job status
        updated_job = job_manager.get_job(batch_id)
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


@router.get("/")
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
        job_manager: JobManager = request.app.state.job_controller.job_manager

        logger.debug("Listing batches", after=after, limit=limit)  # type: ignore[call-arg]

        # Get all jobs from the manager
        # Note: This is a simple implementation. In production, you'd want
        # proper pagination and filtering in the JobManager
        all_jobs: List[BatchJob] = await job_manager.list_jobs()

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

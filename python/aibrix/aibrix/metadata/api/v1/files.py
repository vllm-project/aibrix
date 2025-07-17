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

import os
import time
import uuid
from enum import Enum
from pathlib import Path
from typing import Any, Dict, Optional

from fastapi import APIRouter, File, Form, HTTPException, Request, Response, UploadFile
from pydantic import Field

from aibrix.batch.storage import get_base_path as get_storage_bath
from aibrix.metadata.logger import init_logger
from aibrix.openapi.protocol import NoExtraBaseModel
from aibrix.metadata.setting.config import settings

logger = init_logger(__name__)

router = APIRouter()

supported_extensions = {
    "json",
    "jsonl",
}

# OpenAI Files API models


class FilePurpose(str, Enum):
    """Valid purpose values for OpenAI Files API."""

    FINE_TUNE = "fine-tune"
    ASSISTANTS = "assistants"
    BATCH = "batch"


class FileStatus(str, Enum):
    """File processing status."""

    UPLOADED = "uploaded"
    PENDING = "pending"
    PROCESSED = "processed"
    ERROR = "error"


class FileObject(NoExtraBaseModel):
    """OpenAI File object response model."""

    id: str = Field(description="The file identifier")
    object: str = Field(
        default="file", description="The object type, which is always 'file'"
    )
    bytes: int = Field(description="The size of the file in bytes")
    created_at: int = Field(description="The Unix timestamp when the file was created")
    filename: str = Field(description="The name of the file")
    purpose: FilePurpose = Field(description="The intended purpose of the file")
    status: FileStatus = Field(description="The current status of the file")


class FileError(NoExtraBaseModel):
    """Error response model."""

    error: Dict[str, Any] = Field(description="Error details")

def _validate_file_extension(filename: str) -> bool:
    """Validate if file extension is supported."""
    if "." not in filename:
        return False

    ext = filename.split(".")[-1].lower()
    return ext in supported_extensions


def _create_error_response(
    message: str,
    error_type: str = "invalid_request_error",
    param: Optional[str] = None,
    code: Optional[str] = None,
) -> Dict[str, Any]:
    """Create OpenAI-compatible error response."""
    error_data = {"message": message, "type": error_type, "param": param, "code": code}
    return {"error": error_data}


def _store_file_content(file_id: str, content: bytes) -> str:
    """Store file content to disk and return file path, which simulates storage behavior"""
    directory_path = os.path.join(get_storage_bath(), f"{file_id}")
    os.makedirs(directory_path, exist_ok=True)
    file_path = os.path.join(directory_path, "input.jsonl")
    with open(file_path, "wb") as f:
        f.write(content)
    return file_path


def _read_file_content(file_id: str) -> bytes:
    """Read file content from disk, which simulator storage behavior"""
    file_path = os.path.join(get_storage_bath(), f"{file_id}", "output.jsonl")
    if not os.path.exists(file_path):
        raise FileNotFoundError(f"File not found: {file_id}")

    with open(file_path, "rb") as f:
        return f.read()


@router.post("/")
async def create_file(
    request: Request,
    file: UploadFile = File(..., description="The file to upload"),
    purpose: FilePurpose = Form(..., description="The intended purpose of the file"),
) -> FileObject:
    """Upload a file that can be used across various endpoints.

    Note: The implementation is for e2e test only for now, and can upload batch input file only.
    """
    maxFileSize = settings.MAX_FILE_SIZE
    try:
        # Validate file extension
        if not _validate_file_extension(file.filename or ""):
            error_response = _create_error_response(
                f"Invalid file format. Supported formats: {supported_extensions}",
                param="file",
            )
            raise HTTPException(status_code=400, detail=error_response)

        # Read file content
        file_content = await file.read()
        file_size = len(file_content)

        # Check file size limit (512 MB)
        if file_size > maxFileSize:
            error_response = _create_error_response(
                f"Maximum content size limit exceeded. The content size surpasses the allowed limit of {maxFileSize} bytes.",
                code="content_size_limit_exceeded",
            )
            raise HTTPException(status_code=413, detail=error_response)

        # Generate unique file ID
        file_id = str(uuid.uuid4())

        # Store file content
        file_path = _store_file_content(file_id, file_content)

        # Store file metadata
        created_at = int(time.time())
        metadata = {
            "filename": file.filename or os.path.basename(file_path),
            "bytes": file_size,
            "purpose": purpose.value,
            "created_at": created_at,
            "status": FileStatus.UPLOADED.value,
            "file_path": file_path,
        }

        logger.info(
            "File uploaded successfully",
            file_id=file_id,
            filename=file.filename,
            purpose=purpose.value,
            size_bytes=file_size,
        )  # type: ignore[call-arg]

        return FileObject(
            id=file_id,
            bytes=file_size,
            created_at=created_at,
            filename=metadata["filename"],
            purpose=purpose,
            status=FileStatus.UPLOADED,
        )

    except HTTPException:
        raise
    except Exception as e:
        logger.error("Unexpected error uploading file", error=str(e))  # type: ignore[call-arg]
        error_response = _create_error_response("Internal server error")
        raise HTTPException(status_code=500, detail=error_response)


@router.get("/{file_id}/content")
async def retrieve_file_content(request: Request, file_id: str) -> Response:
    """Returns the contents of the specified file.

    Note: This endpoint returns the raw file content, not a JSON response.

    Note: The implementation is for e2e test only for now, and can download batch output file only.
    """
    # Read file content from storage
    try:
        file_content = _read_file_content(file_id)
        content_type = "application/jsonl"

        logger.info(
            "File content retrieved",
            file_id=file_id,
            content_type=content_type,
        )  # type: ignore[call-arg]

        # Return raw file content
        return Response(
            content=file_content,
            media_type=content_type,
            headers={"Content-Disposition": f"attachment; filename=output.jsonl"},
        )

    except FileNotFoundError:
        logger.error(
            "File not found in storage",
            file_id=file_id,
        )  # type: ignore[call-arg]
        error_response = _create_error_response("File content not found")
        raise HTTPException(status_code=404, detail=error_response)
    except Exception as storage_error:
        logger.error(
            "Failed to retrieve file from storage",
            file_id=file_id,
            error=str(storage_error),
        )  # type: ignore[call-arg]
        error_response = _create_error_response("Failed to retrieve file content")
        raise HTTPException(status_code=500, detail=error_response)

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

import time
import uuid
from enum import Enum
from typing import Any, Dict, Optional

from fastapi import APIRouter, File, Form, HTTPException, Request, Response, UploadFile
from pydantic import Field

from aibrix.logger import init_logger
from aibrix.metadata.setting.config import settings
from aibrix.openapi.protocol import NoExtraBaseModel
from aibrix.storage import BaseStorage, Reader, SizeExceededError, generate_filename

logger = init_logger(__name__)

router = APIRouter()

supported_extensions = {
    "json",
    "jsonl",
}


# OpenAI Files API models
class FilePurpose(str, Enum):
    """Valid purpose values for OpenAI Files API."""

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


class FileMetadata(NoExtraBaseModel):
    """File metadata response model for head_object operations."""

    id: str = Field(description="The file identifier")
    object: str = Field(
        default="file", description="The object type, which is always 'file'"
    )
    bytes: int = Field(description="The size of the file in bytes")
    created_at: int = Field(description="The Unix timestamp when the file was created")
    filename: str = Field(description="The name of the file")
    purpose: Optional[FilePurpose] = Field(
        description="The intended purpose of the file"
    )
    status: FileStatus = Field(description="The current status of the file")
    content_type: Optional[str] = Field(description="The MIME type of the file")
    etag: Optional[str] = Field(description="The entity tag for the file")
    last_modified: Optional[int] = Field(
        description="Unix timestamp of last modification"
    )


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


@router.post("/", include_in_schema=False)
@router.post("")
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

        # Wraps in reader
        reader = Reader(file, size_limiter=maxFileSize)

        # Generate unique file ID
        file_id = str(uuid.uuid4())

        # Store file content (using bytes since we already read the content)
        storage: BaseStorage = request.app.state.storage

        # Generate metadata
        created_at = int(time.time())
        metadata: dict[str, str] = {
            "filename": file.filename or "",
            "purpose": purpose.value,
            "created_at": str(created_at),  # requires all value a string.
        }
        try:
            await storage.put_object(
                file_id, reader, content_type=file.content_type, metadata=metadata
            )
        except SizeExceededError:
            error_response = _create_error_response(
                f"Maximum content size limit exceeded. The content size surpasses the allowed limit of {maxFileSize} bytes.",
                code="content_size_limit_exceeded",
            )
            raise HTTPException(status_code=413, detail=error_response)

        logger.info(
            "File uploaded successfully",
            file_id=file_id,
            filename=file.filename,
            purpose=purpose.value,
            size_bytes=reader.bytes_read(),
        )  # type: ignore[call-arg]

        return FileObject(
            id=file_id,
            bytes=reader.bytes_read(),
            created_at=created_at,
            filename=str(metadata["filename"]),
            purpose=purpose,
            status=FileStatus.UPLOADED,
        )

    except HTTPException:
        raise
    except Exception as e:
        logger.error("Unexpected error uploading file", error=str(e))  # type: ignore[call-arg]
        error_response = _create_error_response("Internal server error")
        raise  # HTTPException(status_code=500, detail=error_response)


@router.get("/{file_id}/content")
async def retrieve_file_content(request: Request, file_id: str) -> Response:
    """Returns the contents of the specified file.

    Note: This endpoint returns the raw file content, not a JSON response.

    Note: The implementation is for e2e test only for now, and can download batch output file only.
    """
    # Read file content from storage
    try:
        storage: BaseStorage = request.app.state.storage

        head_object = await storage.head_object(file_id)
        file_content = await storage.get_object(file_id)
        content_type = head_object.content_type
        if head_object.metadata is not None and "filename" in head_object.metadata:
            filename = head_object.metadata["filename"]
        else:
            filename = generate_filename(
                file_id, head_object.content_type, head_object.metadata
            )

        logger.info(
            "File content retrieved",
            file_id=file_id,
            content_type=content_type,
        )  # type: ignore[call-arg]

        # Return raw file content
        return Response(
            content=file_content,
            media_type=content_type,
            headers={"Content-Disposition": f"attachment; filename={filename}"},
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


@router.get("/{file_id}")
async def retrieve_file_metadata(request: Request, file_id: str) -> FileMetadata:
    """Returns the metadata of the specified file without downloading the content.

    This endpoint provides file information including size, content type, creation time,
    and other metadata without transferring the actual file content.
    """
    try:
        storage: BaseStorage = request.app.state.storage

        # Get file metadata using head_object
        head_object = await storage.head_object(file_id)

        # Extract metadata from the stored object
        metadata = head_object.metadata or {}

        # Get creation timestamp from metadata or use last_modified as fallback
        created_at = None
        if "created_at" in metadata:
            try:
                created_at = int(metadata["created_at"])
            except (ValueError, TypeError):
                pass

        # Fallback to last_modified if created_at is not available
        if created_at is None and head_object.last_modified:
            created_at = int(head_object.last_modified.timestamp())

        # Default created_at if still None
        if created_at is None:
            created_at = int(time.time())

        # Get filename from metadata or generate from file_id
        filename = metadata.get(
            "filename", generate_filename(file_id, head_object.content_type, metadata)
        )

        # Get purpose from metadata (may be None)
        purpose = None
        if "purpose" in metadata:
            try:
                purpose = FilePurpose(metadata["purpose"])
            except ValueError:
                # Invalid purpose, leave as None
                pass

        # Get last_modified timestamp
        last_modified = None
        if head_object.last_modified:
            last_modified = int(head_object.last_modified.timestamp())

        logger.info(
            "File metadata retrieved",
            file_id=file_id,
            filename=filename,
            content_type=head_object.content_type,
            size_bytes=head_object.content_length,
        )  # type: ignore[call-arg]

        return FileMetadata(
            id=file_id,
            bytes=head_object.content_length,
            created_at=created_at,
            filename=filename,
            purpose=purpose,
            status=FileStatus.UPLOADED,  # For now, assume all files are uploaded
            content_type=head_object.content_type,
            etag=head_object.etag or None,
            last_modified=last_modified,
        )

    except FileNotFoundError:
        logger.error(
            "File not found in storage",
            file_id=file_id,
        )  # type: ignore[call-arg]
        error_response = _create_error_response("File not found")
        raise HTTPException(status_code=404, detail=error_response)
    except Exception as storage_error:
        logger.error(
            "Failed to retrieve file metadata from storage",
            file_id=file_id,
            error=str(storage_error),
        )  # type: ignore[call-arg]
        error_response = _create_error_response("Failed to retrieve file metadata")
        raise HTTPException(status_code=500, detail=error_response)


@router.head("/{file_id}")
async def head_file_metadata(request: Request, file_id: str) -> Response:
    """Returns the metadata headers of the specified file without downloading the content.

    This endpoint provides file information in HTTP headers following HTTP HEAD semantics.
    The response contains no body but includes metadata in headers.
    """
    try:
        storage: BaseStorage = request.app.state.storage

        # Get file metadata using head_object
        head_object = await storage.head_object(file_id)

        # Extract metadata from the stored object
        metadata = head_object.metadata or {}

        # Get creation timestamp from metadata or use last_modified as fallback
        created_at = None
        if "created_at" in metadata:
            try:
                created_at = int(metadata["created_at"])
            except (ValueError, TypeError):
                pass

        # Fallback to last_modified if created_at is not available
        if created_at is None and head_object.last_modified:
            created_at = int(head_object.last_modified.timestamp())

        # Default created_at if still None
        if created_at is None:
            created_at = int(time.time())

        # Get filename from metadata or generate from file_id
        filename = metadata.get(
            "filename", generate_filename(file_id, head_object.content_type, metadata)
        )

        # Build response headers with metadata
        headers = {
            "Content-Length": str(head_object.content_length),
            "X-File-ID": file_id,
            "X-File-Name": filename,
            "X-File-Created-At": str(created_at),
            "X-File-Status": FileStatus.UPLOADED.value,
        }

        # Add optional headers
        if head_object.content_type:
            headers["Content-Type"] = head_object.content_type

        if head_object.etag:
            headers["ETag"] = head_object.etag

        if head_object.last_modified:
            headers["Last-Modified"] = head_object.last_modified.strftime(
                "%a, %d %b %Y %H:%M:%S GMT"
            )

        if "purpose" in metadata:
            headers["X-File-Purpose"] = metadata["purpose"]

        logger.info(
            "File metadata headers retrieved",
            file_id=file_id,
            filename=filename,
            content_type=head_object.content_type,
            size_bytes=head_object.content_length,
        )  # type: ignore[call-arg]

        return Response(headers=headers, status_code=200)

    except FileNotFoundError:
        logger.error(
            "File not found in storage",
            file_id=file_id,
        )  # type: ignore[call-arg]
        error_response = _create_error_response("File not found")
        raise HTTPException(status_code=404, detail=error_response)
    except Exception as storage_error:
        logger.error(
            "Failed to retrieve file metadata from storage",
            file_id=file_id,
            error=str(storage_error),
        )  # type: ignore[call-arg]
        error_response = _create_error_response("Failed to retrieve file metadata")
        raise HTTPException(status_code=500, detail=error_response)

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
from datetime import timezone
from email.utils import format_datetime
from enum import Enum
from typing import Any, Dict, Optional
from urllib.parse import quote

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

_LIST_FILES_PAGE_SIZE = 1000


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


class FileListResponse(NoExtraBaseModel):
    """Response model for file listing."""

    object: str = Field(default="list", description="The object type, always 'list'")
    data: list[FileObject] = Field(description="List of file objects")
    has_more: bool = Field(description="Whether there are more results available")


class FileDeletedResponse(NoExtraBaseModel):
    """Response model for file deletion. Mirrors OpenAI's DeletedFile."""

    id: str = Field(description="The file identifier that was deleted")
    object: str = Field(default="file", description="The object type, always 'file'")
    deleted: bool = Field(default=True, description="Whether the file was deleted")


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


def _header_safe_filename(filename: str) -> str:
    """Return a filename that is safe to place in an HTTP header value.

    Uploaded filenames are arbitrary Unicode, but response header values
    must be latin-1 encodable (Starlette raises ``UnicodeEncodeError``
    otherwise). Percent-encode anything outside ASCII so a download or
    HEAD request for a valid file never turns into a 500.
    """
    if filename.isascii():
        return filename
    return quote(filename, safe="")


def _content_disposition(filename: str) -> str:
    """Build an attachment ``Content-Disposition`` value for a download.

    The plain ``filename=`` carries an ASCII fallback for legacy clients,
    while the RFC 5987 ``filename*=UTF-8''`` form preserves the original
    Unicode name. Both are latin-1 encodable, so the response header stays
    valid regardless of the uploaded filename.
    """
    ascii_fallback = filename.encode("ascii", "ignore").decode("ascii") or "file"
    return (
        f'attachment; filename="{ascii_fallback}"; '
        f"filename*=UTF-8''{quote(filename, safe='')}"
    )


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


async def _file_object_from_key(
    storage: BaseStorage, file_id: str, purpose: Optional[str]
) -> Optional[FileObject]:
    """Build a matching file object, skipping unavailable or invalid metadata."""
    try:
        head_object = await storage.head_object(file_id)
        metadata = head_object.metadata or {}

        if purpose and metadata.get("purpose") != purpose:
            return None

        created_at = None
        if "created_at" in metadata:
            try:
                created_at = int(metadata["created_at"])
            except (ValueError, TypeError):
                pass

        if created_at is None and head_object.last_modified:
            created_at = int(head_object.last_modified.timestamp())

        if created_at is None:
            created_at = int(time.time())

        filename = metadata.get(
            "filename", generate_filename(file_id, head_object.content_type, metadata)
        )

        file_purpose_enum = None
        if "purpose" in metadata:
            try:
                file_purpose_enum = FilePurpose(metadata["purpose"])
            except ValueError:
                pass

        file_status = FileStatus.UPLOADED
        if "status" in metadata:
            try:
                file_status = FileStatus(metadata["status"])
            except ValueError:
                pass

        return FileObject(
            id=file_id,
            bytes=head_object.content_length or 0,
            created_at=created_at,
            filename=filename,
            purpose=file_purpose_enum or FilePurpose.BATCH,
            status=file_status,
        )

    except FileNotFoundError:
        logger.warning("File not found during listing", file_id=file_id)  # type: ignore[call-arg]
    except Exception as e:
        logger.error(
            "Error retrieving file metadata during listing",
            file_id=file_id,
            error=str(e),
        )  # type: ignore[call-arg]
    return None


@router.get("", include_in_schema=True)
@router.get("/", include_in_schema=False)
async def list_files(
    request: Request,
    purpose: Optional[str] = None,
    limit: int = 20,
    after: Optional[str] = None,
) -> FileListResponse:
    """List files with optional filtering and pagination.

    This endpoint returns a list of file objects that have been uploaded.
    Compatible with OpenAI Files API.

    Args:
        purpose: Filter files by purpose (e.g., "batch")
        limit: Number of files to return (1-100, default 20)
        after: File ID to use as cursor for pagination

    Returns:
        FileListResponse with list of files and pagination info
    """
    try:
        # Validate limit
        if not (1 <= limit <= 100):
            raise HTTPException(
                status_code=400, detail="Limit must be between 1 and 100"
            )

        storage: BaseStorage = request.app.state.storage

        # Walk storage until enough matches are found; a purpose-aware layout
        # will replace this scan.
        file_objects: list[FileObject] = []
        continuation_token: Optional[str] = None
        cursor_found = after is None
        while len(file_objects) < limit + 1:
            page, next_token = await storage.list_objects(
                prefix="",
                limit=_LIST_FILES_PAGE_SIZE,
                continuation_token=continuation_token,
            )
            for file_id in page:
                if file_id.startswith("."):
                    continue
                if not cursor_found and file_id != after:
                    continue

                file_object = await _file_object_from_key(storage, file_id, purpose)
                if file_object is None:
                    continue
                if not cursor_found:
                    # The cursor must itself pass the purpose filter, as before.
                    cursor_found = True
                    continue

                file_objects.append(file_object)
                if len(file_objects) == limit + 1:
                    break

            if not next_token or next_token == continuation_token:
                break
            continuation_token = next_token

        return FileListResponse(
            data=file_objects[:limit], has_more=len(file_objects) > limit
        )

    except HTTPException:
        raise
    except Exception as e:
        logger.error("Failed to list files", error=str(e))  # type: ignore[call-arg]
        raise HTTPException(status_code=500, detail="Failed to list files")


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
            headers={"Content-Disposition": _content_disposition(filename)},
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
            "X-File-Name": _header_safe_filename(filename),
            "X-File-Created-At": str(created_at),
            "X-File-Status": FileStatus.UPLOADED.value,
        }

        # Add optional headers
        if head_object.content_type:
            headers["Content-Type"] = head_object.content_type

        if head_object.etag:
            headers["ETag"] = head_object.etag

        if head_object.last_modified:
            headers["Last-Modified"] = format_datetime(
                head_object.last_modified.astimezone(timezone.utc), usegmt=True
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


@router.delete("/{file_id}")
async def delete_file(request: Request, file_id: str) -> FileDeletedResponse:
    """Delete a file. Mirrors OpenAI's ``DELETE /v1/files/{file_id}``.

    Returns ``{"id": file_id, "object": "file", "deleted": true}`` on
    success and 404 if the file does not exist. The underlying object
    storage's ``delete_object`` is a silent no-op for missing keys, so
    we probe with ``head_object`` first to keep the wire contract
    aligned with OpenAI (404 must surface).
    """
    try:
        storage: BaseStorage = request.app.state.storage

        await storage.head_object(file_id)
        await storage.delete_object(file_id)

        logger.info("File deleted", file_id=file_id)  # type: ignore[call-arg]
        return FileDeletedResponse(id=file_id)

    except FileNotFoundError:
        logger.error("File not found for deletion", file_id=file_id)  # type: ignore[call-arg]
        error_response = _create_error_response("File not found")
        raise HTTPException(status_code=404, detail=error_response)
    except Exception as storage_error:
        logger.error(
            "Failed to delete file",
            file_id=file_id,
            error=str(storage_error),
        )  # type: ignore[call-arg]
        error_response = _create_error_response("Failed to delete file")
        raise HTTPException(status_code=500, detail=error_response)

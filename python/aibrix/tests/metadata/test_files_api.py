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

import json
from io import BytesIO

import pytest
from fastapi.testclient import TestClient

from aibrix.metadata.setting.config import settings as metadata_settings
from tests.metadata.conftest import create_test_app as _create_app


def create_test_app():
    """Files-only app: batch API disabled to keep this test surface focused."""
    return _create_app(disable_batch_api=True)


def generate_sample_file():
    """Create a sample JSONL file for testing."""
    data = [
        {
            "custom_id": "test-1",
            "method": "POST",
            "url": "/v1/chat/completions",
            "body": {
                "model": "gpt-3.5-turbo",
                "messages": [{"role": "user", "content": "Hello"}],
            },
        }
    ]
    content = "\n".join(json.dumps(item) for item in data)
    return BytesIO(content.encode("utf-8"))


def _upload(client: TestClient, *, filename="test.jsonl", purpose="batch", body=None):
    """Helper to upload a file; returns the parsed JSON of the upload response."""
    file_obj = body if body is not None else generate_sample_file()
    response = client.post(
        "/v1/files",
        files={"file": (filename, file_obj, "application/jsonl")},
        data={"purpose": purpose},
    )
    return response


class TestFilesAPI:
    """Test the Files API endpoints."""

    def test_upload_and_get_metadata(self):
        """Test uploading a file and retrieving its metadata."""
        app = create_test_app()

        with TestClient(app) as client:
            sample_file = generate_sample_file()

            # Upload a file
            response = client.post(
                "/v1/files/",
                files={"file": ("test.jsonl", sample_file, "application/jsonl")},
                data={"purpose": "batch"},
            )

            assert response.status_code == 200
            upload_data = response.json()
            file_id = upload_data["id"]

            # Test GET metadata endpoint
            metadata_response = client.get(f"/v1/files/{file_id}")
            assert metadata_response.status_code == 200

            metadata = metadata_response.json()
            assert metadata["id"] == file_id
            assert metadata["object"] == "file"
            assert metadata["filename"] == "test.jsonl"
            assert metadata["purpose"] == "batch"
            assert metadata["status"] == "uploaded"
            assert metadata["content_type"] == "application/jsonl"
            assert metadata["bytes"] > 0
            assert metadata["created_at"] > 0
            assert metadata["etag"] is not None
            assert metadata["last_modified"] is not None

    def test_head_metadata(self):
        """Test HEAD endpoint for file metadata."""
        app = create_test_app()

        with TestClient(app) as client:
            sample_file = generate_sample_file()

            # Upload a file
            response = client.post(
                "/v1/files/",
                files={"file": ("test.jsonl", sample_file, "application/jsonl")},
                data={"purpose": "batch"},
            )

            assert response.status_code == 200
            upload_data = response.json()
            file_id = upload_data["id"]

            # Test HEAD metadata endpoint
            head_response = client.head(f"/v1/files/{file_id}")
            assert head_response.status_code == 200

            # Check headers
            headers = head_response.headers
            assert headers["X-File-ID"] == file_id
            assert headers["X-File-Name"] == "test.jsonl"
            assert headers["X-File-Status"] == "uploaded"
            assert headers["X-File-Purpose"] == "batch"
            assert headers["Content-Type"] == "application/jsonl"
            assert "Content-Length" in headers
            assert "ETag" in headers
            assert "Last-Modified" in headers
            assert "X-File-Created-At" in headers

            # Verify no body content
            assert head_response.content == b""

    def test_get_metadata_not_found(self):
        """Test GET metadata for non-existent file."""
        app = create_test_app()

        with TestClient(app) as client:
            response = client.get("/v1/files/non-existent-id")
            assert response.status_code == 404

            # OpenAI-shape error: top-level "error" object.
            error = response.json()["error"]
            assert error["message"] == "File not found"
            assert error["type"] == "invalid_request_error"

    def test_head_metadata_not_found(self):
        """Test HEAD metadata for non-existent file."""
        app = create_test_app()

        with TestClient(app) as client:
            response = client.head("/v1/files/non-existent-id")
            assert response.status_code == 404

    def test_get_metadata_without_purpose(self):
        """Test retrieving metadata for file uploaded without explicit purpose."""
        app = create_test_app()

        with TestClient(app) as client:
            # Create a file without specifying purpose in metadata
            sample_content = '{"test": "data"}'

            # Upload file with minimal metadata
            response = client.post(
                "/v1/files/",
                files={
                    "file": (
                        "test.json",
                        BytesIO(sample_content.encode()),
                        "application/json",
                    )
                },
                data={"purpose": "batch"},  # Purpose is required by API
            )

            assert response.status_code == 200
            upload_data = response.json()
            file_id = upload_data["id"]

            # Get metadata
            metadata_response = client.get(f"/v1/files/{file_id}")
            assert metadata_response.status_code == 200

            metadata = metadata_response.json()
            assert metadata["id"] == file_id
            assert metadata["filename"] == "test.json"
            assert metadata["content_type"] == "application/json"

    def test_metadata_consistency(self):
        """Test that GET and HEAD endpoints return consistent metadata."""
        app = create_test_app()

        with TestClient(app) as client:
            sample_file = generate_sample_file()

            # Upload a file
            response = client.post(
                "/v1/files/",
                files={"file": ("test.jsonl", sample_file, "application/jsonl")},
                data={"purpose": "batch"},
            )

            assert response.status_code == 200
            upload_data = response.json()
            file_id = upload_data["id"]

            # Get metadata via GET
            get_response = client.get(f"/v1/files/{file_id}")
            get_metadata = get_response.json()

            # Get metadata via HEAD
            head_response = client.head(f"/v1/files/{file_id}")
            head_headers = head_response.headers

            # Compare key fields
            assert head_headers["X-File-ID"] == get_metadata["id"]
            assert head_headers["X-File-Name"] == get_metadata["filename"]
            assert head_headers["Content-Type"] == get_metadata["content_type"]
            assert head_headers["Content-Length"] == str(get_metadata["bytes"])
            assert head_headers["X-File-Status"] == get_metadata["status"]


class TestUploadErrors:
    def test_upload_rejects_unknown_extension(self):
        with TestClient(create_test_app()) as client:
            response = client.post(
                "/v1/files",
                files={"file": ("input.txt", BytesIO(b"hello"), "text/plain")},
                data={"purpose": "batch"},
            )
            assert response.status_code == 400
            error = response.json()["error"]
            assert "Invalid file format" in error["message"]
            assert error["param"] == "file"

    def test_upload_rejects_missing_file_field(self):
        with TestClient(create_test_app()) as client:
            response = client.post("/v1/files", data={"purpose": "batch"})
            assert response.status_code == 422

    def test_upload_rejects_missing_purpose_field(self):
        with TestClient(create_test_app()) as client:
            response = client.post(
                "/v1/files",
                files={
                    "file": ("input.jsonl", generate_sample_file(), "application/jsonl")
                },
            )
            assert response.status_code == 422

    def test_upload_rejects_invalid_purpose_value(self):
        with TestClient(create_test_app()) as client:
            response = client.post(
                "/v1/files",
                files={
                    "file": ("input.jsonl", generate_sample_file(), "application/jsonl")
                },
                data={"purpose": "fine-tune"},
            )
            assert response.status_code == 422

    @pytest.mark.xfail(
        reason=(
            "Reader.size_limiter only checks before reads, and a single "
            "read-all (bytes_to_read=-1, bytes_read=0) trivially passes "
            "(0 + 0 <= limit), so an oversize payload uploaded in one "
            "shot bypasses the limit. Tracking as a separate fix in the "
            "storage Reader; this case is kept to lock in the wire "
            "contract once the limiter is corrected."
        ),
        strict=True,
    )
    def test_upload_rejects_oversized_file(self, monkeypatch):
        monkeypatch.setattr(metadata_settings, "MAX_FILE_SIZE", 16)
        with TestClient(create_test_app()) as client:
            payload = b'{"x":"' + (b"a" * 64) + b'"}'  # >16 bytes
            response = client.post(
                "/v1/files",
                files={"file": ("big.json", BytesIO(payload), "application/json")},
                data={"purpose": "batch"},
            )
            assert response.status_code == 413
            error = response.json()["error"]
            assert error["code"] == "content_size_limit_exceeded"


class TestListFiles:
    def test_list_empty(self):
        with TestClient(create_test_app()) as client:
            response = client.get("/v1/files")
            assert response.status_code == 200
            body = response.json()
            assert body["object"] == "list"
            assert body["data"] == []
            assert body["has_more"] is False

    def test_list_returns_uploaded_file(self):
        with TestClient(create_test_app()) as client:
            upload = _upload(client, filename="hello.jsonl")
            assert upload.status_code == 200
            file_id = upload.json()["id"]

            response = client.get("/v1/files")
            assert response.status_code == 200
            body = response.json()
            ids = [item["id"] for item in body["data"]]
            assert file_id in ids

    def test_list_rejects_out_of_range_limit(self):
        with TestClient(create_test_app()) as client:
            for limit in (0, 101):
                response = client.get("/v1/files", params={"limit": limit})
                assert response.status_code == 400
                assert (
                    "Limit must be between 1 and 100"
                    in (response.json()["error"]["message"])
                )

    def test_list_purpose_filter(self):
        # Only "batch" is a valid purpose for upload today, so list with
        # an unrelated filter must return an empty page even after we
        # uploaded files. has_more must be False (B.5).
        with TestClient(create_test_app()) as client:
            _upload(client)
            _upload(client)

            response = client.get("/v1/files", params={"purpose": "assistants"})
            assert response.status_code == 200
            body = response.json()
            assert body["data"] == []
            assert body["has_more"] is False

    def test_list_after_cursor_pages(self):
        with TestClient(create_test_app()) as client:
            ids = [_upload(client).json()["id"] for _ in range(3)]
            assert all(ids)

            page1 = client.get("/v1/files", params={"limit": 2}).json()
            assert len(page1["data"]) == 2
            assert page1["has_more"] is True

            cursor = page1["data"][-1]["id"]
            page2 = client.get("/v1/files", params={"limit": 2, "after": cursor}).json()
            assert page2["has_more"] is False
            page1_ids = {item["id"] for item in page1["data"]}
            for item in page2["data"]:
                assert item["id"] not in page1_ids


class TestDownloadFile:
    def test_content_round_trip(self):
        with TestClient(create_test_app()) as client:
            payload = b'{"hello":"world"}'
            upload = client.post(
                "/v1/files",
                files={"file": ("data.json", BytesIO(payload), "application/json")},
                data={"purpose": "batch"},
            )
            assert upload.status_code == 200
            file_id = upload.json()["id"]

            response = client.get(f"/v1/files/{file_id}/content")
            assert response.status_code == 200
            assert response.content == payload
            # Content-Disposition must carry a filename so SDKs can
            # save the result with the original name.
            assert "data.json" in response.headers.get("content-disposition", "")

    def test_content_unknown_id_returns_404(self):
        with TestClient(create_test_app()) as client:
            response = client.get("/v1/files/no-such/content")
            assert response.status_code == 404
            assert response.json()["error"]["message"] == "File content not found"


class TestDeleteFile:
    def test_delete_existing_file(self):
        with TestClient(create_test_app()) as client:
            file_id = _upload(client).json()["id"]

            response = client.delete(f"/v1/files/{file_id}")
            assert response.status_code == 200
            body = response.json()
            assert body == {"id": file_id, "object": "file", "deleted": True}

            # After delete, the file must no longer be retrievable.
            assert client.get(f"/v1/files/{file_id}").status_code == 404

    def test_delete_unknown_file_returns_404(self):
        with TestClient(create_test_app()) as client:
            response = client.delete("/v1/files/no-such-id")
            assert response.status_code == 404
            assert response.json()["error"]["message"] == "File not found"


@pytest.mark.parametrize("attempt", [1, 2])
def test_list_isolation(attempt):
    with TestClient(create_test_app()) as client:
        body = client.get("/v1/files").json()
        assert isinstance(body["data"], list)

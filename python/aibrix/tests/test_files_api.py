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

import argparse
import json
from io import BytesIO

from fastapi.testclient import TestClient

from aibrix.metadata.app import build_app


def create_test_app():
    """Create a FastAPI app configured for testing."""
    return build_app(
        argparse.Namespace(
            host=None,
            port=8100,
            enable_fastapi_docs=False,
            disable_batch_api=True,  # Disable batch API to avoid async issues in tests
            disable_file_api=False,  # Enable file API for testing
            enable_k8s_job=False,
            e2e_test=True,
        )
    )


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

            error = response.json()
            assert "detail" in error
            assert "error" in error["detail"]
            assert error["detail"]["error"]["message"] == "File not found"

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

# Copyright 2026 The Aibrix Team.
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

"""HTTP-level tests for the batches endpoint in
``aibrix/metadata/api/v1/batch.py``.

Mirrors ``tests/metadata/test_files_api.py``: in-process FastAPI app via
``TestClient``, no K8s, no real inference engine. Coverage focuses on
request-validation (4xx) paths plus list pagination and cancel; the
end-to-end happy path through the worker lives in
``tests/batch/test_e2e_openai_batch_api.py``.
"""

import json
from io import BytesIO
from typing import Any, Dict, List, Optional

import pytest
from fastapi.testclient import TestClient

from tests.metadata.conftest import create_test_app


def _make_jsonl(lines: List[Dict[str, Any]]) -> BytesIO:
    return BytesIO("\n".join(json.dumps(line) for line in lines).encode("utf-8"))


def _chat_request(custom_id: str = "req-1") -> Dict[str, Any]:
    return {
        "custom_id": custom_id,
        "method": "POST",
        "url": "/v1/chat/completions",
        "body": {
            "model": "gpt-3.5-turbo",
            "messages": [{"role": "user", "content": "hi"}],
        },
    }


def _upload_jsonl(
    client: TestClient, content: BytesIO, filename: str = "input.jsonl"
) -> str:
    """Upload a JSONL via /v1/files and return the file_id."""
    response = client.post(
        "/v1/files",
        files={"file": (filename, content, "application/jsonl")},
        data={"purpose": "batch"},
    )
    assert response.status_code == 200, response.text
    return response.json()["id"]


def _create_chat_batch(
    client: TestClient,
    *,
    extra_spec: Optional[Dict[str, Any]] = None,
    lines: Optional[List[Dict[str, Any]]] = None,
):
    """Upload a valid input file and create a chat/completions batch.

    Returns the raw response object; tests can assert status_code and parse.
    """
    file_id = _upload_jsonl(
        client, _make_jsonl(lines or [_chat_request()]), filename="batch.jsonl"
    )
    spec: Dict[str, Any] = {
        "input_file_id": file_id,
        "endpoint": "/v1/chat/completions",
        "completion_window": "24h",
    }
    if extra_spec:
        spec.update(extra_spec)
    return client.post("/v1/batches", json=spec)


class TestCreateBatch:
    def test_create_chat_completions_happy_path(self):
        with TestClient(create_test_app()) as client:
            response = _create_chat_batch(client)
            assert response.status_code == 200, response.text

            body = response.json()
            assert body["object"] == "batch"
            assert body["endpoint"] == "/v1/chat/completions"
            assert body["completion_window"] == "24h"
            # CREATED state is mapped to "validating" for OpenAI compatibility.
            assert body["status"] in {"validating", "in_progress"}
            assert body["input_file_id"]
            assert body["created_at"] > 0
            assert body["expires_at"] > body["created_at"]

    def test_create_with_metadata_round_trip(self):
        with TestClient(create_test_app()) as client:
            response = _create_chat_batch(
                client, extra_spec={"metadata": {"team": "growth", "trace": "abc"}}
            )
            assert response.status_code == 200, response.text
            assert response.json()["metadata"] == {"team": "growth", "trace": "abc"}

    def test_create_rejects_metadata_over_16_keys(self):
        with TestClient(create_test_app()) as client:
            big_metadata = {f"k{i}": str(i) for i in range(17)}
            response = _create_chat_batch(client, extra_spec={"metadata": big_metadata})
            assert response.status_code == 422

    def test_create_rejects_missing_input_file_id(self):
        with TestClient(create_test_app()) as client:
            response = client.post(
                "/v1/batches",
                json={"endpoint": "/v1/chat/completions", "completion_window": "24h"},
            )
            assert response.status_code == 422

    def test_create_rejects_unknown_endpoint(self):
        with TestClient(create_test_app()) as client:
            file_id = _upload_jsonl(client, _make_jsonl([_chat_request()]))
            response = client.post(
                "/v1/batches",
                json={
                    "input_file_id": file_id,
                    "endpoint": "/v1/unknown",
                    "completion_window": "24h",
                },
            )
            assert response.status_code == 422

    def test_create_rejects_unknown_completion_window(self):
        with TestClient(create_test_app()) as client:
            file_id = _upload_jsonl(client, _make_jsonl([_chat_request()]))
            response = client.post(
                "/v1/batches",
                json={
                    "input_file_id": file_id,
                    "endpoint": "/v1/chat/completions",
                    "completion_window": "48h",
                },
            )
            assert response.status_code == 422

    def test_create_rejects_unknown_input_file(self):
        # NOTE: When the input file does not exist, the local storage's
        # readline_iter currently yields zero lines instead of raising
        # FileNotFoundError, so the route reports the input as empty rather
        # than not-found. This documents the current behavior; if storage
        # is updated to surface FileNotFoundError, the assertion should
        # tighten to match "not found".
        with TestClient(create_test_app()) as client:
            response = client.post(
                "/v1/batches",
                json={
                    "input_file_id": "does-not-exist",
                    "endpoint": "/v1/chat/completions",
                    "completion_window": "24h",
                },
            )
            assert response.status_code == 400

    def test_create_rejects_input_missing_custom_id(self):
        line = _chat_request()
        line.pop("custom_id")
        with TestClient(create_test_app()) as client:
            response = _create_chat_batch(client, lines=[line])
            assert response.status_code == 400
            assert "custom_id" in response.json()["error"]["message"]

    def test_create_rejects_input_url_mismatching_batch_endpoint(self):
        line = _chat_request()
        line["url"] = "/v1/embeddings"  # mismatch
        with TestClient(create_test_app()) as client:
            response = _create_chat_batch(client, lines=[line])
            assert response.status_code == 400
            assert "does not match" in response.json()["error"]["message"]

    def test_create_rejects_embeddings_input_missing_input_field(self):
        # Embeddings body must carry 'input'; here we omit it.
        line = {
            "custom_id": "e1",
            "method": "POST",
            "url": "/v1/embeddings",
            "body": {"model": "text-embedding-ada-002"},
        }
        with TestClient(create_test_app()) as client:
            file_id = _upload_jsonl(client, _make_jsonl([line]))
            response = client.post(
                "/v1/batches",
                json={
                    "input_file_id": file_id,
                    "endpoint": "/v1/embeddings",
                    "completion_window": "24h",
                },
            )
            assert response.status_code == 400
            assert "input" in response.json()["error"]["message"]

    def test_create_rejects_chat_messages_not_list(self):
        line = _chat_request()
        line["body"]["messages"] = "not-a-list"
        with TestClient(create_test_app()) as client:
            response = _create_chat_batch(client, lines=[line])
            assert response.status_code == 400
            assert "messages" in response.json()["error"]["message"]

    def test_create_rejects_empty_input_file(self):
        with TestClient(create_test_app()) as client:
            file_id = _upload_jsonl(client, BytesIO(b"\n\n  \n"))
            response = client.post(
                "/v1/batches",
                json={
                    "input_file_id": file_id,
                    "endpoint": "/v1/chat/completions",
                    "completion_window": "24h",
                },
            )
            assert response.status_code == 400
            assert "empty" in response.json()["error"]["message"].lower()

    def test_create_trailing_slash_works(self):
        with TestClient(create_test_app()) as client:
            file_id = _upload_jsonl(client, _make_jsonl([_chat_request()]))
            # The batch router registers both '' and '/' paths;
            # redirect_slashes is disabled in build_app so trailing-slash
            # must work without a 307.
            response = client.post(
                "/v1/batches/",
                json={
                    "input_file_id": file_id,
                    "endpoint": "/v1/chat/completions",
                    "completion_window": "24h",
                },
            )
            assert response.status_code == 200, response.text


class TestGetBatch:
    def test_get_existing_batch_round_trip(self):
        with TestClient(create_test_app()) as client:
            create_response = _create_chat_batch(client)
            assert create_response.status_code == 200
            batch_id = create_response.json()["id"]

            get_response = client.get(f"/v1/batches/{batch_id}")
            assert get_response.status_code == 200
            body = get_response.json()
            assert body["id"] == batch_id
            assert body["endpoint"] == "/v1/chat/completions"

    def test_get_unknown_batch_404(self):
        with TestClient(create_test_app()) as client:
            response = client.get("/v1/batches/does-not-exist")
            assert response.status_code == 404
            assert response.json()["error"]["message"] == "Batch not found"


class TestCancelBatch:
    def test_cancel_unknown_batch_404(self):
        with TestClient(create_test_app()) as client:
            response = client.post("/v1/batches/does-not-exist/cancel")
            assert response.status_code == 404
            assert response.json()["error"]["message"] == "Batch not found"

    def test_cancel_pending_batch_marks_cancelling(self):
        # We only assert the synchronous "pending → cancellable" path here.
        # An in_progress batch's cancel is driven asynchronously through
        # the worker and is covered in tests/batch/test_e2e_*.
        # If the local scheduler races the cancel and finalizes the job,
        # cancel_job returns False and we get a 400; either outcome is a
        # legitimate state of the local pipeline, so we allow both and
        # assert wire-format correctness on each.
        with TestClient(create_test_app()) as client:
            create_response = _create_chat_batch(client)
            assert create_response.status_code == 200
            batch_id = create_response.json()["id"]

            response = client.post(f"/v1/batches/{batch_id}/cancel")
            if response.status_code == 200:
                body = response.json()
                assert body["id"] == batch_id
                assert body["status"] in {"cancelling", "cancelled"}
                assert body.get("cancelling_at") is not None
            else:
                assert response.status_code == 400
                assert (
                    response.json()["error"]["message"] == "Batch cannot be cancelled"
                )


class TestListBatches:
    def test_list_empty(self):
        with TestClient(create_test_app()) as client:
            response = client.get("/v1/batches")
            assert response.status_code == 200
            body = response.json()
            assert body["object"] == "list"
            assert body["data"] == []
            assert body["has_more"] is False
            assert body["first_id"] is None
            assert body["last_id"] is None

    def test_list_with_limit_reports_has_more(self):
        with TestClient(create_test_app()) as client:
            ids = [_create_chat_batch(client).json()["id"] for _ in range(3)]
            response = client.get("/v1/batches", params={"limit": 2})
            assert response.status_code == 200
            body = response.json()
            assert len(body["data"]) == 2
            assert body["has_more"] is True
            assert body["first_id"] == body["data"][0]["id"]
            assert body["last_id"] == body["data"][-1]["id"]
            # All returned ids must be a subset of the ones we created.
            returned = {item["id"] for item in body["data"]}
            assert returned.issubset(set(ids))

    def test_list_with_after_cursor_pages(self):
        with TestClient(create_test_app()) as client:
            for _ in range(3):
                _create_chat_batch(client)
            page1 = client.get("/v1/batches", params={"limit": 2}).json()
            page2 = client.get(
                "/v1/batches", params={"limit": 2, "after": page1["last_id"]}
            ).json()
            assert len(page2["data"]) == 1
            assert page2["has_more"] is False
            # Page 2's first item must not be in page 1.
            page1_ids = {item["id"] for item in page1["data"]}
            assert page2["data"][0]["id"] not in page1_ids

    def test_list_after_unknown_cursor_returns_empty(self):
        with TestClient(create_test_app()) as client:
            _create_chat_batch(client)
            response = client.get("/v1/batches", params={"after": "no-such-id"})
            assert response.status_code == 200
            body = response.json()
            assert body["data"] == []
            assert body["has_more"] is False

    @pytest.mark.parametrize("limit", [0, 101])
    def test_list_rejects_out_of_range_limit(self, limit):
        with TestClient(create_test_app()) as client:
            response = client.get("/v1/batches", params={"limit": limit})
            assert response.status_code == 422

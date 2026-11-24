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

"""Persistence boundaries for streamed multipart synchronous video requests."""

import asyncio
import json
from pathlib import Path
from types import SimpleNamespace

import httpx
import pytest
import pytest_asyncio
from starlette.requests import Request

from aibrix.openai_frontend.engine.proxy_engine import EngineState, ProxyEngine
from aibrix.openai_frontend.frontend.fastapi_frontend import FastApiFrontend
from aibrix.openai_frontend.proxy.output import output_writer
from aibrix.openai_frontend.proxy.output.output_writer import OutputWriter
from aibrix.openai_frontend.proxy.runtime import video_sync
from aibrix.openai_frontend.proxy.runtime.engine_spec import VLLM_OMNI_SPEC
from aibrix.openai_frontend.utils.utils import ClientError
from aibrix.storage.types import StorageType

PATH = "/v1/videos/sync?flag=a%2Fb&flag=c"
VIDEO = b"\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom\x00\xffvideo"


@pytest_asyncio.fixture
async def harness(monkeypatch, tmp_path, request):
    h = SimpleNamespace(
        video_id="video_gen_1",
        file_name="download-label.mp4",
        file_state="regular",
        complete_on_create=False,
        job_metadata={},
        storage_type=StorageType.TOS,
        content=VIDEO,
        pause_upload=False,
        reject_upload=False,
        forwarded=bytearray(),
        created=0,
        deleted=[],
        objects={},
        forwarding=asyncio.Event(),
        uploading=asyncio.Event(),
        release_upload=asyncio.Event(),
    )
    monkeypatch.setattr(video_sync, "VIDEO_POLL_INTERVAL_SECONDS", 0)

    async def put_object(key, data, **kwargs):
        h.uploading.set()
        if h.pause_upload:
            await h.release_upload.wait()
        if h.reject_upload:
            return False
        h.objects[key] = (data, kwargs["content_type"])
        return True

    store = SimpleNamespace(
        get_type=lambda: h.storage_type,
        bucket_name="bucket",
        put_object=put_object,
    )
    h.engine = ProxyEngine(
        ["unused"],
        spec=VLLM_OMNI_SPEC,
        media_dir=None if getattr(request, "param", False) else str(tmp_path / "omni"),
        fetch_media=False,
        output_writer=OutputWriter(storage_factory=lambda: store),
    )
    h.storage_path = Path(h.engine._media_dir)
    h.owns_media_dir = h.engine._owned_media_dir is not None
    h.source = h.storage_path / h.video_id
    h.engine._set_state(EngineState.READY)

    class Transport(httpx.AsyncBaseTransport):
        async def handle_async_request(self, request):
            if request.method == "POST":
                h.forwarded_headers = request.headers
                async for chunk in request.stream:
                    h.forwarded.extend(chunk)
                    h.forwarding.set()
                h.created += 1
                if h.file_state == "unsafe":
                    h.video_id = "../outside"
                if not h.complete_on_create:
                    return httpx.Response(
                        200, json={"id": h.video_id, "status": "queued"}
                    )
            elif request.method == "DELETE":
                h.deleted.append(request.url.path)
                h.source.unlink(missing_ok=True)
                return httpx.Response(200, json={"deleted": True})
            else:
                assert not request.url.path.endswith("/content")
            if h.file_state == "symlink":
                target = tmp_path / "outside.mp4"
                target.write_bytes(VIDEO)
                h.source.symlink_to(target)
            elif h.file_state != "missing":
                h.source.write_bytes(h.content)
            h.completed_job = {
                "id": h.video_id,
                "status": "completed",
                "model": "engine-model",
                "file_name": h.file_name,
                **h.job_metadata,
            }
            return httpx.Response(200, json=h.completed_job)

    h.engine._subprocess._client = httpx.AsyncClient(
        transport=Transport(), base_url="http://inner"
    )

    def set_form(output='{"url":"tos://bucket/run"}'):
        upload = httpx.Request(
            "POST",
            f"http://frontend{PATH}",
            files=[
                ("model", (None, "minimax-h3")),
                ("prompt", (None, "生成视频。")),
                ("input_references", ("first.png", b"image\x00\xff", "image/png")),
                ("output", (None, output)),
            ],
        )
        h.body = upload.read()
        h.headers = {
            "content-type": upload.headers["content-type"],
            "content-length": upload.headers["content-length"],
        }
        h.expected = httpx.Request(
            "POST",
            f"http://frontend{PATH}",
            headers={"content-type": h.headers["content-type"]},
            files=[
                ("model", (None, "minimax-h3")),
                ("prompt", (None, "生成视频。")),
                ("input_references", ("first.png", b"image\x00\xff", "image/png")),
            ],
        ).read()

    h.set_form = set_form
    h.set_form()
    async with httpx.AsyncClient(
        transport=httpx.ASGITransport(FastApiFrontend(h.engine).app),
        base_url="http://frontend",
    ) as h.client:
        try:
            yield h
            assert not h.engine._in_flight
            assert len(h.deleted) == h.created
            assert not h.source.exists()
        finally:
            h.release_upload.set()
            await h.engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize("destination", ["local", "tos", "s3"])
@pytest.mark.parametrize(
    "file_name, content, suffix, media_type",
    [
        ("download-label.mp4", VIDEO, ".mp4", "video/mp4"),
        (
            "download-label.webm",
            b"\x1a\x45\xdf\xa3webm-video",
            ".webm",
            "video/webm",
        ),
    ],
)
async def test_video_sync_persists_declared_video_format(
    harness, tmp_path, destination, file_name, content, suffix, media_type
):
    h = harness
    h.file_name = file_name
    h.content = content
    output = (
        str(tmp_path / "out")
        if destination == "local"
        else f"{destination}://bucket/run"
    )
    if destination == "s3":
        h.storage_type = StorageType.S3
    h.set_form(json.dumps({"url": output}))

    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 200, response.text
    receipt = response.json()
    assert receipt["id"].startswith("video-")
    assert receipt["id"] != h.video_id
    assert receipt["status"] == "completed"
    assert receipt["model"] == "engine-model"
    assert isinstance(receipt["created_at"], int)
    assert receipt["output"]["url"] == output
    (artifact,) = receipt["output"]["artifacts"]
    assert artifact == f"{receipt['id']}-0{suffix}"
    if destination == "local":
        assert (tmp_path / "out" / artifact).read_bytes() == content
    else:
        assert h.objects == {f"run/{artifact}": (content, media_type)}


@pytest.mark.asyncio
@pytest.mark.parametrize("complete_on_create", [False, True], ids=["poll", "create"])
@pytest.mark.parametrize("created_at", [123.5, None])
async def test_video_sync_receipt_preserves_completed_job_metadata(
    harness, complete_on_create, created_at
):
    h = harness
    h.complete_on_create = complete_on_create
    h.job_metadata = {
        "created_at": created_at,
        "completed_at": 124.5,
        "inference_time_s": 1.25,
        "stage_durations": {"diffusion": 1.1, "decode": 0.0},
        "peak_memory_mb": 2000.0,
        "progress": {"frames": 8},
        "seconds": "vendor-duration",
        "error": None,
        "data": {"vendor_metadata": [None, False, 0]},
        "extension": {"nested": ["kept", {"value": 1.2345678901234567}]},
    }
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 200, response.text
    receipt = response.json()
    assert receipt == {
        **h.completed_job,
        "id": receipt["id"],
        "output": receipt["output"],
    }
    assert receipt["id"].startswith("video-") and receipt["id"] != h.video_id
    artifact = f"{receipt['id']}-0.mp4"
    assert receipt["output"] == {"url": "tos://bucket/run", "artifacts": [artifact]}
    # Completed-job metadata stays in HTTP; persistence contains only the video.
    assert h.objects == {f"run/{artifact}": (VIDEO, "video/mp4")}


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "file_name", [None, 123, "download-label.unknown", "download-label.txt"]
)
async def test_video_sync_unrecognized_format_never_defaults_to_mp4(harness, file_name):
    h = harness
    h.file_name = file_name
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 500, response.text
    assert "output" not in response.json() and not h.objects


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "file_name, content, media_type",
    [
        ("download-label.mp4", VIDEO, "video/mp4"),
        ("download-label.webm", b"\x1a\x45\xdf\xa3webm-video", "video/webm"),
    ],
)
async def test_video_sync_presigned_url_only_reaches_output_writer(
    harness, monkeypatch, file_name, content, media_type
):
    h = harness
    h.file_name = file_name
    h.content = content
    url = "https://uploads.example/video.mp4?signature=frontend-only-token&expires=123"
    h.set_form(json.dumps({"url": url}))
    uploads = []

    async def put(request):
        uploads.append(
            (
                request.method,
                str(request.url),
                await request.aread(),
                request.headers["content-type"],
            )
        )
        return httpx.Response(200)

    monkeypatch.setattr(
        output_writer,
        "httpx",
        SimpleNamespace(
            AsyncClient=lambda **kwargs: httpx.AsyncClient(
                transport=httpx.MockTransport(put), **kwargs
            )
        ),
    )
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 200, response.text
    assert uploads == [("PUT", url, content, media_type)]
    assert response.json()["output"] == {"url": url, "artifacts": [url]}
    assert h.forwarded == h.expected
    assert "content-length" not in h.forwarded_headers
    assert h.forwarded_headers["transfer-encoding"] == "chunked"
    assert url.encode() not in h.forwarded
    assert b'name="output"' not in h.forwarded


@pytest.mark.asyncio
async def test_video_sync_waits_for_upload_and_uses_unique_names(harness):
    h = harness
    h.pause_upload = True
    task = asyncio.create_task(h.client.post(PATH, content=h.body, headers=h.headers))
    try:
        await asyncio.wait_for(h.uploading.wait(), 2)
        assert not task.done() and h.engine._in_flight
        assert h.source.read_bytes() == VIDEO and not h.deleted
        h.release_upload.set()
        first = await asyncio.wait_for(task, 2)
        second = await h.client.post(PATH, content=h.body, headers=h.headers)
        assert first.status_code == second.status_code == 200
        assert first.json()["id"] != second.json()["id"]
        assert len(h.objects) == 2
    finally:
        h.release_upload.set()
        await asyncio.gather(task, return_exceptions=True)


@pytest.mark.asyncio
async def test_video_sync_output_streams_before_upload_finishes(harness):
    h = harness
    h.headers.pop("content-length")
    release = asyncio.Event()

    async def chunks():
        yield h.body[:100]
        await release.wait()
        for offset in range(100, len(h.body), 13):
            yield h.body[offset : offset + 13]

    task = asyncio.create_task(h.client.post(PATH, content=chunks(), headers=h.headers))
    try:
        await asyncio.wait_for(h.forwarding.wait(), 2)
        assert h.forwarded and h.body.startswith(h.forwarded)
        assert not task.done() and not h.created
        release.set()
        response = await asyncio.wait_for(task, 2)
        assert response.status_code == 200, response.text
        assert h.forwarded == h.expected
    finally:
        release.set()
        task.cancel()
        await asyncio.gather(task, return_exceptions=True)


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "output",
    ["{", "x" * (64 * 1024 + 1)],
    ids=["json", "oversized"],
)
async def test_video_sync_validates_output_after_forwarding(harness, output):
    h = harness
    h.set_form(output)
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 400, response.text
    assert h.forwarded == h.expected
    assert not h.objects


@pytest.mark.asyncio
@pytest.mark.parametrize("failure", ["empty", "upload"])
async def test_video_sync_failure_never_returns_a_receipt(harness, failure):
    h = harness
    if failure == "empty":
        h.content = b""
    else:
        h.reject_upload = True
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 500, response.text
    assert "output" not in response.json() and not h.objects


@pytest.mark.asyncio
@pytest.mark.parametrize("interrupt", ["cancel", "disconnect", "shutdown"])
@pytest.mark.parametrize(
    "harness", [False, True], indirect=True, ids=["explicit-dir", "temporary-dir"]
)
async def test_video_sync_interruption_waits_for_upload_before_deleting_source(
    harness, interrupt
):
    h = harness
    h.pause_upload = True
    messages = asyncio.Queue()
    messages.put_nowait({"type": "http.request", "body": h.body, "more_body": False})
    request = Request(
        {
            "type": "http",
            "method": "POST",
            "path": "/v1/videos/sync",
            "query_string": b"",
            "headers": [
                (key.encode(), value.encode()) for key, value in h.headers.items()
            ],
        },
        messages.get,
    )
    task = asyncio.create_task(h.engine.forward_multipart(request.url.path, request))
    stopper = None
    try:
        await asyncio.wait_for(h.uploading.wait(), 2)
        if interrupt == "cancel":
            task.cancel()
        elif interrupt == "disconnect":
            messages.put_nowait({"type": "http.disconnect"})
        else:
            stopper = asyncio.create_task(h.engine.aclose())
        await asyncio.sleep(0)
        await asyncio.sleep(0)
        if interrupt == "cancel":
            # Repeated cancellation must still wait while storage owns the source.
            task.cancel()
            await asyncio.sleep(0)
        assert not task.done() and h.engine._in_flight
        assert h.storage_path.is_dir() and h.source.exists() and not h.deleted
        if stopper is not None:
            assert not stopper.done()
        h.release_upload.set()
        error = asyncio.CancelledError if interrupt == "cancel" else ClientError
        with pytest.raises(error):
            await asyncio.wait_for(task, 2)
        if stopper is not None:
            await asyncio.wait_for(stopper, 2)
            assert h.storage_path.exists() is (not h.owns_media_dir)
    finally:
        h.release_upload.set()
        await asyncio.gather(task, return_exceptions=True)
        if stopper is not None:
            await asyncio.gather(stopper, return_exceptions=True)


@pytest.mark.asyncio
@pytest.mark.parametrize("file_state", ["missing", "symlink", "unsafe"])
async def test_local_video_invalid_source_never_returns_a_receipt(harness, file_state):
    h = harness
    h.file_state = file_state
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 500, response.text
    assert "output" not in response.json() and not h.objects

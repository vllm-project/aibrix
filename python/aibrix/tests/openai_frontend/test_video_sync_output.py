# Copyright 2026 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#       http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Persistence boundaries for streamed multipart synchronous video requests."""

import asyncio
import json
import tempfile
import time
from types import SimpleNamespace

import httpx
import pytest
import pytest_asyncio
from starlette.requests import Request

from aibrix.openai_frontend.engine.proxy_engine import EngineState, ProxyEngine
from aibrix.openai_frontend.frontend.fastapi_frontend import FastApiFrontend
from aibrix.openai_frontend.proxy.output.output_writer import OutputWriter
from aibrix.openai_frontend.proxy.runtime import video_sync
from aibrix.openai_frontend.proxy.runtime.engine_spec import VLLM_OMNI_SPEC
from aibrix.openai_frontend.utils.utils import ClientError
from aibrix.storage.types import StorageType

PATH = "/v1/videos/sync?flag=a%2Fb&flag=c"
VIDEO = b"\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom\x00\xffvideo"


@pytest_asyncio.fixture
async def harness(monkeypatch, tmp_path):
    h = SimpleNamespace(
        video_id="video_gen_1",
        file_state="regular",
        storage_type=StorageType.TOS,
        content=VIDEO,
        pause_upload=False,
        reject_upload=False,
        forwarded=[],
        upload_chunks=[],
        deleted=[],
        objects={},
        forwarding=asyncio.Event(),
        uploading=asyncio.Event(),
        release_upload=asyncio.Event(),
    )
    h.storage_path = tmp_path / "omni"
    h.storage_path.mkdir()
    h.source = h.storage_path / h.video_id

    def no_upload_spooling(*args, **kwargs):
        pytest.fail("Input uploads must stream without temporary files")

    monkeypatch.setattr(tempfile, "TemporaryFile", no_upload_spooling)
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
        engine_env={"VLLM_OMNI_SERVER_STORAGE__PATH": str(h.storage_path)},
        fetch_media=False,
        output_writer=OutputWriter(storage_factory=lambda: store),
    )
    h.engine._set_state(EngineState.READY)

    class Transport(httpx.AsyncBaseTransport):
        async def handle_async_request(self, request):
            if request.method == "POST":
                chunks = []
                async for chunk in request.stream:
                    chunks.append(chunk)
                    h.upload_chunks.append(chunk)
                    h.forwarding.set()
                h.forwarded.append(
                    (request.url.raw_path, dict(request.headers), b"".join(chunks))
                )
                h.video_id = (
                    f"video_gen_{len(h.forwarded)}"
                    if h.file_state != "unsafe"
                    else "../outside"
                )
                h.source = h.storage_path / f"video_gen_{len(h.forwarded)}"
                return httpx.Response(200, json={"id": h.video_id, "status": "queued"})
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
                return httpx.Response(
                    200,
                    json={
                        "id": h.video_id,
                        "status": "completed",
                        "model": "engine-model",
                        "file_name": "download-label.mp4",
                        "inference_time_s": 1.25,
                    },
                )

    h.engine._subprocess._client = httpx.AsyncClient(
        transport=Transport(), base_url="http://inner"
    )

    def set_form(
        output='{"url":"tos://bucket/run"}',
        *,
        extra=(),
        output_first=False,
    ):
        native_parts = [
            ("model", (None, "minimax-h3")),
            ("prompt", (None, "图1、图2按顺序生成视频。")),
            ("seed", (None, "0")),
            ("input_references", ("first.png", b"first", "image/png")),
            (
                "input_references",
                ("second.png", b"binary\r\n--almost-a-boundary\x00\xff", "image/png"),
            ),
            *extra,
        ]
        output_parts = [] if output is None else [("output", (None, output))]
        upload = httpx.Request(
            "POST",
            f"http://frontend{PATH}",
            files=(
                [*output_parts, *native_parts]
                if output_first
                else [*native_parts, *output_parts]
            ),
        )
        h.body = upload.read()
        h.headers = {
            "content-type": upload.headers["content-type"],
            "content-length": upload.headers["content-length"],
        }

    h.set_form = set_form
    h.set_form()
    async with httpx.AsyncClient(
        transport=httpx.ASGITransport(FastApiFrontend(h.engine).app),
        base_url="http://frontend",
    ) as h.client:
        try:
            yield h
        finally:
            h.release_upload.set()
            await h.engine.aclose()
    assert not h.engine._in_flight
    assert not h.engine._request_done
    assert not h.engine._stream_closers
    assert len(h.deleted) == len(h.forwarded)


@pytest.mark.asyncio
@pytest.mark.parametrize("destination", ["local", "tos", "s3"])
async def test_video_sync_persists_complete_mp4(harness, tmp_path, destination):
    h = harness
    output = (
        str(tmp_path / "out")
        if destination == "local"
        else f"{destination}://bucket/run"
    )
    if destination == "s3":
        h.storage_type = StorageType.S3
    h.set_form(json.dumps({"url": output}))

    started_at = int(time.time())
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 200, response.text
    receipt = response.json()
    assert receipt["id"].startswith("video-")
    assert receipt["id"] != h.video_id
    assert receipt["status"] == "completed"
    assert receipt["model"] == "minimax-h3"
    assert started_at <= receipt["created_at"] <= int(time.time())
    assert receipt["output"]["url"] == output
    (artifact,) = receipt["output"]["artifacts"]
    assert artifact == f"{receipt['id']}-0.mp4"
    assert "content-disposition" not in response.headers
    assert "content-encoding" not in response.headers
    if destination == "local":
        assert (tmp_path / "out" / artifact).read_bytes() == VIDEO
    else:
        assert h.objects == {f"run/{artifact}": (VIDEO, "video/mp4")}
    ((path, headers, body),) = h.forwarded
    assert path == PATH.replace("/sync", "").encode() and body == h.body
    assert headers["content-type"] == h.headers["content-type"]
    assert headers["content-length"] == h.headers["content-length"]
    assert not h.source.exists()


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
@pytest.mark.parametrize(
    "model_part, expected_model",
    [
        ((None, "模型/测试"), "模型/测试"),
        (("model.txt", b"uploaded-model"), None),
        ((None, "x" * (64 * 1024 + 1)), None),
    ],
    ids=["duplicate", "file", "oversized"],
)
async def test_video_sync_receipt_preserves_native_model_fields(
    harness, model_part, expected_model
):
    h = harness
    h.set_form(extra=[("model", model_part)])
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 200, response.text
    assert response.json()["model"] == expected_model
    assert h.forwarded[0][2] == h.body


@pytest.mark.asyncio
@pytest.mark.parametrize("output_first", [False, True])
async def test_video_sync_output_streams_before_upload_finishes(harness, output_first):
    h = harness
    h.set_form(output_first=output_first)
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
        assert h.upload_chunks == [h.body[:100]]
        assert not task.done() and not h.forwarded
        release.set()
        response = await asyncio.wait_for(task, 2)
        assert response.status_code == 200, response.text
        (artifact,) = response.json()["output"]["artifacts"]
        assert h.objects == {f"run/{artifact}": (VIDEO, "video/mp4")}
        assert h.forwarded[0][2] == h.body
        assert "content-length" not in h.forwarded[0][1]
    finally:
        release.set()
        task.cancel()
        await asyncio.gather(task, return_exceptions=True)


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "output",
    ["{", '{"url":"ftp://host/out"}', "x" * (64 * 1024 + 1)],
    ids=["json", "scheme", "oversized"],
)
async def test_video_sync_validates_output_after_forwarding(harness, output):
    h = harness
    h.set_form(output)
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 400, response.text
    assert h.forwarded[0][2] == h.body
    assert not h.objects


@pytest.mark.asyncio
async def test_video_sync_last_output_field_wins(harness):
    h = harness
    # A later text field follows native form lookup semantics.
    upload = httpx.Request(
        "POST",
        f"http://frontend{PATH}",
        files=[
            ("output", ("bad.json", b"invalid")),
            ("output", (None, '{"url":"tos://bucket/run"}')),
        ],
    )
    response = await h.client.post(
        PATH,
        content=upload.read(),
        headers={"content-type": upload.headers["content-type"]},
    )
    assert response.status_code == 200, response.text
    assert h.forwarded[0][2] == upload.read()
    assert response.json()["model"] is None
    (artifact,) = response.json()["output"]["artifacts"]
    assert h.objects == {f"run/{artifact}": (VIDEO, "video/mp4")}


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
async def test_video_sync_cancellation_waits_for_upload_before_deleting_source(harness):
    h = harness
    h.pause_upload = True
    task = asyncio.create_task(h.client.post(PATH, content=h.body, headers=h.headers))
    try:
        await asyncio.wait_for(h.uploading.wait(), 2)
        task.cancel()
        # The storage operation still owns the source, even on repeated cancellation.
        await asyncio.sleep(0)
        task.cancel()
        await asyncio.sleep(0)
        assert not task.done() and h.engine._in_flight
        assert h.source.exists() and not h.deleted
        h.release_upload.set()
        with pytest.raises(asyncio.CancelledError):
            await asyncio.wait_for(task, 2)
        assert not h.engine._in_flight
        assert not h.source.exists()
    finally:
        h.release_upload.set()
        await asyncio.gather(task, return_exceptions=True)


@pytest.mark.asyncio
@pytest.mark.parametrize("interrupt", ["disconnect", "shutdown"])
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
            "query_string": b"flag=a%2Fb&flag=c",
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
        if interrupt == "disconnect":
            messages.put_nowait({"type": "http.disconnect"})
        else:
            stopper = asyncio.create_task(h.engine.aclose())
        await asyncio.sleep(0)
        await asyncio.sleep(0)
        assert not task.done() and h.engine._in_flight
        assert h.source.exists() and not h.deleted
        h.release_upload.set()
        with pytest.raises(ClientError):
            await asyncio.wait_for(task, 2)
        if stopper is not None:
            await asyncio.wait_for(stopper, 2)
        assert h.deleted == ["/v1/videos/video_gen_1"]
        assert not h.source.exists()
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
    assert len(h.deleted) == 1

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

"""Persistence boundaries for the native multipart synchronous video endpoint."""

import asyncio
import gzip
import json
import tempfile
import time
from pathlib import Path
from types import SimpleNamespace
from uuid import UUID

import httpx
import pytest
import pytest_asyncio
from starlette.requests import Request

from aibrix.openai_frontend.engine.proxy_engine import EngineState, ProxyEngine
from aibrix.openai_frontend.frontend.fastapi_frontend import FastApiFrontend
from aibrix.openai_frontend.proxy.output.output_writer import OutputWriter
from aibrix.openai_frontend.proxy.runtime.engine_spec import VLLM_OMNI_SPEC
from aibrix.openai_frontend.utils.utils import ClientError
from aibrix.storage.types import StorageType

PATH = "/v1/videos/sync?flag=a%2Fb&flag=c"
VIDEO = b"\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom\x00\xffvideo"


@pytest_asyncio.fixture
async def harness(monkeypatch, tmp_path):
    h = SimpleNamespace(
        content=VIDEO,
        status=200,
        response_headers={
            "content-type": "video/mp4",
            "content-length": str(len(VIDEO)),
            "content-disposition": 'attachment; filename="../../engine.mp4"',
        },
        read_error=False,
        pause_receive=False,
        pause_upload=False,
        reject_upload=False,
        forwarded=[],
        deleted=[],
        streams=[],
        directories=[],
        request_files=[],
        objects={},
        receiving=asyncio.Event(),
        uploading=asyncio.Event(),
        release_receive=asyncio.Event(),
        release_upload=asyncio.Event(),
    )
    directory_factory = tempfile.TemporaryDirectory

    def directory(*args, **kwargs):
        if kwargs.get("prefix") == "aibrix-video-sync-":
            kwargs["dir"] = tmp_path
            result = directory_factory(*args, **kwargs)
            h.directories.append(Path(result.name))
            return result
        return directory_factory(*args, **kwargs)

    monkeypatch.setattr(tempfile, "TemporaryDirectory", directory)
    file_factory = tempfile.TemporaryFile

    def temporary_file(*args, **kwargs):
        result = file_factory(*args, **kwargs)
        h.request_files.append(result)
        return result

    monkeypatch.setattr(tempfile, "TemporaryFile", temporary_file)

    async def put_object(key, data, **kwargs):
        h.uploading.set()
        if h.pause_upload:
            await h.release_upload.wait()
        if h.reject_upload:
            return False
        h.objects[key] = (data, kwargs["content_type"])
        return True

    store = SimpleNamespace(
        get_type=lambda: StorageType.TOS,
        bucket_name="bucket",
        put_object=put_object,
    )
    h.engine = ProxyEngine(
        ["unused"],
        spec=VLLM_OMNI_SPEC,
        fetch_media=False,
        output_writer=OutputWriter(storage_factory=lambda: store),
    )
    h.engine._set_state(EngineState.READY)

    class Body(httpx.AsyncByteStream):
        closed = False

        async def __aiter__(self):
            yield h.content[:7]
            h.receiving.set()
            if h.pause_receive:
                await h.release_receive.wait()
            if h.read_error:
                raise httpx.RemoteProtocolError("incomplete video response")
            yield h.content[7:]

        async def aclose(self):
            self.closed = True

    async def upstream(request):
        if request.method == "POST":
            h.forwarded.append(
                (request.url.raw_path, dict(request.headers), await request.aread())
            )
            if h.status == 200:
                return httpx.Response(
                    200,
                    json={
                        "id": f"video_gen_{len(h.forwarded)}",
                        "status": "completed",
                        "model": "engine-model",
                        "inference_time_s": 1.25,
                    },
                )
        elif request.method == "DELETE":
            h.deleted.append(request.url.path)
            return httpx.Response(200, json={"deleted": True})
        else:
            assert (
                request.url.path == f"/v1/videos/video_gen_{len(h.forwarded)}/content"
            )
        stream = Body()
        h.streams.append(stream)
        return httpx.Response(h.status, headers=h.response_headers, stream=stream)

    h.engine._subprocess._client = httpx.AsyncClient(
        transport=httpx.MockTransport(upstream), base_url="http://inner"
    )

    def set_form(
        output='{"url":"tos://bucket/run"}',
        *,
        extra=(),
        output_first=False,
        model="minimax-h3",
    ):
        model_parts = [] if model is None else [("model", (None, model))]
        native_parts = [
            *model_parts,
            ("prompt", (None, "图1、图2按顺序生成视频。")),
            ("seed", (None, "0")),
            ("input_references", ("first.png", b"first", "image/png")),
            ("input_references", ("second.png", b"second", "image/png")),
            *extra,
        ]
        output_parts = [] if output is None else [("output", (None, output))]
        parts = (
            [*output_parts, *native_parts]
            if output_first
            else [*native_parts, *output_parts]
        )
        upload = httpx.Request("POST", f"http://frontend{PATH}", files=parts)
        h.body = upload.read()
        h.headers = {
            "content-type": upload.headers["content-type"],
            "content-length": upload.headers["content-length"],
        }
        # Reuse the original boundary to compare every remaining part byte-for-byte.
        expected = httpx.Request(
            "POST",
            "http://inner",
            files=[part for part in native_parts if part[0] != "output"],
            headers={"content-type": h.headers["content-type"]},
        )
        h.native_body = expected.read()

    h.set_form = set_form
    h.set_form()
    app = FastApiFrontend(h.engine).app
    async with httpx.AsyncClient(
        transport=httpx.ASGITransport(app), base_url="http://frontend"
    ) as h.client:
        try:
            yield h
        finally:
            h.release_receive.set()
            h.release_upload.set()
            await h.engine.aclose()
    assert not h.engine._in_flight
    assert not h.engine._request_done
    assert not h.engine._stream_closers
    assert all(stream.closed for stream in h.streams)
    assert all(not path.exists() for path in h.directories)
    assert all(file.closed for file in h.request_files)
    assert len(h.deleted) == (len(h.forwarded) if h.status == 200 else 0)


@pytest.mark.asyncio
@pytest.mark.parametrize("destination", ["local", "tos"])
@pytest.mark.parametrize("encoded", [False, True])
@pytest.mark.parametrize("chunk_size", [7, None])
async def test_video_sync_persists_complete_mp4(
    harness, tmp_path, destination, encoded, chunk_size
):
    h = harness
    output = str(tmp_path / "out") if destination == "local" else "tos://bucket/run"
    h.set_form(json.dumps({"url": output}))
    if encoded:
        h.content = gzip.compress(VIDEO)
        h.response_headers["content-encoding"] = "gzip"
        h.response_headers["content-length"] = str(len(h.content))

    async def chunks():
        for offset in range(0, len(h.body), chunk_size):
            yield h.body[offset : offset + chunk_size]

    started_at = int(time.time())
    response = await h.client.post(
        PATH, content=h.body if chunk_size is None else chunks(), headers=h.headers
    )
    assert response.status_code == 200, response.text
    receipt = response.json()
    assert set(receipt) == {"id", "model", "created", "output"}
    assert receipt["id"].startswith("video-")
    assert UUID(receipt["id"].removeprefix("video-")).version == 4
    assert receipt["model"] == "minimax-h3"
    assert started_at <= receipt["created"] <= int(time.time())
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
    assert path == PATH.replace("/sync", "").encode() and body == h.native_body
    assert headers["content-type"] == h.headers["content-type"]
    assert headers["content-length"] == str(len(h.native_body))
    assert h.directories and not h.engine._in_flight


@pytest.mark.asyncio
async def test_video_sync_waits_for_upload_and_uses_unique_names(harness):
    h = harness
    h.pause_upload = True
    task = asyncio.create_task(h.client.post(PATH, content=h.body, headers=h.headers))
    try:
        await asyncio.wait_for(h.uploading.wait(), 2)
        assert not task.done() and h.engine._in_flight
        assert list(h.directories[0].glob("*.mp4"))
        h.release_upload.set()
        first = await asyncio.wait_for(task, 2)
        second = await h.client.post(PATH, content=h.body, headers=h.headers)
        assert first.status_code == second.status_code == 200
        assert first.json()["id"] != second.json()["id"]
        assert (
            first.json()["output"]["artifacts"] != second.json()["output"]["artifacts"]
        )
        assert len(h.objects) == 2
    finally:
        h.release_upload.set()
        await asyncio.gather(task, return_exceptions=True)


@pytest.mark.asyncio
@pytest.mark.parametrize("model", ["模型/测试", "", None])
async def test_video_sync_receipt_uses_submitted_model(harness, model):
    h = harness
    h.set_form(model=model)
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 200, response.text
    assert response.json()["model"] == model
    assert "usage" not in response.json()
    assert h.forwarded[0][2] == h.native_body


@pytest.mark.asyncio
@pytest.mark.parametrize("output", [None, "null", '{"url":"tos://bucket/run"}'])
@pytest.mark.parametrize(
    "model_part, expected_model",
    [
        ((None, "last-model"), "last-model"),
        (("model.txt", b"uploaded-model"), None),
        ((None, b"\xff"), None),
        ((None, "x" * (64 * 1024 + 1)), None),
    ],
    ids=["duplicate", "file", "non_utf8", "oversized"],
)
async def test_video_sync_model_metadata_preserves_native_form(
    harness, output, model_part, expected_model
):
    h = harness
    h.set_form(output, extra=[("model", model_part)])
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 200, response.text
    if output not in (None, "null"):
        assert response.json()["model"] == expected_model
        assert "usage" not in response.json()
    else:
        assert response.content == VIDEO
    assert h.forwarded[0][2] == (h.body if output is None else h.native_body)


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "invalid",
    [
        "empty",
        "scheme",
        "duplicate",
        "json",
        "missing_url",
        "extra",
        "array",
        "file",
        "stream",
    ],
)
async def test_video_sync_rejects_invalid_output_before_upload(harness, invalid):
    h = harness
    values = {
        "empty": "",
        "scheme": '{"url":"ftp://host/out"}',
        "json": "{",
        "missing_url": "{}",
        "extra": '{"url":"tos://bucket/run","unknown":true}',
        "array": "[]",
    }
    if invalid == "duplicate":
        h.set_form(extra=[("output", (None, '{"url":"tos://bucket/other"}'))])
    elif invalid == "file":
        h.set_form(
            None, extra=[("output", ("spec.json", b'{"url":"tos://bucket/run"}'))]
        )
    elif invalid == "stream":
        h.set_form(extra=[("stream", (None, "true"))])
    else:
        h.set_form(values[invalid])
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 400, response.text
    assert not h.forwarded and not h.objects and not h.directories


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "failure",
    ["upstream", "type", "empty", "truncated", "read", "upload", "bucket"],
)
async def test_video_sync_failure_never_returns_a_receipt(harness, failure):
    h = harness
    if failure in {"upstream", "type"}:
        h.content = b'{"detail":"generation failed"}'
        h.response_headers["content-type"] = "application/json"
        h.response_headers["content-length"] = str(len(h.content))
        if failure == "upstream":
            h.status = 422
    elif failure == "empty":
        h.content = b""
        h.response_headers["content-length"] = "0"
    elif failure == "truncated":
        h.response_headers["content-length"] = str(len(VIDEO) + 1)
    elif failure == "read":
        h.read_error = True
    elif failure == "upload":
        h.reject_upload = True
    else:
        h.set_form('{"url":"tos://other/run"}')
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == (422 if failure == "upstream" else 500), (
        response.text
    )
    assert "output" not in response.json() and not h.objects
    if failure == "upstream":
        assert response.content == h.content
    if failure not in {"upload", "bucket"}:
        assert not h.uploading.is_set()
    assert not h.engine._in_flight


@pytest.mark.asyncio
@pytest.mark.parametrize("phase", ["receive", "upload"])
async def test_video_sync_cancellation_cleans_only_after_work_settles(harness, phase):
    h = harness
    h.pause_receive = phase == "receive"
    h.pause_upload = phase == "upload"
    entered = h.receiving if phase == "receive" else h.uploading
    task = asyncio.create_task(h.client.post(PATH, content=h.body, headers=h.headers))
    try:
        await asyncio.wait_for(entered.wait(), 2)
        task.cancel()
        if phase == "upload":
            # The storage operation still owns the source, even on repeated cancellation.
            await asyncio.sleep(0)
            task.cancel()
            await asyncio.sleep(0)
            assert not task.done() and h.engine._in_flight
            assert list(h.directories[0].glob("*.mp4"))
            h.release_upload.set()
        with pytest.raises(asyncio.CancelledError):
            await asyncio.wait_for(task, 2)
        assert not h.engine._in_flight
        assert all(not path.exists() for path in h.directories)
    finally:
        h.release_receive.set()
        h.release_upload.set()
        await asyncio.gather(task, return_exceptions=True)


@pytest.mark.asyncio
@pytest.mark.parametrize("phase", ["receive", "upload"])
async def test_video_sync_output_disconnect_cleans_internal_job(harness, phase):
    h = harness
    h.pause_receive = phase == "receive"
    h.pause_upload = phase == "upload"
    entered = h.receiving if phase == "receive" else h.uploading
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
    try:
        await asyncio.wait_for(entered.wait(), 2)
        messages.put_nowait({"type": "http.disconnect"})
        if phase == "upload":
            await asyncio.sleep(0)
            await asyncio.sleep(0)
            assert not task.done() and h.engine._in_flight
            assert list(h.directories[0].glob("*.mp4"))
            h.release_upload.set()
        with pytest.raises(ClientError, match="Client disconnected"):
            await asyncio.wait_for(task, 2)
        assert h.deleted == ["/v1/videos/video_gen_1"]
        assert not h.engine._in_flight
        assert all(not path.exists() for path in h.directories)
    finally:
        h.release_receive.set()
        h.release_upload.set()
        await asyncio.gather(task, return_exceptions=True)


@pytest.mark.asyncio
@pytest.mark.parametrize("output", [None, "null"])
async def test_video_sync_without_output_keeps_native_response(harness, output):
    h = harness
    h.set_form(output)
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 200 and response.content == VIDEO
    assert not h.objects and not h.directories
    assert h.forwarded[0][2] == (h.body if output is None else h.native_body)


@pytest.mark.asyncio
@pytest.mark.parametrize("output_first", [False, True])
async def test_video_sync_output_position_and_chunked_upload(harness, output_first):
    h = harness
    # Include bytes resembling a boundary inside a file; they are media, not framing.
    h.set_form(
        extra=[
            (
                "input_references",
                ("third.png", b"binary\r\n--almost-a-boundary\x00\xff"),
            )
        ],
        output_first=output_first,
    )
    h.headers.pop("content-length")

    async def chunks():
        for offset in range(0, len(h.body), 13):
            yield h.body[offset : offset + 13]

    response = await h.client.post(PATH, content=chunks(), headers=h.headers)
    assert response.status_code == 200, response.text
    assert h.forwarded[0][2] == h.native_body


@pytest.mark.asyncio
async def test_video_sync_incomplete_form_never_reaches_engine(harness):
    h = harness
    h.body = h.body[:-10]
    h.headers["content-length"] = str(len(h.body))
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 400, response.text
    assert not h.forwarded and not h.objects


@pytest.mark.asyncio
async def test_video_sync_cancelled_form_upload_closes_spooled_files(harness):
    h = harness
    entered = asyncio.Event()
    release = asyncio.Event()

    async def chunks():
        yield h.body[:100]
        entered.set()
        await release.wait()
        yield h.body[100:]

    task = asyncio.create_task(h.client.post(PATH, content=chunks(), headers=h.headers))
    try:
        await asyncio.wait_for(entered.wait(), 2)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await asyncio.wait_for(task, 2)
        assert not h.forwarded and not h.engine._in_flight
        assert h.request_files and all(file.closed for file in h.request_files)
    finally:
        release.set()
        await asyncio.gather(task, return_exceptions=True)

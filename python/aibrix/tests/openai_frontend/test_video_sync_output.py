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
        upload_chunks=[],
        deleted=[],
        streams=[],
        directories=[],
        objects={},
        forwarding=asyncio.Event(),
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

    def no_upload_spooling(*args, **kwargs):
        pytest.fail("Input uploads must stream without temporary files")

    monkeypatch.setattr(tempfile, "TemporaryDirectory", directory)
    monkeypatch.setattr(tempfile, "TemporaryFile", no_upload_spooling)

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
                    request.url.path
                    == f"/v1/videos/video_gen_{len(h.forwarded)}/content"
                )
            stream = Body()
            h.streams.append(stream)
            return httpx.Response(h.status, headers=h.response_headers, stream=stream)

    h.engine._subprocess._client = httpx.AsyncClient(
        transport=Transport(), base_url="http://inner"
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
            h.release_receive.set()
            h.release_upload.set()
            await h.engine.aclose()
    assert not h.engine._in_flight
    assert not h.engine._request_done
    assert not h.engine._stream_closers
    assert all(stream.closed for stream in h.streams)
    assert all(not path.exists() for path in h.directories)
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
    assert path == PATH.replace("/sync", "").encode() and body == h.body
    assert headers["content-type"] == h.headers["content-type"]
    assert headers["content-length"] == h.headers["content-length"]
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
        assert len(h.objects) == 2
    finally:
        h.release_upload.set()
        await asyncio.gather(task, return_exceptions=True)


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "model_part, expected_model",
    [
        ((None, "模型/测试"), "模型/测试"),
        ((None, ""), ""),
        (("model.txt", b"uploaded-model"), None),
        ((None, b"\xff"), None),
        ((None, "x" * (64 * 1024 + 1)), None),
    ],
    ids=["duplicate", "empty", "file", "non-utf8", "oversized"],
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
@pytest.mark.parametrize("cancel", [False, True])
async def test_video_sync_output_streams_before_upload_finishes(
    harness, output_first, cancel
):
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
        assert not task.done() and not h.forwarded and not h.directories
        if cancel:
            task.cancel()
            with pytest.raises(asyncio.CancelledError):
                await asyncio.wait_for(task, 2)
            assert not h.objects and not h.deleted
        else:
            release.set()
            response = await asyncio.wait_for(task, 2)
            assert response.status_code == 200, response.text
            (artifact,) = response.json()["output"]["artifacts"]
            assert h.objects == {f"run/{artifact}": (VIDEO, "video/mp4")}
            assert h.forwarded[0][2] == h.body
            assert "content-length" not in h.forwarded[0][1]
        assert not h.engine._in_flight
    finally:
        release.set()
        task.cancel()
        await asyncio.gather(task, return_exceptions=True)


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "output",
    ["", "{", "{}", "[]", '{"url":"ftp://host/out"}', "x" * (64 * 1024 + 1)],
    ids=["empty", "json", "missing-url", "array", "scheme", "oversized"],
)
async def test_video_sync_validates_output_after_forwarding(harness, output):
    h = harness
    h.set_form(output)
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 400, response.text
    assert h.forwarded[0][2] == h.body
    assert not h.objects and not h.directories


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
@pytest.mark.parametrize(
    "failure", ["upstream", "type", "empty", "truncated", "read", "upload", "bucket"]
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
            h.release_upload.set()
        with pytest.raises(ClientError, match="Client disconnected"):
            await asyncio.wait_for(task, 2)
        assert h.deleted == ["/v1/videos/video_gen_1"]
        assert not h.engine._in_flight
    finally:
        h.release_receive.set()
        h.release_upload.set()
        await asyncio.gather(task, return_exceptions=True)

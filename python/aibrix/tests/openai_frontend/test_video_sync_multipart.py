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

"""Native multipart forwarding without preflight validation or upload spooling."""

import asyncio
import gzip
import tempfile
from types import SimpleNamespace
from unittest.mock import AsyncMock

import httpx
import pytest
import pytest_asyncio

from aibrix.openai_frontend.engine.proxy_engine import EngineState, ProxyEngine
from aibrix.openai_frontend.frontend.fastapi_frontend import FastApiFrontend
from aibrix.openai_frontend.proxy.runtime.engine_spec import VLLM_OMNI_SPEC

PATH = "/v1/videos/sync?flag=a%2Fb&flag=c"
VIDEO = b"\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom\x00\xffvideo"


@pytest_asyncio.fixture
async def harness(monkeypatch):
    h = SimpleNamespace(
        content=VIDEO,
        status=200,
        response_headers={"content-type": "video/mp4"},
        forwarded=[],
        chunks=[],
        deleted=[],
        streams=[],
        receiving=asyncio.Event(),
    )
    h.engine = ProxyEngine(["unused"], spec=VLLM_OMNI_SPEC, fetch_media=False)
    h.engine._set_state(EngineState.READY)
    h.engine._write_output = AsyncMock(
        side_effect=AssertionError("Multipart requests must not persist output")
    )

    def no_spooling(*args, **kwargs):
        pytest.fail("Multipart requests must not spool uploads or responses")

    monkeypatch.setattr(tempfile, "TemporaryFile", no_spooling)
    monkeypatch.setattr(tempfile, "TemporaryDirectory", no_spooling)

    class Body(httpx.AsyncByteStream):
        closed = False

        async def __aiter__(self):
            for offset in range(0, len(h.content), 7):
                yield h.content[offset : offset + 7]

        async def aclose(self):
            self.closed = True

    class Transport(httpx.AsyncBaseTransport):
        async def handle_async_request(self, request):
            if request.method == "POST":
                chunks = []
                # Unlike MockTransport, observe bytes before upload completion.
                async for chunk in request.stream:
                    chunks.append(chunk)
                    h.chunks.append(chunk)
                    h.receiving.set()
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
        output=None,
        *,
        extra=(),
        output_first=False,
        model="minimax-h3",
    ):
        native_parts = [
            *([] if model is None else [("model", (None, model))]),
            ("prompt", (None, "图1、图2按顺序生成视频。")),
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
            await h.engine.aclose()
    assert not h.engine._in_flight
    assert not h.engine._request_done
    assert not h.engine._stream_closers
    h.engine._write_output.assert_not_awaited()
    assert all(stream.closed for stream in h.streams)
    assert len(h.deleted) == (len(h.forwarded) if h.status == 200 else 0)


@pytest.mark.asyncio
@pytest.mark.parametrize("output", [None, "null"])
@pytest.mark.parametrize("encoded", [False, True])
@pytest.mark.parametrize("chunk_size", [None, 7])
async def test_video_sync_preserves_form_and_native_response(
    harness, output, encoded, chunk_size
):
    h = harness
    h.set_form(output)
    if encoded:
        h.content = gzip.compress(VIDEO)
        h.response_headers["content-encoding"] = "gzip"

    async def chunks():
        for offset in range(0, len(h.body), chunk_size):
            yield h.body[offset : offset + chunk_size]

    response = await h.client.post(
        PATH, content=h.body if chunk_size is None else chunks(), headers=h.headers
    )
    assert response.status_code == 200 and response.content == VIDEO
    assert response.headers["content-type"] == "video/mp4"
    assert response.headers.get("content-encoding") == ("gzip" if encoded else None)
    ((path, headers, body),) = h.forwarded
    assert path == PATH.replace("/sync", "").encode() and body == h.body
    assert headers["content-type"] == h.headers["content-type"]
    assert headers["content-length"] == h.headers["content-length"]


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "output, extra",
    [
        ("", []),
        ("{", []),
        ("{}", []),
        ("[]", []),
        ('{"url":"ftp://host/out"}', []),
        ('{"url":"tos://other/run","unknown":true}', []),
        ("x" * (64 * 1024 + 1), []),
        ("null", [("output", (None, '{"url":"tos://bucket/other"}'))]),
        (None, [("output", ("spec.json", b"not-json", "application/json"))]),
        ('{"url":"tos://bucket/run"}', [("stream", (None, "true"))]),
    ],
    ids=[
        "empty",
        "invalid-json",
        "missing-url",
        "array",
        "scheme",
        "extra",
        "oversized",
        "duplicate",
        "file",
        "stream",
    ],
)
async def test_video_sync_native_error_precedes_output_validation(
    harness, output, extra
):
    h = harness
    h.set_form(output, extra=extra)
    h.status = 422
    h.content = b'{"detail":"Invalid model input"}'
    h.response_headers["content-type"] = "application/json"
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 422 and response.content == h.content
    assert h.forwarded[0][2] == h.body


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "model_part",
    [
        (None, "last-model"),
        ("model.txt", b"uploaded-model"),
        (None, b"\xff"),
        (None, "x" * (64 * 1024 + 1)),
    ],
    ids=["duplicate", "file", "non-utf8", "oversized"],
)
async def test_video_sync_preserves_native_model_fields(harness, model_part):
    h = harness
    h.set_form(extra=[("model", model_part)])
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 200 and response.content == VIDEO
    assert h.forwarded[0][2] == h.body


@pytest.mark.asyncio
@pytest.mark.parametrize("status", [400, 422, 500])
@pytest.mark.parametrize("content_type", ["application/problem+json", "text/plain"])
async def test_video_sync_preserves_engine_validation_error(
    harness, status, content_type
):
    h = harness
    h.set_form("{")
    h.status = status
    h.content = b'{"detail":"Invalid video form"}'
    h.response_headers["content-type"] = content_type
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == status and response.content == h.content
    expected_type = (
        "text/plain; charset=utf-8" if content_type == "text/plain" else content_type
    )
    assert response.headers["content-type"] == expected_type
    assert h.forwarded[0][2] == h.body


@pytest.mark.asyncio
@pytest.mark.parametrize("invalid", ["incomplete", "missing-boundary"])
async def test_video_sync_forwards_malformed_form_for_engine_to_reject(
    harness, invalid
):
    h = harness
    if invalid == "incomplete":
        h.body = h.body[:-10]
        h.headers["content-length"] = str(len(h.body))
    else:
        h.headers["content-type"] = "multipart/form-data"
    h.status = 400
    h.content = b'{"detail":"Malformed multipart form"}'
    h.response_headers["content-type"] = "application/json"
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 400 and response.content == h.content
    assert h.forwarded[0][2] == h.body
    assert h.forwarded[0][1]["content-type"] == h.headers["content-type"]


@pytest.mark.asyncio
@pytest.mark.parametrize("output_first", [False, True])
@pytest.mark.parametrize("cancel", [False, True])
async def test_video_sync_streams_before_upload_finishes(harness, output_first, cancel):
    h = harness
    h.set_form("null", output_first=output_first)
    h.headers.pop("content-length")
    release = asyncio.Event()

    async def chunks():
        yield h.body[:100]
        await release.wait()
        yield h.body[100:]

    task = asyncio.create_task(h.client.post(PATH, content=chunks(), headers=h.headers))
    try:
        await asyncio.wait_for(h.receiving.wait(), 2)
        assert h.chunks == [h.body[:100]]
        assert not task.done() and not h.forwarded and h.engine._in_flight
        if cancel:
            task.cancel()
            with pytest.raises(asyncio.CancelledError):
                await asyncio.wait_for(task, 2)
        else:
            release.set()
            response = await asyncio.wait_for(task, 2)
            assert response.status_code == 200 and response.content == VIDEO
            ((path, headers, body),) = h.forwarded
            assert path == PATH.replace("/sync", "").encode() and body == h.body
            assert "content-length" not in headers
            assert headers["transfer-encoding"] == "chunked"
        assert not h.engine._in_flight
    finally:
        release.set()
        task.cancel()
        await asyncio.gather(task, return_exceptions=True)

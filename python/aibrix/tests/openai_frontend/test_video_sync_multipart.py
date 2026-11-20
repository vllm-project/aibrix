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

"""Native multipart compatibility and engine error forwarding."""

from types import SimpleNamespace

import httpx
import pytest
import pytest_asyncio

from aibrix.openai_frontend.engine.proxy_engine import EngineState, ProxyEngine
from aibrix.openai_frontend.frontend.fastapi_frontend import FastApiFrontend
from aibrix.openai_frontend.proxy.runtime.engine_spec import VLLM_OMNI_SPEC

PATH = "/v1/videos/sync?flag=a%2Fb&flag=c"
VIDEO = b"\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom\x00\xffvideo"


@pytest_asyncio.fixture
async def harness():
    h = SimpleNamespace(
        content=VIDEO,
        status=200,
        response_headers={"content-type": "video/mp4"},
        forwarded=[],
        deleted=[],
        streams=[],
    )
    h.engine = ProxyEngine(["unused"], spec=VLLM_OMNI_SPEC, fetch_media=False)
    h.engine._set_state(EngineState.READY)

    class Body(httpx.AsyncByteStream):
        closed = False

        async def __aiter__(self):
            for offset in range(0, len(h.content), 7):
                yield h.content[offset : offset + 7]

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
                        "id": "video_gen_1",
                        "status": "completed",
                        "model": "engine-model",
                        "inference_time_s": 1.25,
                    },
                )
        elif request.method == "DELETE":
            h.deleted.append(request.url.path)
            return httpx.Response(200, json={"deleted": True})
        else:
            assert request.url.path == "/v1/videos/video_gen_1/content"
        stream = Body()
        h.streams.append(stream)
        return httpx.Response(h.status, headers=h.response_headers, stream=stream)

    h.engine._subprocess._client = httpx.AsyncClient(
        transport=httpx.MockTransport(upstream), base_url="http://inner"
    )

    def set_form(output=None):
        native_parts = [
            ("model", (None, "minimax-h3")),
            ("prompt", (None, "图1、图2按顺序生成视频。")),
            ("input_references", ("first.png", b"first", "image/png")),
            (
                "input_references",
                ("second.png", b"binary\r\n--almost-a-boundary\x00\xff", "image/png"),
            ),
        ]
        output_parts = [] if output is None else [("output", (None, output))]
        upload = httpx.Request(
            "POST",
            f"http://frontend{PATH}",
            files=[*native_parts, *output_parts],
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
    assert all(stream.closed for stream in h.streams)
    assert len(h.deleted) == (len(h.forwarded) if h.status == 200 else 0)


@pytest.mark.asyncio
@pytest.mark.parametrize("output", [None, "null"])
async def test_video_sync_preserves_form_and_native_response(harness, output):
    h = harness
    h.set_form(output)
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 200 and response.content == VIDEO
    assert response.headers["content-type"] == "video/mp4"
    ((path, headers, body),) = h.forwarded
    assert path == PATH.replace("/sync", "").encode() and body == h.body
    assert headers["content-type"] == h.headers["content-type"]
    assert headers["content-length"] == h.headers["content-length"]


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "status, content_type",
    [
        (422, "application/problem+json"),
        (500, "text/plain"),
    ],
)
async def test_video_sync_native_error_precedes_output_validation(
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

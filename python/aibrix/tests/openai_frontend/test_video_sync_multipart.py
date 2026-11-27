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
        forwarded=None,
        deleted=[],
    )
    h.engine = ProxyEngine(["unused"], spec=VLLM_OMNI_SPEC, fetch_media=False)
    h.engine._set_state(EngineState.READY)

    async def upstream(request):
        if request.method == "POST":
            h.forwarded = request
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
        return httpx.Response(
            h.status,
            content=h.content,
            headers={
                "content-type": ("video/mp4" if h.status == 200 else "application/json")
            },
        )

    h.engine._subprocess._client = httpx.AsyncClient(
        transport=httpx.MockTransport(upstream), base_url="http://inner"
    )

    def set_form(output="null"):
        upload = httpx.Request(
            "POST",
            f"http://frontend{PATH}",
            files=[
                ("model", (None, "minimax-h3")),
                ("input_references", ("first.png", b"image\x00\xff", "image/png")),
                ("output", (None, output)),
            ],
        )
        h.body = upload.read()
        h.headers = {
            "content-type": upload.headers["content-type"],
        }
        h.expected = httpx.Request(
            "POST",
            f"http://frontend{PATH}",
            headers=h.headers,
            files=[
                ("model", (None, "minimax-h3")),
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
            assert h.deleted == (["/v1/videos/video_gen_1"] if h.status == 200 else [])
        finally:
            await h.engine.aclose()


@pytest.mark.asyncio
async def test_video_sync_null_output_returns_native_mp4(harness):
    h = harness
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 200 and response.content == VIDEO
    assert response.headers["content-type"] == "video/mp4"
    assert h.forwarded.content == h.expected


@pytest.mark.asyncio
async def test_video_sync_native_error_precedes_output_validation(harness):
    h = harness
    h.set_form("{")
    h.status = 422
    h.content = b'{"detail":"Invalid video form"}'
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 422 and response.content == h.content
    assert response.headers["content-type"] == "application/json"
    assert h.forwarded.content == h.expected
    assert h.forwarded.headers["content-type"] == h.headers["content-type"]


@pytest.mark.asyncio
async def test_video_sync_rejects_missing_boundary_before_forwarding(harness):
    h = harness
    h.headers["content-type"] = "multipart/form-data"
    h.status = 422
    response = await h.client.post(PATH, content=h.body, headers=h.headers)
    assert response.status_code == 400, response.text
    assert h.forwarded is None

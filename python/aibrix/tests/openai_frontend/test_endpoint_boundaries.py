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

"""Boundary regressions for newly exposed native endpoints."""

import asyncio
import gzip
import json
from unittest.mock import AsyncMock

import httpcore
import httpx
import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from httpcore._async.connection_pool import AsyncPoolRequest, PoolByteStream
from pydantic import BaseModel
from starlette.requests import Request
from starlette.websockets import WebSocketDisconnect

from aibrix.openai_frontend.engine.proxy_engine import EngineState, ProxyEngine
from aibrix.openai_frontend.frontend.fastapi_frontend import FastApiFrontend
from aibrix.openai_frontend.proxy.runtime.engine_spec import (
    SGLANG_OMNI_SPEC,
    SGLANG_SPEC,
    VLLM_OMNI_SPEC,
)
from aibrix.openai_frontend.schemas.responses import (
    CreateResponseRequest,
    ResponseCompactParams,
)


@pytest.mark.asyncio
@pytest.mark.parametrize("kind", ["video_inputs", "video_sources", "response_inputs"])
async def test_retained_media_overlapping_cleanup(monkeypatch, kind):
    from aibrix.openai_frontend.engine.proxy_engine import (
        _RetainedVideoInputs,
        _RetainedVideoSources,
    )
    from aibrix.openai_frontend.schemas.output import OutputRequest

    engine = ProxyEngine(["unused"], fetch_media=False)
    entered, release = asyncio.Event(), asyncio.Event()
    cleaned = []
    source_ids = {}
    for req_id in ("a", "b"):
        if kind == "video_inputs":
            engine._retained.retain_video_inputs(
                req_id, _RetainedVideoInputs("job", [])
            )
        elif kind == "video_sources":
            entry = _RetainedVideoSources(OutputRequest(url="/tmp/out"), {})
            engine._retained.retain_video_sources(req_id, entry)
            source_ids[id(entry.identities)] = req_id
        else:
            engine._retained.retain_response_inputs(req_id, [])

    async def cleanup(req_id, inputs=None):
        cleaned.append(req_id)
        if req_id == "a":
            entered.set()
            await release.wait()

    async def cleanup_sources(identities, output):
        await cleanup(source_ids[id(identities)])

    monkeypatch.setattr(engine, "_release", cleanup)
    monkeypatch.setattr(
        engine._output_writer, "cleanup_completed_sources", cleanup_sources
    )
    first = asyncio.create_task(engine._release_retained_resources())
    try:
        await asyncio.wait_for(entered.wait(), 1)
        # Only a has transferred: b must remain available to another releaser.
        assert engine._retained.pending() and cleaned == ["a"]
        await asyncio.wait_for(engine._release_retained_resources(), 1)
        assert cleaned == ["a", "b"] and not engine._retained.pending()
        assert engine._request_done  # a's real cleanup has not finished yet.
        release.set()
        await asyncio.wait_for(first, 1)
        assert cleaned == ["a", "b"] and not engine._request_done
    finally:
        release.set()
        await asyncio.gather(first, return_exceptions=True)
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize("operation", ["create", "compact"])
async def test_responses_output_receipt(tmp_path, operation):
    engine = ProxyEngine(["unused"], fetch_media=False)
    engine._set_state(EngineState.READY)
    items = [
        {"type": "message", "content": [{"type": "output_text", "text": "hello"}]},
        {"type": "function_call", "name": "lookup", "arguments": "{}"},
    ]
    body = {
        "id": "resp_test",
        "object": "response" if operation == "create" else "response.compaction",
        "created_at": 1,
        "model": "m",
        "output": items,
        "vendor": {"keep": None},
    }
    upstream = AsyncMock(
        return_value=httpx.Response(
            200, json=body, request=httpx.Request("POST", "http://inner")
        )
    )
    engine._subprocess.forward = upstream
    model = CreateResponseRequest if operation == "create" else ResponseCompactParams
    try:
        result = await engine.responses(
            operation, model(model="m", input="hi", output={"url": str(tmp_path)})
        )
        receipt = json.loads(result.body)
        assert receipt == {**body, "output": receipt["output"]}
        assert json.loads(
            (tmp_path / receipt["output"]["artifacts"][0]).read_bytes()
        ) == {"output": items}
        assert "output" not in upstream.call_args.kwargs["json"]
        assert upstream.await_count == 1
        assert not engine._request_done
    finally:
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize("invalid", ["destination", "stream", "background"])
async def test_responses_output_rejected_before_forwarding(tmp_path, invalid):
    from aibrix.openai_frontend.utils.utils import ClientError

    engine = ProxyEngine(["unused"], fetch_media=False)
    engine._set_state(EngineState.READY)
    upstream = AsyncMock()
    engine._subprocess.forward = upstream
    kwargs = {
        "output": {"url": "ftp://bad" if invalid == "destination" else str(tmp_path)}
    }
    if invalid != "destination":
        kwargs[invalid] = True
    try:
        with pytest.raises(ClientError):
            await engine.responses(
                "create", CreateResponseRequest(input="hi", **kwargs)
            )
        upstream.assert_not_awaited()
        assert not engine._request_done and not list(tmp_path.iterdir())
    finally:
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize("status_code", [200, 422])
async def test_vllm_omni_video_sync_preserves_multipart_and_response(status_code):
    engine = ProxyEngine(["unused"], spec=VLLM_OMNI_SPEC, fetch_media=False)
    engine._set_state(EngineState.READY)
    path = "/v1/videos/sync?response_format=json&flag=a%2Fb&flag=c"
    fields = {
        "model": "minimax-h3",
        "prompt": "参考图1与参考图2，生成视频。",
        "short_edge": "768",
        "aspect_ratio": "auto",
        "num_inference_steps": "1",
        "flow_shift": "12.0",
        "quality": "lossless",
        "seed": "0",
        "extra_params": '{"task":"ref2va","duration":5,"audio_flow_shift":3.0,'
        '"modalities":["video"],"stream":false}',
    }
    upload = httpx.Request(
        "POST",
        f"http://frontend{path}",
        files=[
            *((name, (None, value)) for name, value in fields.items()),
            ("input_references", ("first.png", b"\x89PNG\r\n\x1a\nfirst", "image/png")),
            (
                "input_references",
                ("second.png", b"\x89PNG\r\n\x1a\nsecond", "image/png"),
            ),
        ],
    )
    body = upload.read()
    content = (
        b"\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom\x00\xffvideo"
        if status_code == 200
        else b'{"detail":[{"loc":["body","model"],"msg":"Unknown model"}]}'
    )
    content_type = "video/mp4" if status_code == 200 else "application/json"

    class Body(httpx.AsyncByteStream):
        closed = False

        async def __aiter__(self):
            for offset in range(0, len(content), 7):
                yield content[offset : offset + 7]

        async def aclose(self):
            self.closed = True

    stream = Body()
    forwarded = []

    async def upstream(request):
        forwarded.append(
            (
                request.method,
                request.url.raw_path,
                request.headers["content-type"],
                request.headers["content-length"],
                await request.aread(),
            )
        )
        return httpx.Response(
            status_code,
            stream=stream,
            headers={
                "content-type": content_type,
                "content-disposition": 'attachment; filename="video.mp4"',
            },
        )

    engine._subprocess._client = httpx.AsyncClient(
        transport=httpx.MockTransport(upstream), base_url="http://inner"
    )
    app = FastApiFrontend(
        engine, restricted_apis=[["inference", "x-test-key", "expected"]]
    ).app
    try:
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app), base_url="http://frontend"
        ) as client:
            headers = {
                "content-type": upload.headers["content-type"],
                "content-length": upload.headers["content-length"],
            }
            denied = await client.post(path, content=body, headers=headers)
            assert denied.status_code == 401 and not forwarded
            headers["x-test-key"] = "expected"

            async def chunks():
                for offset in range(0, len(body), 64):
                    yield body[offset : offset + 64]

            response = await client.post(path, content=chunks(), headers=headers)
            assert response.status_code == status_code, response.text
            assert response.content == content
            assert response.headers["content-type"] == content_type
            if status_code == 200:
                assert (
                    response.headers["content-disposition"]
                    == 'attachment; filename="video.mp4"'
                )
            assert forwarded == [
                (
                    "POST",
                    path.encode(),
                    upload.headers["content-type"],
                    upload.headers["content-length"],
                    body,
                )
            ]
            assert stream.closed and not engine._request_done
            assert not engine._stream_closers
    finally:
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize("buffered", [False, True])
async def test_native_stream_preserves_encoding_query_and_shutdown(buffered):
    engine = ProxyEngine(["unused"], spec=VLLM_OMNI_SPEC, fetch_media=False)
    engine._set_state(EngineState.READY)
    content = gzip.compress(b'data: {"delta":"hello"}\n\n')

    class Body(httpx.AsyncByteStream):
        closed = False

        async def __aiter__(self):
            yield content

        async def aclose(self):
            self.closed = True

    stream = Body()
    raw = httpx.Response(
        200,
        stream=stream,
        headers={
            "Content-Type": "text/event-stream",
            "Content-Encoding": "gzip",
            "Connection": "X-Hop",
            "X-Hop": "drop",
        },
        request=httpx.Request("POST", "http://inner/v1/messages"),
    )
    if buffered:
        await raw.aread()
    engine._subprocess.forward = AsyncMock(return_value=raw)
    try:
        result = await engine.forward_json(
            "/v1/messages", {"stream": True}, query="flag=a%2Fb&flag=c"
        )
        assert (
            engine._subprocess.forward.call_args.args[1]
            == "/v1/messages?flag=a%2Fb&flag=c"
        )
        assert "x-hop" not in result.headers
        assert result.headers.get("content-encoding") == (None if buffered else "gzip")
        payload = b"".join([chunk async for chunk in result.body_iterator])
        assert payload == (gzip.decompress(content) if buffered else content)
        assert stream.closed and not engine._request_done
        raw = httpx.Response(
            200,
            stream=Body(),
            request=httpx.Request("POST", "http://inner/v1/messages"),
        )
        engine._subprocess.forward.return_value = raw
        result = await engine.forward_json("/v1/messages", {"stream": True})
        assert engine._stream_closers and not engine._in_flight_tasks
        await asyncio.wait_for(engine.aclose(), 2)
        assert raw.is_closed and not engine._request_done
    finally:
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "mode", ["native", "multipart", "responses-create", "responses-retrieve"]
)
@pytest.mark.parametrize("read_error", [False, True], ids=["eof", "read-error"])
@pytest.mark.parametrize("status_code", [200, 400])
async def test_stream_close_survives_shutdown_and_repeated_cancel(
    mode, read_error, status_code
):
    entered, release = asyncio.Event(), asyncio.Event()

    class Body(httpx.AsyncByteStream):
        closed = False
        close_calls = 0

        async def __aiter__(self):
            yield b"data: [DONE]\n\n"
            if read_error:
                raise httpcore.ReadError("upstream read failed")

        async def aclose(self):
            self.close_calls += 1
            entered.set()
            await release.wait()
            self.closed = True

    body = Body()
    pool = httpcore.AsyncConnectionPool()
    pool_request = AsyncPoolRequest(httpcore.Request("POST", "http://inner"))
    pool._requests.append(pool_request)
    # Use httpcore's real exception-time auto-close, not a plain mock stream.
    pooled = PoolByteStream(body, pool_request, pool)

    class TransportBody(httpx.AsyncByteStream):
        async def __aiter__(self):
            async for chunk in pooled:
                yield chunk

        async def aclose(self):
            await pooled.aclose()

    engine = ProxyEngine(["unused"], spec=VLLM_OMNI_SPEC, fetch_media=False)
    engine._set_state(EngineState.READY)

    async def upstream(request):
        await request.aread()
        return httpx.Response(status_code, stream=TransportBody())

    engine._subprocess._client = httpx.AsyncClient(
        transport=httpx.MockTransport(upstream), base_url="http://inner"
    )
    consumer = stopper = iterator = None
    try:

        async def consume():
            nonlocal iterator
            if mode == "multipart":

                async def receive():
                    return {
                        "type": "http.request",
                        "body": b"--b--\r\n",
                        "more_body": False,
                    }

                request = Request(
                    {
                        "type": "http",
                        "method": "POST",
                        "scheme": "http",
                        "server": ("frontend", 80),
                        "path": "/v1/audio/transcriptions",
                        "query_string": b"",
                        "headers": [
                            (b"content-type", b"multipart/form-data; boundary=b")
                        ],
                    },
                    receive,
                )
                response = await engine.forward_multipart(request.url.path, request)
                iterator = response.body_iterator
            elif mode == "native":
                response = await engine.forward_json("/v1/messages", {"stream": True})
                iterator = response.body_iterator
            elif mode == "responses-create":
                iterator = await engine.responses(
                    "create",
                    CreateResponseRequest(input="hi", store=False, stream=True),
                )
            else:
                iterator = await engine.responses(
                    "retrieve", response_id="resp_test", query=[("stream", "true")]
                )
            async for _ in iterator:
                pass

        consumer = asyncio.create_task(consume())
        await asyncio.wait_for(entered.wait(), 1)
        consumer.cancel()
        await asyncio.sleep(0)
        consumer.cancel("repeat cancellation while closing")
        # A cancelled reader may wait for shielded transport cleanup to finish.
        stopper = asyncio.create_task(engine.aclose())
        await asyncio.sleep(0)
        await asyncio.sleep(0)
        assert not stopper.done() and engine._request_done
        assert not body.closed and body.close_calls == 1
        release.set()
        await asyncio.wait_for(stopper, 1)
        await asyncio.gather(consumer, return_exceptions=True)
        if iterator is not None:
            await iterator.aclose()
        assert body.closed and body.close_calls == 1
        assert not pool._requests
        assert not engine._request_done and not engine._stream_closers
    finally:
        release.set()
        if consumer is not None:
            await asyncio.gather(consumer, return_exceptions=True)
        if stopper is not None:
            await stopper
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "path,result",
    [
        ("/generate", {"text": "hello", "output_ids": [1]}),
        ("/encode", {"embedding": [0.5]}),
        ("/classify", {"embedding": [0.5]}),
        ("/v1/rerank", [{"index": 0, "score": 0.5}]),
    ],
)
async def test_sglang_native_methods_preserve_auth_and_output(tmp_path, path, result):
    for spec in (SGLANG_SPEC, SGLANG_OMNI_SPEC):
        assert spec.methods(path) == ("POST", "PUT")
    engine = ProxyEngine(["unused"], spec=SGLANG_SPEC, fetch_media=False)
    engine._set_state(EngineState.READY)
    forwarded = []

    async def upstream(request):
        forwarded.append(
            (request.method, str(request.url), json.loads(await request.aread()))
        )
        return httpx.Response(200, json=result)

    engine._subprocess._client = httpx.AsyncClient(
        transport=httpx.MockTransport(upstream), base_url="http://inner"
    )
    engine.start = AsyncMock()
    app = FastApiFrontend(
        engine, restricted_apis=[["inference", "x-test-key", "expected"]]
    ).app
    try:
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app), base_url="http://frontend"
        ) as client:
            for method in ("POST", "PUT"):
                payload = {"text": "hello", "vendor": {"keep": None}}
                denied = await client.request(method, path, json=payload)
                assert denied.status_code == 401
                calls = len(forwarded)
                headers = {"x-test-key": "expected"}
                url = f"{path}?flag=a%2Fb&flag=c"
                plain = await client.request(method, url, json=payload, headers=headers)
                assert plain.status_code == 200 and plain.json() == result
                stored = await client.request(
                    method,
                    url,
                    json={**payload, "output": {"url": str(tmp_path)}},
                    headers=headers,
                )
                assert stored.status_code == 200, stored.text
                receipt = stored.json()["output"]
                assert (
                    json.loads((tmp_path / receipt["artifacts"][0]).read_bytes())
                    == result
                )
                assert (
                    forwarded[calls:] == [(method, f"http://inner{url}", payload)] * 2
                )
            assert not engine._request_done
    finally:
        await engine.aclose()


def test_sglang_put_is_not_enabled_on_other_paths():
    engine = ProxyEngine(["unused"], spec=SGLANG_SPEC, fetch_media=False)
    engine._set_state(EngineState.READY)
    engine._subprocess.forward = AsyncMock()
    engine.start = AsyncMock()
    try:
        with TestClient(FastApiFrontend(engine).app) as client:
            assert client.put("/v1/score", json={}).status_code == 405
        engine._subprocess.forward.assert_not_awaited()
        assert not engine._request_done
    finally:
        asyncio.run(engine.aclose())


@pytest.mark.asyncio
async def test_native_stream_bool_matches_upstream_and_output(tmp_path):
    from aibrix.openai_frontend.frontend.fastapi.errors import add_exception_handlers
    from aibrix.openai_frontend.frontend.fastapi.routers.engine_routes import (
        create_router,
    )

    class UpstreamRequest(BaseModel):
        stream: bool | None = False

    engine = ProxyEngine(["unused"], spec=VLLM_OMNI_SPEC, fetch_media=False)
    engine._set_state(EngineState.READY)
    forwarded = []

    async def upstream(request):
        payload = json.loads(await request.aread())
        forwarded.append(payload)
        stream = UpstreamRequest.model_validate(payload).stream
        return httpx.Response(
            200,
            content=b'data: {"delta":"ok"}\n\n'
            if stream
            else b'{"message":{"content":[]}}',
            headers={
                "content-type": "text/event-stream" if stream else "application/json"
            },
        )

    engine._subprocess._client = httpx.AsyncClient(
        transport=httpx.MockTransport(upstream), base_url="http://inner"
    )
    app = FastAPI()
    app.engine = engine
    app.include_router(create_router(engine))
    add_exception_handlers(app)
    try:
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app), base_url="http://frontend"
        ) as client:
            for value in (
                False,
                None,
                0,
                "false",
                "0",
                "off",
                "n",
                "NO",
                True,
                1,
                "true",
                "1",
                "yes",
                "on",
                "t",
                "Y",
            ):
                expected = bool(UpstreamRequest(stream=value).stream)
                request = {"stream": value, "vendor": {"keep": None}}
                plain = await client.post("/cohere/v2/chat", json=request)
                assert plain.status_code == 200
                assert plain.headers["content-type"] == (
                    "text/event-stream" if expected else "application/json"
                )
                assert forwarded[-1] == request
                calls = len(forwarded)
                result = await client.post(
                    "/cohere/v2/chat",
                    json={**request, "output": {"url": str(tmp_path)}},
                )
                assert result.status_code == (400 if expected else 200), result.text
                assert len(forwarded) == calls + (not expected)
                if not expected:
                    assert forwarded[-1] == request
                    receipt = result.json()["output"]
                    assert json.loads(
                        (tmp_path / receipt["artifacts"][0]).read_bytes()
                    ) == {"message": {"content": []}}
            calls = len(forwarded)
            invalid = await client.post("/cohere/v2/chat", json={"stream": "invalid"})
            assert invalid.status_code == 400 and len(forwarded) == calls
            assert not engine._request_done
    finally:
        await engine.aclose()


@pytest.mark.parametrize(
    "path,method",
    [
        ("/v1/chat/completions", "POST"),
        ("/v1/responses", "POST"),
        ("/v1/responses/r", "DELETE"),
        ("/v1/rerank", "POST"),
        ("/tokenizer_info", "GET"),
    ],
)
def test_new_http_routes_respect_inference_auth(path, method):
    engine = ProxyEngine(["unused"], spec=VLLM_OMNI_SPEC, fetch_media=False)
    engine._set_state(EngineState.READY)
    upstream = AsyncMock(
        return_value=httpx.Response(
            400,
            content=b'{"error":"upstream"}',
            request=httpx.Request(method, "http://inner"),
        )
    )
    engine._subprocess.forward = upstream
    engine.start = AsyncMock()
    app = FastApiFrontend(
        engine, restricted_apis=[["inference", "x-test-key", "expected"]]
    ).app
    try:
        with TestClient(app, raise_server_exceptions=False) as client:
            for origin, status in (
                ("http://localhost", 200),
                ("https://untrusted.test", 400),
            ):
                preflight = client.options(
                    path,
                    headers={
                        "origin": origin,
                        "access-control-request-method": method,
                        "access-control-request-headers": "x-test-key,content-type",
                    },
                )
                assert preflight.status_code == status
                assert preflight.headers.get("access-control-allow-origin") == (
                    origin if status == 200 else None
                )
            assert upstream.await_count == 0
            body = (
                {"model": "m", "messages": [{"role": "user", "content": "hi"}]}
                if path == "/v1/chat/completions"
                else {"input": "hi"}
            )
            response = client.request(method, path, json=body)
            assert response.status_code == 401
            assert upstream.await_count == 0
            response = client.request(
                method, path, json=body, headers={"x-test-key": "expected"}
            )
            assert response.status_code == 400
            assert upstream.await_count == 1
            if method == "GET":
                engine._fetch_media = True
                engine._fetch_media_for = AsyncMock(
                    side_effect=AssertionError("A bodiless GET must not inspect media")
                )
                forwarded = []

                async def get_upstream(request):
                    assert await request.aread() == b""
                    assert "content-type" not in request.headers
                    forwarded.append((request.method, request.url.raw_path))
                    return httpx.Response(
                        200,
                        content=b'{"tokenizer":"test"}',
                        headers={
                            "x-vendor": "keep",
                            "connection": "x-hop",
                            "x-hop": "drop",
                        },
                    )

                engine._subprocess._client = httpx.AsyncClient(
                    transport=httpx.MockTransport(get_upstream), base_url="http://inner"
                )
                del engine._subprocess.forward  # Use the real request-building path.
                response = client.get(
                    path + "?flag=a%2Fb&flag=c", headers={"x-test-key": "expected"}
                )
                assert response.status_code == 200
                assert response.content == b'{"tokenizer":"test"}'
                assert response.headers["content-type"] == "application/json"
                assert response.headers["x-vendor"] == "keep"
                assert "x-hop" not in response.headers
                assert forwarded == [("GET", (path + "?flag=a%2Fb&flag=c").encode())]
                engine._fetch_media_for.assert_not_awaited()
                assert not engine._request_done
    finally:
        asyncio.run(engine.aclose())


def test_realtime_respects_inference_auth(monkeypatch):
    engine = ProxyEngine(["unused"], spec=VLLM_OMNI_SPEC, fetch_media=False)
    engine._set_state(EngineState.READY)
    calls = []

    async def bridge(ws, *args, **kwargs):
        calls.append(True)
        await ws.accept()
        await ws.close(code=1000)

    monkeypatch.setattr(
        "aibrix.openai_frontend.engine.proxy_engine.bridge_websocket", bridge
    )
    engine.start = AsyncMock()
    app = FastApiFrontend(
        engine, restricted_apis=[["inference", "x-test-key", "expected"]]
    ).app
    try:
        with TestClient(app) as client:
            with pytest.raises((WebSocketDisconnect, httpx.HTTPStatusError)):
                with client.websocket_connect("/v1/realtime") as ws:
                    ws.receive_text()
            assert not calls
            with client.websocket_connect(
                "/v1/realtime", headers={"x-test-key": "expected"}
            ):
                pass
            assert calls == [True]
    finally:
        asyncio.run(engine.aclose())


@pytest.mark.asyncio
@pytest.mark.parametrize("cancel_at", ["waiting", "draining"])
async def test_websocket_cancel_drains_both_directions(monkeypatch, cancel_at):
    from aibrix.openai_frontend.proxy.runtime import ws_bridge

    entered, release = asyncio.Event(), asyncio.Event()

    async def first(*args):
        if cancel_at == "waiting":
            await asyncio.Event().wait()
        return "client", 1000, ""

    async def second(*args):
        try:
            await asyncio.Event().wait()
        finally:
            entered.set()
            await release.wait()

    monkeypatch.setattr(ws_bridge, "_client_to_engine", first)
    monkeypatch.setattr(ws_bridge, "_engine_to_client", second)
    ws, upstream = AsyncMock(), AsyncMock()
    ws.application_state = ws_bridge.WebSocketState.CONNECTED
    closing, finish_close = asyncio.Event(), asyncio.Event()

    async def slow_close(*args, **kwargs):
        closing.set()
        await finish_close.wait()

    ws.close.side_effect = slow_close
    task = asyncio.create_task(ws_bridge._pump(ws, upstream))
    try:
        if cancel_at == "draining":
            await asyncio.wait_for(entered.wait(), 1)
        else:
            await asyncio.sleep(0)
            await asyncio.sleep(0)
        task.cancel()
        if cancel_at == "draining":
            await asyncio.sleep(0)
            assert not task.done()
            release.set()
        await asyncio.wait_for(closing.wait(), 1)
        task.cancel("repeat cancellation during client close")
        await asyncio.sleep(0)
        assert not task.done()
        finish_close.set()
        await asyncio.wait_for(entered.wait(), 1)
        await asyncio.sleep(0)
        if cancel_at == "waiting":
            assert not task.done()
        release.set()
        with pytest.raises(asyncio.CancelledError):
            await asyncio.wait_for(task, 1)
        ws.close.assert_awaited_once_with(code=1012, reason="inner engine unavailable")
        upstream.close.assert_awaited_once_with(code=1012)
    finally:
        finish_close.set()
        release.set()
        await asyncio.gather(task, return_exceptions=True)

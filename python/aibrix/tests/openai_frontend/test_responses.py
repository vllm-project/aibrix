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

"""Contract/route checks for generated inference schemas and Responses forwarding."""

from __future__ import annotations

from unittest.mock import AsyncMock

import httpx
import pytest
from fastapi import FastAPI

from aibrix.openai_frontend.engine.proxy_engine import EngineState, ProxyEngine
from aibrix.openai_frontend.frontend.fastapi.errors import add_exception_handlers
from aibrix.openai_frontend.frontend.fastapi.routers.responses import router
from aibrix.openai_frontend.schemas.responses import CreateResponseRequest

RESPONSE = {
    "id": "resp_test",
    "object": "response",
    "created_at": 1.0,
    "model": "test",
    "output": [],
    "status": "completed",
    "vendor_extension": {"keep": True},
}


@pytest.mark.asyncio
async def test_responses_routes_forward_methods_queries_and_bodies():
    engine = ProxyEngine(engine_cmd=["unused"], fetch_media=False)
    engine._set_state(EngineState.READY)
    calls = []

    async def forward(method, endpoint, *, json=None, stream=False):
        calls.append((method, endpoint, json, stream))
        body = RESPONSE
        if method == "DELETE" and endpoint.endswith("/empty"):
            return httpx.Response(
                204, request=httpx.Request(method, "http://inner" + endpoint)
            )
        if method == "DELETE":
            body = {"id": "resp_test", "object": "response.deleted", "deleted": True}
        elif "/input_items" in endpoint:
            body = {"object": "list", "data": [], "has_more": False}
        elif endpoint.endswith("/input_tokens"):
            body = {"object": "response.input_tokens", "input_tokens": 7}
        elif endpoint.endswith("/compact"):
            body = {
                "id": "resp_test",
                "object": "response.compaction",
                "created_at": 1.0,
                "output": [],
                "usage": {
                    "input_tokens": 1,
                    "output_tokens": 1,
                    "total_tokens": 2,
                    "input_tokens_details": {
                        "cached_tokens": 0,
                        "cache_write_tokens": 0,
                    },
                    "output_tokens_details": {"reasoning_tokens": 0},
                },
            }
        return httpx.Response(
            200, json=body, request=httpx.Request(method, "http://inner" + endpoint)
        )

    engine._subprocess.forward = AsyncMock(side_effect=forward)
    app = FastAPI()
    app.engine = engine
    add_exception_handlers(app)
    app.include_router(router)
    async with httpx.AsyncClient(
        transport=httpx.ASGITransport(app), base_url="http://frontend"
    ) as client:
        for method, path, body in [
            (
                "POST",
                "/v1/responses",
                {"model": "test", "input": "hi", "vendor_request": 3},
            ),
            (
                "GET",
                "/v1/responses/resp_test?include=a&include=b&starting_after=2",
                None,
            ),
            ("DELETE", "/v1/responses/resp_test", None),
            ("POST", "/v1/responses/resp_test/cancel", None),
            (
                "GET",
                "/v1/responses/resp_test/input_items?after=item_1&limit=3&order=asc",
                None,
            ),
            ("POST", "/v1/responses/compact", {"model": "test", "input": "hi"}),
            ("POST", "/v1/responses/input_tokens", {"model": "test", "input": "hi"}),
        ]:
            if "include=a" in path:
                path = path.replace(
                    "include=a&include=b",
                    "include=reasoning.encrypted_content&include=file_search_call.results",
                )
            response = (
                await client.request(method, path, json=body)
                if body
                else await client.request(method, path)
            )
            assert response.status_code == 200, response.text
        assert calls[0][2] == {"model": "test", "input": "hi", "vendor_request": 3}
        assert calls[1][0] == "GET" and calls[1][2] is None
        assert calls[1][1].count("include=") == 2
        assert calls[2][0] == "DELETE" and calls[2][2] is None
        assert calls[3][0] == "POST" and calls[3][1].endswith("/cancel")
        assert (
            await client.get("/v1/responses/resp_test/input_items?limit=0")
        ).status_code == 422
        deleted = await client.delete("/v1/responses/empty")
        assert deleted.status_code == 204 and deleted.content == b""
        assert (await client.get("/not-a-route")).status_code == 404
    assert not engine._in_flight
    await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "mode,pre_output",
    [
        (mode, False)
        for mode in (
            "stored",
            "default",
            "ephemeral",
            "background",
            "stream",
            "rejected",
            "server_error",
            "connect_error",
            "cancelled",
            "shutdown",
        )
    ]
    + [
        (mode, True)
        for mode in (
            "stored",
            "default",
            "ephemeral",
            "rejected",
            "cancelled",
            "shutdown",
            "write_error",
        )
    ],
)
async def test_responses_media_outlives_stored_and_background_requests(
    tmp_path, mode, pre_output
):
    import asyncio
    import json
    from pathlib import Path

    from aibrix.openai_frontend.proxy.output.output_writer import OutputWriteError
    from aibrix.openai_frontend.proxy.processors import ProcessorRegistry
    from aibrix.openai_frontend.proxy.processors.demo_processors import DEMO_PROCESSORS

    engine = ProxyEngine(
        ["unused"],
        media_dir=str(tmp_path),
        materialize_inline_media=True,
        enable_processors=pre_output,
        processor_registry=ProcessorRegistry(DEMO_PROCESSORS),
    )
    engine._set_state(EngineState.READY)
    paths = []
    accepted = asyncio.Event()
    history = []

    async def forward(request):
        body = json.loads(await request.aread())
        if body.get("previous_response_id"):
            assert history
            assert (
                Path(
                    history[0]["content"][0]["image_url"].removeprefix("file://")
                ).read_bytes()
                == b"PIXELS"
            )
            return httpx.Response(200, json=RESPONSE)
        path = Path(body["input"][0]["content"][0]["image_url"].removeprefix("file://"))
        paths.append(path)
        assert path.read_bytes() == b"PIXELS"
        if mode == "connect_error":
            raise httpx.ConnectError("not submitted", request=request)
        accepted.set()
        if mode in ("cancelled", "shutdown"):
            await asyncio.Event().wait()
        if mode in ("rejected", "server_error"):
            return httpx.Response(
                400 if mode == "rejected" else 500, json={"error": "failed"}
            )
        history.extend(body["input"])
        if mode == "stream":
            return httpx.Response(
                200,
                content=b'event: response.created\ndata: {"type":"response.created"}\n\n',
            )
        return httpx.Response(
            200,
            json={
                **RESPONSE,
                "status": "queued" if mode == "background" else "completed",
            },
        )

    engine._subprocess._client = httpx.AsyncClient(
        transport=httpx.MockTransport(forward), base_url="http://inner"
    )
    app = FastAPI()
    app.engine = engine
    app.include_router(router)
    add_exception_handlers(app)
    body = {
        "input": [
            {
                "role": "user",
                "content": [
                    {
                        "type": "input_image",
                        "image_url": "data:image/png;base64,UElYRUxT",
                    }
                ],
            }
        ]
    }
    if mode != "default":
        body["store"] = mode not in ("ephemeral", "background")
    if mode == "background":
        body["background"] = True
    if mode == "stream":
        body["stream"] = True
    if pre_output:
        body["processors"] = {
            "pre": [{"type": "processor", "processor": {"name": "demo.pre.copy"}}]
        }
        body["output"] = {"url": str(tmp_path / "out")}
        if mode == "write_error":
            engine._output_writer._write_local_entries = AsyncMock(
                side_effect=OutputWriteError("injected write failure")
            )
    retained = mode not in ("ephemeral", "rejected", "connect_error")

    def shutdown():
        assert all(path.exists() == retained for path in paths)

    engine._subprocess.shutdown = shutdown
    try:
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app), base_url="http://frontend"
        ) as client:
            if mode in ("cancelled", "shutdown"):
                task = asyncio.create_task(client.post("/v1/responses", json=body))
                await asyncio.wait_for(accepted.wait(), 1)
                if mode == "shutdown":
                    await asyncio.wait_for(engine.aclose(), 2)
                    stopped = await task
                    assert stopped.status_code == 400
                    assert stopped.json() == {"detail": "Proxy engine is stopping"}
                    assert (
                        not engine._retained.response_inputs
                        and not engine._request_done
                    )
                    assert not list(tmp_path.iterdir())
                    return
                task.cancel()
                with pytest.raises(asyncio.CancelledError):
                    await task
            else:
                response = await client.post("/v1/responses", json=body)
                expected = (
                    400
                    if mode == "rejected"
                    else 500
                    if mode in ("server_error", "connect_error", "write_error")
                    else 200
                )
                assert response.status_code == expected, response.text
            assert paths and all(path.exists() == retained for path in paths)
            assert bool(engine._retained.response_inputs) == retained
            if pre_output:
                assert bool(engine._retained.processor_dirs) == retained
                assert all(path.parent.exists() == retained for path in paths)
            assert not engine._request_done
            if mode in ("stored", "default", "background", "stream", "write_error"):
                followup = await client.post(
                    "/v1/responses",
                    json={"input": "continue", "previous_response_id": "resp_test"},
                )
                assert followup.status_code == 200, followup.text

            await engine.aclose()
            assert not engine._retained.pending() and not engine._request_done
            assert not list(tmp_path.glob("aibrix-*"))
            assert not any(path.exists() for path in paths)
            if not pre_output:
                assert not list(tmp_path.iterdir())
    finally:
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize("retrieve", [False, True])
@pytest.mark.parametrize("buffered", [False, True])
async def test_responses_sse_preserves_named_events(retrieve, buffered):
    import gzip

    engine = ProxyEngine(engine_cmd=["unused"], fetch_media=False)
    engine._set_state(EngineState.READY)

    content = gzip.compress(
        b'event: response.output_text.delta\rid: 7\rdata: {"type":"response.output_text.delta","delta":"hi","item_id":"msg_1","output_index":0,"content_index":0,"sequence_number":7}\r\r'
    )

    class Stream(httpx.AsyncByteStream):
        closed = False

        async def __aiter__(self):
            # Split inside the gzip header and compressed SSE event.
            for offset in range(0, len(content), 7):
                yield content[offset : offset + 7]

        async def aclose(self):
            self.closed = True

    stream = Stream()
    raw = httpx.Response(
        200,
        stream=stream,
        headers={"content-type": "text/event-stream", "content-encoding": "gzip"},
        request=httpx.Request("POST", "http://inner/v1/responses"),
    )
    if buffered:
        await raw.aread()
    engine._subprocess.forward = AsyncMock(return_value=raw)
    try:
        result = await engine.responses(
            "retrieve" if retrieve else "create",
            None if retrieve else CreateResponseRequest(input="hi", stream=True),
            response_id="resp_test" if retrieve else None,
            query=[("stream", "true")] if retrieve else None,
        )
        frames = [frame async for frame in result]
        assert "event: response.output_text.delta\nid: 7\n" in frames[0]
        assert '"delta":"hi"' in frames[0]
        assert stream.closed and not engine._in_flight
    finally:
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "query",
    [None]
    + [[("stream", value)] for value in ("true", "1", "yes", "on", "t", "y", "TRUE")]
    + [[("stream", "false"), ("stream", "yes")]],
    ids=["create", "true", "1", "yes", "on", "t", "y", "uppercase", "last-value"],
)
async def test_responses_stream_controls_and_close_before_iteration(query):
    engine = ProxyEngine(["unused"], fetch_media=False)
    engine._set_state(EngineState.READY)

    class Stream(httpx.AsyncByteStream):
        closed = False

        async def __aiter__(self):
            pytest.fail("Closing before iteration must not read the response")
            yield b""  # Make this an async iterator.

        async def aclose(self):
            self.closed = True

    stream = Stream()
    engine._subprocess.forward = AsyncMock(
        return_value=httpx.Response(
            200, stream=stream, request=httpx.Request("POST", "http://inner")
        )
    )
    try:
        result = await engine.responses(
            "retrieve" if query is not None else "create",
            None
            if query is not None
            else CreateResponseRequest(input="hi", stream=True),
            response_id="resp_test" if query is not None else None,
            query=query,
        )
        assert engine._subprocess.forward.call_args.kwargs["stream"] is True
        await result.aclose()
        assert stream.closed and not engine._in_flight and not engine._stream_closers
    finally:
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize("native", [False, True])
async def test_responses_upstream_error_is_preserved(tmp_path, native):
    engine = ProxyEngine(engine_cmd=["unused"], fetch_media=False)
    engine._set_state(EngineState.READY)
    engine._subprocess.forward = AsyncMock(
        return_value=httpx.Response(
            501,
            content=b'{"engine_error":"unsupported"}',
            headers={"content-type": "application/problem+json"},
            request=httpx.Request("POST", "http://inner/v1/responses"),
        )
    )
    app = FastAPI()
    app.engine = engine
    add_exception_handlers(app)
    app.include_router(router)
    path = "/v1/rerank" if native else "/v1/responses"
    if native:
        from aibrix.openai_frontend.frontend.fastapi.routers.engine_routes import (
            create_router,
        )
        from aibrix.openai_frontend.proxy.runtime.engine_spec import VLLM_SPEC

        engine._spec = VLLM_SPEC
        app.include_router(create_router(engine))
    async with httpx.AsyncClient(
        transport=httpx.ASGITransport(app), base_url="http://frontend"
    ) as client:
        response = await client.post(
            path, json={"input": "hi", "output": {"url": str(tmp_path)}}
        )
        assert not list(tmp_path.iterdir())
        if native:
            rejected = await client.post(
                path, json={"stream": True, "output": {"url": str(tmp_path)}}
            )
            assert rejected.status_code == 400
            assert engine._subprocess.forward.await_count == 1
    assert response.status_code == 501
    assert response.content == b'{"engine_error":"unsupported"}'
    assert response.headers["content-type"] == "application/problem+json"
    await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "spec_name,path",
    [
        ("sglang", "/generate"),
        ("sglang", "/encode"),
        ("vllm", "/v1/rerank"),
        ("sglang", "/v1/responses"),
        ("vllm", "/v1/responses"),
    ],
)
async def test_native_media_is_rewritten_only_on_generate(tmp_path, spec_name, path):
    from aibrix.openai_frontend.frontend.fastapi.routers import chat, engine_routes
    from aibrix.openai_frontend.proxy.runtime.engine_spec import get_engine_spec

    engine = ProxyEngine(
        engine_cmd=["unused"],
        spec=get_engine_spec(spec_name),
        media_dir=str(tmp_path),
        materialize_inline_media=True,
    )
    engine._set_state(EngineState.READY)
    app = FastAPI()
    app.engine = engine
    add_exception_handlers(app)
    app.include_router(chat.router)
    app.include_router(router)
    app.include_router(engine_routes.create_router(engine))
    native_url = "data:image/png;base64,UElYRUxT"
    calls = []

    async def forward(method, endpoint, *, json=None, stream=False):
        calls.append(json)
        if path == "/generate":
            from pathlib import Path

            assert (
                Path(json["image_data"][0].removeprefix("file://")).read_bytes()
                == b"PIXELS"
            )
            body = {"text": "ok"}
        elif path == "/v1/responses":
            assert json["image_data"] == [native_url]
            body = RESPONSE
        else:
            assert json["image_data"] == [native_url]
            body = {
                "id": "chat_1",
                "object": "chat.completion",
                "created": 1,
                "model": "m",
                "choices": [],
            }
        return httpx.Response(
            200, json=body, request=httpx.Request(method, "http://inner" + endpoint)
        )

    engine._subprocess.forward = AsyncMock(side_effect=forward)
    try:
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app), base_url="http://frontend"
        ) as client:
            response = await client.post(
                path,
                json={
                    "model": "m",
                    "input": "hi",
                    "messages": [{"role": "user", "content": "hi"}],
                    "image_data": [native_url],
                },
            )
            assert response.status_code == 200, response.text
        assert len(calls) == 1
        assert engine._metrics_registry.get_sample_value("proxy_fetch_bytes_total") == (
            6 if path == "/generate" else 0
        )
        assert not list(tmp_path.iterdir())
        assert not engine._in_flight
    finally:
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "spec_name,path,body,stored,cleared",
    [
        (
            "sglang",
            "/generate",
            {
                "text": "hello",
                "output_ids": [7],
                "meta_info": {
                    "id": "r",
                    "completion_tokens": 1,
                    "output_token_logprobs": [[-0.1, 7]],
                },
            },
            {
                "text": "hello",
                "output_ids": [7],
                "meta_info": {"output_token_logprobs": [[-0.1, 7]]},
            },
            {
                "text": "",
                "output_ids": [],
                "meta_info": {
                    "id": "r",
                    "completion_tokens": 1,
                    "output_token_logprobs": [],
                },
            },
        ),
        (
            "sglang-omni",
            "/generate",
            [
                {"text": "a", "output_ids": [1], "meta_info": {"id": "a"}},
                {"output_ids": [2], "meta_info": {"id": "b"}},
            ],
            [{"text": "a", "output_ids": [1]}, {"output_ids": [2]}],
            [
                {"text": "", "output_ids": [], "meta_info": {"id": "a"}},
                {"output_ids": [], "meta_info": {"id": "b"}},
            ],
        ),
        (
            "sglang",
            "/generate",
            {
                "text": "a",
                "meta_info": {
                    "beam_results": [
                        {
                            "text": "a",
                            "output_ids": [1],
                            "meta_info": {"sequence_score": -1},
                        },
                        {
                            "text": "b",
                            "output_ids": [2],
                            "meta_info": {"sequence_score": -2},
                        },
                    ]
                },
            },
            {
                "text": "a",
                "meta_info": {
                    "beam_results": [
                        {"text": "a", "output_ids": [1]},
                        {"text": "b", "output_ids": [2]},
                    ]
                },
            },
            {
                "text": "",
                "meta_info": {
                    "beam_results": [
                        {
                            "text": "",
                            "output_ids": [],
                            "meta_info": {"sequence_score": -1},
                        },
                        {
                            "text": "",
                            "output_ids": [],
                            "meta_info": {"sequence_score": -2},
                        },
                    ]
                },
            },
        ),
        (
            "sglang",
            "/encode",
            {
                "embedding": [0.1, 0.2],
                "pooled_hidden_state": [0.3],
                "meta_info": {"prompt_tokens": 2},
            },
            {"embedding": [0.1, 0.2], "pooled_hidden_state": [0.3]},
            {
                "embedding": [],
                "pooled_hidden_state": [],
                "meta_info": {"prompt_tokens": 2},
            },
        ),
        (
            "sglang",
            "/classify",
            [{"embedding": [0.2], "meta_info": {"id": "c"}}],
            [{"embedding": [0.2]}],
            [{"embedding": [], "meta_info": {"id": "c"}}],
        ),
        (
            "vllm",
            "/v1/rerank",
            {
                "id": "r",
                "results": [
                    {"index": 0, "relevance_score": 0.9, "document": {"text": "a"}}
                ],
            },
            {
                "results": [
                    {"index": 0, "relevance_score": 0.9, "document": {"text": "a"}}
                ]
            },
            {"id": "r", "results": []},
        ),
        (
            "sglang",
            "/v1/rerank",
            [{"index": 0, "score": 0.0, "document": "a", "meta_info": {"id": "r"}}],
            [{"index": 0, "score": 0.0, "document": "a"}],
            [{"index": 0, "score": None, "document": "", "meta_info": {"id": "r"}}],
        ),
        (
            "sglang",
            "/v1/score",
            {
                "scores": [[0.0, 0.9]],
                "token_logprobs": [[-1]],
                "usage": {"total_tokens": 2},
            },
            {"scores": [[0.0, 0.9]], "token_logprobs": [[-1]]},
            {"scores": [], "token_logprobs": [], "usage": {"total_tokens": 2}},
        ),
        (
            "vllm",
            "/pooling",
            {"data": {"plugin": [1, 2]}, "created_at": 1},
            {"data": {"plugin": [1, 2]}},
            {"data": {}, "created_at": 1},
        ),
        (
            "vllm",
            "/inference/v1/generate",
            {"choices": [{"token_ids": [1, 2]}], "request_id": "r"},
            {"choices": [{"token_ids": [1, 2]}]},
            {"choices": [], "request_id": "r"},
        ),
        (
            "vllm",
            "/generative_scoring",
            {"data": [{"score": 0.1}], "usage": {"total_tokens": 1}},
            {"data": [{"score": 0.1}]},
            {"data": [], "usage": {"total_tokens": 1}},
        ),
        (
            "vllm",
            "/v2/embed",
            {"embeddings": {"float": [[0.1]], "binary": None}, "texts": ["input"]},
            {"embeddings": {"float": [[0.1]], "binary": None}},
            {"embeddings": {}, "texts": ["input"]},
        ),
        (
            "vllm",
            "/tokenize",
            {"tokens": [1, 2], "token_strs": ["a", "b"], "count": 2},
            {"tokens": [1, 2], "token_strs": ["a", "b"]},
            {"tokens": [], "token_strs": [], "count": 2},
        ),
        (
            "sglang",
            "/v1/tokenize",
            {"tokens": [[1], [2]], "count": [1, 1]},
            {"tokens": [[1], [2]]},
            {"tokens": [], "count": [1, 1]},
        ),
        (
            "vllm",
            "/detokenize",
            {"prompt": "hello"},
            {"prompt": "hello"},
            {"prompt": ""},
        ),
        (
            "sglang",
            "/v1/detokenize",
            {"text": ["a", "b"]},
            {"text": ["a", "b"]},
            {"text": []},
        ),
        (
            "vllm",
            "/v1/messages",
            {
                "content": [{"type": "text", "text": "hello"}],
                "role": "assistant",
                "stop_reason": "end_turn",
            },
            {"content": [{"type": "text", "text": "hello"}]},
            {"content": [], "role": "assistant", "stop_reason": "end_turn"},
        ),
        (
            "vllm",
            "/cohere/v2/chat",
            {
                "message": {
                    "role": "assistant",
                    "content": [{"type": "text", "text": "hello"}],
                    "tool_plan": None,
                },
                "logprobs": [{"logprobs": [-1]}],
                "finish_reason": "COMPLETE",
            },
            {
                "message": {
                    "content": [{"type": "text", "text": "hello"}],
                    "tool_plan": None,
                },
                "logprobs": [{"logprobs": [-1]}],
            },
            {
                "message": {"role": "assistant", "content": [], "tool_plan": None},
                "logprobs": [],
                "finish_reason": "COMPLETE",
            },
        ),
        (
            "vllm",
            "/v1/decisions",
            {"answers": [{"choice": "a"}], "model": "m"},
            {"answers": [{"choice": "a"}]},
            {"answers": [], "model": "m"},
        ),
        (
            "sglang",
            "/v1/systemone",
            {"answers": {"q": {"score": 1}}, "model": "m"},
            {"answers": {"q": {"score": 1}}},
            {"answers": {}, "model": "m"},
        ),
        (
            "vllm",
            "/v1/messages/count_tokens",
            {"input_tokens": 0},
            {"input_tokens": 0},
            {"input_tokens": None},
        ),
        ("sglang", "/v1/rerank", [], [], []),
        ("vllm", "/v1/rerank", {"results": []}, {"results": []}, {"results": []}),
        *[
            ("vllm", "/pooling", {"data": value}, {"data": value}, {"data": cleared})
            for value, cleared in ((0.25, None), (False, None), ("encoded", ""))
        ],
        *[
            (
                "sglang",
                "/generate",
                {"text": "a", "meta_info": {"id": "r", **fields}},
                {"text": "a", "meta_info": fields},
                {"text": "", "meta_info": {"id": "r", **cleared}},
            )
            for fields, cleared in (
                (
                    {
                        "output_top_logprobs_val_flat": [-0.1],
                        "output_top_logprobs_idx_flat": [7],
                        "output_top_logprobs_shape": [1, 1],
                        "output_top_logprobs_null_prefix": 0,
                    },
                    {
                        "output_top_logprobs_val_flat": [],
                        "output_top_logprobs_idx_flat": [],
                        "output_top_logprobs_shape": [],
                        "output_top_logprobs_null_prefix": None,
                    },
                ),
                (
                    {
                        "output_top_logprobs_val_flat_b64": "zczMvQ==",
                        "output_top_logprobs_idx_flat_b64": "BwAAAA==",
                        "output_top_logprobs_val_flat_b64_dtype": "float32",
                        "output_top_logprobs_idx_flat_b64_dtype": "int32",
                        "output_top_logprobs_shape": [1, 1],
                        "output_top_logprobs_null_prefix": 0,
                    },
                    {
                        "output_top_logprobs_val_flat_b64": "",
                        "output_top_logprobs_idx_flat_b64": "",
                        "output_top_logprobs_val_flat_b64_dtype": "float32",
                        "output_top_logprobs_idx_flat_b64_dtype": "int32",
                        "output_top_logprobs_shape": [],
                        "output_top_logprobs_null_prefix": None,
                    },
                ),
            )
        ],
    ],
)
async def test_native_result_persistence_contract(
    tmp_path, spec_name, path, body, stored, cleared
):
    import json

    from aibrix.openai_frontend.frontend.fastapi.routers.engine_routes import (
        create_router,
    )
    from aibrix.openai_frontend.proxy.runtime.engine_spec import get_engine_spec

    writer_failure_case = path == "/v1/rerank" and bool(
        body.get("results") if isinstance(body, dict) else body
    )
    engine = ProxyEngine(["unused"], spec=get_engine_spec(spec_name), fetch_media=False)
    engine._set_state(EngineState.READY)
    # Unmarked fields that look like output paths must not be opened or removed.
    sentinel = tmp_path / "source.txt"
    sentinel.write_text("keep")
    if isinstance(body, dict):
        body = {**body, "vendor": {"keep": None}, "file_path": str(sentinel)}
        cleared = {**cleared, "vendor": {"keep": None}, "file_path": str(sentinel)}
    engine._subprocess.forward = AsyncMock(
        return_value=httpx.Response(
            200, json=body, request=httpx.Request("POST", "http://inner" + path)
        )
    )
    app = FastAPI()
    app.engine = engine
    app.include_router(create_router(engine))
    add_exception_handlers(app)
    destination = tmp_path / "out"
    try:
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app), base_url="http://frontend"
        ) as client:
            plain = await client.post(path, json={"input": "hi"})
            assert plain.status_code == 200 and plain.json() == body
            result = await client.post(
                path, json={"input": "hi", "output": {"url": str(destination)}}
            )
            assert result.status_code == 200, result.text
            receipt = result.json()
            output = receipt.pop("output")
            assert receipt == (
                {"data": cleared} if isinstance(cleared, list) else cleared
            )
            content = (destination / output["artifacts"][0]).read_bytes()
            assert json.loads(content) == stored
            assert "output" not in engine._subprocess.forward.call_args.kwargs["json"]
            assert engine._metrics_registry.get_sample_value(
                "proxy_output_bytes_total", {"scheme": "file"}
            ) == len(content)
            assert sentinel.read_text() == "keep"
            assert not engine._request_done

            # Writer failure is independent of the result shape. Keep one object
            # and one batch representative rather than repeating every contract.
            if writer_failure_case:
                from aibrix.openai_frontend.proxy.output.output_writer import (
                    OutputWriteError,
                )

                engine._output_writer._write_local_entries = AsyncMock(
                    side_effect=OutputWriteError("upload failed")
                )
                failed = await client.post(
                    path, json={"input": "hi", "output": {"url": str(destination)}}
                )
                assert failed.status_code == 500 and "output" not in failed.json()
                assert not engine._request_done
                assert engine._metrics_registry.get_sample_value(
                    "proxy_output_bytes_total", {"scheme": "file"}
                ) == len(content)
    finally:
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "spec_name,path,body",
    [
        ("sglang", "/generate", {"meta_info": {"id": "r"}}),
        ("sglang", "/generate", {"text": None, "output_ids": None}),
        ("sglang", "/generate", [{"text": "valid"}, {"meta_info": {"id": "missing"}}]),
        ("vllm", "/v1/rerank", {"choices": [{"text": "wrong schema"}]}),
        ("vllm", "/v1/rerank", {"results": "not a list"}),
        ("vllm", "/cohere/v2/chat", {"message": {"role": "assistant"}}),
        ("sglang", "/encode", "not an object"),
    ],
)
async def test_native_missing_result_is_not_persisted(tmp_path, spec_name, path, body):
    from aibrix.openai_frontend.proxy.runtime.engine_spec import get_engine_spec
    from aibrix.openai_frontend.utils.utils import ServerError

    engine = ProxyEngine(["unused"], spec=get_engine_spec(spec_name), fetch_media=False)
    engine._set_state(EngineState.READY)
    engine._subprocess.forward = AsyncMock(
        return_value=httpx.Response(
            200, json=body, request=httpx.Request("POST", "http://inner" + path)
        )
    )
    engine._output_writer._write_local_entries = AsyncMock()
    try:
        with pytest.raises(ServerError):
            await engine.forward_json(path, {"output": {"url": str(tmp_path)}})
        engine._output_writer._write_local_entries.assert_not_awaited()
        assert not list(tmp_path.iterdir()) and not engine._request_done
    finally:
        await engine.aclose()

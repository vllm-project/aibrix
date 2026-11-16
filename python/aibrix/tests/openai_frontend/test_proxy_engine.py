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

from __future__ import annotations

import asyncio
import logging
import os
import sys

import pytest

from aibrix.openai_frontend.proxy.runtime.engine_spec import (
    EndpointSet,
    EngineSpec,
    ForwardKind,
)
from aibrix.openai_frontend.utils.utils import make_prefix_formatter, prefix_line

# ---------------------------------------------------------------------------
# prefix_line
# ---------------------------------------------------------------------------


class TestPrefixLine:
    def test_plain_line(self):
        assert prefix_line("hello world\n", "[worker-0]") == "[worker-0] hello world\n"

    def test_line_without_newline(self):
        assert prefix_line("hello", "[main]") == "[main] hello"

    def test_empty_string_unchanged(self):
        assert prefix_line("", "[p]") == ""

    def test_whitespace_only_unchanged(self):
        assert prefix_line("   \n", "[p]") == "   \n"

    def test_carriage_return_only(self):
        assert prefix_line("\rprogress", "[p]") == "\r[p] progress"

    def test_trailing_carriage_return(self):
        assert prefix_line("status\r", "[p]") == "\r[p] status"


# ---------------------------------------------------------------------------
# make_prefix_formatter
# ---------------------------------------------------------------------------


class TestMakePrefixFormatter:
    def test_formatter_includes_prefix(self):
        fmt = make_prefix_formatter("[proxy]")
        record = logging.LogRecord(
            name="test",
            level=logging.INFO,
            pathname="",
            lineno=0,
            msg="hello",
            args=(),
            exc_info=None,
        )
        output = fmt.format(record)
        assert output.startswith("[proxy] ")
        assert "hello" in output

    def test_formatter_preserves_levelname(self):
        fmt = make_prefix_formatter("[proxy]")
        record = logging.LogRecord(
            name="test",
            level=logging.WARNING,
            pathname="",
            lineno=0,
            msg="warn msg",
            args=(),
            exc_info=None,
        )
        output = fmt.format(record)
        assert "WARNING" in output


# ---------------------------------------------------------------------------
# EngineSpec
# ---------------------------------------------------------------------------


class TestEngineSpec:
    def test_defaults(self):
        spec = EngineSpec(name="test")
        assert spec.chat_endpoint == "/v1/chat/completions"
        assert spec.health_endpoint == "/health"
        assert spec.host_arg == "--host"
        assert spec.port_arg == "--port"

    def test_custom_endpoints(self):
        spec = EngineSpec(
            name="custom",
            chat_endpoint="/api/chat",
            health_endpoint="/ready",
        )
        assert spec.chat_endpoint == "/api/chat"
        assert spec.health_endpoint == "/ready"

    def test_require_chat_raises(self):
        spec = EngineSpec(
            name="no-chat",
            endpoints=EndpointSet(
                (
                    ("/v1/completions", ForwardKind.SSE),
                    ("/v1/embeddings", ForwardKind.JSON),
                    ("/v1/models", ForwardKind.JSON),
                )
            ),
        )
        with pytest.raises(NotImplementedError):
            spec.require_chat()

    def test_require_embedding_raises(self):
        spec = EngineSpec(
            name="no-emb",
            endpoints=EndpointSet(
                (
                    ("/v1/chat/completions", ForwardKind.SSE),
                    ("/v1/completions", ForwardKind.SSE),
                    ("/v1/models", ForwardKind.JSON),
                )
            ),
        )
        with pytest.raises(NotImplementedError):
            spec.require_embedding()

    def test_get_engine_spec(self):
        from aibrix.openai_frontend.proxy.runtime.engine_spec import get_engine_spec

        vllm = get_engine_spec("vllm")
        assert vllm.name == "vllm"
        with pytest.raises(ValueError):
            get_engine_spec("nonexistent")


class TestBuildProxyEngine:
    def test_builds_from_args(self):
        import argparse

        from aibrix.openai_frontend.main import build_proxy_engine

        args = argparse.Namespace(
            proxy_engine_cmd=["python", "-m", "vllm.entrypoints.openai.api_server"],
            proxy_engine_host="127.0.0.1",
            proxy_engine_port=8001,
            proxy_startup_timeout=60.0,
            proxy_engine_spec="vllm",
            proxy_credentials='{"https://cdn.example.com": {"headers": {"Authorization": "Bearer tok"}}}',
        )
        engine = build_proxy_engine(args)
        from aibrix.openai_frontend.engine.proxy_engine import ProxyEngine

        assert isinstance(engine, ProxyEngine)
        assert engine._subprocess._port == 8001
        assert engine._subprocess._host == "127.0.0.1"
        # vLLM reads file:// media only under --allowed-local-media-path, so the
        # proxy appends it, pointing at the directory it downloads media into.
        cmd = engine._subprocess._cmd
        assert cmd[:3] == [
            "python",
            "-m",
            "vllm.entrypoints.openai.api_server",
        ]
        assert cmd[3:] == ["--allowed-local-media-path", engine._media_dir]
        assert engine._fetch_media is True
        assert engine._startup_timeout == 60.0
        asyncio.run(engine.aclose())


class TestProxyClientLifecycle:
    @pytest.mark.parametrize("from_thread", [False, True])
    def test_shutdown_closes_pool_once(self, monkeypatch, from_thread):
        from unittest.mock import AsyncMock

        from aibrix.openai_frontend.proxy.runtime.subprocess_engine import (
            SubprocessEngine,
        )

        async def _run():
            engine = SubprocessEngine(cmd=["unused"])
            await engine.start_client()
            client = engine._client
            assert client is not None
            loop = asyncio.get_running_loop()
            original_close = client.aclose

            async def close_on_owner():
                assert asyncio.get_running_loop() is loop
                await asyncio.sleep(0)
                await original_close()

            close = AsyncMock(side_effect=close_on_owner)
            monkeypatch.setattr(client, "aclose", close)
            if from_thread:
                await asyncio.to_thread(engine.shutdown)
            else:
                engine.shutdown()
            engine.shutdown()
            # Allow the scheduled cleanup to run without explicitly closing it.
            for _ in range(10):
                if client.is_closed:
                    break
                await asyncio.sleep(0)
            assert client.is_closed
            await engine.close_client()
            assert engine._client is None
            close.assert_awaited_once()
            with pytest.raises(RuntimeError, match="after shutdown"):
                await engine.start_client()

        asyncio.run(_run())

    def test_shutdown_closes_real_keepalive_connection(self):
        from aibrix.openai_frontend.proxy.runtime.subprocess_engine import (
            SubprocessEngine,
        )

        async def _run():
            disconnected = asyncio.Event()
            handlers = []

            async def serve(reader, writer):
                task = asyncio.current_task()
                assert task is not None
                handlers.append(task)
                try:
                    await reader.readuntil(b"\r\n\r\n")
                    writer.write(
                        b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n"
                        b"Connection: keep-alive\r\n\r\nok"
                    )
                    await writer.drain()
                    assert await reader.read() == b""
                    disconnected.set()
                finally:
                    writer.close()
                    await writer.wait_closed()

            server = await asyncio.start_server(serve, "127.0.0.1", 0)
            port = server.sockets[0].getsockname()[1]
            engine = SubprocessEngine(cmd=["unused"], port=port)
            try:
                await engine.start_client()
                client = engine._client
                assert client is not None
                response = await engine.forward("GET", "/v1/models")
                assert response.text == "ok"
                assert not disconnected.is_set()
                engine.shutdown()
                await engine.close_client()
                await asyncio.wait_for(disconnected.wait(), timeout=5)
                assert client.is_closed
            finally:
                await engine.close_client()
                server.close()
                await server.wait_closed()
                for task in handlers:
                    if not task.done():
                        task.cancel()
                await asyncio.gather(*handlers, return_exceptions=True)

        asyncio.run(_run())

    def test_lifespan_awaits_pool_close(self):
        import httpx

        from aibrix.openai_frontend.engine.proxy_engine import ProxyEngine
        from aibrix.openai_frontend.frontend.fastapi_frontend import FastApiFrontend

        class DelayedTransport(httpx.AsyncBaseTransport):
            closed = False

            async def aclose(self):
                await asyncio.sleep(0.01)
                self.closed = True

        class ClientOnlyEngine(ProxyEngine):
            async def start(self):
                await self._subprocess.start_client()
                await self._subprocess.close_client()
                self._subprocess._client = httpx.AsyncClient(transport=transport)

        transport = DelayedTransport()
        engine = ClientOnlyEngine(engine_cmd=["unused"])
        frontend = FastApiFrontend(engine=engine)

        async def _run():
            async with frontend.app.router.lifespan_context(frontend.app):
                client = engine._subprocess._client
                assert client is not None
                assert not client.is_closed
            assert client.is_closed
            assert transport.closed
            assert engine._subprocess._client is None

        asyncio.run(_run())


class TestAsyncEngineClose:
    @pytest.mark.parametrize("engine_kind", ["proxy", "vipe"])
    def test_closed_engine_cannot_restart(self, monkeypatch, engine_kind):
        from unittest.mock import Mock

        from aibrix.openai_frontend.engine.proxy_engine import EngineState, ProxyEngine
        from aibrix.openai_frontend.engine.vipe_engine import ViPEEngine

        async def _run():
            if engine_kind == "proxy":
                engine = ProxyEngine(engine_cmd=["unused"])
                spawn = Mock(side_effect=AssertionError("spawned after close"))
                monkeypatch.setattr(engine._subprocess, "spawn", spawn)
            else:
                engine = ViPEEngine()
                spawn = Mock(side_effect=AssertionError("spawned after close"))
                monkeypatch.setattr(engine, "_spawn_worker", spawn)
            await engine.aclose()
            with pytest.raises(RuntimeError, match="closed"):
                await engine.start()
            spawn.assert_not_called()
            await engine.aclose()
            assert not await engine.health()
            if engine_kind == "proxy":
                assert engine.state == EngineState.STOPPED
            else:
                assert engine._result_listener_task is None

        asyncio.run(_run())

    @pytest.mark.parametrize("engine_kind", ["proxy", "vipe"])
    def test_concurrent_close_waits_for_one_cleanup_despite_caller_cancellation(
        self, monkeypatch, engine_kind
    ):
        import threading
        from unittest.mock import Mock

        from aibrix.openai_frontend.engine.proxy_engine import EngineState, ProxyEngine
        from aibrix.openai_frontend.engine.vipe_engine import ViPEEngine

        async def _run():
            entered = threading.Event()
            release = threading.Event()

            def blocking_cleanup():
                entered.set()
                assert release.wait(5), "test did not release cleanup"

            cleanup = Mock(side_effect=blocking_cleanup)
            if engine_kind == "proxy":
                engine = ProxyEngine(engine_cmd=["unused"])
                monkeypatch.setattr(engine._subprocess, "shutdown", cleanup)
            else:
                engine = ViPEEngine()
                monkeypatch.setattr(engine, "_stop_workers", cleanup)
            assert not hasattr(engine, "stop")
            first = asyncio.create_task(engine.aclose())
            second = None
            try:
                assert await asyncio.to_thread(entered.wait, 5)
                first.cancel()
                with pytest.raises(asyncio.CancelledError):
                    await first
                second = asyncio.create_task(engine.aclose())
                await asyncio.sleep(0)
                assert not second.done()
                release.set()
                await asyncio.wait_for(second, 5)
                await engine.aclose()
                cleanup.assert_called_once()
                assert engine._stopping
                if engine_kind == "proxy":
                    assert engine.state == EngineState.STOPPED
                else:
                    assert not await engine.health()
            finally:
                release.set()
                await engine.aclose()
                if second is not None:
                    await second

        asyncio.run(_run())

    def test_lifespan_closes_engine_after_startup_failure(self):
        from aibrix.openai_frontend.engine.proxy_engine import ProxyEngine
        from aibrix.openai_frontend.frontend.fastapi_frontend import FastApiFrontend

        class FailingEngine(ProxyEngine):
            async def start(self):
                await self._subprocess.start_client()
                raise RuntimeError("startup failed")

        engine = FailingEngine(engine_cmd=["unused"])
        frontend = FastApiFrontend(engine=engine)
        assert not hasattr(frontend, "stop")

        async def _run():
            with pytest.raises(RuntimeError, match="startup failed"):
                async with frontend.app.router.lifespan_context(frontend.app):
                    pass
            assert engine._subprocess._client is None
            assert engine._close_task is not None
            assert engine._close_task.done()

        asyncio.run(_run())


class TestProxyPoolEnvironment:
    @pytest.mark.parametrize(
        "values,expected",
        [
            (None, (100, 20, 30.0)),
            (("64", "0", "2.5"), (64, 0, 2.5)),
        ],
    )
    def test_env_reaches_client(self, monkeypatch, values, expected):
        import httpx

        from aibrix.openai_frontend.main import build_proxy_engine, parse_args

        names = [
            "AIBRIX_PROXY_MAX_CONNECTIONS",
            "AIBRIX_PROXY_MAX_KEEPALIVE_CONNECTIONS",
            "AIBRIX_PROXY_KEEPALIVE_EXPIRY_SECONDS",
        ]
        for name in names:
            monkeypatch.delenv(name, raising=False)
        original_client = httpx.AsyncClient
        captured = {}

        def make_client(**kwargs):
            captured.update(kwargs)
            return original_client(**kwargs)

        monkeypatch.setattr(httpx, "AsyncClient", make_client)
        argv = ["--engine", "proxy", "--proxy-engine-cmd", "unused"]
        if values is not None:
            argv.append("--setenv")
            for name, value in zip(names, values):
                argv.extend([name, value])
        engine = build_proxy_engine(parse_args(argv))

        async def _run():
            try:
                await engine._subprocess.start_client()
                limits = captured["limits"]
                assert (
                    limits.max_connections,
                    limits.max_keepalive_connections,
                    limits.keepalive_expiry,
                ) == expected
                assert captured["timeout"].read is None
            finally:
                await engine.aclose()

        asyncio.run(_run())

    @pytest.mark.parametrize(
        "name,value",
        [
            ("AIBRIX_PROXY_MAX_CONNECTIONS", "0"),
            ("AIBRIX_PROXY_KEEPALIVE_EXPIRY_SECONDS", "nan"),
        ],
    )
    def test_invalid_env_fails_before_client_creation(self, monkeypatch, name, value):
        from pydantic import ValidationError

        from aibrix.openai_frontend.proxy.runtime.subprocess_engine import (
            SubprocessEngine,
        )

        monkeypatch.setenv(name, value)

        async def _run():
            engine = SubprocessEngine(cmd=["unused"])
            with pytest.raises(ValidationError) as exc:
                await engine.start_client()
            assert exc.value.errors()[0]["loc"] == (
                name.removeprefix("AIBRIX_PROXY_").lower(),
            )
            assert engine._client is None
            engine.shutdown()

        asyncio.run(_run())


class TestStreamDelivery:
    @pytest.mark.parametrize("phase", ["before_iteration", "read", "send", "normal"])
    @pytest.mark.parametrize(
        "native,with_media", [(False, False), (False, True), (True, True)]
    )
    def test_disconnect_releases_pool_and_media(
        self, monkeypatch, phase, tmp_path, native, with_media
    ):
        """Real ASGI disconnect across before_iteration/read/send phases must
        release inline-materialized media temp files immediately via the shared
        close_response callback, before engine.aclose()."""

        from pathlib import Path

        from aibrix.openai_frontend.common.artifact_fetcher import (
            DefaultArtifactFetcher,
        )
        from aibrix.openai_frontend.engine.proxy_engine import EngineState, ProxyEngine
        from aibrix.openai_frontend.frontend.fastapi.routers.chat import (
            create_chat_completion,
        )
        from aibrix.openai_frontend.schemas.openai import CreateChatCompletionRequest

        async def _run():
            handlers = []
            disconnected = asyncio.Event()
            upstream_eof = asyncio.Event()
            requests = 0
            media_dir = str(tmp_path / "media")
            import os

            os.makedirs(media_dir)

            async def serve(reader, writer):
                nonlocal requests
                task = asyncio.current_task()
                handlers.append(task)
                try:
                    headers = await reader.readuntil(b"\r\n\r\n")
                    for line in headers.split(b"\r\n"):
                        if line.lower().startswith(b"content-length:"):
                            await reader.readexactly(int(line.split(b":", 1)[1]))
                    requests += 1
                    if requests == 1:
                        writer.write(
                            b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n"
                        )
                        if phase == "send":
                            writer.write(b'data: {"vendor":1}\n\n')
                        elif phase == "normal":
                            writer.write(
                                b'data: {"choices":[{"delta":{"content":"hi"}}]}\n\n'
                                b"data: [DONE]\n\n"
                            )
                        await writer.drain()
                        if phase != "normal":
                            await reader.read()
                        upstream_eof.set()
                    else:
                        writer.write(
                            b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
                        )
                        await writer.drain()
                finally:
                    writer.close()
                    await writer.wait_closed()

            server = await asyncio.start_server(serve, "127.0.0.1", 0)
            engine = ProxyEngine(
                engine_cmd=["unused"], engine_port=server.sockets[0].getsockname()[1]
            )
            monkeypatch.setenv("AIBRIX_PROXY_MAX_CONNECTIONS", "1")
            await engine._subprocess.start_client()
            engine._set_state(EngineState.READY)
            # Enable media materialization against a real fetcher.
            from aibrix.openai_frontend.proxy.media.request_inspector import (
                MediaURLInspector,
            )

            engine._fetch_media = with_media
            engine._fetcher = DefaultArtifactFetcher(base_temp_dir=media_dir)
            engine._media_dir = media_dir
            engine._owned_media_dir = None
            engine._media_inspector = engine._chat_inspector = MediaURLInspector(
                materialize_inline=True
            )
            cleanup_calls = 0
            real_cleanup = engine._fetcher.cleanup

            async def spy_cleanup(results):
                nonlocal cleanup_calls
                cleanup_calls += 1
                await real_cleanup(results)

            engine._fetcher.cleanup = spy_cleanup  # type: ignore[method-assign]

            response = None
            try:
                request = CreateChatCompletionRequest(
                    model="m",
                    messages=[
                        {
                            "role": "user",
                            "content": [
                                {"type": "text", "text": "hi"},
                                {
                                    "type": "image_url",
                                    "image_url": {
                                        "url": "data:image/png;base64,SU5MSU5FLU1FRElB"
                                    },
                                },
                            ],
                        }
                    ],
                    stream=True,
                )
                if native:
                    response = await engine.forward_json(
                        "/v1/chat/completions", request.model_dump(exclude_unset=True)
                    )
                else:
                    response = await create_chat_completion(request, engine)

                async def send(message):
                    if phase == "normal":
                        return  # deliver start/body naturally, no block
                    if (
                        phase == "before_iteration"
                        and message["type"] == "http.response.start"
                    ):
                        disconnected.set()
                        await asyncio.Event().wait()
                    elif phase == "read" and message["type"] == "http.response.start":
                        asyncio.get_running_loop().call_later(0.02, disconnected.set)
                    elif message["type"] == "http.response.body":
                        disconnected.set()
                        await asyncio.Event().wait()

                async def receive():
                    if phase == "normal":
                        await (
                            asyncio.Event().wait()
                        )  # no disconnect; stream ends naturally
                    await disconnected.wait()
                    return {"type": "http.disconnect"}

                # The temp file must exist before disconnect.
                inline_before = list(Path(media_dir).rglob("inline_*"))
                assert bool(inline_before) == with_media
                expected_bytes = len(b"INLINE-MEDIA") if with_media else 0
                assert (
                    engine._metrics_registry.get_sample_value("proxy_fetch_bytes_total")
                    == expected_bytes
                )

                await asyncio.wait_for(response({"type": "http"}, receive, send), 3)
                await asyncio.wait_for(upstream_eof.wait(), 1)
                assert engine._in_flight == {}
                assert engine._in_flight_tasks == {}

                # Media temp file must be released before engine.aclose().
                inline_after = list(Path(media_dir).rglob("inline_*"))
                assert inline_after == [], (
                    f"media temp files leaked after ASGI disconnect: {inline_after}"
                )
                assert (
                    engine._metrics_registry.get_sample_value("proxy_fetch_bytes_total")
                    == expected_bytes
                )
                assert cleanup_calls == int(with_media)

                # Single-slot pool must be reusable for the next request.
                assert (
                    await asyncio.wait_for(
                        engine._subprocess.forward("GET", "/next"), 1
                    )
                ).text == "ok"
            finally:
                if response is not None:
                    close = getattr(response.body_iterator, "aclose", None)
                    if close is not None:
                        await close()
                await engine.aclose()
                server.close()
                await server.wait_closed()
                for task in handlers:
                    if task is not None and not task.done():
                        task.cancel()
                await asyncio.gather(
                    *[task for task in handlers if task is not None],
                    return_exceptions=True,
                )

        asyncio.run(_run())

    def test_multiline_typed_event_and_unknown_event_are_preserved(self):
        import json

        import httpx

        from aibrix.openai_frontend.engine.proxy_engine import ProxyEngine
        from aibrix.openai_frontend.schemas.openai import (
            CreateChatCompletionStreamResponse,
        )

        async def _run():
            payload = {
                "id": "chat-1",
                "choices": [],
                "created": 1,
                "model": "test",
                "object": "chat.completion.chunk",
                "vendor_extra": True,
            }
            lines = json.dumps(payload, indent=2).splitlines()
            typed_event = (
                "event: message\nid: 7\nretry: 1000\n"
                + "\n".join(f"data: {line}" for line in lines)
                + "\n\n"
            )
            unknown_event = 'event: vendor\ndata: {"vendor":\ndata: true}\n\n'

            class Body(httpx.AsyncByteStream):
                async def __aiter__(self):
                    yield (typed_event + unknown_event + "data: [DONE]").encode()

            engine = ProxyEngine(engine_cmd=["unused"])
            response = httpx.Response(200, stream=Body())
            try:
                events = [
                    event
                    async for event in engine._typed_stream_events(
                        response, CreateChatCompletionStreamResponse, "test", "chat"
                    )
                ]
                assert len(events) == 3
                assert events[0].startswith(
                    "event: message\nid: 7\nretry: 1000\ndata: "
                )
                assert json.loads(events[0].split("data: ", 1)[1]) == payload
                assert events[1] == unknown_event
                assert events[2] == "data: [DONE]\n\n"
                assert response.is_closed
            finally:
                await engine.aclose()

        asyncio.run(_run())

    def test_complete_event_is_delivered_before_next_chunk(self):
        import httpx

        from aibrix.openai_frontend.engine.proxy_engine import ProxyEngine
        from aibrix.openai_frontend.schemas.openai import (
            CreateChatCompletionStreamResponse,
        )

        async def _run():
            release = asyncio.Event()

            class PausedBody(httpx.AsyncByteStream):
                async def __aiter__(self):
                    yield b': keepalive\n\nevent: vendor\nid: 7\ndata: {"vendor_event":1}\n\n'
                    await release.wait()
                    yield b"data: [DONE]\n\n"

            engine = ProxyEngine(engine_cmd=["unused"])
            response = httpx.Response(200, stream=PausedBody())
            stream = engine._typed_stream_events(
                response, CreateChatCompletionStreamResponse, "test", "chat"
            )
            try:
                assert await asyncio.wait_for(anext(stream), 0.5) == ": keepalive\n\n"
                event = await asyncio.wait_for(anext(stream), 0.5)
                assert event == 'event: vendor\nid: 7\ndata: {"vendor_event":1}\n\n'
                release.set()
                assert [event async for event in stream] == ["data: [DONE]\n\n"]
            finally:
                release.set()
                await stream.aclose()
                await engine.aclose()

        asyncio.run(_run())


# ---------------------------------------------------------------------------
# E2E helpers
# ---------------------------------------------------------------------------


def _mock_engine_cmd(port: int) -> list[str]:
    mock_script = os.path.join(os.path.dirname(__file__), "mock_engine.py")
    return [sys.executable, mock_script, "--port", str(port)]


# ---------------------------------------------------------------------------
# E2E tests with real subprocess spawn
# ---------------------------------------------------------------------------


class TestProxyEngineE2E:
    def test_spawn_chat_streaming(self):
        """ProxyEngine spawns inner engine, streaming chat yields typed SSE chunks."""

        async def _async():
            inner_port = 18765

            from aibrix.openai_frontend.engine.proxy_engine import ProxyEngine

            spec = EngineSpec(name="test")
            engine = ProxyEngine(
                engine_cmd=_mock_engine_cmd(inner_port),
                engine_port=inner_port,
                engine_host="127.0.0.1",
                spec=spec,
            )

            await engine.start()

            from aibrix.openai_frontend.schemas.openai import (
                CreateChatCompletionRequest,
            )

            request = CreateChatCompletionRequest(
                model="test-model",
                messages=[{"role": "user", "content": "hi"}],
                stream=True,
            )

            try:
                events = []
                async for event in await engine.chat(request):
                    events.append(event)
                joined = "".join(events)
                assert "data: " in joined and joined.endswith("[DONE]\n\n")
                chunk = next(e for e in events if "data: " in e and "[DONE]" not in e)
                assert '"object":"chat.completion.chunk"' in chunk.replace(" ", "")
            finally:
                await engine.aclose()

        asyncio.run(_async())

    def test_spawn_models_forwarded(self):

        async def _async():
            inner_port = 18768

            from aibrix.openai_frontend.engine.proxy_engine import ProxyEngine

            spec = EngineSpec(name="test")
            engine = ProxyEngine(
                engine_cmd=_mock_engine_cmd(inner_port),
                engine_port=inner_port,
                engine_host="127.0.0.1",
                spec=spec,
            )

            await engine.start()
            try:
                models = await engine.models()
                assert len(models) == 1
                assert models[0].id == "test-model"
            finally:
                await engine.aclose()

        asyncio.run(_async())

    def test_spawn_metrics_combined(self):
        """ProxyEngine metrics() combines inner engine + proxy metrics."""

        async def _async():
            inner_port = 18769

            from aibrix.openai_frontend.engine.proxy_engine import ProxyEngine

            spec = EngineSpec(name="test")
            engine = ProxyEngine(
                engine_cmd=_mock_engine_cmd(inner_port),
                engine_port=inner_port,
                engine_host="127.0.0.1",
                spec=spec,
            )

            await engine.start()
            try:
                metrics_text = await engine.metrics()
                assert "proxy_requests_total" in metrics_text
                assert "proxy_engine_state" in metrics_text
                assert "proxy_fetch_total" in metrics_text
                assert "proxy_fetch_errors" in metrics_text
            finally:
                await engine.aclose()

        asyncio.run(_async())

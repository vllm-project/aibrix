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

"""Fault tolerance, media hijacking and output persistence of ProxyEngine.

Process tests spawn the real ``mock_engine.py`` subprocess and use its
``/_test/*`` hooks to crash, hang, or fork workers.
"""

from __future__ import annotations

import asyncio
import json
import os
import signal
import socket
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from typing import TYPE_CHECKING, Any, Dict, List, Optional

import httpx
import pytest

from aibrix.openai_frontend.common.artifact_fetcher import (
    ArtifactFetcher,
    ArtifactFetchError,
    FetchResult,
)
from aibrix.openai_frontend.proxy.engine_spec import (
    SGLANG_SPEC,
    TRTLLM_SPEC,
    VLLM_SPEC,
    EndpointSet,
    EngineSpec,
    ForwardKind,
)
from aibrix.openai_frontend.proxy.output_writer import OutputWriteError, OutputWriter
from aibrix.openai_frontend.proxy.process_tree import (
    describe_returncode,
    kill_tree,
    snapshot_tree,
)
from aibrix.openai_frontend.proxy.request_inspector import MediaURLInspector
from aibrix.openai_frontend.proxy.subprocess_engine import (
    EngineStartupError,
    SubprocessEngine,
)
from aibrix.openai_frontend.schemas.output import OutputRequest
from tests.openai_frontend.test_media_foundation import _StubDownloader

if TYPE_CHECKING:
    from aibrix.openai_frontend.engine.proxy_engine import ProxyEngine

psutil = pytest.importorskip("psutil")

MOCK = os.path.join(os.path.dirname(__file__), "mock_engine.py")
# Mock engine takes --allowed-local-media-path like vLLM, so the vLLM-style
# spawn args are exercised end to end.
MOCK_SPEC = EngineSpec(
    name="mock",
    supports_local_media=True,
    local_media_args=("--allowed-local-media-path", "{dir}"),
    endpoints=EndpointSet(
        (
            ("/v1/chat/completions", ForwardKind.SSE),
            ("/v1/completions", ForwardKind.SSE),
            ("/v1/embeddings", ForwardKind.JSON),
            ("/v1/models", ForwardKind.JSON),
            ("/v1/responses", ForwardKind.SSE),
            ("/v1/responses/{response_id}", ForwardKind.JSON),
            ("/v1/responses/{response_id}/cancel", ForwardKind.JSON),
            ("/v1/rerank", ForwardKind.JSON),
            ("/v1/audio/transcriptions", ForwardKind.MULTIPART),
            ("/v1/audio/translations", ForwardKind.MULTIPART),
            ("/v1/realtime", ForwardKind.WEBSOCKET),
        )
    ),
)


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _alive(pid: int) -> bool:
    try:
        return psutil.Process(pid).status() != psutil.STATUS_ZOMBIE
    except psutil.NoSuchProcess:
        return False


def _wait_until(pred, timeout: float = 10.0, interval: float = 0.05) -> bool:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if pred():
            return True
        time.sleep(interval)
    return pred()


async def _await_until(pred, timeout: float = 20.0, interval: float = 0.05) -> bool:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if pred():
            return True
        await asyncio.sleep(interval)
    return pred()


def _make_engine(
    port: int,
    extra_cmd: Optional[List[str]] = None,
    *,
    spec: EngineSpec = MOCK_SPEC,
    health_check_interval: float = 0.05,
    shutdown_grace_seconds: float = 3.0,
    startup_timeout: Optional[float] = 60.0,
    fetch_media: Optional[bool] = None,
    media_dir: Optional[str] = None,
    artifact_fetcher: Any = None,
    materialize_inline_media: bool = False,
) -> ProxyEngine:
    from aibrix.openai_frontend.engine.proxy_engine import ProxyEngine

    return ProxyEngine(
        engine_cmd=[sys.executable, MOCK, *(extra_cmd or [])],
        engine_port=port,
        engine_host="127.0.0.1",
        spec=spec,
        health_check_interval=health_check_interval,
        shutdown_grace_seconds=shutdown_grace_seconds,
        startup_timeout=startup_timeout,
        fetch_media=fetch_media,
        media_dir=media_dir,
        artifact_fetcher=artifact_fetcher,
        materialize_inline_media=materialize_inline_media,
    )


def _mock_engine(upstream, **kwargs) -> ProxyEngine:
    """Attach a loop-owned mock transport; the test still owns aclose()."""
    from aibrix.openai_frontend.engine.proxy_engine import EngineState, ProxyEngine

    kwargs.setdefault("fetch_media", False)
    engine = ProxyEngine(["unused"], **kwargs)
    engine._set_state(EngineState.READY)
    engine._subprocess._client = httpx.AsyncClient(
        base_url="http://engine", transport=httpx.MockTransport(upstream)
    )
    engine._subprocess._client_loop = asyncio.get_running_loop()
    return engine


async def _post_inner(
    engine: ProxyEngine, path: str, body: dict
) -> Optional[httpx.Response]:
    async with httpx.AsyncClient(base_url=engine._subprocess.base_url) as c:
        try:
            return await c.post(path, json=body, timeout=5.0)
        except httpx.HTTPError:
            return None  # the crash hooks kill the server mid-response


# ---------------------------------------------------------------------------
# process_tree
# ---------------------------------------------------------------------------


class TestDescribeReturncode:
    def test_signals_and_exits(self):
        assert describe_returncode(None) == "running"
        assert describe_returncode(0) == "exited_code=0"
        assert "SIGKILL(9)" in describe_returncode(-9)
        assert "OOM" in describe_returncode(-9)
        assert "native/CUDA crash" in describe_returncode(-11)
        # Shell wrappers report a signalled child as 128+N.
        assert "128+SIGKILL(9)" in describe_returncode(137)


class TestKillTree:
    def test_without_psutil_kills_group_after_root_exit(self, monkeypatch):
        import subprocess

        from aibrix.openai_frontend.proxy import process_tree

        code = (
            "import subprocess,sys,time\n"
            "child=subprocess.Popen([sys.executable,'-c',"
            '"import signal,time; signal.signal(signal.SIGTERM,signal.SIG_IGN); '
            "print('ready',flush=True); time.sleep(60)\"],stdout=subprocess.PIPE,text=True)\n"
            "child.stdout.readline()\nprint(child.pid,flush=True)\ntime.sleep(60)"
        )
        root = subprocess.Popen(
            [sys.executable, "-c", code],
            start_new_session=True,
            stdout=subprocess.PIPE,
            text=True,
        )
        child = None
        try:
            assert root.stdout is not None
            child = psutil.Process(int(root.stdout.readline()))
            monkeypatch.setattr(process_tree, "HAS_PSUTIL", False)
            snap = process_tree.snapshot_tree(root.pid)
            root.kill()
            root.wait(5)
            assert child.is_running()
            process_tree.kill_tree(snap, grace_seconds=0.1, reap=lambda t: root.poll())
            assert _wait_until(
                lambda: (
                    not child.is_running() or child.status() == psutil.STATUS_ZOMBIE
                ),
                5,
            )
        finally:
            if child is not None:
                try:
                    child.kill()
                    child.wait(5)
                except psutil.NoSuchProcess:
                    pass
            if root.poll() is None:
                root.kill()
                root.wait(5)
            if root.stdout is not None:
                root.stdout.close()

    def _spawn_tree(self, new_session_grandchild: bool):
        import subprocess

        code = (
            "import subprocess, sys, time\n"
            "c = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(3600)'],"
            f" start_new_session={new_session_grandchild})\n"
            "print(c.pid, flush=True)\n"
            "time.sleep(3600)\n"
        )
        root = subprocess.Popen(
            [sys.executable, "-c", code],
            start_new_session=True,
            stdout=subprocess.PIPE,
            text=True,
        )
        assert root.stdout is not None
        child_pid = int(root.stdout.readline())
        return root, child_pid

    def test_kills_setsid_grandchild_after_root_crash(self):
        """A worker in its own session, orphaned by a root crash, is still
        found through the identity snapshot and killed."""
        root, child = self._spawn_tree(new_session_grandchild=True)
        try:
            snap = snapshot_tree(root.pid)
            assert child in snap.members
            os.kill(root.pid, signal.SIGKILL)
            root.wait(5)
            assert _alive(child), "grandchild should survive the root's death"
            survivors = kill_tree(snap, grace_seconds=2.0, reap=lambda t: None)
            assert survivors == []
            assert _wait_until(lambda: not _alive(child), 5)
        finally:
            for pid in (root.pid, child):
                try:
                    os.kill(pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            # Close the Popen pipe before reaping so its FileIO is not left
            # for the GC to close (which raises under strict resource flags).
            if root.stdout is not None:
                root.stdout.close()
            try:
                root.wait(timeout=5)
            except subprocess.TimeoutExpired:
                pass

    def test_never_signals_recycled_pid(self, monkeypatch):
        """Neither a stale PID nor its recycled process group may be signalled."""
        import subprocess
        from unittest.mock import Mock

        from aibrix.openai_frontend.proxy import process_tree

        bystander = subprocess.Popen(
            [sys.executable, "-c", "import time; time.sleep(60)"],
            start_new_session=True,
        )
        signal_group = Mock()
        monkeypatch.setattr(process_tree, "_signal_group", signal_group)
        try:
            snap = snapshot_tree(bystander.pid)
            snap.members[bystander.pid] = snap.members[bystander.pid] - 1000.0
            kill_tree(snap, grace_seconds=0.2, reap=lambda t: None)
            signal_group.assert_not_called()
            assert bystander.poll() is None, "recycled pid was killed"

            # A group we cannot inspect still exists; never infer it stopped.
            def denied_group(pgid, sig):
                raise PermissionError("cannot inspect group")

            monkeypatch.setattr(process_tree.os, "killpg", denied_group)
            assert process_tree._group_exists(bystander.pid) is True
        finally:
            bystander.kill()
            bystander.wait()

    def test_sigterm_ignoring_process_is_sigkilled(self):
        import subprocess

        proc = subprocess.Popen(
            [
                sys.executable,
                "-c",
                "import signal, time; signal.signal(signal.SIGTERM, signal.SIG_IGN);"
                " print('ready', flush=True); time.sleep(60)",
            ],
            start_new_session=True,
            stdout=subprocess.PIPE,
            text=True,
        )
        try:
            proc.stdout.readline()
            snap = snapshot_tree(proc.pid)
            t0 = time.monotonic()
            survivors = kill_tree(
                snap,
                grace_seconds=0.5,
                reap=lambda t: proc.wait(timeout=t) if t else proc.poll(),
            )
            assert survivors == []
            assert proc.wait(5) == -signal.SIGKILL
            assert time.monotonic() - t0 < 5
        finally:
            # Close the Popen pipe before reaping so its FileIO is not left
            # for the GC to close (which raises under strict resource flags).
            if proc.stdout is not None:
                proc.stdout.close()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                pass


# ---------------------------------------------------------------------------
# SubprocessEngine
# ---------------------------------------------------------------------------


class TestSubprocessEngine:
    @pytest.mark.parametrize("error_type", [httpx.RemoteProtocolError, httpx.ReadError])
    def test_wait_ready_retries_http_errors(self, monkeypatch, error_type):
        from unittest.mock import AsyncMock, Mock

        async def _run():
            eng = SubprocessEngine(cmd=["unused"])
            get = AsyncMock(
                side_effect=[
                    error_type("transient startup error"),
                    httpx.Response(503),
                    httpx.Response(200),
                ]
            )
            refresh = Mock()
            monkeypatch.setattr(eng, "is_alive", lambda: True)
            monkeypatch.setattr(eng, "refresh_snapshot", refresh)
            monkeypatch.setattr(httpx.AsyncClient, "get", get)
            await eng.wait_ready(timeout=5, poll_interval=0)
            assert get.await_count == 3
            refresh.assert_called_once()

        asyncio.run(_run())

    def test_wait_ready_persistent_protocol_error_times_out(self, monkeypatch):
        from types import SimpleNamespace
        from unittest.mock import AsyncMock

        from aibrix.openai_frontend.proxy import subprocess_engine

        async def _run():
            eng = SubprocessEngine(cmd=["unused"])
            ticks = iter([0.0, 0.5, 1.0])
            monkeypatch.setattr(
                subprocess_engine,
                "time",
                SimpleNamespace(monotonic=lambda: next(ticks)),
            )
            monkeypatch.setattr(eng, "is_alive", lambda: True)
            get = AsyncMock(side_effect=httpx.RemoteProtocolError("malformed response"))
            monkeypatch.setattr(httpx.AsyncClient, "get", get)
            with pytest.raises(EngineStartupError, match="not healthy after 1s"):
                await eng.wait_ready(timeout=1, poll_interval=0)
            assert get.await_count == 2

        asyncio.run(_run())

    def test_startup_failure_is_fast_and_explained(self):
        """An engine that dies during startup fails wait_ready immediately with
        the exit reason and its last output, not after the startup timeout."""

        async def _run():
            port = _free_port()
            eng = SubprocessEngine(
                cmd=[sys.executable, MOCK, "--exit-immediately", "3"],
                port=port,
                spec=EngineSpec(name="mock"),
            )
            eng.spawn()
            t0 = time.monotonic()
            with pytest.raises(EngineStartupError) as ei:
                await eng.wait_ready(timeout=60, poll_interval=0.05)
            assert time.monotonic() - t0 < 15
            msg = str(ei.value)
            assert "exited_code=3" in msg
            assert "CUDA out of memory" in msg
            eng.shutdown()

        asyncio.run(_run())

    @pytest.mark.skipif(os.name != "posix", reason="POSIX signal semantics")
    def test_engine_gets_default_sigterm_even_if_parent_blocks_it(self):
        """A SIGTERM mask / SIG_IGN inherited by the proxy (pytest, wrapper
        scripts) must not leak into the engine; otherwise every stop waits
        out the full grace period and ends in SIGKILL."""
        probe = (
            "import json, signal, sys, time\n"
            "print(json.dumps({'blocked': signal.SIGTERM in "
            "signal.pthread_sigmask(signal.SIG_BLOCK, []), "
            "'ignored': signal.getsignal(signal.SIGTERM) == signal.SIG_IGN, "
            "'argv': sys.argv[1:]}), flush=True)\n"
            "time.sleep(60)\n"
        )
        old_mask = signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGTERM})
        old_disp = signal.signal(signal.SIGTERM, signal.SIG_IGN)
        try:
            eng = SubprocessEngine(
                cmd=[sys.executable, "-c", probe],
                port=_free_port(),
                spec=EngineSpec(name="probe"),
                shutdown_grace_seconds=20.0,
            )
            eng.spawn()
        finally:
            signal.signal(signal.SIGTERM, old_disp)
            signal.pthread_sigmask(signal.SIG_SETMASK, old_mask)
        try:
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline and not any(
                line.startswith("{") for line in eng.log_tail()
            ):
                time.sleep(0.05)
            line = next(x for x in eng.log_tail() if x.startswith("{"))
            seen = json.loads(line[line.index("{") :])
            assert seen["blocked"] is False
            assert seen["ignored"] is False
            # The trampoline exec()s the engine in place: same pid, own argv.
            assert seen["argv"][-4:-2] == ["--host", "127.0.0.1"]
            assert psutil.Process(eng.pid).cmdline()[1:3] == ["-c", probe]
            t0 = time.monotonic()
            eng.shutdown()
            assert time.monotonic() - t0 < 5, "engine ignored SIGTERM"
        finally:
            eng.shutdown(grace_seconds=1.0)

    @pytest.mark.skipif(os.name != "posix", reason="POSIX signal semantics")
    def test_unexecutable_engine_cmd_fails_fast_with_reason(self):
        """With the trampoline in front, a bad command fails in the child; it
        must still surface as a fast, explained startup error."""
        old_mask = signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGTERM})
        try:
            eng = SubprocessEngine(
                cmd=["/nonexistent/inner-engine"],
                port=_free_port(),
                spec=EngineSpec(name="missing"),
            )
            eng.spawn()
        finally:
            signal.pthread_sigmask(signal.SIG_SETMASK, old_mask)

        async def _run():
            with pytest.raises(EngineStartupError) as ei:
                await eng.wait_ready(timeout=30, poll_interval=0.05)
            assert "exited_code=127" in str(ei.value)
            assert "cannot exec inner engine" in str(ei.value)

        try:
            asyncio.run(_run())
        finally:
            eng.shutdown()

    def test_refuses_port_owned_by_another_process(self):
        with socket.socket() as squatter:
            squatter.bind(("127.0.0.1", 0))
            squatter.listen()
            port = squatter.getsockname()[1]
            eng = SubprocessEngine(
                cmd=[sys.executable, MOCK],
                port=port,
                spec=EngineSpec(name="mock"),
                port_release_timeout=0.3,
            )
            with pytest.raises(EngineStartupError, match="already in use"):
                eng.spawn()
            assert eng.pid is None

    def test_log_pipe_survives_binary_and_flood(self):
        """Non-UTF-8 bytes and >64 KiB of output must neither crash the pipe
        thread nor block the child (the old pipe thread died on bytes)."""
        # 150 lines x 1000 bytes = ~150 KiB, well past the 64 KiB pipe buffer,
        # yet under the 200-line tail so every marker stays observable.
        code = (
            "import sys\n"
            "sys.stdout.buffer.write(b'bad \\xff\\xfe bytes\\n'); sys.stdout.flush()\n"
            "sys.stderr.write(('y' * 1000 + '\\n') * 150); sys.stderr.flush()\n"
            "print('DONE', flush=True)\n"
        )
        eng = SubprocessEngine(
            cmd=[sys.executable, "-c", code, "--"],
            port=_free_port(),
            spec=EngineSpec(name="t", host_arg="--h", port_arg="--p"),
        )
        eng.spawn()
        assert eng._proc.wait(timeout=10) == 0
        eng._join_pipe_threads(5)
        tail = eng.log_tail(200)
        assert "DONE" in tail
        assert any("bad" in line and "\ufffd" in line for line in tail)
        eng.shutdown()

    def test_closed_engine_cannot_spawn(self, monkeypatch):
        import subprocess
        from unittest.mock import Mock

        eng = SubprocessEngine(cmd=["unused"])
        eng.shutdown()
        popen = Mock(side_effect=AssertionError("attempted to spawn after shutdown"))
        monkeypatch.setattr(subprocess, "Popen", popen)
        with pytest.raises(RuntimeError, match="shut down"):
            eng.spawn()
        popen.assert_not_called()


# ---------------------------------------------------------------------------
# ProxyEngine supervision (real subprocess)
# ---------------------------------------------------------------------------


class TestProxyEngineSupervision:
    @pytest.mark.asyncio
    @pytest.mark.parametrize("with_worker", [False, True])
    async def test_crash_fails_requests_and_reaps_tree(self, with_worker):
        from aibrix.openai_frontend.engine.proxy_engine import EngineState
        from aibrix.openai_frontend.schemas.openai import CreateChatCompletionRequest
        from aibrix.openai_frontend.utils.utils import EngineUnavailableError

        engine = _make_engine(_free_port())
        worker = None
        await engine.start()
        try:
            root = engine._subprocess.pid
            assert engine.state == EngineState.READY
            if with_worker:
                response = await _post_inner(
                    engine, "/_test/spawn_worker", {"new_session": True}
                )
                assert response is not None
                worker = response.json()["pid"]
                assert await _await_until(
                    lambda: worker in engine._subprocess.snapshot.members, 5
                )
            await _post_inner(engine, "/_test/crash", {"signal": 9})
            assert await _await_until(lambda: engine.state == EngineState.FAILED, 10)
            assert "SIGKILL" in engine._last_exit
            assert not engine._subprocess.is_alive() and not await engine.health()
            assert engine._subprocess.pid in (None, root), "engine was restarted"
            if worker is not None:
                assert await _await_until(lambda: not _alive(worker), 10)
            with pytest.raises(EngineUnavailableError):
                await engine.chat(
                    CreateChatCompletionRequest(
                        model="m", messages=[{"role": "user", "content": "x"}]
                    )
                )
        finally:
            await engine.aclose()
            if worker and _alive(worker):
                os.kill(worker, signal.SIGKILL)

    def test_double_aclose_reaps_started_tree_exactly_once(self, monkeypatch):
        """Idempotency on a live engine: repeated aclose() must share one
        teardown, must not re-kill the tree, and must not return before the
        process (including an own-session worker) is actually reaped."""

        async def _run():
            from unittest.mock import Mock

            from aibrix.openai_frontend.engine.proxy_engine import EngineState

            engine = _make_engine(_free_port())
            await engine.start()
            resp = await _post_inner(
                engine, "/_test/spawn_worker", {"new_session": True}
            )
            assert resp is not None
            worker = resp.json()["pid"]
            root = engine._subprocess.pid
            assert await _await_until(
                lambda: worker in engine._subprocess.snapshot.members, 5
            )
            shutdown = Mock(wraps=engine._subprocess.shutdown)
            monkeypatch.setattr(engine._subprocess, "shutdown", shutdown)
            await engine.aclose()
            # By the time aclose() returned the whole tree is already gone;
            # a second call shares the finished close task and does no more
            # process teardown.
            assert not _alive(root)
            assert not _alive(worker)
            await engine.aclose()
            shutdown.assert_called_once()
            assert engine.state == EngineState.STOPPED
            assert engine._subprocess._proc is None
            assert engine._subprocess._client is None

        asyncio.run(_run())

    @pytest.mark.asyncio
    async def test_startup_failure_reaps_half_started_tree(self):
        """A failed start reaps its child and supports idempotent close."""
        from aibrix.openai_frontend.engine.proxy_engine import EngineState

        engine = _make_engine(
            _free_port(), extra_cmd=["--exit-immediately", "3"], startup_timeout=10.0
        )
        try:
            with pytest.raises(EngineStartupError):
                await engine.start()
            assert engine.state == EngineState.FAILED
            assert engine._subprocess.pid is None
            assert engine._subprocess._proc is None
            assert engine._subprocess._client is None
            await engine.aclose()
            assert engine.state == EngineState.STOPPED
            assert engine._close_task is not None and engine._close_task.done()
        finally:
            await engine.aclose()


# ---------------------------------------------------------------------------
# Media URL hijacking
# ---------------------------------------------------------------------------


class TestProxyFetchBytes:
    @pytest.mark.parametrize("reject_media", [False, True])
    def test_actual_bytes_and_deduplication_before_upstream_response(
        self, monkeypatch, tmp_path, reject_media
    ):
        from aibrix.openai_frontend.engine.proxy_engine import EngineState, ProxyEngine
        from aibrix.openai_frontend.schemas.openai import CreateChatCompletionRequest

        stub = _StubDownloader(payload=b"REMOTE-MEDIA")
        monkeypatch.setattr(
            "aibrix.openai_frontend.common.artifact_fetcher.get_downloader",
            lambda url: stub,
        )

        async def _run():
            engine = ProxyEngine(
                engine_cmd=["unused"],
                spec=TRTLLM_SPEC,
                media_dir=str(tmp_path),
                materialize_inline_media=True,
            )
            engine._set_state(EngineState.READY)
            parts = [
                {"type": "video_url", "video_url": {"url": "https://h/clip.mp4"}},
                {"type": "video_url", "video_url": {"url": "https://h/clip.mp4"}},
                {
                    "type": "audio_url",
                    "audio_url": {"url": "data:audio/wav;base64,SU5MSU5FLU1FRElB"},
                },
            ]
            request = CreateChatCompletionRequest(
                model="m", messages=[{"role": "user", "content": parts}]
            )

            async def forward(method, endpoint, *, json=None, stream=False):
                received = json["messages"][0]["content"]
                video = received[0]["video_url"]["url"]
                assert video == received[1]["video_url"]["url"]
                assert Path(video.removeprefix("file://")).read_bytes() == stub.payload
                audio = received[2]["audio_url"]["url"]
                assert (
                    Path(audio.removeprefix("file://")).read_bytes() == b"INLINE-MEDIA"
                )
                return httpx.Response(
                    415 if reject_media else 200,
                    request=httpx.Request(method, "http://engine" + endpoint),
                    json={"error": {"message": "unsupported media"}}
                    if reject_media
                    else {
                        "id": "chat-1",
                        "object": "chat.completion",
                        "created": 1,
                        "model": "m",
                        "choices": [],
                    },
                )

            monkeypatch.setattr(engine._subprocess, "forward", forward)
            try:
                for count in (1, 2):
                    if reject_media:
                        with pytest.raises(httpx.HTTPStatusError) as exc:
                            await engine.chat(request)
                        assert exc.value.response.status_code == 415
                        assert exc.value.response.json() == {
                            "error": {"message": "unsupported media"}
                        }
                    else:
                        await engine.chat(request)
                    assert engine._metrics_registry.get_sample_value(
                        "proxy_fetch_bytes_total"
                    ) == count * (len(stub.payload) + len(b"INLINE-MEDIA"))
                    assert (
                        engine._metrics_registry.get_sample_value("proxy_fetch_total")
                        == count * 2
                    )
                    assert not list(tmp_path.iterdir())
                assert len(stub.calls) == 2
                assert "proxy_fetch_bytes_total" in await engine.metrics()
            finally:
                await engine.aclose()

        asyncio.run(_run())

    def test_failed_fetch_and_passthrough_do_not_count_bytes(
        self, monkeypatch, tmp_path
    ):
        from aibrix.openai_frontend.engine.proxy_engine import ProxyEngine
        from aibrix.openai_frontend.utils.utils import ClientError

        async def _run():
            engine = ProxyEngine(
                engine_cmd=["unused"], media_dir=str(tmp_path), fetch_media=True
            )
            try:

                def unsupported(url):
                    raise ValueError("unsupported URL")

                monkeypatch.setattr(
                    "aibrix.openai_frontend.common.artifact_fetcher.get_downloader",
                    unsupported,
                )
                result = await engine._fetcher.fetch("unknown://media")
                assert result.local_path == "unknown://media" and result.size_bytes == 0
                await engine._fetch_media_for(
                    "test",
                    {
                        "input": [
                            {"type": "input_image", "image_url": "file:///existing.png"}
                        ]
                    },
                    MediaURLInspector(),
                )
                assert (
                    engine._metrics_registry.get_sample_value("proxy_fetch_bytes_total")
                    == 0
                )
                monkeypatch.setattr(
                    "aibrix.openai_frontend.common.artifact_fetcher.get_downloader",
                    lambda url: _StubDownloader(fail=True),
                )
                with pytest.raises(ClientError, match="403"):
                    await engine._fetch_media_for(
                        "test",
                        {
                            "input": [
                                {"type": "input_image", "image_url": "https://h/x.png"}
                            ]
                        },
                        MediaURLInspector(),
                    )
                assert (
                    engine._metrics_registry.get_sample_value("proxy_fetch_bytes_total")
                    == 0
                )
                assert (
                    engine._metrics_registry.get_sample_value(
                        "proxy_fetch_errors_total"
                    )
                    == 1
                )
                assert not list(tmp_path.iterdir())
            finally:
                await engine.aclose()

        asyncio.run(_run())


class _DirFetcher(ArtifactFetcher):
    """Writes each 'downloaded' URL to a real file under the media dir."""

    def __init__(self, media_dir: str, fail_on: Optional[str] = None):
        self.media_dir = media_dir
        self.fail_on = fail_on
        self.cleaned: List[str] = []

    async def fetch(self, url, credentials=None):
        if url == self.fail_on:
            raise ArtifactFetchError(url, "404 Not Found")
        d = tempfile.mkdtemp(dir=self.media_dir)
        p = Path(d) / os.path.basename(url)
        p.write_text(f"content-of:{url}")
        return FetchResult(original_url=url, local_path=p.as_uri(), temp_dir=None)

    async def fetch_many(self, urls, credentials_map=None):
        return [await self.fetch(u) for u in urls]

    async def cleanup(self, results):
        for r in results:
            path = Path(r.local_path[len("file://") :])
            self.cleaned.append(str(path))
            path.unlink(missing_ok=True)


class TestProxyEngineMediaE2E:
    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "mode", ["remote", "inline", "invalid_inline", "missing", "disabled"]
    )
    async def test_media_rewriting_and_cleanup(self, tmp_path, mode):
        from aibrix.openai_frontend.schemas.openai import CreateChatCompletionRequest
        from aibrix.openai_frontend.utils.utils import ClientError

        media_dir = tmp_path / "media"
        media_dir.mkdir()
        fetcher = _DirFetcher(str(media_dir), fail_on="tos://b/missing.png")
        engine = _make_engine(
            _free_port(),
            media_dir=str(media_dir) if mode != "disabled" else None,
            fetch_media=mode != "disabled",
            artifact_fetcher=fetcher if mode in {"remote", "missing"} else None,
            materialize_inline_media=mode in {"inline", "invalid_inline"},
        )
        urls = {
            "remote": [
                "tos://b/cat.png",
                "s3://b/clip.mp4",
                "data:image/png;base64,AA",
            ],
            "inline": ["data:image/png;base64,SU5MSU5FLU1FRElB"],
            "invalid_inline": ["data:image/png;base64,***"],
            "missing": ["tos://b/missing.png"],
            "disabled": ["https://h/x.png"],
        }[mode]
        parts = [{"type": "image_url", "image_url": {"url": url}} for url in urls]
        if mode == "remote":
            parts[1] = {"type": "video_url", "video_url": {"url": urls[1]}}
        request = CreateChatCompletionRequest(
            model="m", messages=[{"role": "user", "content": parts}], top_k=5
        )
        if mode == "disabled":
            assert engine._media_dir is None
            assert "--allowed-local-media-path" not in engine._subprocess._cmd
        await engine.start()
        try:
            if mode in {"invalid_inline", "missing"}:
                with pytest.raises(
                    ClientError,
                    match="invalid base64" if mode == "invalid_inline" else "404",
                ):
                    await engine.chat(request)
            else:
                body = (await engine.chat(request)).model_dump()
                media = body["media"]
                assert body["received"]["top_k"] == 5
                if mode == "remote":
                    assert [item.get("content") for item in media[:2]] == [
                        "content-of:tos://b/cat.png",
                        "content-of:s3://b/clip.mp4",
                    ]
                    assert all("error" not in item for item in media[:2])
                    assert media[2] == {"url": urls[2], "local": False}
                    assert len(fetcher.cleaned) == 2
                    assert all(not Path(path).exists() for path in fetcher.cleaned)
                elif mode == "inline":
                    assert len(media) == 1 and media[0]["local"] is True
                    assert media[0]["url"].startswith(media_dir.resolve().as_uri())
                    assert media[0]["content"] == "INLINE-MEDIA"
                else:
                    assert media == [{"url": urls[0], "local": False}]
            assert not engine._in_flight
            assert not any(media_dir.rglob("inline_*"))
        finally:
            await engine.aclose()

    def test_concurrent_aclose_and_cancel_releases_media_exactly_once(self, tmp_path):
        """Two concurrent waiters on the shared close task: one cancelled
        mid-wait, the other still awaiting. Cancellation must not cancel the
        shared cleanup task; media cleanup must run exactly once."""

        async def _run():
            from unittest.mock import patch

            from aibrix.openai_frontend.schemas.openai import (
                CreateChatCompletionRequest,
            )

            media_dir = str(tmp_path / "media")
            os.makedirs(media_dir)
            engine = _make_engine(
                _free_port(),
                media_dir=media_dir,
                materialize_inline_media=True,
            )
            await engine.start()
            try:
                req = CreateChatCompletionRequest(
                    model="m",
                    stream=True,
                    messages=[
                        {
                            "role": "user",
                            "content": [
                                {
                                    "type": "image_url",
                                    "image_url": {
                                        "url": "data:image/png;base64,SU5MSU5FLU1FRElB"
                                    },
                                }
                            ],
                        }
                    ],
                )
                stream = await engine.chat(req)

                cleanup_calls = []
                cleanup_entered = asyncio.Event()
                cleanup_release = asyncio.Event()
                real_cleanup = engine._fetcher.cleanup

                async def spy_cleanup(results):
                    cleanup_calls.append(list(results))
                    cleanup_entered.set()
                    await cleanup_release.wait()
                    await real_cleanup(results)

                with patch.object(engine._fetcher, "cleanup", spy_cleanup):
                    t1 = asyncio.create_task(stream.aclose())
                    await asyncio.wait_for(cleanup_entered.wait(), 1)
                    # t1 is now awaiting shield(close_task); cancel it.
                    t1.cancel()
                    with pytest.raises(asyncio.CancelledError):
                        await t1
                    # The shared close_task must still be running; t2 joins.
                    t2 = asyncio.create_task(stream.aclose())
                    await asyncio.sleep(0.05)
                    assert not t2.done()
                    cleanup_release.set()
                    await asyncio.wait_for(t2, 1)

                assert len(cleanup_calls) == 1, (
                    f"media cleanup ran {len(cleanup_calls)} times"
                )
                assert not list(Path(media_dir).rglob("inline_*"))
                assert engine._in_flight == {}
            finally:
                await engine.aclose()

        asyncio.run(_run())


# ---------------------------------------------------------------------------
# Output persistence
# ---------------------------------------------------------------------------


class _FakeStorage:
    def __init__(self, kind: str, bucket: str):
        from aibrix.storage.types import StorageType

        self._type = StorageType(kind)
        self.bucket_name = bucket
        self.objects: Dict[str, bytes] = {}

    def get_type(self):
        return self._type

    async def put_object(self, key, data, content_type=None, **kw):
        self.objects[key] = data if isinstance(data, bytes) else data.read()
        return True


class TestOutputReviewRegressions:
    @pytest.mark.asyncio
    async def test_shutdown_waits_for_download_thread(self, monkeypatch, tmp_path):
        import threading

        from aibrix.openai_frontend.engine.proxy_engine import EngineState, ProxyEngine
        from aibrix.openai_frontend.schemas.openai import CreateChatCompletionRequest
        from aibrix.runtime.downloaders import S3ArtifactDownloader

        started, release, finished = (threading.Event() for _ in range(3))
        downloader = S3ArtifactDownloader()

        def download(url, local_path, credentials=None):
            started.set()
            try:
                assert release.wait(5)
                (Path(local_path) / "media.png").write_bytes(b"PIXELS")
                return local_path
            finally:
                finished.set()

        monkeypatch.setattr(downloader, "_download_sync", download)
        monkeypatch.setattr(
            "aibrix.openai_frontend.common.artifact_fetcher.get_downloader",
            lambda url: downloader,
        )
        engine = ProxyEngine(["unused"], fetch_media=True, media_dir=str(tmp_path))
        engine._set_state(EngineState.READY)
        monkeypatch.setattr(engine._subprocess, "shutdown", lambda: None)

        async def close_client():
            pass

        monkeypatch.setattr(engine._subprocess, "close_client", close_client)
        request = CreateChatCompletionRequest(
            model="m",
            messages=[
                {
                    "role": "user",
                    "content": [
                        {"type": "image_url", "image_url": {"url": "s3://b/media.png"}}
                    ],
                }
            ],
        )
        task = asyncio.create_task(engine.chat(request))
        closing = None
        try:
            assert await asyncio.to_thread(started.wait, 5)
            closing = asyncio.create_task(engine.aclose())
            await asyncio.sleep(0.05)
            assert not closing.done()
            assert engine._fetcher._active_tmpdirs
            assert list(tmp_path.iterdir())
            release.set()
            with pytest.raises(asyncio.CancelledError):
                await asyncio.wait_for(task, 5)
            await asyncio.wait_for(closing, 5)
            assert finished.is_set()
            assert not list(tmp_path.iterdir())
            assert not engine._in_flight_tasks
            assert not engine._in_flight
        finally:
            release.set()
            await asyncio.gather(task, return_exceptions=True)
            await engine.aclose()
            if closing is not None:
                await closing

    @pytest.mark.asyncio
    @pytest.mark.parametrize("stream", [False, True])
    async def test_shutdown_drains_request_cleanup(self, monkeypatch, tmp_path, stream):
        from aibrix.openai_frontend.engine.proxy_engine import EngineState, ProxyEngine
        from aibrix.openai_frontend.schemas.openai import CreateChatCompletionRequest

        engine = ProxyEngine(
            ["unused"],
            fetch_media=True,
            media_dir=str(tmp_path),
            materialize_inline_media=True,
        )
        engine._set_state(EngineState.READY)
        entered, release = asyncio.Event(), asyncio.Event()
        real_cleanup = engine._fetcher.cleanup
        calls = 0

        async def cleanup(results):
            nonlocal calls
            calls += 1
            entered.set()
            await release.wait()
            await real_cleanup(results)

        async def forward(*args, **kwargs):
            return httpx.Response(
                200,
                json={
                    "id": "c",
                    "object": "chat.completion",
                    "created": 1,
                    "model": "m",
                    "choices": [],
                },
                request=httpx.Request("POST", "http://engine/chat"),
            )

        async def close_client():
            pass

        monkeypatch.setattr(engine._fetcher, "cleanup", cleanup)
        monkeypatch.setattr(engine, "_forward", forward)
        monkeypatch.setattr(engine._subprocess, "shutdown", lambda: None)
        monkeypatch.setattr(engine._subprocess, "close_client", close_client)
        request = CreateChatCompletionRequest(
            model="m",
            stream=stream,
            messages=[
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "image_url",
                            "image_url": {"url": "data:image/png;base64,UElYRUxT"},
                        }
                    ],
                }
            ],
        )
        task = asyncio.create_task(engine.chat(request))
        closing = None
        response = None
        try:
            if stream:
                response = await asyncio.wait_for(task, 5)
                closing = asyncio.create_task(engine.aclose())
            await asyncio.wait_for(entered.wait(), 5)
            if not stream:
                task.cancel("first cancellation during cleanup")
                await asyncio.sleep(0)
                task.cancel("second cancellation during cleanup")
                closing = asyncio.create_task(engine.aclose())
            await asyncio.sleep(0.05)
            assert not closing.done()
            assert list(tmp_path.rglob("inline_*"))
            release.set()
            if not stream:
                with pytest.raises(asyncio.CancelledError):
                    await asyncio.wait_for(task, 5)
            await asyncio.wait_for(closing, 5)
            if response is not None:
                await response.aclose()
            assert calls == 1
            assert not list(tmp_path.iterdir())
            assert not engine._in_flight
            assert not engine._in_flight_tasks
        finally:
            release.set()
            await asyncio.gather(task, return_exceptions=True)
            if response is not None:
                await response.aclose()
            await engine.aclose()
            if closing is not None:
                await closing


class TestOutputWriter:
    @pytest.mark.asyncio
    @pytest.mark.parametrize("outcome", ["success", "read_error", "cancel"])
    async def test_tos_output_read_is_off_loop_and_settles_before_cleanup(
        self, monkeypatch, tmp_path, outcome
    ):
        import io
        import threading
        from types import SimpleNamespace

        from aibrix.storage import tos as tos_storage

        main_thread = threading.get_ident()
        entered, release, finished = (
            threading.Event(),
            threading.Event(),
            threading.Event(),
        )
        readers, uploaded = [], []
        fdopen = os.fdopen

        class Reader(io.BufferedReader):
            def read(self, *args):
                assert threading.get_ident() != main_thread, (
                    "TOS media read blocked the event loop"
                )
                entered.set()
                try:
                    assert release.wait(5)
                    if outcome == "read_error":
                        raise OSError("media read failed")
                    return super().read(*args)
                finally:
                    finished.set()

        def open_reader(fd, *args, **kwargs):
            reader = Reader(fdopen(fd, *args, **kwargs).detach())
            readers.append(reader)
            return reader

        def put_object(**kwargs):
            uploaded.append(kwargs["content"].read())

        monkeypatch.setattr(
            tos_storage.tos,
            "TosClientV2",
            lambda *args: SimpleNamespace(put_object=put_object),
        )
        store = tos_storage.TOSStorage("b", "test", "test", "test", "test")
        writer = OutputWriter(storage_factory=lambda: store)
        source = tmp_path / "video.mp4"
        source.write_bytes(b"video")
        monkeypatch.setattr(os, "fdopen", open_reader)
        task = asyncio.create_task(
            writer.write_bundle(
                OutputRequest(url="tos://b/out"), None, "job.json", [str(source)]
            )
        )
        try:
            assert await asyncio.to_thread(entered.wait, 2)
            if outcome == "cancel":
                task.cancel()
                await asyncio.sleep(0)
                task.cancel()
            await asyncio.sleep(0.01)
            assert not task.done() and source.exists() and not readers[0].closed
            release.set()
            if outcome == "success":
                result, total = await task
                assert result.artifacts == ["job-0.mp4"] and total == 5
                assert uploaded == [b"video"]
            else:
                with pytest.raises(
                    asyncio.CancelledError if outcome == "cancel" else OutputWriteError
                ):
                    await task
                assert not uploaded
            assert finished.is_set() and readers[0].closed and not source.exists()
        finally:
            release.set()
            await asyncio.gather(task, return_exceptions=True)

    @pytest.mark.asyncio
    async def test_artifact_outside_destination_is_wrapped(self, monkeypatch, tmp_path):
        writer = OutputWriter()

        async def misplaced(*args):
            return str(tmp_path / "elsewhere.json")

        monkeypatch.setattr(writer, "_write_local", misplaced)
        with pytest.raises(OutputWriteError) as caught:
            await writer.write(
                OutputRequest(url=str(tmp_path / "out")), b"{}", "job.json"
            )
        assert isinstance(caught.value.__cause__, ValueError)

    @pytest.mark.asyncio
    @pytest.mark.parametrize("cancel", [False, True])
    async def test_failed_bundle_cleans_sources_after_writer_finishes(
        self, monkeypatch, tmp_path, cancel
    ):
        import threading

        started, release, finished = (
            threading.Event(),
            threading.Event(),
            threading.Event(),
        )
        source = tmp_path / "video.mp4"
        source.write_bytes(b"video")
        store = _FakeStorage("s3", "b")
        cleanup_started, cleanup_release = threading.Event(), threading.Event()
        reader_used = []

        async def put(key, data, **kwargs):
            assert isinstance(data, bytes)
            if key == "out/r.json":
                store.objects[key] = data
                return True

            def blocked():
                started.set()
                try:
                    assert release.wait(5)
                    assert data == b"video"
                    raise OSError("upload failed")
                finally:
                    finished.set()

            return await asyncio.to_thread(blocked)

        monkeypatch.setattr(store, "put_object", put)
        writer = OutputWriter(storage_factory=lambda: store)
        write_object_store = writer._write_object_store

        async def capture_reader(*args):
            if not isinstance(args[4], bytes):
                reader_used.append(args[4])
            return await write_object_store(*args)

        monkeypatch.setattr(writer, "_write_object_store", capture_reader)
        remove_sources = writer._remove_sources

        def cleanup(*args):
            assert finished.is_set()
            assert reader_used[0].closed
            cleanup_started.set()
            assert cleanup_release.wait(5)
            remove_sources(*args)

        monkeypatch.setattr(writer, "_remove_sources", cleanup)
        task = asyncio.create_task(
            writer.write_bundle(
                OutputRequest(url="s3://b/out"), b"{}", "r.json", [str(source)]
            )
        )
        try:
            assert await asyncio.to_thread(started.wait, 5)
            if cancel:
                task.cancel()
                await asyncio.sleep(0)
                task.cancel()
                await asyncio.sleep(0.02)
                assert not task.done()
            assert source.exists()
            release.set()
            assert await asyncio.to_thread(cleanup_started.wait, 5)
            if cancel:
                task.cancel()
                await asyncio.sleep(0)
                task.cancel()
            await asyncio.sleep(0.02)
            assert not task.done()
            assert source.exists()
            cleanup_release.set()
            with pytest.raises(
                asyncio.CancelledError if cancel else OutputWriteError,
                match=None if cancel else "upload failed",
            ):
                await asyncio.wait_for(task, 5)
            assert finished.is_set()
            assert not source.exists()
            assert store.objects == {"out/r.json": b"{}"}
        finally:
            release.set()
            cleanup_release.set()
            await asyncio.gather(task, return_exceptions=True)

    @pytest.mark.asyncio
    @pytest.mark.parametrize("failure", ["missing", "stat", "cleanup"])
    async def test_bundle_failure_cleans_siblings_and_preserves_primary(
        self, monkeypatch, tmp_path, failure
    ):
        sources = [tmp_path / "first.mp4", tmp_path / "second.mp4"]
        for path in sources:
            path.write_bytes(b"video")
        writer = OutputWriter()
        lstat, unlink = Path.lstat, Path.unlink
        if failure == "missing":
            sources.insert(0, tmp_path / "missing.mp4")
            message = "Engine output is missing"
        elif failure == "stat":

            def inspect(path, *args, **kwargs):
                if path == sources[0]:
                    raise PermissionError("first stat denied")
                return lstat(path, *args, **kwargs)

            monkeypatch.setattr(Path, "lstat", inspect)
            message = "first stat denied"
        else:

            async def fail(*args, **kwargs):
                raise OutputWriteError("primary upload failure")

            def remove(path, *args, **kwargs):
                if path == sources[0]:
                    raise PermissionError("cleanup denied")
                return unlink(path, *args, **kwargs)

            monkeypatch.setattr(writer, "write", fail)
            monkeypatch.setattr(Path, "unlink", remove)
            message = "primary upload failure"
        with pytest.raises(OutputWriteError, match=message):
            await writer.write_bundle(
                OutputRequest(url=str(tmp_path / "out")),
                None,
                "r.json",
                [str(path) for path in sources],
            )
        assert not sources[-1].exists()
        if failure == "missing":
            assert not sources[1].exists()
        else:
            assert lstat(sources[0])
            unlink(sources[0])

    @pytest.mark.asyncio
    @pytest.mark.parametrize("retry", [False, True])
    async def test_storage_initialization_shared_and_nonblocking(self, retry):
        import threading

        started, release = threading.Event(), threading.Event()
        store = _FakeStorage("s3", "b")
        calls = 0

        def factory():
            nonlocal calls
            calls += 1
            if retry and calls == 1:
                raise OSError("initialization failed")
            started.set()
            assert release.wait(5)
            return store

        writer = OutputWriter(storage_factory=factory)
        spec = OutputRequest(url="s3://b/out")
        if retry:
            with pytest.raises(OutputWriteError, match="initialization failed"):
                await writer.write(spec, b"{}", "failed.json")
        expected_calls = 2 if retry else 1
        first = asyncio.create_task(writer.write(spec, b"one", "1.json"))
        second = None
        try:
            assert await asyncio.to_thread(started.wait, 5)
            second = asyncio.create_task(writer.write(spec, b"two", "2.json"))
            await asyncio.sleep(0)
            first.cancel()
            with pytest.raises(asyncio.CancelledError):
                await first
            assert calls == expected_calls
            release.set()
            await asyncio.wait_for(second, 5)
            await writer.write(spec, b"three", "3.json")
            assert calls == expected_calls
            assert store.objects == {"out/2.json": b"two", "out/3.json": b"three"}
        finally:
            release.set()
            await asyncio.gather(
                first, *([second] if second is not None else []), return_exceptions=True
            )

    def test_scheme_or_bucket_mismatch_is_rejected(self):
        w = OutputWriter(storage_factory=lambda: _FakeStorage("tos", "bkt"))
        with pytest.raises(OutputWriteError, match="STORAGE_TYPE=s3"):
            asyncio.run(w.write(OutputRequest(url="s3://bkt/p"), b"x", "r.json"))
        with pytest.raises(OutputWriteError, match="does not match"):
            asyncio.run(w.write(OutputRequest(url="tos://other/p"), b"x", "r.json"))

    def test_local_directory(self, tmp_path):
        from urllib.parse import unquote, urlparse

        from pydantic import ValidationError

        w = OutputWriter(storage_factory=lambda: pytest.fail("env storage not used"))
        cases = [
            (str(tmp_path / "a"), "sub/out.json"),
            ((tmp_path / "b").as_uri(), "other.json"),
            (str(tmp_path / "c"), "chatcmpl-é.json"),
            (str(tmp_path / "d"), "extensionless"),
            ((tmp_path / "space é %20").as_uri(), "chatcmpl-é.json"),
            (str(tmp_path / "literal%20"), "result.json"),
            (str(tmp_path / "literal?tag=x#fragment"), "result.json"),
            ((tmp_path / "escaped?tag=x#fragment").as_uri(), "result.json"),
        ]
        for url, name in cases:
            res = asyncio.run(w.write(OutputRequest(url=url), b'{"k":1}', name=name))
            if url.startswith("file://"):
                path = Path(
                    unquote(urlparse(url.rstrip("/") + "/" + res.artifacts[0]).path)
                )
            else:
                path = Path(url) / res.artifacts[0]
            assert path.read_bytes() == b'{"k":1}'
            assert res.url == url
            # The same decoded destination must be protected from source cleanup.
            asyncio.run(
                w.cleanup_sources(
                    {path: path.lstat()}, OutputRequest(url=url), res.artifacts
                )
            )
            assert path.exists()
        for suffix in ("/%2e%2e/out", "/%2E.%2Fout", "/bad%00", "/%FF"):
            with pytest.raises(ValidationError):
                OutputRequest(url=tmp_path.as_uri() + suffix)
        for base in (tmp_path.as_uri(), "s3://b/out", "tos://b/out"):
            for suffix in ("?tag=x", "#fragment", "?", "#"):
                with pytest.raises(ValidationError, match="query or fragment"):
                    OutputRequest(url=base + suffix)
        for scheme in ("http", "https"):
            url = f"{scheme}://upload/result?signature=a%2Fb#fragment"
            assert OutputRequest(url=url).url == url


class TestProxyEngineOutputE2E:
    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "failure,with_output",
        [
            (failure, output)
            for failure in ("rejected", "connect", "cancel_backoff")
            for output in (False, True)
        ]
        + [("expired", True)],
    )
    async def test_unsubmitted_video_releases_inputs(
        self, monkeypatch, tmp_path, with_output, failure
    ):
        from aibrix.openai_frontend.engine import proxy_engine as proxy
        from aibrix.openai_frontend.schemas.generation import VideoGenerationRequest
        from aibrix.openai_frontend.utils.utils import ServerError

        monkeypatch.setattr(proxy, "FORWARD_RETRY_BACKOFF_SECONDS", 0.005)
        attempts, paths = [], []
        connected = asyncio.Event()

        async def upstream(request):
            attempts.append(request)
            if failure in {"connect", "cancel_backoff"}:
                connected.set()
                raise httpx.ConnectError("connection refused", request=request)
            return httpx.Response(422, json={"detail": "invalid size"})

        engine = _mock_engine(
            upstream,
            media_dir=str(tmp_path),
            materialize_inline_media=True,
            video_output_timeout=0.01 if failure == "expired" else 0.2,
        )
        fetch = engine._fetch_media_for

        async def fetched(*args):
            result = await fetch(*args)
            paths.append(Path(result[1][0].temp_dir.name))
            if failure == "expired":
                await asyncio.sleep(0.03)
            return result

        monkeypatch.setattr(engine, "_fetch_media_for", fetched)
        request = VideoGenerationRequest(
            prompt="video",
            input=[
                {"type": "input_image", "image_url": "data:image/png;base64,UElYRUxT"}
            ],
            **({"output": {"url": str(tmp_path / "out")}} if with_output else {}),
        )
        task = asyncio.create_task(engine.video(request))
        try:
            if failure == "cancel_backoff":
                await asyncio.wait_for(connected.wait(), 2)
                task.cancel()
            expected = {
                "cancel_backoff": asyncio.CancelledError,
                "rejected": httpx.HTTPStatusError,
                "connect": ServerError,
                "expired": ServerError,
            }[failure]
            with pytest.raises(expected) as caught:
                await task
            if failure == "rejected":
                assert caught.value.response.status_code == 422
            assert paths and not paths[0].exists()
            assert not engine._retained.video_inputs and not engine._request_done
            assert not engine._fetcher._active_tmpdirs
            assert (
                len(attempts)
                == {"rejected": 1, "connect": 3, "cancel_backoff": 1, "expired": 0}[
                    failure
                ]
            )
        finally:
            await engine.aclose()
            await asyncio.gather(task, return_exceptions=True)

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "primary",
        ["success", "remote_success", "poll_error", "cancel", "same_inode", "replaced"],
    )
    async def test_video_cleanup_preserves_identity_and_primary(
        self, monkeypatch, tmp_path, primary
    ):
        from aibrix.openai_frontend.schemas.generation import VideoGenerationRequest

        final = tmp_path / "final.mp4"
        final.write_bytes(b"persisted-video")
        source = tmp_path / "historical.mp4"
        source.write_bytes(b"historical")
        polling, terminal = asyncio.Event(), asyncio.Event()
        identity_check = primary in {"same_inode", "replaced"}
        calls = 0

        async def upstream(request):
            nonlocal calls
            if request.method == "POST":
                return httpx.Response(
                    200,
                    json={"id": "job", "status": "queued", "file_path": str(source)},
                )
            calls += 1
            polling.set()
            if identity_check:
                await terminal.wait()
                # Earlier path disappears from terminal metadata.
                return httpx.Response(
                    200,
                    json={
                        "id": "job",
                        "status": "failed",
                        "error": {"message": "engine failed"},
                    },
                )
            if primary == "cancel" and calls == 1:
                await asyncio.Event().wait()
            if primary == "poll_error" and calls == 1:
                return httpx.Response(503, json={"error": "primary poll failure"})
            return httpx.Response(
                200,
                json={
                    "id": "job",
                    "status": "completed",
                    "url": "https://engine/video.mp4",
                    **({"file_path": str(final)} if primary == "success" else {}),
                },
            )

        engine = _mock_engine(upstream)
        unlink = Path.unlink

        def denied(path, *args, **kwargs):
            if path == source:
                raise PermissionError("cleanup denied")
            return unlink(path, *args, **kwargs)

        if not identity_check:
            monkeypatch.setattr(Path, "unlink", denied)
        task = asyncio.create_task(
            engine.video(
                VideoGenerationRequest(
                    prompt="video", output={"url": str(tmp_path / "out")}
                )
            )
        )
        try:
            if identity_check:
                await asyncio.wait_for(polling.wait(), 2)
                if primary == "replaced":
                    replacement = tmp_path / "other.mp4"
                    replacement.write_bytes(b"another job's file")
                    replacement.replace(source)
                else:
                    source.write_bytes(b"engine finished writing to the same inode")
                terminal.set()
                await asyncio.wait_for(task, 2)
                assert source.exists() is (primary == "replaced")
                if primary == "replaced":
                    assert source.read_bytes() == b"another job's file"
            else:
                if primary == "cancel":
                    await asyncio.wait_for(polling.wait(), 2)
                    task.cancel()
                if primary in {"success", "remote_success"}:
                    result = await asyncio.wait_for(task, 2)
                    assert result.status_code == 200
                    receipt = json.loads(result.body)
                    if primary == "success":
                        assert receipt["output"]["artifacts"] == ["job-0.mp4"]
                        assert (
                            tmp_path / "out" / "job-0.mp4"
                        ).read_bytes() == b"persisted-video"
                    assert receipt == (await engine.retrieve_video("job")).model_dump(
                        mode="json", exclude_unset=True
                    )
                else:
                    with pytest.raises(
                        httpx.HTTPStatusError
                        if primary == "poll_error"
                        else asyncio.CancelledError
                    ) as caught:
                        await asyncio.wait_for(task, 2)
                    if primary == "poll_error":
                        assert caught.value.response.status_code == 503
                assert source.exists()
            if engine._video_tasks:
                await asyncio.wait_for(asyncio.gather(*engine._video_tasks.values()), 2)
            cached = await engine.retrieve_video("job")
            if primary in {"success", "remote_success"}:
                assert cached.status == "completed" and cached.output is not None
            else:
                assert cached.status == "failed" and cached.output is None
            assert not engine._request_done
        finally:
            terminal.set()
            await engine.aclose()
            await asyncio.gather(task, return_exceptions=True)

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "outcome",
        [
            "completed",
            "failed",
            "canceled",
            "cancelled",
            "queued_stop",
            "post_terminal",
            "post_invalid",
            "timeout",
            "cancel",
            "cancel_plain",
            "shutdown",
            "exit",
            "post_500",
            "post_503",
            "post_500_plain",
            "post_503_plain",
            "get_404",
            "get_410",
        ],
    )
    async def test_video_inputs_live_until_terminal_get_or_engine_stop(
        self, monkeypatch, tmp_path, outcome
    ):
        from urllib.parse import unquote, urlparse

        from aibrix.openai_frontend.schemas.generation import VideoGenerationRequest
        from aibrix.openai_frontend.utils.utils import ServerError

        plain = outcome in {
            "completed",
            "failed",
            "canceled",
            "cancelled",
            "queued_stop",
            "post_terminal",
            "post_invalid",
            "cancel_plain",
            "post_500_plain",
            "post_503_plain",
            "get_404",
            "get_410",
        }
        server_error = outcome.startswith(("post_500", "post_503"))
        terminal_get = (
            plain
            and not server_error
            and outcome
            not in {
                "queued_stop",
                "post_invalid",
                "cancel_plain",
            }
        )
        post_body = {
            "id": "job",
            "status": "completed" if outcome == "post_terminal" else "queued",
        }
        if outcome == "post_invalid":
            post_body = {"vendor": "unknown job"}
        accepted, read_later = asyncio.Event(), asyncio.Event()
        worker = None
        inputs = []
        get_body = {"id": "job", "status": "queued"}
        output_file = tmp_path / "engine.mp4"
        output_file.write_bytes(b"engine output must remain")
        calls, cleanups = [], []
        stopped = False

        async def generate(path):
            await read_later.wait()
            assert path.read_bytes() == b"PIXELS"
            if server_error:
                assert output_file.read_bytes() == b"engine output must remain"

        async def upstream(request):
            nonlocal worker
            calls.append(request.method)
            if request.method == "GET":
                status_code = (
                    int(outcome.removeprefix("get_"))
                    if outcome.startswith("get_") and get_body.get("status") == "failed"
                    else 200
                )
                return httpx.Response(status_code, json=get_body)
            body = json.loads(request.content)
            path = Path(unquote(urlparse(body["input"][0]["image_url"]).path))
            inputs.append(path)
            worker = asyncio.create_task(generate(path))
            accepted.set()
            if server_error:
                return httpx.Response(
                    500 if outcome.startswith("post_500") else 503,
                    json={
                        "error": {"message": "response failed after scheduling"},
                        "status": "queued",
                        "file_path": str(output_file),
                    },
                )
            if plain and outcome != "cancel_plain":
                return httpx.Response(200, json=post_body)
            await asyncio.Event().wait()

        engine = _mock_engine(
            upstream,
            fetch_media=True,
            media_dir=str(tmp_path / "media"),
            materialize_inline_media=True,
            video_output_timeout=0.05,
        )
        cleanup = engine._fetcher.cleanup
        cleanup_entered, cleanup_release = asyncio.Event(), asyncio.Event()
        exiting = closing = joined = None

        async def record_cleanup(results):
            if not terminal_get:
                assert stopped, "input cleanup preceded engine stop"
            cleanups.append(list(results))
            if outcome == "queued_stop":
                cleanup_entered.set()
                await cleanup_release.wait()
            await cleanup(results)

        def stop():
            nonlocal stopped
            if not terminal_get and not stopped:
                assert inputs[0].exists(), "input was removed while engine was alive"
            stopped = True

        monkeypatch.setattr(engine._fetcher, "cleanup", record_cleanup)
        monkeypatch.setattr(engine._subprocess, "shutdown", stop)
        monkeypatch.setattr(
            engine._subprocess, "kill_leftovers", lambda timeout: stop() or []
        )
        request = VideoGenerationRequest(
            prompt="video",
            input=[
                {"type": "input_image", "image_url": "data:image/png;base64,UElYRUxT"}
            ],
            **({} if plain else {"output": {"url": str(tmp_path / "out")}}),
        )
        post = asyncio.create_task(engine.video(request))
        try:
            await asyncio.wait_for(accepted.wait(), 2)
            if outcome in {"cancel", "cancel_plain"}:
                post.cancel()
            if outcome == "shutdown":
                read_later.set()
                await worker
                await asyncio.wait_for(engine.aclose(), 2)
                await asyncio.gather(post, return_exceptions=True)
            elif plain and outcome != "cancel_plain" and not server_error:
                response = await post
                assert json.loads(response.body) == post_body
                assert inputs[0].exists() and engine._retained.video_inputs
                assert calls == ["POST"] and not engine._video_tasks
                read_later.set()
                await worker
                for body in (
                    {"id": "other", "status": "completed"},
                    {"id": "job"},
                    {"status": "completed"},
                    {"id": None, "status": "completed"},
                    get_body,
                ):
                    get_body = body
                    response = await engine.retrieve_video("job")
                    assert json.loads(response.body) == body
                    assert inputs[0].exists() and not cleanups
                if not terminal_get:
                    assert inputs[0].exists() and not cleanups
                    if outcome == "queued_stop":
                        # Input-only cleanup remains visible after popping ownership,
                        # just like source cleanup during concurrent exit/close.
                        exiting = asyncio.create_task(engine._handle_engine_exit())
                        await asyncio.wait_for(cleanup_entered.wait(), 2)
                        closing = asyncio.create_task(engine.aclose())
                        await asyncio.sleep(0.01)
                        assert not closing.done() and inputs[0].exists()
                        assert (
                            engine._request_done and not engine._retained.video_inputs
                        )
                        closing.cancel()
                        await asyncio.sleep(0)
                        closing.cancel()
                        joined = asyncio.create_task(engine.aclose())
                        await asyncio.sleep(0.01)
                        assert not joined.done() and inputs[0].exists()
                        cleanup_release.set()
                        await asyncio.wait_for(joined, 2)
                        await exiting
                        await asyncio.gather(closing, return_exceptions=True)
                    else:
                        await asyncio.wait_for(engine.aclose(), 2)
                else:
                    if outcome.startswith("get_"):
                        get_body = {"status": "failed", "error": "job unavailable"}
                        with pytest.raises(httpx.HTTPStatusError) as unavailable:
                            await engine.retrieve_video("job")
                        assert unavailable.value.response.status_code == int(
                            outcome.removeprefix("get_")
                        )
                        assert inputs[0].exists() and not cleanups
                    get_body = {
                        "id": "job",
                        "status": "completed"
                        if outcome == "post_terminal"
                        else "failed"
                        if outcome.startswith("get_")
                        else outcome,
                        "file_path": str(output_file),
                    }
                    if outcome.startswith("get_"):
                        results = await asyncio.gather(
                            engine.retrieve_video("job"),
                            engine.retrieve_video("job"),
                            return_exceptions=True,
                        )
                        assert all(
                            isinstance(result, httpx.HTTPStatusError)
                            for result in results
                        )
                        assert all(
                            result.response.json() == get_body for result in results
                        )
                    else:
                        await asyncio.gather(
                            engine.retrieve_video("job"), engine.retrieve_video("job")
                        )
                        assert (
                            json.loads((await engine.retrieve_video("job")).body)
                            == get_body
                        )
                assert not engine._uploaded_videos and not engine._video_tasks
            else:
                expected = (
                    httpx.HTTPStatusError
                    if server_error
                    else asyncio.CancelledError
                    if outcome in {"cancel", "cancel_plain"}
                    else ServerError
                )
                with pytest.raises(expected) as caught:
                    await post
                if server_error:
                    assert caught.value.response.status_code == (
                        500 if outcome.startswith("post_500") else 503
                    )
                assert engine._retained.video_inputs and inputs[0].exists()
                read_later.set()
                await worker
                assert not engine._video_tasks and calls == ["POST"]
                if outcome == "exit":
                    await asyncio.wait_for(engine._handle_engine_exit(), 2)
                else:
                    await asyncio.wait_for(engine.aclose(), 2)
            assert not inputs[0].exists() and not engine._retained.video_inputs
            assert len(cleanups) == 1 and not engine._request_done
            if server_error and not plain:
                assert not output_file.exists() and not engine._retained.video_sources
            else:
                assert output_file.read_bytes() == b"engine output must remain"
        finally:
            cleanup_release.set()
            read_later.set()
            if worker is not None:
                await asyncio.gather(worker, return_exceptions=True)
            await engine.aclose()
            await asyncio.gather(
                post,
                *(task for task in (exiting, closing, joined) if task is not None),
                return_exceptions=True,
            )

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "outcome",
        [
            "success",
            "server_error",
            "missing",
            "replaced",
            "exit",
            "late_stop",
            "concurrent_exit_close",
        ],
    )
    async def test_unknown_video_output_sources_wait_for_engine_stop(
        self, monkeypatch, tmp_path, outcome
    ):
        from aibrix.openai_frontend.schemas.generation import VideoGenerationRequest
        from aibrix.openai_frontend.utils.utils import ServerError

        source = tmp_path / "partial.mp4"
        import threading

        cleanup_entered, cleanup_release = threading.Event(), threading.Event()
        exiting = closing = joined = None
        if outcome != "missing":
            source.write_bytes(b"partial")
        read_later = asyncio.Event()
        worker = None
        stopped = False

        async def generate():
            await read_later.wait()
            if outcome == "missing":
                source.write_bytes(b"late output")
            else:
                assert source.read_bytes() == b"partial"
                source.write_bytes(b"finished output")

        def upstream(request):
            nonlocal worker
            if outcome != "late_stop":
                worker = asyncio.create_task(generate())
            return httpx.Response(
                503 if outcome == "server_error" else 200,
                json={"status": "queued", "file_path": str(source)},
            )

        engine = _mock_engine(upstream)

        def stop():
            nonlocal stopped
            stopped = True

        monkeypatch.setattr(engine._subprocess, "shutdown", stop)
        monkeypatch.setattr(
            engine._subprocess, "kill_leftovers", lambda timeout: stop() or []
        )
        if outcome == "late_stop":
            record = engine._record_output_sources

            def record_after_stop(*args):
                record(*args)
                stop()
                engine._video_writers_stopped.set()

            monkeypatch.setattr(engine, "_record_output_sources", record_after_stop)
            read_later.set()
        try:
            with pytest.raises(
                httpx.HTTPStatusError if outcome == "server_error" else ServerError
            ):
                await engine.video(
                    VideoGenerationRequest(
                        prompt="video", output={"url": str(tmp_path / "out")}
                    )
                )
            if outcome == "late_stop":
                assert (
                    stopped
                    and not engine._retained.video_sources
                    and not source.exists()
                )
                return
            assert engine._retained.video_sources and not engine._retained.video_inputs
            assert not stopped and not engine._video_tasks and not engine._request_done
            read_later.set()
            await worker
            if outcome == "replaced":
                replacement = tmp_path / "replacement.mp4"
                replacement.write_bytes(b"another job")
                replacement.replace(source)
            if outcome == "concurrent_exit_close":
                remove_sources = engine._output_writer._remove_sources
                cleanup_calls = []

                def blocked_cleanup(*args):
                    assert stopped and source.exists()
                    cleanup_calls.append(True)
                    cleanup_entered.set()
                    assert cleanup_release.wait(5)
                    remove_sources(*args)

                monkeypatch.setattr(
                    engine._output_writer, "_remove_sources", blocked_cleanup
                )
                exiting = asyncio.create_task(engine._handle_engine_exit())
                assert await asyncio.to_thread(cleanup_entered.wait, 2)
                closing = asyncio.create_task(engine.aclose())
                await asyncio.sleep(0.01)
                assert not closing.done() and source.exists() and engine._request_done
                closing.cancel()
                await asyncio.sleep(0)
                closing.cancel()
                joined = asyncio.create_task(engine.aclose())
                await asyncio.sleep(0.01)
                assert not joined.done() and source.exists()
                cleanup_release.set()
                await asyncio.wait_for(joined, 2)
                await exiting
                await asyncio.gather(closing, return_exceptions=True)
                assert cleanup_calls == [True] and not engine._request_done
            elif outcome == "exit":
                await engine._handle_engine_exit()
            else:
                await engine.aclose()
            assert stopped and not engine._retained.video_sources
            if outcome == "replaced":
                assert source.read_bytes() == b"another job"
            else:
                assert not source.exists()
            assert not (tmp_path / "out").exists()
        finally:
            cleanup_release.set()
            read_later.set()
            if worker is not None:
                await asyncio.gather(worker, return_exceptions=True)
            await engine.aclose()
            await asyncio.gather(
                *(task for task in (exiting, closing, joined) if task is not None),
                return_exceptions=True,
            )

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "poll_result",
        [
            "post_blocked",
            "unknown",
            "blocked",
            404,
            410,
            "terminal_404",
            "terminal_410",
            "mismatched_404",
            "mismatched_410",
            "missing_404",
            "missing_410",
        ],
    )
    async def test_video_poll_deadlines_keep_sources_until_shutdown(
        self, monkeypatch, tmp_path, poll_result
    ):
        from aibrix.openai_frontend.proxy import video_job as proxy
        from aibrix.openai_frontend.schemas.generation import VideoGenerationRequest
        from aibrix.openai_frontend.utils.utils import ServerError

        monkeypatch.setattr(proxy, "VIDEO_POLL_INTERVAL_SECONDS", 0.005)
        monkeypatch.setattr(proxy, "VIDEO_CLEANUP_TIMEOUT_SECONDS", 0.03)
        source = tmp_path / "video.mp4"
        source.write_bytes(b"partial")
        body = {"id": "job", "status": "queued", "file_path": str(source)}
        polls = 0

        async def upstream(request):
            nonlocal polls
            if request.method == "POST":
                if poll_result == "post_blocked":
                    await asyncio.Event().wait()
                return httpx.Response(200, json=body)
            polls += 1
            if isinstance(poll_result, str) and poll_result.startswith(
                ("terminal_", "mismatched_", "missing_")
            ):
                if polls == 1:
                    return httpx.Response(503, json={"error": "transient poll failure"})
                return httpx.Response(
                    int(poll_result.rsplit("_", 1)[1]),
                    json={
                        **(
                            {
                                "id": "job"
                                if poll_result.startswith("terminal_")
                                else "other"
                            }
                            if not poll_result.startswith("missing_")
                            else {}
                        ),
                        "status": "failed",
                    },
                )
            if poll_result == "blocked":
                await asyncio.Event().wait()
            if isinstance(poll_result, int):
                return httpx.Response(poll_result, json={"error": "job unavailable"})
            return httpx.Response(200, json={**body, "status": "vendor_unknown"})

        engine = _mock_engine(upstream, video_output_timeout=0.03)
        try:
            with pytest.raises((ServerError, httpx.HTTPStatusError)):
                await asyncio.wait_for(
                    engine.video(
                        VideoGenerationRequest(
                            prompt="video",
                            output=OutputRequest(url=str(tmp_path / "out")),
                        )
                    ),
                    1,
                )
            if isinstance(poll_result, str) and poll_result.startswith("terminal_"):
                tasks = list(engine._video_tasks.values())
                await asyncio.wait_for(asyncio.gather(*tasks), 1)
                assert polls == 2 and not source.exists()
                assert not engine._request_done and not engine._video_tasks
                return
            if poll_result == "post_blocked":
                assert not engine._request_done and not engine._video_tasks
                assert source.exists() and polls == 0
                return  # No response means no declared paths or job ID to claim.
            await asyncio.sleep(0.1)
            count = polls
            await asyncio.sleep(0.03)
            assert count == polls and count > 0
            assert engine._request_done and not engine._video_tasks["job"].done()
            assert source.exists() and not (tmp_path / "out").exists()
            source.write_bytes(b"generation continued after timeout")
            await asyncio.wait_for(engine.aclose(), 1)
            assert not source.exists()
            assert not engine._request_done and not engine._video_tasks
        finally:
            await engine.aclose()

    @pytest.mark.asyncio
    @pytest.mark.parametrize("scheme", ["file", "s3", "https"])
    async def test_image_output_copies_files_then_removes_sources(
        self, monkeypatch, tmp_path, scheme
    ):
        import io
        import zipfile

        from aibrix.openai_frontend.engine.proxy_engine import EngineState, ProxyEngine
        from aibrix.openai_frontend.frontend.fastapi_frontend import FastApiFrontend

        sources = [tmp_path / "image.png", tmp_path / "second.png"]
        for index, path in enumerate(sources):
            path.write_bytes(bytes([index]) * 11)
        payload = [{"file_path": str(path), "b64_json": "AA==\n"} for path in sources]
        body = {
            "id": "image-result",
            "created": 1,
            "data": payload,
            "usage": {"total_tokens": 2},
            "extra": {"preserved": None},
        }
        seen = {}
        store = _FakeStorage("s3", "b")
        real_client = httpx.AsyncClient

        def upstream(request):
            seen["request"] = json.loads(request.content)
            return httpx.Response(200, json=body)

        def upload(request):
            seen["upload"] = request.content
            seen["content_type"] = request.headers["content-type"]
            return httpx.Response(200)

        class UploadClient(real_client):
            def __init__(self, *args, **kwargs):
                kwargs.setdefault("transport", httpx.MockTransport(upload))
                super().__init__(*args, **kwargs)

        engine = ProxyEngine(
            ["unused"],
            fetch_media=False,
            output_writer=OutputWriter(storage_factory=lambda: store),
        )
        engine._set_state(EngineState.READY)
        engine._subprocess._client = real_client(
            base_url="http://engine", transport=httpx.MockTransport(upstream)
        )
        engine._subprocess._client_loop = asyncio.get_running_loop()
        frontend = FastApiFrontend(engine=engine)
        url = {
            "file": str(tmp_path / "out"),
            "s3": "s3://b/out",
            "https": "https://upload/result?sig=1",
        }[scheme]
        monkeypatch.setattr(httpx, "AsyncClient", UploadClient)
        try:
            async with real_client(
                transport=httpx.ASGITransport(app=frontend.app),
                base_url="http://frontend",
            ) as client:
                response = await client.post(
                    "/v1/images/generations",
                    json={"prompt": "image", "output": {"url": url}},
                )
            assert response.status_code == 200, response.text
            receipt = response.json()
            assert receipt == {**body, "data": [], "output": receipt["output"]}
            assert seen["request"] == {"prompt": "image"}
            names = ["image-result.json", "image-result-0.png", "image-result-1.png"]
            if scheme == "file":
                objects = {
                    name: (tmp_path / "out" / name).read_bytes() for name in names
                }
                assert receipt["output"]["artifacts"] == names
            elif scheme == "s3":
                objects = {
                    key.removeprefix("out/"): value
                    for key, value in store.objects.items()
                }
                assert receipt["output"]["artifacts"] == names
            else:
                assert seen["content_type"] == "application/zip"
                with zipfile.ZipFile(io.BytesIO(seen["upload"])) as archive:
                    assert archive.namelist() == names
                    objects = {name: archive.read(name) for name in names}
                assert receipt["output"]["artifacts"] == [url]
            assert json.loads(objects[names[0]]) == {"data": payload}
            assert objects[names[1]] == bytes([0]) * 11
            assert objects[names[2]] == bytes([1]) * 11
            assert all(not path.exists() for path in sources)
            assert engine._metrics_registry.get_sample_value(
                "proxy_output_bytes_total", {"scheme": scheme}
            ) == sum(map(len, objects.values()))
            assert not engine._in_flight
        finally:
            await engine.aclose()

    @pytest.mark.asyncio
    @pytest.mark.parametrize("with_payload", [False, True])
    async def test_video_upload_map_hit_and_engine_passthrough(
        self, tmp_path, with_payload
    ):
        from aibrix.openai_frontend.frontend.fastapi_frontend import FastApiFrontend

        source = tmp_path / "video.mp4"
        source.write_bytes(b"generated-video")
        body = {
            "id": "job",
            "object": "video",
            "status": "completed",
            "file_path": str(source),
            "file_paths": [str(source)],
            "extra": {"preserved": None},
        }
        if with_payload:
            body["data"] = [{"b64_json": "AA=="}]
        calls = []

        def upstream(request):
            calls.append((request.method, request.url.raw_path, request.content))
            if request.method == "POST" and request.url.path == "/v1/videos":
                assert json.loads(request.content) == {
                    "prompt": "video",
                }
                return httpx.Response(200, json=body)
            if request.url.path.endswith("/job"):
                return httpx.Response(200, json=body)
            return httpx.Response(
                418,
                content=b'{"engine":"original-error"}',
                headers={"content-type": "application/json"},
            )

        engine = _mock_engine(upstream)
        frontend = FastApiFrontend(engine=engine)
        try:
            async with httpx.AsyncClient(
                transport=httpx.ASGITransport(app=frontend.app),
                base_url="http://frontend",
            ) as client:
                # A non-uploaded job stays unmodified and does not enter the map.
                untouched = await client.get("/v1/videos/job")
                assert untouched.json() == body
                assert not engine._uploaded_videos
                for output in (
                    json.dumps({"url": str(tmp_path / "ignored")}),
                    "invalid",
                ):
                    ignored = await client.get(
                        "/v1/videos/job", params={"output": output}
                    )
                    assert ignored.status_code == 400
                    assert source.exists() and not engine._uploaded_videos
                created = await client.post(
                    "/v1/videos",
                    json={"prompt": "video", "output": {"url": str(tmp_path / "out")}},
                )
                assert created.status_code == 200, created.text
                uploaded = await client.get("/v1/videos/job")
                assert uploaded.json() == created.json()
                receipt = uploaded.json()
                expected_path = str(tmp_path / "out" / "job-0.mp4")
                assert receipt["url"] == expected_path
                assert receipt["urls"] == [expected_path]
                assert receipt["file_path"] == expected_path
                assert receipt["file_paths"] == [expected_path]
                assert receipt["extra"] == body["extra"]
                assert Path(expected_path).read_bytes() == b"generated-video"
                if with_payload:
                    assert receipt["data"] == []
                    assert json.loads((tmp_path / "out" / "job.json").read_bytes()) == {
                        "data": body["data"]
                    }
                assert not source.exists()
                count = len(calls)
                hit = await client.get("/v1/videos/job")
                assert hit.json() == receipt
                assert len(calls) == count
                missed = await client.get("/v1/videos/other")
                assert missed.status_code == 418
                assert missed.content == b'{"engine":"original-error"}'
                # Without output, creation and content are raw engine passthrough.
                created = await client.post("/v1/videos", json={"prompt": "video"})
                assert created.status_code == 200
                assert created.json() == body
                denied = await client.get("/v1/videos/job/content")
                assert denied.status_code == 404
                assert "uploaded output location" in denied.json()["detail"]
                content = await client.get("/v1/videos/other/content?variant=thumbnail")
                assert content.status_code == 418
                assert content.content == b'{"engine":"original-error"}'
                assert calls[-1][:2] == (
                    "GET",
                    b"/v1/videos/other/content?variant=thumbnail",
                )
                assert len(calls) == count + 3
            assert calls[0] == ("GET", b"/v1/videos/job", b"")
            assert calls[1][:2] == ("POST", b"/v1/videos")
            assert json.loads(calls[1][2]) == {"prompt": "video"}
            assert list(engine._uploaded_videos) == ["job"]
            from aibrix.openai_frontend.schemas.generation import VideoResponse

            cached = engine._uploaded_videos["job"]
            assert isinstance(cached, VideoResponse)
            assert cached.url == expected_path
            assert cached.output is not None
            assert cached.model_dump(mode="json", exclude_unset=True) == receipt
            copied = await engine.retrieve_video("job")
            assert isinstance(copied, VideoResponse)
            copied.url = "changed by caller"
            assert cached.url == expected_path
            assert not engine._in_flight
        finally:
            await engine.aclose()

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "outcome",
        [
            "completed",
            "history_removed",
            "upload_error",
            "engine_error",
            "poll_error",
            "timeout",
            "cancelled",
        ],
    )
    async def test_video_output_waits_for_terminal_cache_and_cleanup(
        self, monkeypatch, tmp_path, outcome
    ):
        from aibrix.openai_frontend.frontend.fastapi_frontend import FastApiFrontend
        from aibrix.openai_frontend.schemas.generation import VideoResponse

        monkeypatch.setattr(
            "aibrix.openai_frontend.proxy.video_job.VIDEO_POLL_INTERVAL_SECONDS", 0
        )
        monkeypatch.setattr(
            "aibrix.openai_frontend.proxy.video_job.VIDEO_POLL_REQUEST_TIMEOUT_SECONDS",
            2.0,
        )
        source = tmp_path / "video.mp4"
        partial = tmp_path / "partial.mp4"
        source.write_bytes(b"generated-video")
        partial.write_bytes(b"partial")
        body = {
            "id": "job",
            "status": "queued",
            "file_path": str(source),
            "file_paths": [str(source), str(partial)],
            "extension": {"null": None, "float": 1.2345678901234567},
            "seconds": 5,
            "created_at": 1.5,
            "error": None,
        }
        payload = [{"b64_json": "AA==\n", "tokens": [1, 2]}]
        polling = asyncio.Event()
        proceed = asyncio.Event()
        first_poll = asyncio.Event()
        complete = asyncio.Event()
        failure_seen = asyncio.Event()
        recovery = asyncio.Event()
        late = tmp_path / "late.mp4"
        calls = []

        async def upstream(request):
            calls.append((request.method, request.url.raw_path, request.content))
            if request.method == "POST":
                assert json.loads(request.content) == {"prompt": "video"}
                return httpx.Response(200, json=body)
            polling.set()
            await proceed.wait()
            if outcome == "timeout" and not first_poll.is_set():
                first_poll.set()
                return httpx.Response(200, json={**body, "status": "in_progress"})
            if outcome in {"completed", "history_removed"} and not first_poll.is_set():
                first_poll.set()
                return httpx.Response(
                    200, json={**body, "status": "in_progress", "progress": 50.5}
                )
            if outcome in {"completed", "history_removed"}:
                await complete.wait()
                if outcome == "history_removed":
                    partial.unlink()
            if outcome in {"poll_error", "timeout"} and not failure_seen.is_set():
                failure_seen.set()
                if outcome == "poll_error":
                    return httpx.Response(503, json={"error": "engine gone"})
                await asyncio.Event().wait()
            if outcome in {"poll_error", "timeout"}:
                await recovery.wait()
                late.write_bytes(b"late-generated video")
                return httpx.Response(
                    200,
                    json={
                        **body,
                        "status": "completed",
                        "file_paths": [str(source), str(late)],
                    },
                )
            return httpx.Response(
                200,
                json={
                    **body,
                    "status": "failed" if outcome == "engine_error" else "completed",
                    "file_paths": [str(source)],
                    "data": payload,
                    "error": {"message": "generation failed"}
                    if outcome == "engine_error"
                    else None,
                },
            )

        engine = _mock_engine(upstream)
        if outcome == "upload_error":

            async def fail(*args, **kwargs):
                raise OutputWriteError("upload failed")

            monkeypatch.setattr(engine._output_writer, "write", fail)
        try:
            frontend = FastApiFrontend(engine=engine)
            async with httpx.AsyncClient(
                transport=httpx.ASGITransport(app=frontend.app),
                base_url="http://frontend",
            ) as client:
                plain = await client.post("/v1/videos", json={"prompt": "video"})
                assert plain.json() == body
                assert not engine._uploaded_videos and not engine._video_tasks
                assert source.exists() and partial.exists()
                created = asyncio.create_task(
                    client.post(
                        "/v1/videos",
                        json={
                            "prompt": "video",
                            "output": {"url": str(tmp_path / "out")},
                        },
                    )
                )
                assert await _await_until(polling.is_set, timeout=2)
                assert not created.done()
                assert (
                    "job" in engine._uploaded_videos
                    and engine._uploaded_videos["job"] is None
                )
                task = engine._video_tasks["job"]
                pending_calls = len(calls)
                assert (
                    await client.get("/v1/videos/job?output=invalid")
                ).status_code == 400
                nil_hit = await client.get("/v1/videos/job")
                assert nil_hit.status_code == 200
                assert nil_hit.json()["id"] == "job"
                assert nil_hit.json()["status"] == "in_progress"
                assert all(
                    nil_hit.json()[key] is None
                    for key in ("file_path", "file_paths", "url", "urls")
                )
                assert "output" not in nil_hit.json()
                assert len(calls) == pending_calls
                assert engine._uploaded_videos["job"] is None
                assert (await client.get("/v1/videos/job/content")).status_code == 404
                assert source.exists() and partial.exists()
                if outcome == "cancelled":
                    # Shutdown owns/waits the job's cleanup, not the ASGI caller.
                    await asyncio.wait_for(engine.aclose(), timeout=2)
                else:
                    if outcome == "timeout":
                        monkeypatch.setattr(
                            "aibrix.openai_frontend.proxy.video_job.VIDEO_POLL_REQUEST_TIMEOUT_SECONDS",
                            0.1,
                        )
                    proceed.set()
                    if outcome in {"completed", "history_removed"}:
                        assert await _await_until(first_poll.is_set, timeout=2)
                        count = len(calls)
                        polled = await client.get("/v1/videos/job")
                        assert polled.json()["status"] == "in_progress"
                        assert polled.json()["progress"] == 50.5
                        assert "output" not in polled.json()
                        assert polled.json()["file_path"] is None
                        assert isinstance(engine._uploaded_videos["job"], VideoResponse)
                        assert len(calls) == count
                        complete.set()
                    if outcome in {"poll_error", "timeout"}:
                        assert await _await_until(
                            lambda: (
                                (cached := engine._uploaded_videos["job"]) is not None
                                and cached.status == "failed"
                            ),
                            timeout=2,
                        )
                        failed = engine._uploaded_videos["job"]
                        assert failed is not None and failed.status == "failed"
                        assert failure_seen.is_set()
                        failed_response = await asyncio.wait_for(created, timeout=2)
                        assert failed_response.status_code >= 400
                        assert task.done() and engine._request_done
                        assert source.exists() and partial.exists()
                        assert (
                            "output" not in (await client.get("/v1/videos/job")).json()
                        )
                        cleanup = engine._video_tasks["job"]
                        assert cleanup is not task and not cleanup.done()
                        recovery.set()
                        await asyncio.wait_for(cleanup, timeout=2)
                    await asyncio.wait_for(
                        asyncio.gather(task, return_exceptions=True), timeout=2
                    )
                result = (await asyncio.gather(created, return_exceptions=True))[0]
                if outcome in {"completed", "history_removed"}:
                    assert (
                        isinstance(result, httpx.Response) and result.status_code == 200
                    )
                    assert (
                        result.json()["status"] == "completed"
                        and result.json()["output"]
                    )
                elif outcome in {"upload_error", "poll_error", "timeout"}:
                    assert (
                        isinstance(result, httpx.Response) and result.status_code >= 400
                    )
                cached = engine._uploaded_videos["job"]
                assert isinstance(cached, VideoResponse)
                assert cached.status == (
                    "completed"
                    if outcome in {"completed", "history_removed"}
                    else "failed"
                )
                assert cached.model_dump()["extension"] == body["extension"]
                assert (
                    not source.exists() and not partial.exists() and not late.exists()
                )
                assert (
                    not engine._in_flight
                    and not engine._request_done
                    and not engine._video_tasks
                )
                hit = await client.get("/v1/videos/job")
                assert hit.json() == cached.model_dump(mode="json", exclude_unset=True)
                copied = await engine.retrieve_video("job")
                assert isinstance(copied, VideoResponse)
                copied.status = "changed"
                assert cached.status != "changed"
                assert (await client.get("/v1/videos/job/content")).status_code == 404
                if outcome in {"completed", "history_removed"}:
                    assert (
                        tmp_path / "out" / "job-0.mp4"
                    ).read_bytes() == b"generated-video"
                    assert not (tmp_path / "out" / "job-1.mp4").exists()
                    assert cached.output is not None
                    assert cached.output.artifacts == ["job.json", "job-0.mp4"]
                    assert cached.seconds == 5 and cached.created_at == 1.5
                    assert json.loads((tmp_path / "out" / "job.json").read_bytes()) == {
                        "data": payload
                    }
                    assert hit.json()["data"] == []
                    assert cached.url == str(tmp_path / "out" / "job-0.mp4")
                else:
                    assert cached.url is None
                    assert cached.output is None
                    assert "output" not in hit.json()
                    assert cached.error is not None
        finally:
            proceed.set()
            complete.set()
            recovery.set()
            await engine.aclose()

    @pytest.mark.asyncio
    @pytest.mark.parametrize("cancel", [False, True])
    async def test_video_job_resources_and_adapter_task_identity(
        self, monkeypatch, tmp_path, cancel
    ):
        from aibrix.openai_frontend.engine.proxy_engine import _ProxyRequestContext
        from aibrix.openai_frontend.schemas.generation import VideoResponse

        entered, release = asyncio.Event(), asyncio.Event()
        engine = _mock_engine(
            lambda request: pytest.fail("completed jobs must not poll")
        )
        req_id, started_at = engine._requests_begin()
        original_done = engine._request_done[req_id]
        context = _ProxyRequestContext(
            req_id, started_at, OutputRequest(url=str(tmp_path))
        )
        response = VideoResponse.model_validate(
            {"id": "job", "status": "completed", "url": "https://engine/video.mp4"}
        )
        job = engine._new_video_job(context, response)
        assert job.resources_done is original_done
        assert engine._request_done == {req_id: original_done}

        async def finish_sources(*args, **kwargs):
            entered.set()
            await release.wait()

        monkeypatch.setattr(
            engine._output_writer, "cleanup_completed_sources", finish_sources
        )
        waiter = asyncio.create_task(
            job.run(httpx.Response(200), response.model_dump(exclude_unset=True))
        )
        successor = None
        try:
            await asyncio.wait_for(entered.wait(), 2)
            assert not waiter.done() and not original_done.done()
            if cancel:
                waiter.cancel()
                await asyncio.sleep(0)
                waiter.cancel()
                await asyncio.sleep(0)
                assert not waiter.done() and not original_done.done()
            # Actual _VideoJobAdapter.finish must preserve another task's identity.
            successor = asyncio.create_task(release.wait())
            engine._video_tasks["job"] = successor
            release.set()
            result = (await asyncio.gather(waiter, return_exceptions=True))[0]
            if cancel:
                assert isinstance(result, asyncio.CancelledError)
            else:
                assert result.status_code == 200
            assert original_done.done() and not engine._request_done
            assert engine._video_tasks["job"] is successor
        finally:
            release.set()
            await asyncio.gather(
                waiter, *([successor] if successor else []), return_exceptions=True
            )
            engine._video_tasks.pop("job", None)
            await engine.aclose()

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        ("after_timeout", "before_start"),
        [(False, False), (True, False), (False, True)],
    )
    async def test_video_cancel_or_timeout_releases_resources(
        self, monkeypatch, tmp_path, after_timeout, before_start
    ):
        from aibrix.openai_frontend.engine.proxy_engine import EngineState, ProxyEngine
        from aibrix.openai_frontend.schemas.generation import VideoGenerationRequest

        source = tmp_path / "partial.mp4"
        # A worker finishes a file while the generation process is shutting down.
        body = {"id": "early", "status": "queued", "file_path": str(source)}
        engine = ProxyEngine(["unused"], fetch_media=False)
        engine._set_state(EngineState.READY)
        cleanup_poll = asyncio.Event()
        calls = 0

        async def upstream(request):
            nonlocal calls
            if request.method == "POST":
                return httpx.Response(200, json=body)
            calls += 1
            if after_timeout and calls == 1:
                raise httpx.ReadTimeout("initial poll timed out")
            cleanup_poll.set()
            await asyncio.Event().wait()

        engine._subprocess._client = httpx.AsyncClient(
            base_url="http://engine", transport=httpx.MockTransport(upstream)
        )
        engine._subprocess._client_loop = asyncio.get_running_loop()

        def stop_generation():
            source.write_bytes(b"worker finished before shutdown returned")

        monkeypatch.setattr(engine._subprocess, "shutdown", stop_generation)
        if before_start:
            from aibrix.openai_frontend.proxy.video_job import VideoOutputJob

            create_task = asyncio.create_task
            cancelled_before_start = asyncio.Event()

            def schedule(coro, *args, **kwargs):
                task = create_task(coro, *args, **kwargs)
                if coro.cr_code is VideoOutputJob.upload.__code__:
                    task.cancel("cancelled before first step")
                    cancelled_before_start.set()
                return task

            monkeypatch.setattr(asyncio, "create_task", schedule)
        try:
            task = asyncio.create_task(
                engine.video(
                    VideoGenerationRequest(
                        prompt="video", output=OutputRequest(url=str(tmp_path / "out"))
                    )
                )
            )
            await asyncio.wait_for(cleanup_poll.wait(), 2)
            if before_start:
                assert cancelled_before_start.is_set()
            if after_timeout:
                with pytest.raises(Exception, match="ReadTimeout"):
                    await asyncio.wait_for(asyncio.shield(task), 2)
            else:
                assert not task.done()
            if after_timeout:
                failed = engine._uploaded_videos["early"]
                assert failed is not None and failed.status == "failed"
            else:
                task.cancel()
                await asyncio.sleep(0.01)
                task.cancel()
                assert not task.done() and engine._request_done
            await asyncio.wait_for(engine.aclose(), timeout=2)
            result = (await asyncio.gather(task, return_exceptions=True))[0]
            if not after_timeout:
                assert isinstance(result, asyncio.CancelledError)
            assert not source.exists()
            early = engine._uploaded_videos["early"]
            assert early is not None and early.status == "failed"
            assert (
                not engine._request_done
                and not engine._in_flight
                and not engine._video_tasks
            )
        finally:
            await engine.aclose()

    @pytest.mark.asyncio
    @pytest.mark.parametrize("url_kind", ["file", "https"])
    @pytest.mark.parametrize("with_payload", [False, True])
    async def test_video_payload_only_output(
        self, monkeypatch, tmp_path, url_kind, with_payload
    ):
        from aibrix.openai_frontend.schemas.generation import VideoGenerationRequest

        payload = [{"b64_json": "AA==\n", "tokens": [1, 2]}]
        body = {"id": "payload", "status": "completed", "extra": None}
        if with_payload:
            body["data"] = payload
        else:
            body["url"] = "https://engine/video.mp4"
            body["seconds"] = 5
            body["progress"] = 100.5
        historical = tmp_path / "intermediate.mp4"
        historical.write_bytes(b"cleanup-only")

        def upstream(request):
            if request.method == "POST":
                return httpx.Response(
                    200,
                    json={
                        "id": "payload",
                        "status": "queued",
                        "file_path": str(historical),
                    },
                )
            return httpx.Response(200, json=body)

        engine = _mock_engine(upstream)
        captured = []
        if url_kind == "https":

            async def put(url, data, content_type):
                captured.append(data)
                return url

            monkeypatch.setattr(engine._output_writer, "_write_http", put)
        url = (
            str(tmp_path / "out")
            if url_kind == "file"
            else "https://upload/result?signature=exact"
        )
        try:
            await engine.video(
                VideoGenerationRequest(prompt="video", output=OutputRequest(url=url))
            )
            cached = engine._uploaded_videos["payload"]
            assert not historical.exists()
            assert not (tmp_path / "out" / "payload-0.mp4").exists()
            assert cached is not None and cached.status == "completed"
            assert cached.output is not None
            assert cached.model_dump()["extra"] is None
            if not with_payload:
                assert cached.url == body["url"] and not cached.output.artifacts
                assert cached.seconds == 5 and cached.progress == 100.5
                assert not captured and not (tmp_path / "out").exists()
                assert (
                    engine._metrics_registry.get_sample_value(
                        "proxy_output_writes_total",
                        {"scheme": url_kind, "result": "ok"},
                    )
                    is None
                )
                assert not engine._request_done
                return
            assert cached.url is None and cached.urls is None
            assert cached.model_dump()["data"] == []
            stored = (
                (tmp_path / "out" / "payload.json").read_bytes()
                if url_kind == "file"
                else captured[0]
            )
            assert json.loads(stored) == {"data": payload}
            assert (
                engine._metrics_registry.get_sample_value(
                    "proxy_output_writes_total", {"scheme": url_kind, "result": "ok"}
                )
                == 1
            )
            assert not engine._request_done
        finally:
            await engine.aclose()

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "kind", ["http_error", "invalid_metadata", "invalid_pending", "image_error"]
    )
    async def test_generation_error_still_owns_sources(self, tmp_path, kind):
        from aibrix.openai_frontend.frontend.fastapi_frontend import FastApiFrontend

        source = tmp_path / ("image.png" if kind == "image_error" else "video.mp4")
        source.write_bytes(b"engine output")
        body = {"id": "job", "status": "completed", "file_path": str(source)}
        if kind == "http_error":
            body.update(status="failed", error={"message": "generation failed"})
        if kind == "invalid_metadata":
            body["id"] = 4
        if kind == "invalid_pending":
            body.pop("status")
        if kind == "image_error":
            body = {"error": "failed", "data": [{"file_path": str(source)}]}
        cleanup_poll = asyncio.Event()

        async def upstream(request):
            if kind == "invalid_pending" and request.method == "GET":
                cleanup_poll.set()
                await asyncio.Event().wait()
            return httpx.Response(
                500 if kind in {"http_error", "image_error"} else 200, json=body
            )

        engine = _mock_engine(upstream)
        try:
            async with httpx.AsyncClient(
                base_url="http://proxy",
                transport=httpx.ASGITransport(app=FastApiFrontend(engine=engine).app),
            ) as client:
                path = (
                    "/v1/images/generations" if kind == "image_error" else "/v1/videos"
                )
                response = await client.post(
                    path,
                    json={
                        "prompt": "video",
                        "output": {"url": str(tmp_path / "out")},
                    },
                )
                assert response.status_code == 500
                if kind in {"http_error", "image_error"}:
                    assert response.json() == body
                if kind == "invalid_pending":
                    await asyncio.wait_for(cleanup_poll.wait(), 2)
                    assert source.exists() and engine._request_done
                    await asyncio.wait_for(engine.aclose(), 2)
                elif kind == "invalid_metadata":
                    # An invalid identity cannot establish trusted job termination.
                    assert source.exists() and engine._retained.video_sources
                    assert not engine._video_tasks and not engine._request_done
                    await asyncio.wait_for(engine.aclose(), 2)
                    assert not engine._retained.video_sources
                assert not source.exists()
                assert not engine._request_done
        finally:
            await engine.aclose()

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "outcome",
        [
            "completed",
            "client_cancel",
            "client_cancel_shutdown",
            "client_cancel_engine_failure",
            "shutdown",
            "engine_failure",
        ],
    )
    async def test_video_post_waits_for_upload_and_get_never_writes(
        self, monkeypatch, tmp_path, outcome
    ):
        from aibrix.openai_frontend.engine.proxy_engine import EngineState
        from aibrix.openai_frontend.frontend.fastapi_frontend import FastApiFrontend

        source = tmp_path / "video.mp4"
        source.write_bytes(b"video")
        body = {"id": "job", "status": "completed", "file_path": str(source)}
        calls = []

        def upstream(request):
            calls.append(request.url.path)
            return httpx.Response(200, json=body)

        engine = _mock_engine(upstream)
        entered, release = asyncio.Event(), asyncio.Event()
        write = engine._output_writer.write
        writes = 0

        async def blocked(*args, **kwargs):
            nonlocal writes
            writes += 1
            entered.set()
            await release.wait()
            return await write(*args, **kwargs)

        monkeypatch.setattr(engine._output_writer, "write", blocked)
        post = None
        try:
            async with httpx.AsyncClient(
                base_url="http://proxy",
                transport=httpx.ASGITransport(app=FastApiFrontend(engine=engine).app),
            ) as client:
                rejected = await client.post(
                    "/v1/videos",
                    json={
                        "prompt": "video",
                        "stream": True,
                        "output": {"url": str(tmp_path / "out")},
                    },
                )
                assert rejected.status_code == 422
                assert not calls and not engine._uploaded_videos and source.exists()
                post = asyncio.create_task(
                    client.post(
                        "/v1/videos",
                        json={
                            "prompt": "video",
                            "output": {"url": str(tmp_path / "out")},
                        },
                    )
                )
                await asyncio.wait_for(entered.wait(), 2)
                assert not post.done() and source.exists()
                for output in (
                    "",
                    "invalid",
                    json.dumps({"url": str(tmp_path / "other")}),
                ):
                    rejected = await client.get(
                        "/v1/videos/job", params={"output": output}
                    )
                    assert rejected.status_code == 400
                raw = await client.get("/v1/videos/job")
                assert raw.json()["status"] == "in_progress" and source.exists()
                assert raw.json()["file_path"] is None and raw.json()["url"] is None
                assert writes == 1 and len(calls) == 1
                if outcome.startswith("client_cancel"):
                    post.cancel()
                    # Latch the later service cancellation before the waiter resumes.
                    if outcome == "client_cancel_engine_failure":
                        engine._fail_permanently(
                            "engine died after client cancellation"
                        )
                    elif outcome == "client_cancel_shutdown":
                        engine._stopping = True
                        engine._set_state(EngineState.STOPPING)
                        engine._cancel_in_flight("shutdown after client cancellation")
                        await asyncio.wait_for(engine.aclose(), 2)
                elif outcome == "shutdown":
                    await asyncio.wait_for(engine.aclose(), 2)
                elif outcome == "engine_failure":
                    engine._fail_permanently("engine died during upload")
                else:
                    release.set()
                response = (
                    await asyncio.wait_for(
                        asyncio.gather(post, return_exceptions=True), 2
                    )
                )[0]
                assert not source.exists() and not engine._request_done
                cached = await client.get("/v1/videos/job")
                if outcome.startswith("client_cancel"):
                    assert isinstance(response, asyncio.CancelledError)
                    assert post.cancelled()
                    assert cached.json()["status"] == "failed"
                else:
                    assert isinstance(response, httpx.Response)
                    assert (
                        response.status_code
                        == {"completed": 200, "shutdown": 400, "engine_failure": 503}[
                            outcome
                        ]
                    )
                    if outcome == "completed":
                        assert response.json()["status"] == "completed"
                        assert response.json()["output"]["artifacts"] == ["job-0.mp4"]
                        assert cached.json() == response.json()
                        assert (tmp_path / "out" / "job-0.mp4").read_bytes() == b"video"
                    else:
                        assert cached.json()["status"] == "failed"
                assert writes == 1 and len(calls) == 1
                assert not (tmp_path / "other").exists()
        finally:
            release.set()
            if post is not None:
                await asyncio.gather(post, return_exceptions=True)
            await engine.aclose()

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        ("status", "cancel_at"),
        [
            (206, None),
            (416, None),
            (200, "read"),
            (200, "close"),
            (200, "error_close"),
            (200, "before_start"),
        ],
    )
    async def test_video_content_raw_encoded_range(
        self, monkeypatch, status, cancel_at
    ):
        import gzip

        import anyio

        from aibrix.openai_frontend.frontend.fastapi_frontend import FastApiFrontend

        compressed = gzip.compress(b"video bytes")
        content_path = "/v1/videos/other/content" + (
            "?variant=thumbnail" if status == 206 else ""
        )
        closed = []
        reading, closing, release = asyncio.Event(), asyncio.Event(), asyncio.Event()

        class Body(httpx.AsyncByteStream):
            async def __aiter__(self):
                reading.set()
                if cancel_at == "read":
                    await release.wait()
                if cancel_at == "error_close":
                    # Match HTTPX's internal read-error close and early closed flag.
                    with anyio.CancelScope(shield=True):
                        await self.aclose()
                    raise httpx.RemoteProtocolError("truncated video response")
                yield compressed

            async def aclose(self):
                if closing.is_set():
                    return
                closing.set()
                if cancel_at is not None:
                    await release.wait()
                closed.append(True)

        if cancel_at == "before_start":
            create_task = asyncio.create_task

            def schedule(coro, *args, **kwargs):
                task = create_task(coro, *args, **kwargs)
                if coro.cr_code.co_name == "read_raw":
                    task.cancel("raw reader cancelled before first step")
                return task

            monkeypatch.setattr(asyncio, "create_task", schedule)

        async def upstream(request):
            assert request.url.raw_path == content_path.encode()
            assert request.headers["range"] == "bytes=0-30"
            assert request.headers["if-range"] == '"etag"'
            assert request.headers["accept-encoding"] == "gzip"
            return httpx.Response(
                status,
                stream=Body(),
                headers={
                    "content-type": "video/mp4",
                    "content-encoding": "gzip",
                    "accept-ranges": "bytes",
                    "content-disposition": 'attachment; filename="video.mp4"',
                    "etag": '"etag"',
                    "content-range": f"bytes 0-{len(compressed) - 1}/{len(compressed)}",
                },
            )

        engine = _mock_engine(upstream)
        try:
            async with httpx.AsyncClient(
                base_url="http://proxy",
                transport=httpx.ASGITransport(app=FastApiFrontend(engine=engine).app),
            ) as client:
                if cancel_at is not None:
                    download = asyncio.create_task(
                        client.get(
                            content_path,
                            headers={
                                "Range": "bytes=0-30",
                                "If-Range": '"etag"',
                                "Accept-Encoding": "gzip",
                            },
                        )
                    )
                    try:
                        if cancel_at == "read":
                            await asyncio.wait_for(reading.wait(), 2)
                            download.cancel("disconnect during raw read")
                        await asyncio.wait_for(closing.wait(), 2)
                        assert not download.done() and engine._request_done
                        download.cancel("disconnect during close")
                        await asyncio.sleep(0)
                        download.cancel("repeat disconnect during close")
                        await asyncio.sleep(0)
                        assert not download.done() and not closed
                        assert engine._request_done
                        release.set()
                        with pytest.raises(asyncio.CancelledError):
                            await asyncio.wait_for(download, 2)
                    finally:
                        release.set()
                        await asyncio.gather(download, return_exceptions=True)
                    assert closed == [True] and not engine._request_done
                    return
                async with client.stream(
                    "GET",
                    content_path,
                    headers={
                        "Range": "bytes=0-30",
                        "If-Range": '"etag"',
                        "Accept-Encoding": "gzip",
                    },
                ) as response:
                    assert response.status_code == status
                    assert response.headers["content-encoding"] == "gzip"
                    assert response.headers["content-type"] == "video/mp4"
                    assert response.headers["accept-ranges"] == "bytes"
                    assert (
                        response.headers["content-disposition"]
                        == 'attachment; filename="video.mp4"'
                    )
                    assert response.headers["etag"] == '"etag"'
                    assert (
                        response.headers["content-range"]
                        == f"bytes 0-{len(compressed) - 1}/{len(compressed)}"
                    )
                    raw = b"".join([chunk async for chunk in response.aiter_raw()])
                    assert raw == compressed
                    assert int(response.headers["content-length"]) == len(compressed)
            assert closed == [True]
            assert not engine._request_done
        finally:
            release.set()
            await engine.aclose()

    @pytest.mark.asyncio
    @pytest.mark.parametrize("key", ["choices", "data"])
    async def test_payload_only_preserves_base64_and_all_metadata(self, tmp_path, key):
        from aibrix.openai_frontend.engine.proxy_engine import ProxyEngine

        payload = [{"b64_json": "AA==\n", "tokens": [1, 2], "finish_reason": "stop"}]
        body = {
            key: payload,
            "id": "result",
            "usage": {"total_tokens": 2},
            "metadata": {"nested": [None, False]},
            "extension": 1.2345678901234567,
        }
        raw = httpx.Response(200, json=body)
        engine = ProxyEngine(["unused"], fetch_media=False)
        try:
            response = await engine._persist_output(
                "r", OutputRequest(url=str(tmp_path)), raw, None
            )
            receipt = json.loads(response.body)
            assert receipt == {**body, key: [], "output": receipt["output"]}
            stored = tmp_path / receipt["output"]["artifacts"][0]
            assert json.loads(stored.read_bytes()) == {key: payload}
            assert (
                engine._metrics_registry.get_sample_value(
                    "proxy_output_bytes_total", {"scheme": "file"}
                )
                == stored.stat().st_size
            )
        finally:
            await engine.aclose()

    @pytest.mark.asyncio
    @pytest.mark.parametrize("cancel", [False, True])
    async def test_output_failure_and_cancellation_metrics(self, monkeypatch, cancel):
        from aibrix.openai_frontend.engine.proxy_engine import EngineState, ProxyEngine
        from aibrix.openai_frontend.schemas.openai import CreateChatCompletionRequest
        from aibrix.openai_frontend.utils.utils import ServerError

        engine = ProxyEngine(["unused"], fetch_media=False)
        engine._set_state(EngineState.READY)
        entered = asyncio.Event()
        release = asyncio.Event()

        async def forward(*args, **kwargs):
            return httpx.Response(
                200,
                json={
                    "id": "c",
                    "object": "chat.completion",
                    "created": 1,
                    "model": "m",
                    "choices": [],
                },
                request=httpx.Request("POST", "http://engine/chat"),
            )

        async def write(*args, **kwargs):
            entered.set()
            await release.wait()
            raise OutputWriteError("storage failed")

        monkeypatch.setattr(engine, "_forward", forward)
        monkeypatch.setattr(engine._output_writer, "write", write)
        req = CreateChatCompletionRequest(
            model="m",
            messages=[{"role": "user", "content": "hi"}],
            output={"url": "https://upload.example.com/result"},
        )
        task = asyncio.create_task(engine.chat(req))
        try:
            await asyncio.wait_for(entered.wait(), 5)
            await asyncio.sleep(0.01)
            if cancel:
                task.cancel()
            else:
                release.set()
            with pytest.raises(asyncio.CancelledError if cancel else ServerError):
                await asyncio.wait_for(task, 5)
            label = {"scheme": "https", "result": "cancelled" if cancel else "error"}
            registry = engine._metrics_registry
            assert registry.get_sample_value("proxy_output_writes_total", label) == 1
            assert registry.get_sample_value("proxy_output_seconds_total", label) > 0
            assert registry.get_sample_value(
                "proxy_output_bytes_total", {"scheme": "https"}
            ) in (None, 0)
            assert not engine._in_flight
        finally:
            release.set()
            await asyncio.gather(task, return_exceptions=True)
            await engine.aclose()

    @pytest.mark.asyncio
    @pytest.mark.parametrize("force_error", [False, True])
    async def test_chat_output_persistence_and_engine_error(
        self, tmp_path, force_error
    ):
        from aibrix.openai_frontend.schemas.openai import CreateChatCompletionRequest

        out_dir = tmp_path / "out"
        engine = _make_engine(_free_port(), fetch_media=False)
        await engine.start()
        try:
            request = CreateChatCompletionRequest(
                model="m",
                messages=[{"role": "user", "content": "hi"}],
                **({"user": "force-error"} if force_error else {}),
                output={"url": str(out_dir)},
            )
            if force_error:
                with pytest.raises(httpx.HTTPStatusError) as caught:
                    await engine.chat(request)
                assert caught.value.response.status_code == 400
                assert b"forced engine error" in caught.value.response.content
                assert not out_dir.exists()
                assert not any(
                    metric.samples
                    for metric in engine._metrics_registry.collect()
                    if metric.name.startswith("proxy_output_")
                )
                return
            response = await engine.chat(request)
            body = json.loads(response.body)
            assert body["choices"] == [] and body["media"] == []
            assert body["received"]["model"] == "m"
            assert "output" not in body["received"]
            assert body["id"] == "chatcmpl-test" and body["usage"]["total_tokens"] == 6
            artifact = body["output"]["artifacts"][0]
            assert artifact == "chatcmpl-test.json" and body["output"]["url"] == str(
                out_dir
            )
            stored = json.loads((out_dir / artifact).read_text())
            assert list(stored) == ["choices"]
            assert stored["choices"][0]["message"]["content"] == "hello"
            registry = engine._metrics_registry
            assert (
                registry.get_sample_value(
                    "proxy_output_bytes_total", {"scheme": "file"}
                )
                == (out_dir / artifact).stat().st_size
            )
            labels = {"scheme": "file", "result": "ok"}
            assert registry.get_sample_value("proxy_output_writes_total", labels) == 1
            assert registry.get_sample_value("proxy_output_seconds_total", labels) > 0
            assert "proxy_output_bytes_total" in await engine.metrics()
        finally:
            await engine.aclose()

    def test_streaming_output_rejected(self):
        # `output` persists one complete response; streaming is not supported.
        from pydantic import ValidationError

        from aibrix.openai_frontend.schemas.openai import (
            CreateChatCompletionRequest,
            CreateCompletionRequest,
        )

        with pytest.raises(ValidationError, match="cannot be combined"):
            CreateChatCompletionRequest(
                model="m",
                messages=[{"role": "user", "content": "hi"}],
                stream=True,
                output={"url": "/tmp/out"},
            )
        with pytest.raises(ValidationError, match="cannot be combined"):
            CreateCompletionRequest(
                model="m", prompt="hi", stream=True, output={"url": "/tmp/out"}
            )


# ---------------------------------------------------------------------------
# Passthrough endpoints through the FastAPI app
# ---------------------------------------------------------------------------


class TestPassthroughRouter:
    def test_responses_api_with_media_and_output(self, tmp_path):
        from fastapi.testclient import TestClient

        from aibrix.openai_frontend.frontend.fastapi_frontend import FastApiFrontend

        media_dir = str(tmp_path / "media")
        os.makedirs(media_dir)
        engine = _make_engine(
            _free_port(), media_dir=media_dir, artifact_fetcher=_DirFetcher(media_dir)
        )
        frontend = FastApiFrontend(engine=engine)
        try:
            with TestClient(frontend.app) as client:
                r = client.post(
                    "/v1/responses",
                    json={
                        "model": "m",
                        "input": [
                            {
                                "role": "user",
                                "content": [
                                    {"type": "input_text", "text": "describe"},
                                    {
                                        "type": "input_image",
                                        "image_url": "tos://b/p.png",
                                    },
                                ],
                            }
                        ],
                        "output": {"url": str(tmp_path / "out")},
                    },
                )
                assert r.status_code == 200, r.text
                body = r.json()
                # Only native Responses output is persisted; metadata stays in HTTP.
                assert body["media"][0]["content"] == "content-of:tos://b/p.png"
                assert "output" not in body["echo"]
                out_url = body["output"]["url"]
                artifact_rel = body["output"]["artifacts"][0]
                assert not artifact_rel.startswith("/")
                stored = json.loads(Path(out_url, artifact_rel).read_text())
                assert set(stored) == {"output"}
                assert stored["output"][0]["content"][0]["text"] == "hello"

        finally:
            # TestClient lifespan already awaited engine.aclose().
            assert not engine._in_flight


# ---------------------------------------------------------------------------
# Omni endpoints: multipart audio + realtime websocket + templated paths
# and the four built-in specs' endpoint catalogs
# ---------------------------------------------------------------------------


class TestEngineSpecCatalog:
    def test_builtin_catalogs_and_media_capabilities(self):
        from aibrix.openai_frontend.proxy.engine_spec import (
            SGLANG_OMNI_SPEC,
            VLLM_OMNI_SPEC,
        )

        assert VLLM_SPEC.spawn_args("/m") == ("--allowed-local-media-path", "/m")
        assert SGLANG_SPEC.supports_local_media and SGLANG_SPEC.spawn_args("/m") == ()
        assert "/v1/responses" in VLLM_SPEC.passthrough_endpoints
        assert "/generate" in SGLANG_SPEC.passthrough_endpoints
        assert EngineSpec(name="x").spawn_args("/m") == ()
        speech = {"/v1/audio/transcriptions", "/v1/realtime"}
        assert speech <= VLLM_OMNI_SPEC.passthrough_endpoints
        assert not speech <= VLLM_SPEC.passthrough_endpoints
        assert {"/v1/audio/transcriptions", "/v1/realtime"} <= (
            SGLANG_OMNI_SPEC.passthrough_endpoints
        )
        assert "/v1/audio/translations" in VLLM_OMNI_SPEC.passthrough_endpoints
        assert "/v1/audio/translations" not in SGLANG_OMNI_SPEC.passthrough_endpoints
        assert not SGLANG_SPEC.passthrough_endpoints & speech
        for path in (
            "/v1/chat/completions",
            "/v1/completions",
            "/v1/embeddings",
            "/v1/models",
            "/v1/responses",
            "/v1/messages",
            "/v1/score",
            "/v1/rerank",
            "/tokenize",
            "/detokenize",
        ):
            assert VLLM_OMNI_SPEC.supports(path), path
        for path in ("/generate", "/encode", "/v1/classify", "/v1/loads"):
            assert SGLANG_OMNI_SPEC.supports(path), path
        assert VLLM_OMNI_SPEC.kind("/v1/audio/transcriptions") == ForwardKind.MULTIPART
        assert VLLM_OMNI_SPEC.kind("/v1/realtime") == ForwardKind.WEBSOCKET
        assert VLLM_OMNI_SPEC.kind("/v1/chat/completions") == ForwardKind.SSE
        assert VLLM_OMNI_SPEC.kind("/v1/rerank") == ForwardKind.JSON
        assert VLLM_OMNI_SPEC.kind("/v1/responses/abc") == ForwardKind.JSON
        assert VLLM_OMNI_SPEC.kind("/v1/responses/abc/cancel") == ForwardKind.JSON
        assert VLLM_OMNI_SPEC.kind("/v1/nope") is None


class TestOmniEndpointsE2E:
    def test_templated_routes_match_correctly(self):
        """Pure routing check: GET /cancel/unknown must not falsely match."""
        from aibrix.openai_frontend.proxy.engine_spec import _matches

        tpl = "/v1/responses/{response_id}"
        assert _matches(tpl, "/v1/responses/resp-123")
        assert not _matches(tpl, "/v1/responses/resp-123/cancel")
        cancel = "/v1/responses/{response_id}/cancel"
        assert _matches(cancel, "/v1/responses/resp-123/cancel")
        assert not _matches(cancel, "/v1/responses/resp-123/missing")
        assert MOCK_SPEC.kind("/v1/responses/resp-123") == ForwardKind.JSON
        assert MOCK_SPEC.kind("/v1/responses/resp-123/cancel") == ForwardKind.JSON
        assert MOCK_SPEC.kind("/v1/responses/resp-123/missing") is None


class TestEngineMediaPolicy:
    def test_engine_spec_does_not_filter_media_types(self):
        from aibrix.openai_frontend.engine.proxy_engine import ProxyEngine

        req = {
            "messages": [
                {
                    "role": "user",
                    "content": [
                        {"type": "image_url", "image_url": {"url": "tos://b/a.png"}},
                        {"type": "video_url", "video_url": {"url": "tos://b/v.mp4"}},
                        {"type": "audio_url", "audio_url": {"url": "tos://b/a.wav"}},
                    ],
                }
            ],
            "input": [{"type": "input_file", "file_url": "https://h/document.pdf"}],
        }

        async def _run():
            engine = ProxyEngine(engine_cmd=["unused"], spec=TRTLLM_SPEC)
            try:
                locs = engine._chat_inspector.extract_urls(req)
                assert [loc.media_type for loc in locs] == [
                    "image_url",
                    "video_url",
                    "audio_url",
                    "file_url",
                ]
            finally:
                await engine.aclose()

        asyncio.run(_run())

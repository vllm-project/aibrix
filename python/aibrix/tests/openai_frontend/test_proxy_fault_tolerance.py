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

"""Fault tolerance of ProxyEngine.

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
import time
from typing import TYPE_CHECKING, List, Optional

import httpx
import pytest

from aibrix.openai_frontend.proxy.engine_spec import EngineSpec
from aibrix.openai_frontend.proxy.process_tree import (
    describe_returncode,
    kill_tree,
    snapshot_tree,
)
from aibrix.openai_frontend.proxy.subprocess_engine import (
    EngineStartupError,
    SubprocessEngine,
)

if TYPE_CHECKING:
    from aibrix.openai_frontend.engine.proxy_engine import ProxyEngine

psutil = pytest.importorskip("psutil")

MOCK = os.path.join(os.path.dirname(__file__), "mock_engine.py")
MOCK_SPEC = EngineSpec(name="mock")


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
    )


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
    def test_crash_latches_failed_and_reports_reason(self):
        async def _run():
            from aibrix.openai_frontend.engine.proxy_engine import EngineState

            engine = _make_engine(_free_port())
            await engine.start()
            try:
                first_pid = engine._subprocess.pid
                assert engine.state == EngineState.READY
                await _post_inner(engine, "/_test/crash", {"signal": 9})

                assert await _await_until(
                    lambda: engine.state == EngineState.FAILED, 10
                )
                assert "SIGKILL" in engine._last_exit
                assert not engine._subprocess.is_alive()
                assert not await engine.health()
                # The pid never changes: nothing was respawned.
                assert engine._subprocess.pid in (None, first_pid)
            finally:
                await engine.aclose()

        asyncio.run(_run())

    def test_orphaned_worker_killed_when_engine_crashes(self):
        """A worker forked into its own session (vLLM engine core style) must
        not survive its API server crashing, even though nothing is restarted."""

        async def _run():
            engine = _make_engine(_free_port())
            await engine.start()
            worker = None
            try:
                resp = await _post_inner(
                    engine, "/_test/spawn_worker", {"new_session": True}
                )
                assert resp is not None
                worker = resp.json()["pid"]
                # Let the supervisor's periodic snapshot see the worker.
                assert await _await_until(
                    lambda: worker in engine._subprocess.snapshot.members, 5
                )
                await _post_inner(engine, "/_test/crash", {"signal": 9})
                assert await _await_until(lambda: not _alive(worker), 10), (
                    "orphaned worker survived the engine crash"
                )
            finally:
                await engine.aclose()
                if worker and _alive(worker):
                    os.kill(worker, signal.SIGKILL)

        asyncio.run(_run())

    def test_request_after_crash_fails_with_503(self):
        """Once the engine is FAILED, new requests fail immediately instead of
        being sent to a dead port."""

        async def _run():
            from aibrix.openai_frontend.engine.proxy_engine import EngineState
            from aibrix.openai_frontend.schemas.openai import (
                CreateChatCompletionRequest,
            )
            from aibrix.openai_frontend.utils.utils import EngineUnavailableError

            engine = _make_engine(_free_port())
            await engine.start()
            try:
                await _post_inner(engine, "/_test/crash", {"signal": 9})
                assert await _await_until(
                    lambda: engine.state == EngineState.FAILED, 10
                )
                with pytest.raises(EngineUnavailableError):
                    await engine.chat(
                        CreateChatCompletionRequest(
                            model="m", messages=[{"role": "user", "content": "x"}]
                        )
                    )
            finally:
                await engine.aclose()

        asyncio.run(_run())

    def test_stop_kills_whole_tree(self):
        async def _run():
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
            await engine.aclose()
            assert not _alive(root)
            assert not _alive(worker)

        asyncio.run(_run())

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

    def test_startup_failure_reaps_half_started_tree(self):
        """start() re-raises and must leave neither a live child nor an
        attached HTTP client behind; aclose() afterwards is a no-op."""

        async def _run():
            from aibrix.openai_frontend.engine.proxy_engine import EngineState

            engine = _make_engine(
                _free_port(),
                extra_cmd=["--exit-immediately", "3"],
                startup_timeout=10.0,
            )
            with pytest.raises(EngineStartupError):
                await engine.start()
            assert engine.state == EngineState.FAILED
            assert engine._subprocess.pid is None
            assert engine._subprocess._proc is None
            assert engine._subprocess._client is None
            # Idempotent cleanup: the close task finishes immediately.
            await engine.aclose()
            assert engine.state == EngineState.STOPPED
            assert engine._close_task is not None and engine._close_task.done()

        asyncio.run(_run())

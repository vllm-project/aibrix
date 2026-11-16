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

"""Cross-process tests for the proxy's streaming paths.

Every test talks to a *real* proxy process -- ``python -m
aibrix.openai_frontend.main --engine proxy`` served by uvicorn on a TCP port
-- which itself spawns ``mock_engine.py`` as its inner engine subprocess.
Nothing runs in-process, so these cover what TestClient cannot:

* ``ws_bridge``: real WebSocket handshakes on both hops, subprotocol and
  header forwarding, handshake rejection relay, close-code propagation in
  both directions, engine crash mid-session, proxy shutdown mid-session.
* ``forward_multipart``: uploads relayed chunk by chunk (framing preserved,
  bounded proxy memory), streamed SSE responses, size limits enforced on a
  streamed body, client aborts.
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import os
import shlex
import signal
import socket
import subprocess
import sys
import threading
import time
from typing import AsyncIterator, Dict, Iterator, List, Optional

import httpx
import pytest

psutil = pytest.importorskip("psutil")
websockets = pytest.importorskip("websockets")

from websockets.asyncio.client import connect as ws_connect  # noqa: E402
from websockets.exceptions import ConnectionClosed, InvalidStatus  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
PKG_ROOT = os.path.dirname(os.path.dirname(HERE))  # .../python/aibrix
MOCK = os.path.join(HERE, "mock_engine.py")
MiB = 1 << 20
DEFAULT_MAX_INPUT = 64 * MiB  # HTTP_DEFAULT_MAX_INPUT_SIZE


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def run(coro):
    return asyncio.run(coro)


class ProxyProcess:
    """One proxy CLI process (plus its inner engine subprocess)."""

    def __init__(self, tmp_dir: str, *extra_args: str) -> None:
        self.port = _free_port()
        self.engine_port = _free_port()
        self.log_path = os.path.join(tmp_dir, f"proxy-{self.port}.log")
        self._log = open(self.log_path, "wb")
        cmd = [
            sys.executable,
            "-m",
            "aibrix.openai_frontend.main",
            "--engine",
            "proxy",
            "--host",
            "127.0.0.1",
            "--port",
            str(self.port),
            "--uvicorn-log-level",
            "warning",
            "--proxy-engine-spec",
            "vllm-omni",
            "--proxy-engine-port",
            str(self.engine_port),
            "--proxy-startup-timeout",
            "60",
            "--proxy-shutdown-grace",
            "3",
            "--proxy-health-check-interval",
            "0.2",
            *extra_args,
            "--proxy-engine-cmd",
            shlex.join([sys.executable, "-u", MOCK]),
        ]
        self.proc = subprocess.Popen(
            cmd,
            cwd=PKG_ROOT,
            env={**os.environ, "PYTHONPATH": PKG_ROOT, "PYTHONUNBUFFERED": "1"},
            stdout=self._log,
            stderr=subprocess.STDOUT,
            stdin=subprocess.DEVNULL,
        )
        self.url = f"http://127.0.0.1:{self.port}"
        self.ws_url = f"ws://127.0.0.1:{self.port}"
        self.engine_url = f"http://127.0.0.1:{self.engine_port}"

    def wait_ready(self, timeout: float = 90.0) -> None:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if self.proc.poll() is not None:
                raise RuntimeError(
                    f"proxy exited with {self.proc.returncode}:\n{self.log_tail()}"
                )
            try:
                if httpx.get(f"{self.url}/health", timeout=2).status_code == 200:
                    return
            except httpx.HTTPError:
                pass
            time.sleep(0.2)
        raise TimeoutError(f"proxy not ready after {timeout}s:\n{self.log_tail()}")

    def engine_pids(self) -> List[int]:
        try:
            return [c.pid for c in psutil.Process(self.proc.pid).children(True)]
        except psutil.NoSuchProcess:
            return []

    def rss(self) -> int:
        return psutil.Process(self.proc.pid).memory_info().rss

    def metric(self, name: str) -> Optional[float]:
        text = httpx.get(f"{self.url}/metrics", timeout=5).text
        for line in text.splitlines():
            if line.startswith(name + " ") or line.startswith(name + "{"):
                return float(line.rsplit(" ", 1)[1])
        return None

    def log_tail(self, n: int = 60) -> str:
        self._log.flush()
        with open(self.log_path, "rb") as f:
            return b"\n".join(f.read().splitlines()[-n:]).decode(errors="replace")

    def stop(self) -> None:
        children = self.engine_pids()
        if self.proc.poll() is None:
            self.proc.send_signal(signal.SIGTERM)
            try:
                self.proc.wait(20)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(5)
        for pid in children:  # never leak an engine, even on test failure
            try:
                psutil.Process(pid).kill()
            except psutil.NoSuchProcess:
                pass
        self._log.close()


class TestSignalShutdown:
    @pytest.mark.parametrize("sig", [signal.SIGTERM, signal.SIGINT])
    def test_uvicorn_lifespan_closes_engine_tree(self, tmp_path, sig):
        proxy = ProxyProcess(
            str(tmp_path),
            "--setenv",
            "aibrix-test-foo",
            "bar-value",
            "--setenv",
            "aibrix-test-eq",
            "https://x?y=1",
        )
        worker_process = None
        try:
            proxy.wait_ready()
            # Check CLI environment inheritance without launching another process.
            inherited = httpx.get(
                f"{proxy.engine_url}/_test/env",
                params=[("name", "AIBRIX_TEST_FOO"), ("name", "AIBRIX_TEST_EQ")],
                timeout=10,
            )
            assert inherited.status_code == 200
            assert inherited.json() == {
                "AIBRIX_TEST_FOO": "bar-value",
                "AIBRIX_TEST_EQ": "https://x?y=1",
            }
            response = httpx.post(
                f"{proxy.engine_url}/_test/spawn_worker",
                json={"new_session": True},
                timeout=5,
            )
            response.raise_for_status()
            worker_process = psutil.Process(response.json()["pid"])
            identities = [psutil.Process(pid) for pid in proxy.engine_pids()]
            identities.append(worker_process)
            # Let the periodic identity snapshot include the new worker.
            time.sleep(0.5)
            if sig == signal.SIGTERM:

                async def close_active_session():
                    async with ws_connect(f"{proxy.ws_url}/v1/realtime") as ws:
                        await ws.send("ping")
                        assert await ws.recv() == "ping"
                        proxy.proc.send_signal(sig)
                        with pytest.raises(ConnectionClosed) as exc:
                            await asyncio.wait_for(ws.recv(), 15)
                        assert exc.value.rcvd is not None
                        assert exc.value.rcvd.code == 1012

                run(close_active_session())
            else:
                proxy.proc.send_signal(sig)
            proxy.proc.wait(timeout=15)
            deadline = time.monotonic() + 5

            def alive(process):
                try:
                    return (
                        process.is_running()
                        and process.status() != psutil.STATUS_ZOMBIE
                    )
                except psutil.NoSuchProcess:
                    return False

            while (
                any(alive(process) for process in identities)
                and time.monotonic() < deadline
            ):
                time.sleep(0.05)
            assert not any(alive(process) for process in identities)
            log = proxy.log_tail(200)
            assert "Traceback" not in log
            assert "Event loop is closed" not in log
            assert "Task was destroyed" not in log
        finally:
            proxy.stop()
            if worker_process is not None:
                try:
                    worker_process.kill()
                except psutil.NoSuchProcess:
                    pass


@pytest.fixture(scope="module")
def proxy(tmp_path_factory) -> Iterator[ProxyProcess]:
    p = ProxyProcess(str(tmp_path_factory.mktemp("proxy")))
    try:
        p.wait_ready()
        yield p
    finally:
        p.stop()


@pytest.fixture
def ready_proxy(proxy: ProxyProcess) -> ProxyProcess:
    return proxy


# ---------------------------------------------------------------------------
# ws_bridge across processes
# ---------------------------------------------------------------------------


class TestWebSocketBridgeCrossProcess:
    def test_text_and_binary_frames_round_trip(self, ready_proxy):
        async def go():
            async with ws_connect(
                f"{ready_proxy.ws_url}/v1/realtime", max_size=None
            ) as ws:
                await ws.send('{"type":"session.update"}')
                assert await ws.recv() == '{"type":"session.update"}'
                # Binary comes back reversed: proof it reached the engine.
                await ws.send(b"\x00\x01\x02\xff")
                assert await ws.recv() == b"\xff\x02\x01\x00"
                # A frame larger than the websockets default 1 MiB max_size.
                big = os.urandom(4 * MiB)
                await ws.send(big)
                assert await ws.recv() == big[::-1]
                # Multi-byte UTF-8 text survives both hops.
                await ws.send("音频转写 ✓")
                assert await ws.recv() == "音频转写 ✓"

        run(go())

    def test_handshake_forwarding(self, ready_proxy):
        async def go():
            async with ws_connect(
                f"{ready_proxy.ws_url}/v1/realtime?model=whisper&intent=transcription",
                subprotocols=["realtime", "openai-beta.realtime-v1"],
                additional_headers={
                    "Authorization": "Bearer sk-test",
                    "OpenAI-Beta": "realtime=v1",
                    "Cookie": "session=secret",
                },
            ) as ws:
                # The engine picked the subprotocol; the client sees that pick.
                assert ws.subprotocol == "realtime"
                await ws.send("hello")
                info = json.loads(await ws.recv())
                assert info["subprotocol"] == "realtime"
                assert info["authorization"] == "Bearer sk-test"
                assert info["openai_beta"] == "realtime=v1"
                assert info["cookie"] is None  # hop-local, not forwarded
                assert info["query"] == {
                    "model": "whisper",
                    "intent": "transcription",
                }
                assert info["pid"] in ready_proxy.engine_pids()

        run(go())

    @pytest.mark.parametrize("status_code", [401, 307])
    def test_engine_handshake_rejection_is_relayed(self, ready_proxy, status_code):
        from websockets.asyncio.server import serve

        async def go():
            received = []

            async def redirect_target(ws):
                received.append(dict(ws.request.headers))
                await ws.close()

            async with serve(redirect_target, "127.0.0.1", 0) as target:
                port = target.sockets[0].getsockname()[1]
                query = f"reject={status_code}"
                if status_code == 307:
                    query += f"&redirect=ws://127.0.0.1:{port}/unexpected"
                with pytest.raises(InvalidStatus) as exc:
                    async with ws_connect(
                        f"{ready_proxy.ws_url}/v1/realtime?{query}",
                        additional_headers={
                            "Authorization": "Bearer synthetic-review-token",
                            "X-API-Key": "synthetic-review-key",
                        },
                    ):
                        pass
                assert exc.value.response.status_code == status_code
                assert json.loads(exc.value.response.body) == {
                    "error": "rejected by engine"
                }
                assert not received  # No redirect handshake or credential forwarding.

        run(go())

    @pytest.mark.parametrize("path", ["/v1/nope", "/v1/chat/completions"])
    def test_non_websocket_paths_rejected_without_upgrade(self, ready_proxy, path):
        async def go():
            with pytest.raises(InvalidStatus) as exc:
                async with ws_connect(f"{ready_proxy.ws_url}{path}"):
                    pass
            # No catch-all WebSocket route: Starlette refuses unmatched upgrades.
            assert exc.value.response.status_code == 403

        run(go())

    def test_engine_close_code_and_reason_propagate(self, ready_proxy):
        async def go():
            async with ws_connect(f"{ready_proxy.ws_url}/v1/realtime") as ws:
                await ws.send("close:4001:session expired")
                with pytest.raises(ConnectionClosed) as exc:
                    await asyncio.wait_for(ws.recv(), 5)
            assert exc.value.rcvd is not None
            assert exc.value.rcvd.code == 4001
            assert exc.value.rcvd.reason == "session expired"

        run(go())

    def test_concurrent_sessions_are_isolated(self, ready_proxy):
        async def session(i: int) -> List[str]:
            got = []
            async with ws_connect(f"{ready_proxy.ws_url}/v1/realtime") as ws:
                for j in range(20):
                    await ws.send(f"s{i}-m{j}")
                    message = await ws.recv()
                    assert isinstance(message, str)
                    got.append(message)
            return got

        async def go():
            results = await asyncio.gather(*(session(i) for i in range(8)))
            for i, got in enumerate(results):
                assert got == [f"s{i}-m{j}" for j in range(20)]

        run(go())
        # Sessions are tracked as in-flight requests and released on close.
        assert ready_proxy.metric("proxy_in_flight_requests") == 0

    def test_engine_crash_mid_session_closes_client_and_proxy_latches_failed(
        self, tmp_path
    ):
        p = ProxyProcess(str(tmp_path))
        try:
            p.wait_ready()
            old_pids = p.engine_pids()

            async def crash_session():
                async with ws_connect(f"{p.ws_url}/v1/realtime") as ws:
                    await ws.send("hello")
                    await ws.recv()
                    await ws.send("crash")  # engine dies without a close frame
                    t0 = time.monotonic()
                    with pytest.raises(ConnectionClosed) as exc:
                        await asyncio.wait_for(ws.recv(), 10)
                    # Prompt 1011, not a ping-timeout hang.
                    assert time.monotonic() - t0 < 5
                    assert exc.value.rcvd is not None
                    assert exc.value.rcvd.code == 1011

            run(crash_session())
            for pid in old_pids:
                deadline = time.monotonic() + 10
                while time.monotonic() < deadline and psutil.pid_exists(pid):
                    time.sleep(0.1)
                assert not psutil.pid_exists(pid), "engine process leaked"

            # The proxy does not restart the engine: /health fails and stays
            # failed, no engine is left running...
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                if httpx.get(f"{p.url}/health").status_code != 200:
                    break
                time.sleep(0.1)
            time.sleep(1.0)
            assert httpx.get(f"{p.url}/health").status_code != 200
            assert p.engine_pids() == []

            # ...and new sessions are refused with a real HTTP status instead
            # of being accepted onto a dead upstream.
            async def refused():
                with pytest.raises(InvalidStatus) as exc:
                    async with ws_connect(f"{p.ws_url}/v1/realtime"):
                        pass
                assert exc.value.response.status_code in (502, 503)

            run(refused())
        finally:
            p.stop()


# ---------------------------------------------------------------------------
# Streaming multipart across processes
# ---------------------------------------------------------------------------


def _multipart(
    fields: Dict[str, str], filename: str, payload_chunks: List[bytes]
) -> "tuple[str, List[bytes]]":
    """Build a multipart body as a list of parts (the file split in chunks)."""
    boundary = "aibrix-test-boundary-7f3a"
    head = b""
    for name, value in fields.items():
        head += (
            f"--{boundary}\r\n"
            f'Content-Disposition: form-data; name="{name}"\r\n\r\n'
            f"{value}\r\n"
        ).encode()
    head += (
        f"--{boundary}\r\n"
        f'Content-Disposition: form-data; name="file"; filename="{filename}"\r\n'
        "Content-Type: audio/wav\r\n\r\n"
    ).encode()
    tail = f"\r\n--{boundary}--\r\n".encode()
    return f"multipart/form-data; boundary={boundary}", [head, *payload_chunks, tail]


def _audio_chunks(total: int, chunk: int = MiB) -> "tuple[List[bytes], str]":
    block = os.urandom(chunk)
    chunks = [block] * (total // chunk)
    if total % chunk:
        chunks.append(block[: total % chunk])
    digest = hashlib.sha256()
    for c in chunks:
        digest.update(c)
    return chunks, digest.hexdigest()


class _PeakRSS:
    """Sample a process' RSS in the background and keep the maximum."""

    def __init__(self, pid: int) -> None:
        self._proc = psutil.Process(pid)
        self.baseline = self._proc.memory_info().rss
        self.peak = self.baseline
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._run, daemon=True)

    def _run(self) -> None:
        while not self._stop.is_set():
            self.peak = max(self.peak, self._proc.memory_info().rss)
            time.sleep(0.005)

    def __enter__(self) -> "_PeakRSS":
        self._thread.start()
        return self

    def __exit__(self, *exc) -> None:
        self._stop.set()
        self._thread.join()

    @property
    def growth(self) -> int:
        return self.peak - self.baseline


class TestStreamingMultipartCrossProcess:
    def test_large_upload_streams_with_framing_preserved_and_bounded_memory(
        self, ready_proxy
    ):
        size = 48 * MiB
        chunks, sha = _audio_chunks(size)
        content_type, parts = _multipart(
            {"model": "whisper", "language": "en"}, "speech.wav", chunks
        )
        body_len = sum(len(p) for p in parts)

        # Warm up allocator/import paths so the baseline is meaningful.
        warm_ct, warm_parts = _multipart({"model": "whisper"}, "w.wav", [b"x" * 1024])
        httpx.post(
            f"{ready_proxy.url}/v1/audio/transcriptions",
            content=b"".join(warm_parts),
            headers={"content-type": warm_ct},
            timeout=30,
        ).raise_for_status()

        def gen() -> Iterator[bytes]:
            yield from parts

        with _PeakRSS(ready_proxy.proc.pid) as rss:
            r = httpx.post(
                f"{ready_proxy.url}/v1/audio/transcriptions?response_format=json",
                content=gen(),
                headers={
                    "content-type": content_type,
                    "content-length": str(body_len),
                },
                timeout=120,
            )
        assert r.status_code == 200, r.text
        got = r.json()
        assert got["size"] == size
        assert got["sha256"] == sha
        assert got["filename"] == "speech.wav"
        assert (got["model"], got["language"]) == ("whisper", "en")
        assert got["query"] == {"response_format": "json"}
        # Client framing kept: same Content-Length, not re-chunked.
        assert got["content_length"] == str(body_len)
        assert got["transfer_encoding"] is None
        # A buffering proxy holds the whole 48 MiB body (often twice).
        assert rss.growth < 16 * MiB, (
            f"proxy RSS grew {rss.growth / MiB:.1f} MiB for a "
            f"{size / MiB:.0f} MiB upload"
        )

    def test_chunked_upload_without_content_length(self, ready_proxy):
        chunks, sha = _audio_chunks(5 * MiB + 123, chunk=256 * 1024)
        content_type, parts = _multipart({"model": "whisper"}, "c.wav", chunks)

        async def go():
            async def agen() -> AsyncIterator[bytes]:
                for part in parts:
                    yield part
                    await asyncio.sleep(0)

            async with httpx.AsyncClient(timeout=60) as c:
                r = await c.post(
                    f"{ready_proxy.url}/v1/audio/transcriptions",
                    content=agen(),
                    headers={"content-type": content_type},
                )
            assert r.status_code == 200, r.text
            got = r.json()
            assert got["sha256"] == sha
            assert got["content_length"] is None
            assert got["transfer_encoding"] == "chunked"

        run(go())

    def test_streamed_sse_response_is_relayed_incrementally(self, ready_proxy):
        chunks, sha = _audio_chunks(2 * MiB)
        content_type, parts = _multipart(
            {"model": "whisper", "stream": "true"}, "s.wav", chunks
        )

        async def go():
            events = []
            async with httpx.AsyncClient(timeout=60) as c:
                async with c.stream(
                    "POST",
                    f"{ready_proxy.url}/v1/audio/transcriptions?gate=incremental",
                    content=b"".join(parts),
                    headers={"content-type": content_type},
                ) as r:
                    assert r.status_code == 200
                    assert r.headers["content-type"].startswith("text/event-stream")
                    lines = r.aiter_lines()
                    try:
                        # The engine cannot finish until the client observes a delta.
                        first = await asyncio.wait_for(anext(lines), 3)
                        assert first == 'data: {"delta": "hello"}'
                        events.append(first[6:])
                    finally:
                        released = await c.post(
                            f"{ready_proxy.engine_url}/_test/release_stream/incremental"
                        )
                        released.raise_for_status()
                    async for line in lines:
                        if line.startswith("data: "):
                            events.append(line[6:])
            assert events[0] == '{"delta": "hello"}'
            assert events[1] == '{"delta": "world"}'
            assert json.loads(events[2])["sha256"] == sha
            assert events[-1] == "[DONE]"

        run(go())

    def test_declared_oversize_rejected_before_body_is_read(self, ready_proxy):
        # Raw socket: declare 1 GiB, send no body; the 413 must come anyway.
        with socket.create_connection(("127.0.0.1", ready_proxy.port), 10) as s:
            s.sendall(
                b"POST /v1/audio/transcriptions HTTP/1.1\r\n"
                b"Host: x\r\nContent-Type: multipart/form-data; boundary=b\r\n"
                b"Content-Length: 1073741824\r\n\r\n"
            )
            s.settimeout(10)
            head = s.recv(4096)
        assert head.startswith(b"HTTP/1.1 413"), head[:200]

    def test_chunked_oversize_rejected_while_streaming(self, ready_proxy):
        # No Content-Length: the limit must trip on the streamed byte count.
        # The overflowing chunk is the last one, so the whole body is on the
        # wire before the 413 is read.
        chunk = 256 * 1024
        n = DEFAULT_MAX_INPUT // chunk
        content_type, parts = _multipart(
            {"model": "whisper"}, "big.wav", [b"\0" * chunk] * n
        )

        async def go():
            async def agen() -> AsyncIterator[bytes]:
                for part in parts:
                    yield part

            async with httpx.AsyncClient(timeout=120) as c:
                r = await c.post(
                    f"{ready_proxy.url}/v1/audio/transcriptions",
                    content=agen(),
                    headers={"content-type": content_type},
                )
            assert r.status_code == 413, r.text[:300]
            assert r.json()["error"]["code"] == "content_too_large"

        run(go())
        assert ready_proxy.metric("proxy_in_flight_requests") == 0

    def test_client_abort_mid_upload_does_not_wedge_proxy(self, ready_proxy):
        content_type, _ = _multipart({"model": "whisper"}, "a.wav", [])
        with socket.create_connection(("127.0.0.1", ready_proxy.port), 10) as s:
            s.sendall(
                b"POST /v1/audio/transcriptions HTTP/1.1\r\nHost: x\r\n"
                + f"Content-Type: {content_type}\r\n".encode()
                + b"Content-Length: 10485760\r\n\r\n"
                + b"\0" * (2 * MiB)
            )
            time.sleep(0.3)  # let the proxy start relaying upstream
        # Socket closed with 8 MiB still owed.
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            if ready_proxy.metric("proxy_in_flight_requests") == 0:
                break
            time.sleep(0.1)
        assert ready_proxy.metric("proxy_in_flight_requests") == 0
        # And the next upload still works end to end.
        chunks, sha = _audio_chunks(MiB)
        ct, parts = _multipart({"model": "whisper"}, "ok.wav", chunks)
        r = httpx.post(
            f"{ready_proxy.url}/v1/audio/transcriptions",
            content=b"".join(parts),
            headers={"content-type": ct},
            timeout=30,
        )
        assert r.status_code == 200 and r.json()["sha256"] == sha

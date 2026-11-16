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
import atexit
import collections
import logging
import os
import signal
import socket
import subprocess
import sys
import threading
import time
import weakref
from typing import IO, Any, AsyncIterable, Deque, Dict, List, Optional, Union

import anyio
import httpx
from pydantic import Field
from pydantic_settings import BaseSettings, SettingsConfigDict

from aibrix.openai_frontend.proxy.runtime.engine_spec import EngineSpec
from aibrix.openai_frontend.proxy.runtime.process_tree import (
    TreeSnapshot,
    describe_returncode,
    kill_tree,
    snapshot_tree,
)
from aibrix.openai_frontend.utils.utils import prefix_line
from aibrix.runtime.downloaders import await_thread_completion

logger = logging.getLogger(__name__)

LOG_PREFIX = "[proxy-engine]"
LOG_TAIL_LINES = 200
DEFAULT_STARTUP_TIMEOUT_SECONDS = 1800.0
DEFAULT_SHUTDOWN_GRACE_SECONDS = 30.0
PORT_RELEASE_TIMEOUT_SECONDS = 15.0
HEALTH_PROBE_TIMEOUT_SECONDS = 5.0


class _HTTPPoolSettings(BaseSettings):
    """Proxy pool settings from process env; no .env files or import-time cache."""

    model_config = SettingsConfigDict(env_prefix="AIBRIX_PROXY_", case_sensitive=False)

    max_connections: int = Field(default=100, gt=0)
    max_keepalive_connections: int = Field(default=20, ge=0)
    keepalive_expiry_seconds: float = Field(default=30.0, ge=0, allow_inf_nan=False)


def _http_connection_limits() -> httpx.Limits:
    """Read fresh settings at client creation so CLI ``--setenv`` takes effect."""
    settings = _HTTPPoolSettings()
    return httpx.Limits(
        max_connections=settings.max_connections,
        max_keepalive_connections=settings.max_keepalive_connections,
        keepalive_expiry=settings.keepalive_expiry_seconds,
    )


class EngineStartupError(RuntimeError):
    """The inner engine exited, or never became healthy, during startup."""


# Every live SubprocessEngine, so an abnormal interpreter exit (unhandled
# exception, sys.exit from a thread) still tears down the inner engine.
# SIGKILL of this process cannot run it; the inner engine runs in its own
# session, so that case is left to the container runtime.
_LIVE_ENGINES: "weakref.WeakSet[SubprocessEngine]" = weakref.WeakSet()


def _atexit_shutdown_all() -> None:  # pragma: no cover - interpreter exit path
    for engine in list(_LIVE_ENGINES):
        try:
            engine.shutdown(grace_seconds=5.0)
        except Exception:
            pass


atexit.register(_atexit_shutdown_all)


# Signals the proxy uses to stop the engine (SIGTERM) or that the engine
# relies on for its own graceful shutdown (SIGINT).
_STOP_SIGNALS = (signal.SIGTERM, signal.SIGINT)

# exec()s the real engine command after clearing an inherited block mask and
# SIG_IGN for the stop signals.  Both survive fork+exec, and Popen offers no
# thread-safe way to reset them (preexec_fn is unsafe in a threaded parent).
_SIGNAL_RESET_TRAMPOLINE = """\
import os, signal, sys
sigs = (signal.SIGTERM, signal.SIGINT)
signal.pthread_sigmask(signal.SIG_UNBLOCK, sigs)
for s in sigs:
    signal.signal(s, signal.SIG_DFL)
try:
    os.execvp(sys.argv[1], sys.argv[1:])
except OSError as e:
    sys.stderr.write(f"cannot exec inner engine {sys.argv[1]!r}: {e}\\n")
    sys.exit(127)
"""


def _inherited_stop_signals_disabled() -> bool:
    """True if a child spawned from this thread would not die on SIGTERM.

    The block mask is per thread (and inherited by the child of the thread
    that forks); the disposition is process wide.
    """
    blocked = signal.pthread_sigmask(signal.SIG_BLOCK, [])
    return any(
        sig in blocked or signal.getsignal(sig) == signal.SIG_IGN
        for sig in _STOP_SIGNALS
    )


def _with_signal_reset(cmd: List[str]) -> List[str]:
    """Prefix ``cmd`` with the trampoline when the inherited signal state would
    make the engine deaf to SIGTERM; otherwise return it unchanged."""
    if os.name != "posix" or not _inherited_stop_signals_disabled():
        return cmd
    # -I: ignore PYTHON* env vars and user site for the trampoline itself; the
    # engine still gets the full environment through os.execvp.
    return [sys.executable, "-I", "-S", "-c", _SIGNAL_RESET_TRAMPOLINE, *cmd]


def _port_in_use(host: str, port: int) -> bool:
    try:
        with socket.create_connection((host, port), timeout=0.5):
            return True
    except OSError:
        return False


class SubprocessEngine:
    """Owns the inner-engine process tree: spawn, readiness, I/O, teardown.

    One engine is spawned per instance; a crashed engine is not re-spawned.
    """

    def __init__(
        self,
        cmd: List[str],
        port: int = 8001,
        host: str = "127.0.0.1",
        env: Optional[Dict[str, str]] = None,
        spec: Optional[EngineSpec] = None,
        shutdown_grace_seconds: float = DEFAULT_SHUTDOWN_GRACE_SECONDS,
        port_release_timeout: float = PORT_RELEASE_TIMEOUT_SECONDS,
    ):
        self._cmd = cmd
        self._port = port
        self._host = host
        self._env = env
        self._spec = spec or EngineSpec(name="default")
        self._shutdown_grace_seconds = shutdown_grace_seconds
        self._port_release_timeout = port_release_timeout

        self._proc: Optional[subprocess.Popen] = None
        self._base_url = f"http://{host}:{port}"
        self._client: Optional[httpx.AsyncClient] = None
        self._client_loop: Optional[asyncio.AbstractEventLoop] = None
        self._client_close_task: Optional[asyncio.Task[None]] = None

        self._snapshot: Optional[TreeSnapshot] = None
        self._pipe_threads: List[threading.Thread] = []
        self._log_tail: Deque[str] = collections.deque(maxlen=LOG_TAIL_LINES)
        self._log_lock = threading.Lock()
        self._lifecycle_lock = threading.RLock()
        self._spawned_at: Optional[float] = None
        # Latched by close(): no spawn may happen afterwards, even one racing
        # on another thread during shutdown.
        self._shut_down = False

    # ------------------------------------------------------------------
    # Introspection
    # ------------------------------------------------------------------

    @property
    def base_url(self) -> str:
        return self._base_url

    @property
    def pid(self) -> Optional[int]:
        return self._proc.pid if self._proc else None

    @property
    def returncode(self) -> Optional[int]:
        """Exit code of the root process; None while running or before spawn."""
        if self._proc is None:
            return None
        return self._proc.poll()

    def is_alive(self) -> bool:
        return self._proc is not None and self._proc.poll() is None

    @property
    def snapshot(self) -> Optional[TreeSnapshot]:
        return self._snapshot

    @property
    def uptime_seconds(self) -> Optional[float]:
        if self._spawned_at is None:
            return None
        return time.monotonic() - self._spawned_at

    def log_tail(self, n: int = 50) -> List[str]:
        with self._log_lock:
            return list(self._log_tail)[-n:]

    def describe_exit(self) -> str:
        """One-line forensic summary of the engine's final state."""
        parts = [f"pid={self.pid}", describe_returncode(self.returncode)]
        if self._snapshot is not None:
            if self._snapshot.rss_bytes:
                parts.append(f"last_rss={self._snapshot.rss_bytes / 2**20:.0f}MiB")
            parts.append(f"tree={self._snapshot.pids}")
        uptime = self.uptime_seconds
        if uptime is not None:
            parts.append(f"uptime={uptime:.1f}s")
        return ", ".join(parts)

    def format_failure(self, headline: str, tail_lines: int = 20) -> str:
        msg = f"{headline} ({self.describe_exit()})"
        tail = self.log_tail(tail_lines)
        if tail:
            msg += "\nLast inner engine output:\n" + "\n".join(tail)
        return msg

    # ------------------------------------------------------------------
    # Lifecycle
    # ------------------------------------------------------------------

    def spawn(self) -> int:
        with self._lifecycle_lock:
            if self._shut_down:
                raise RuntimeError("Inner engine is shut down")
            if self.is_alive():
                raise RuntimeError(f"Inner engine already running (pid={self.pid})")
            if self._proc is not None:
                # The previous engine died on its own: make sure nothing it
                # forked is still holding the GPU or the port.
                self._kill_tree_locked(grace_seconds=5.0)
                self._proc = None

            # A stale server on our port would answer the readiness probe
            # and we would proxy traffic to a process we do not own.
            if not self._wait_port_free(self._port_release_timeout):
                raise EngineStartupError(
                    f"Port {self._host}:{self._port} is already in use; "
                    "refusing to start the inner engine on top of another process"
                )

            # The proxy's own environment (including anything set via the CLI
            # --setenv flags) is inherited by the engine; self._env wins.
            env = {
                **os.environ,
                "TQDM_MININTERVAL": "5",
                "PYTHONUNBUFFERED": "1",
                **(self._env or {}),
            }
            full_cmd = [
                *self._cmd,
                self._spec.host_arg,
                self._host,
                self._spec.port_arg,
                str(self._port),
            ]
            with self._log_lock:
                self._log_tail.clear()
            # Own session => own process group, so teardown can signal the
            # whole tree and a Ctrl-C on our terminal does not reach it.
            # stdin is /dev/null so the engine never holds our terminal.
            # If we inherited SIGTERM blocked/ignored (pytest, some wrappers),
            # the engine would too and every stop would wait out the whole
            # grace period before SIGKILL; the trampoline undoes that.
            launch_cmd = _with_signal_reset(full_cmd)
            if launch_cmd is not full_cmd:
                logger.info(
                    "SIGTERM/SIGINT are blocked or ignored in this process; "
                    "resetting them for the inner engine"
                )
            self._proc = subprocess.Popen(
                launch_cmd,
                env=env,
                start_new_session=True,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
            )
            self._spawned_at = time.monotonic()
            self._snapshot = snapshot_tree(self._proc.pid)
            self._pipe_threads = [
                self._start_pipe_thread(self._proc.stdout, sys.__stdout__, "stdout"),
                self._start_pipe_thread(self._proc.stderr, sys.__stderr__, "stderr"),
            ]
            _LIVE_ENGINES.add(self)
            logger.info(
                "Spawned inner engine: cmd=%s, pid=%d, port=%d",
                " ".join(self._cmd),
                self._proc.pid,
                self._port,
            )
            return self._proc.pid

    def _wait_port_free(self, timeout: float) -> bool:
        deadline = time.monotonic() + timeout
        while _port_in_use(self._host, self._port):
            if time.monotonic() >= deadline:
                return False
            time.sleep(0.2)
        return True

    def _start_pipe_thread(
        self, pipe: Optional[IO[bytes]], target: Optional[IO[str]], name: str
    ) -> threading.Thread:
        t = threading.Thread(
            target=self._pipe_with_prefix,
            args=(pipe, target, LOG_PREFIX),
            name=f"proxy-engine-{name}",
            daemon=True,
        )
        t.start()
        return t

    def _pipe_with_prefix(
        self, pipe: Optional[IO[bytes]], target: Optional[IO[str]], prefix: str
    ) -> None:
        """Forward child output line by line, prefixed, until EOF.

        Must keep draining no matter what: if this thread dies, the pipe
        buffer fills and the inner engine blocks forever on its next write.
        """
        if pipe is None:
            return
        try:
            for raw in iter(pipe.readline, b""):
                line = raw.decode("utf-8", errors="replace")
                stripped = line.rstrip("\r\n")
                if stripped.strip():
                    with self._log_lock:
                        self._log_tail.append(stripped)
                if target is None:
                    continue
                try:
                    target.write(prefix_line(line, prefix))
                    target.flush()
                except (ValueError, OSError):
                    # Our own stdout went away; keep draining the child.
                    target = None
        except (ValueError, OSError):
            pass
        finally:
            try:
                pipe.close()
            except OSError:
                pass

    async def wait_ready(
        self,
        timeout: Optional[float] = DEFAULT_STARTUP_TIMEOUT_SECONDS,
        poll_interval: float = 1.0,
    ) -> None:
        """Block until the health endpoint returns 200.

        Fails fast with :class:`EngineStartupError` when the process exits
        (OOM, bad flag, missing model) instead of polling a dead port until
        the timeout, and includes the exit reason plus the output tail.
        """
        deadline = None if timeout is None else time.monotonic() + timeout
        url = f"{self._base_url}{self._spec.health_endpoint}"
        async with httpx.AsyncClient(timeout=httpx.Timeout(5.0)) as probe:
            while True:
                if not self.is_alive():
                    self._join_pipe_threads(2.0)
                    raise EngineStartupError(
                        self.format_failure("Inner engine exited during startup")
                    )
                try:
                    resp = await probe.get(url)
                    if resp.status_code == 200:
                        self.refresh_snapshot()
                        logger.info(
                            "Inner engine is ready (%.1fs)",
                            self.uptime_seconds or 0.0,
                        )
                        return
                except httpx.HTTPError:
                    # Startup can briefly yield incomplete HTTP responses or
                    # transport failures; retry within the readiness deadline.
                    pass
                if deadline is not None and time.monotonic() >= deadline:
                    raise EngineStartupError(
                        self.format_failure(
                            f"Inner engine not healthy after {timeout:.0f}s"
                        )
                    )
                await asyncio.sleep(poll_interval)

    def refresh_snapshot(self) -> Optional[TreeSnapshot]:
        """Re-record tree membership while the root is alive.

        Called periodically so that workers which are reparented to init
        when the root crashes can still be found and killed.
        """
        if self.is_alive():
            assert self._proc is not None
            self._snapshot = snapshot_tree(self._proc.pid, previous=self._snapshot)
        return self._snapshot

    def kill_leftovers(self, grace_seconds: Optional[float] = None) -> List[int]:
        """Kill whatever remains of the current tree; returns survivors."""
        with self._lifecycle_lock:
            return self._kill_tree_locked(grace_seconds)

    def _kill_tree_locked(self, grace_seconds: Optional[float]) -> List[int]:
        if self._proc is None:
            return []
        grace = self._shutdown_grace_seconds if grace_seconds is None else grace_seconds
        snap = self._snapshot or snapshot_tree(self._proc.pid)
        proc = self._proc

        def _reap(timeout: float) -> None:
            try:
                proc.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                pass

        survivors = kill_tree(snap, grace_seconds=grace, reap=_reap, log=logger)
        _reap(0)
        self._join_pipe_threads(2.0)
        return survivors

    def _join_pipe_threads(self, timeout: float) -> None:
        for t in self._pipe_threads:
            if t is not threading.current_thread():
                t.join(timeout=timeout)

    def shutdown(self, grace_seconds: Optional[float] = None) -> None:
        try:
            with self._lifecycle_lock:
                if self._shut_down:
                    return
                self._shut_down = True
                if self._proc is None:
                    return
                pid = self._proc.pid
                survivors = self._kill_tree_locked(grace_seconds)
                logger.info(
                    "Inner engine process tree terminated (pid=%d, %s)%s",
                    pid,
                    describe_returncode(self._proc.returncode),
                    f"; survivors={survivors}" if survivors else "",
                )
                self._proc = None
                self._snapshot = None
                _LIVE_ENGINES.discard(self)
        finally:
            # Also close a client created without a subprocess, or on a repeated stop.
            self._schedule_client_close()

    def _schedule_client_close(self) -> None:
        loop = self._client_loop
        if self._client is None or loop is None:
            return
        if loop.is_closed():
            logger.warning(
                "Cannot close inner engine HTTP client: its event loop is closed"
            )
            return
        try:
            current = asyncio.get_running_loop()
        except RuntimeError:
            current = None
        if current is loop:
            self._begin_client_close()
        elif loop.is_running():
            # shutdown() may run on a worker or signal-handling thread.
            loop.call_soon_threadsafe(self._begin_client_close)
        elif current is None:
            loop.run_until_complete(self.close_client())
        else:
            logger.warning(
                "Cannot close inner engine HTTP client on a different event loop"
            )

    # ------------------------------------------------------------------
    # HTTP client
    # ------------------------------------------------------------------

    async def start_client(self) -> None:
        # Fresh pool for the one engine this instance owns.
        await self.close_client()
        limits = _http_connection_limits()
        if self._shut_down:
            raise RuntimeError("Cannot create an HTTP client after shutdown")
        self._client_loop = asyncio.get_running_loop()
        self._client = httpx.AsyncClient(
            base_url=self._base_url,
            timeout=httpx.Timeout(
                connect=10.0,
                read=None,
                write=10.0,
                pool=5.0,
            ),
            limits=limits,
        )

    def _begin_client_close(self) -> Optional[asyncio.Task[None]]:
        """Detach the pool once, then close it on its owning event loop."""
        if self._client_close_task is not None and not self._client_close_task.done():
            return self._client_close_task
        if self._client is None:
            return None
        client, self._client = self._client, None
        self._client_close_task = asyncio.create_task(self._close_http_client(client))
        return self._client_close_task

    @staticmethod
    async def _close_http_client(client: httpx.AsyncClient) -> None:
        try:
            await client.aclose()
        except Exception:
            logger.warning("Failed to close inner engine HTTP client", exc_info=True)

    async def close_client(self) -> None:
        task = self._begin_client_close()
        if task is not None:
            # A second caller must wait for the same cleanup, not cancel it.
            await asyncio.shield(task)

    def _require_client(self) -> httpx.AsyncClient:
        if self._client is None:
            raise httpx.ConnectError("Inner engine client is not connected")
        return self._client

    async def forward(
        self,
        method: str,
        endpoint: str,
        *,
        json: Optional[Dict[str, Any]] = None,
        content: Optional[Union[bytes, AsyncIterable[bytes]]] = None,
        content_type: Optional[str] = None,
        headers: Optional[Dict[str, str]] = None,
        stream: bool = False,
        raw_body: bool = False,
    ) -> httpx.Response:
        """Send one request to ``endpoint`` on the inner engine.

        ``json`` sends an application/json body; ``content`` + ``content_type``
        sends a raw body (used for multipart forms proxied byte-for-byte).
        ``content`` may be an async iterable, in which case the body is
        streamed upstream chunk by chunk (chunked transfer encoding unless the
        caller supplies ``Content-Length`` in ``headers``).
        Non-2xx responses are returned, not raised, so the engine's own error
        body reaches the client. With ``stream=True`` the response body is not
        read; the caller iterates it and calls ``aclose()``.
        """
        client = self._require_client()
        kwargs: Dict[str, Any] = {}
        if headers:
            kwargs["headers"] = dict(headers)
        if json is not None:
            kwargs["json"] = json
        elif content is not None:
            kwargs["content"] = content
            if content_type:
                kwargs.setdefault("headers", {})
                kwargs["headers"] = {
                    **kwargs["headers"],
                    "Content-Type": content_type,
                }
        request = client.build_request(method, endpoint, **kwargs)
        resp = await client.send(request, stream=stream or raw_body)
        if raw_body or (stream and resp.status_code >= 300):
            read_scope = anyio.CancelScope()

            async def read_body() -> httpx.Response:
                with read_scope:
                    try:
                        if not raw_body:
                            await resp.aread()
                            return resp
                        if resp.is_stream_consumed:
                            content = resp.content
                        else:
                            # Avoid aiter_raw()'s unshielded Response.aclose() at EOF.
                            assert isinstance(resp.stream, httpx.AsyncByteStream)
                            content = b"".join([chunk async for chunk in resp.stream])
                        headers = dict(resp.headers)
                        encoding = headers.pop("content-encoding", None)
                        result = httpx.Response(
                            resp.status_code,
                            content=content,
                            headers=headers,
                            request=request,
                        )
                        if encoding is not None:
                            result.headers["content-encoding"] = encoding
                        return result
                    finally:
                        await await_thread_completion(resp.aclose())
                raise asyncio.CancelledError()

            # HTTPX shields internal read-error cleanup with an AnyIO scope.
            # Cancel through that scope so an already-started close stays shielded.
            task = asyncio.create_task(read_body())
            try:
                return await asyncio.shield(task)
            except asyncio.CancelledError:
                read_scope.cancel()
                try:
                    await await_thread_completion(task)
                except BaseException:
                    pass  # Preserve caller cancellation, including late close errors.
                # A pre-start cancellation never enters read_body()'s finally.
                try:
                    await await_thread_completion(resp.aclose())
                except BaseException:
                    pass
                raise
        return resp

    async def forward_health(
        self, timeout: float = HEALTH_PROBE_TIMEOUT_SECONDS
    ) -> bool:
        if not self.is_alive() or self._client is None:
            return False
        try:
            # The shared client has no read timeout (generations can run long);
            # a hung engine must not hang the health probe with it.
            resp = await self._client.get(self._spec.health_endpoint, timeout=timeout)
            return resp.status_code == 200
        except httpx.HTTPError:
            return False

    async def forward_metrics(
        self, timeout: float = HEALTH_PROBE_TIMEOUT_SECONDS
    ) -> str:
        self._spec.require_metrics()
        try:
            # Scrapes must be bounded even though inference reads are unlimited.
            resp = await self._require_client().get(
                self._spec.metrics_endpoint, timeout=timeout
            )
            resp.raise_for_status()
            return resp.text
        except Exception:
            return ""

    async def forward_models(self) -> httpx.Response:
        self._spec.require_models()
        resp = await self._require_client().get(self._spec.models_endpoint)
        resp.raise_for_status()
        return resp

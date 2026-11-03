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

"""Cross-process tests for the proxy CLI.

Every test talks to a *real* proxy process -- ``python -m
aibrix.openai_frontend.main --engine proxy`` served by uvicorn on a TCP port
-- which itself spawns ``mock_engine.py`` as its inner engine subprocess.
Nothing runs in-process, so these cover the CLI wiring TestClient cannot.
"""

from __future__ import annotations

import os
import shlex
import signal
import socket
import subprocess
import sys
import time
from typing import List, Optional

import httpx
import pytest

psutil = pytest.importorskip("psutil")


HERE = os.path.dirname(os.path.abspath(__file__))
PKG_ROOT = os.path.dirname(os.path.dirname(HERE))  # .../python/aibrix
MOCK = os.path.join(HERE, "mock_engine.py")
MiB = 1 << 20
DEFAULT_MAX_INPUT = 64 * MiB  # HTTP_DEFAULT_MAX_INPUT_SIZE


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


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
            "vllm",
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
        proxy = ProxyProcess(str(tmp_path))
        worker_process = None
        try:
            proxy.wait_ready()
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


class TestSetenv:
    def test_setenv_args_reach_proxy_and_engine(self, tmp_path):
        """--setenv KEY VALUE is applied to the proxy process and inherited by
        the inner engine subprocess."""
        p = ProxyProcess(
            str(tmp_path),
            "--setenv",
            "aibrix-test-foo",
            "bar-value",
            "aibrix-test-eq",
            "https://x?y=1",
        )
        try:
            p.wait_ready()
            # The inner engine inherited the variables (it is the consumer the
            # flags ultimately exist for, e.g. storage SDK clients).
            r = httpx.get(
                f"{p.engine_url}/_test/env",
                params=[
                    ("name", "AIBRIX_TEST_FOO"),
                    ("name", "AIBRIX_TEST_EQ"),
                ],
                timeout=10,
            )
            assert r.status_code == 200
            assert r.json() == {
                "AIBRIX_TEST_FOO": "bar-value",
                "AIBRIX_TEST_EQ": "https://x?y=1",
            }
            # The proxy's own process has them too (its storage clients read
            # the same environment).
            assert p.proc.poll() is None
        finally:
            p.stop()

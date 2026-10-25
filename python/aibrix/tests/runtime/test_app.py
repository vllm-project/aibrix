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

import os
import subprocess
import sys

import aibrix.app as app_module
from aibrix.app import initial_prometheus_multiproc_dir, parse_runtime_args


def test_runtime_defaults_to_all_interfaces():
    assert parse_runtime_args([]).host == "0.0.0.0"


def test_restart_keeps_metric_files_of_running_engines(tmp_path, monkeypatch):
    reaped = subprocess.Popen([sys.executable, "-c", "pass"])
    reaped.wait()
    zombie = subprocess.Popen([sys.executable, "-c", "pass"])
    try:
        # Wait for the exit without reaping, so the child stays a zombie.
        os.waitid(os.P_PID, zombie.pid, os.WEXITED | os.WNOWAIT)
        running = os.getpid()
        for pid in (running, reaped.pid, zombie.pid):
            (tmp_path / f"counter_{pid}.db").touch()
            (tmp_path / f"gauge_livesum_{pid}.db").touch()
        monkeypatch.setenv("PROMETHEUS_MULTIPROC_DIR", str(tmp_path))

        initial_prometheus_multiproc_dir()
    finally:
        zombie.wait()

    assert sorted(p.name for p in tmp_path.iterdir()) == [
        f"counter_{running}.db",
        f"gauge_livesum_{running}.db",
    ]


def test_cleanup_decision_per_file(tmp_path, monkeypatch):
    def fake_pid_is_alive(pid):
        if pid > 2**63:
            raise OverflowError  # what os.kill raises for a pid past the C range
        return pid != 202

    monkeypatch.setattr(app_module, "pid_is_alive", fake_pid_is_alive)
    for name in (
        "counter_101.db",
        "counter_202.db",
        "counter_0.db",
        "counter_0101.db",
        "counter_99999999999999999999.db",
        "notes.txt",
    ):
        (tmp_path / name).touch()
    (tmp_path / "leftover").mkdir()
    monkeypatch.setenv("PROMETHEUS_MULTIPROC_DIR", str(tmp_path))

    initial_prometheus_multiproc_dir()

    assert sorted(p.name for p in tmp_path.iterdir()) == ["counter_101.db"]


def test_cleanup_tolerates_a_file_removed_meanwhile(tmp_path, monkeypatch):
    # An exiting vLLM engine removes its own live-gauge files (multiprocess.mark_process_dead), so a listed file can
    # be gone by the time cleanup unlinks it.
    stale = tmp_path / "gauge_livesum_404.db"
    stale.touch()

    def engine_exits_meanwhile(pid):
        stale.unlink()
        return False

    monkeypatch.setattr(app_module, "pid_is_alive", engine_exits_meanwhile)
    monkeypatch.setenv("PROMETHEUS_MULTIPROC_DIR", str(tmp_path))

    initial_prometheus_multiproc_dir()

    assert list(tmp_path.iterdir()) == []

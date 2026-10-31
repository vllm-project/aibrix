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

"""Failure-convergence tests for the ModelClaim runtime engine supervisor."""

import os
import select
import signal
import subprocess
import sys
from datetime import datetime, timedelta, timezone

import pytest

import aibrix.runtime.model_runtime as runtime_module
from aibrix.runtime.engine_registry import EngineRegistry
from aibrix.runtime.model_runtime import EngineLauncher, ModelRuntime


class _ControlledProcess:
    def __init__(self):
        self.alive = True

    def poll(self):
        return None if self.alive else 1


class _ControlledLauncher(EngineLauncher):
    def __init__(self):
        self.launches = []
        self.processes = {}
        self.stopped = []
        self.stop_exits = True

    def launch(self, inst, artifact_url, engine_config, additional_config):
        process = _ControlledProcess()
        inst.proc = process
        self.launches.append(inst.model_name)
        self.processes.setdefault(inst.model_name, []).append(process)
        return 5000 + len(self.launches)

    def stop(self, inst):
        self.stopped.append(inst.model_name)
        if self.stop_exits and inst.proc is not None:
            inst.proc.alive = False

    def sleep(self, inst, level):
        raise AssertionError("sleep is not part of this test launcher")

    def wake(self, inst):
        raise AssertionError("wake is not part of this test launcher")


def _record():
    return {
        "model_name": "qwen",
        "port": 8100,
        "ipc_name": "kvc_qwen",
        "pid": 4321,
        "pid_start_time": "123456",
        "engine": "vllm",
        "artifact_url": "hf://Qwen/Qwen3-0.6B",
        "engine_config": {"args": {"--max-model-len": "2048"}},
        "additional_config": None,
        "claim_ref": {"namespace": "default", "name": "qwen", "uid": "uid-1"},
        "phase": "active",
        "restart_count": 1,
        "last_error": None,
        "last_transition": "2026-07-14T00:00:00+00:00",
        "next_restart_at": None,
    }


def test_re_adopt_keeps_a_live_engine_without_relaunching(tmp_path, monkeypatch):
    registry = EngineRegistry(tmp_path / "engines.json")
    registry.save([_record()])
    launcher = _ControlledLauncher()
    monkeypatch.setattr(runtime_module, "pid_is_alive", lambda pid: pid == 4321)
    monkeypatch.setattr(runtime_module, "process_start_time", lambda pid: "123456")
    monkeypatch.setattr(runtime_module, "engine_ready", lambda port: port == 8100)

    runtime = ModelRuntime(launcher, registry=registry)

    adopted = runtime.list_models()
    assert launcher.launches == []
    assert len(adopted) == 1
    assert adopted[0].model_name == "qwen"
    assert adopted[0].port == 8100
    assert adopted[0].ipc_name == "kvc_qwen"
    assert adopted[0].phase == "active"
    assert adopted[0].restart_count == 1
    assert runtime.snapshot_metrics().re_adoption_outcomes == {"success": 1}


def test_re_adopt_rejects_a_reused_pid(tmp_path, monkeypatch):
    clock = [datetime(2026, 7, 14, tzinfo=timezone.utc)]
    record = _record()
    record["restart_count"] = 0
    registry = EngineRegistry(tmp_path / "engines.json")
    registry.save([record])
    monkeypatch.setattr(runtime_module, "pid_is_alive", lambda pid: pid == 4321)
    monkeypatch.setattr(runtime_module, "process_start_time", lambda pid: "other")

    runtime = ModelRuntime(
        _ControlledLauncher(), registry=registry, now=lambda: clock[0]
    )

    adopted = runtime.list_models()[0]
    assert adopted.phase == "restarting"
    assert adopted.restart_count == 1
    assert adopted.next_restart_at == clock[0] + timedelta(seconds=2)
    assert runtime.snapshot_metrics().re_adoption_outcomes == {"restart_scheduled": 1}


def test_supervisor_restarts_only_the_dead_engine_after_backoff(tmp_path):
    clock = [datetime(2026, 7, 14, tzinfo=timezone.utc)]
    launcher = _ControlledLauncher()
    runtime = ModelRuntime(
        launcher,
        registry=EngineRegistry(tmp_path / "engines.json"),
        now=lambda: clock[0],
    )
    failed = runtime.activate(
        model_name="failed", artifact_url="hf://Org/Failed", engine="sglang"
    )
    healthy = runtime.activate(
        model_name="healthy", artifact_url="hf://Org/Healthy", engine="sglang"
    )
    healthy_process = healthy.proc
    failed.proc.alive = False

    runtime.supervise_once()

    assert failed.phase == "restarting"
    assert failed.restart_count == 1
    assert failed.next_restart_at == clock[0] + timedelta(seconds=2)
    assert launcher.launches == ["failed", "healthy"]
    assert healthy.proc is healthy_process

    clock[0] += timedelta(seconds=2)
    runtime.supervise_once()

    assert failed.phase == "booting"
    assert launcher.launches == ["failed", "healthy", "failed"]
    assert healthy.proc is healthy_process
    assert runtime.snapshot_metrics().restart_attempts == {"failed": 1}


def test_supervisor_reports_terminal_failure_after_restart_budget(tmp_path):
    clock = [datetime(2026, 7, 14, tzinfo=timezone.utc)]
    launcher = _ControlledLauncher()
    runtime = ModelRuntime(
        launcher,
        registry=EngineRegistry(tmp_path / "engines.json"),
        now=lambda: clock[0],
        max_restarts=2,
    )
    failed = runtime.activate(
        model_name="failed", artifact_url="hf://Org/Failed", engine="sglang"
    )

    failed.proc.alive = False
    runtime.supervise_once()
    clock[0] += timedelta(seconds=2)
    runtime.supervise_once()

    failed.proc.alive = False
    runtime.supervise_once()
    clock[0] += timedelta(seconds=4)
    runtime.supervise_once()

    failed.proc.alive = False
    runtime.supervise_once()

    snapshot = runtime.snapshot()["models"]
    assert launcher.launches == ["failed", "failed", "failed"]
    assert failed.phase == "failed"
    assert failed.restart_count == 2
    assert failed.last_error == "engine exited; restart budget exhausted"
    assert snapshot[0]["alive"] is False
    assert snapshot[0]["ready"] is False
    assert snapshot[0]["phase"] == "failed"
    metrics = runtime.snapshot_metrics()
    assert metrics.restart_attempts == {"failed": 2}
    assert metrics.restart_budget_exhausted == {"failed": 1}


def test_supervisor_cleans_an_orphaned_process_group_before_backoff(
    tmp_path, monkeypatch
):
    clock = [datetime(2026, 7, 14, tzinfo=timezone.utc)]
    launcher = _ControlledLauncher()
    runtime = ModelRuntime(
        launcher,
        registry=EngineRegistry(tmp_path / "engines.json"),
        now=lambda: clock[0],
    )
    failed = runtime.activate(
        model_name="failed", artifact_url="hf://Org/Failed", engine="sglang"
    )
    stale_pid = failed.pid
    failed.proc.alive = False

    group_liveness = iter((True, True, False))
    monkeypatch.setattr(
        runtime_module,
        "process_group_is_alive",
        lambda process_group_id: next(group_liveness, False),
    )
    killed = []
    monkeypatch.setattr(
        runtime_module.os,
        "killpg",
        lambda process_group_id, signum: killed.append((process_group_id, signum)),
    )

    runtime.supervise_once()

    assert launcher.launches == ["failed"]
    assert launcher.stopped == ["failed"]
    assert killed == [(stale_pid, signal.SIGKILL)]
    assert failed.next_restart_at == clock[0] + timedelta(seconds=2)

    clock[0] += timedelta(seconds=2)
    monkeypatch.setattr(runtime_module, "process_group_is_alive", lambda _: False)
    runtime.supervise_once()

    assert launcher.launches == ["failed", "failed"]


def test_supervisor_treats_missing_process_group_id_as_clean(tmp_path, monkeypatch):
    launcher = _ControlledLauncher()
    runtime = ModelRuntime(launcher, registry=EngineRegistry(tmp_path / "engines.json"))
    inst = runtime.activate(
        model_name="no-pid", artifact_url="hf://Org/NoPID", engine="sglang"
    )
    inst.pid = None
    monkeypatch.setattr(
        runtime_module,
        "process_group_is_alive",
        lambda _: pytest.fail("missing PID must not be probed"),
    )

    assert runtime._terminate_stale_process_group(inst)
    assert launcher.stopped == []


def test_re_adopt_stops_an_orphaned_group_for_a_terminal_engine(tmp_path, monkeypatch):
    record = _record()
    record["phase"] = "failed"
    registry = EngineRegistry(tmp_path / "engines.json")
    registry.save([record])
    launcher = _ControlledLauncher()
    monkeypatch.setattr(runtime_module, "process_group_is_alive", lambda _: True)
    monkeypatch.setattr(runtime_module.os, "killpg", lambda *_: None)

    runtime = ModelRuntime(launcher, registry=registry)

    assert runtime.list_models()[0].phase == "failed"
    assert launcher.stopped == ["qwen"]


def test_deactivate_keeps_real_process_in_stopping_state_until_exit(tmp_path):
    registry = EngineRegistry(tmp_path / "engines.json")
    launcher = _ControlledLauncher()
    launcher.stop_exits = False
    runtime = ModelRuntime(launcher, registry=registry)
    inst = runtime.activate(
        model_name="stopping", artifact_url="hf://Org/Stopping", engine="sglang"
    )

    runtime.deactivate("stopping")

    assert runtime.list_models() == [inst]
    assert inst.phase == "stopping"
    assert registry.load()[0]["phase"] == "stopping"

    inst.proc.alive = False
    runtime.supervise_once()

    assert runtime.list_models() == []
    assert registry.load() == []


@pytest.mark.skipif(os.name != "posix", reason="requires POSIX process groups")
def test_stop_terminates_a_real_process_ignoring_sigterm(tmp_path, monkeypatch):
    clock = [datetime(2026, 7, 14, tzinfo=timezone.utc)]
    launcher = runtime_module.SubprocessEngineLauncher()
    processes = []

    def launch(inst, *_):
        proc = subprocess.Popen(
            [
                sys.executable,
                "-u",
                "-c",
                "import signal; signal.signal(signal.SIGTERM, signal.SIG_IGN); "
                "print('ready'); signal.pause()",
            ],
            stdout=subprocess.PIPE,
            start_new_session=True,
        )
        processes.append(proc)
        inst.proc = proc
        assert select.select([proc.stdout], [], [], 5)[0], "child did not become ready"
        assert proc.stdout.readline() == b"ready\n"
        return proc.pid

    monkeypatch.setattr(launcher, "launch", launch)
    monkeypatch.setattr(runtime_module, "instance_ready", lambda _: True)
    runtime = ModelRuntime(
        launcher,
        registry=EngineRegistry(tmp_path / "engines.json"),
        now=lambda: clock[0],
    )
    try:
        stuck = runtime.activate(
            model_name="stuck", artifact_url="hf://Org/Stuck", engine="sglang"
        )
        other = runtime.activate(
            model_name="other", artifact_url="hf://Org/Other", engine="sglang"
        )
        runtime.deactivate("stuck")
        clock[0] += timedelta(seconds=29.9)
        runtime.supervise_once()
        assert stuck.proc.poll() is None
        clock[0] += timedelta(seconds=0.1)
        runtime.supervise_once()
        assert stuck.proc.wait(timeout=5) == -signal.SIGKILL
        runtime.supervise_once()
        assert runtime.list_models() == [other]
        assert other.proc.poll() is None
    finally:
        for proc in processes:
            if proc.poll() is None:
                os.killpg(proc.pid, signal.SIGKILL)
            proc.wait(timeout=5)
            proc.stdout.close()


def test_production_singleton_enables_registry_and_supervisor(tmp_path, monkeypatch):
    monkeypatch.delenv("AIBRIX_MODEL_RUNTIME_MOCK", raising=False)
    monkeypatch.setenv("AIBRIX_ENGINE_REGISTRY_PATH", str(tmp_path / "engines.json"))
    monkeypatch.setattr(runtime_module, "SubprocessEngineLauncher", _ControlledLauncher)
    runtime_module._AGENT = None

    runtime = runtime_module.get_model_runtime()
    try:
        assert runtime._registry.path == tmp_path / "engines.json"
        assert runtime._supervisor_thread is not None
        assert runtime._supervisor_thread.is_alive()
    finally:
        runtime.stop_supervisor()
        runtime_module._AGENT = None


@pytest.fixture
def stopping_runtime(tmp_path, monkeypatch):
    clock = [datetime(2026, 7, 14, tzinfo=timezone.utc)]
    launcher = _ControlledLauncher()
    launcher.stop_exits = False
    registry = EngineRegistry(tmp_path / "engines.json")
    runtime = ModelRuntime(launcher, registry=registry, now=lambda: clock[0])
    signals = []
    monkeypatch.setattr(
        runtime_module.os, "killpg", lambda pid, sig: signals.append((pid, sig))
    )
    monkeypatch.setattr(runtime_module, "process_group_is_alive", lambda _: False)
    monkeypatch.setattr(runtime_module, "process_start_time", lambda _: "123456")
    monkeypatch.setattr(runtime_module, "instance_ready", lambda _: True)
    inst = runtime.activate(model_name="stuck", artifact_url="hf://Org/Stuck")
    return runtime, inst, launcher, registry, clock, signals


@pytest.mark.parametrize("operation", ["deactivate", "supervise"])
def test_expired_mock_stop_never_signals_host_processes(monkeypatch, operation):
    clock = [datetime(2026, 7, 14, tzinfo=timezone.utc)]
    launcher = runtime_module.MockEngineLauncher()
    runtime = ModelRuntime(launcher, now=lambda: clock[0])
    inst = runtime.activate(model_name="mock", artifact_url="hf://Org/Mock")
    # Simulate an interrupted stop rather than the normal mock deactivation,
    # which immediately removes an instance without a process handle.
    inst.phase = "stopping"
    clock[0] += timedelta(seconds=31)
    signals = []
    monkeypatch.setattr(runtime_module, "process_start_time", lambda _: None)
    monkeypatch.setattr(
        runtime_module.os, "killpg", lambda pid, sig: signals.append((pid, sig))
    )

    if operation == "deactivate":
        runtime.deactivate("mock")
        assert runtime.list_models() == []
    else:
        runtime.supervise_once()
    assert signals == []
    assert launcher.stopped == ["mock"]


def test_stop_escalates_only_after_grace_and_retains_record_until_exit(
    stopping_runtime,
):
    runtime, inst, launcher, registry, clock, signals = stopping_runtime
    other = runtime.activate(model_name="other", artifact_url="hf://Org/Other")
    runtime.deactivate("stuck")
    clock[0] += timedelta(seconds=29.9)
    runtime.supervise_once()
    assert signals == []

    clock[0] += timedelta(seconds=0.1)
    runtime.supervise_once()
    assert signals == [(inst.pid, signal.SIGKILL)]
    assert inst in runtime.list_models()
    assert any(record["model_name"] == "stuck" for record in registry.load())
    assert other.proc.alive
    assert other.phase == "active"
    assert set(launcher.stopped) == {"stuck"}

    inst.proc.alive = False
    runtime.supervise_once()
    assert runtime.list_models() == [other]
    assert [record["model_name"] for record in registry.load()] == ["other"]


def test_stop_does_not_force_kill_an_engine_that_exits_during_grace(stopping_runtime):
    runtime, inst, _, registry, clock, signals = stopping_runtime
    runtime.deactivate("stuck")
    inst.proc.alive = False
    clock[0] += timedelta(seconds=31)
    runtime.supervise_once()
    assert runtime.list_models() == []
    assert registry.load() == []
    assert signals == []


@pytest.mark.parametrize("retry", ["deactivate", "supervise"])
def test_stop_errors_do_not_extend_the_grace_period(
    stopping_runtime, monkeypatch, retry
):
    runtime, inst, launcher, registry, clock, signals = stopping_runtime
    runtime.deactivate("stuck")
    started = inst.last_transition
    clock[0] += timedelta(seconds=20)

    def fail_stop(_):
        raise RuntimeError("stop unavailable")

    monkeypatch.setattr(launcher, "stop", fail_stop)
    if retry == "deactivate":
        with pytest.raises(RuntimeError, match="stop unavailable"):
            runtime.deactivate("stuck")
    else:
        runtime.supervise_once()
    assert inst.last_transition == started
    assert registry.load()[0]["last_transition"] == started.isoformat()

    clock[0] += timedelta(seconds=10)
    runtime.supervise_once()
    assert signals == [(inst.pid, signal.SIGKILL)]


def test_repeated_deactivate_does_not_extend_stop_grace(stopping_runtime):
    runtime, inst, _, _, clock, signals = stopping_runtime
    runtime.deactivate("stuck")
    clock[0] += timedelta(seconds=20)
    runtime.deactivate("stuck")
    clock[0] += timedelta(seconds=10)
    runtime.supervise_once()
    assert signals == [(inst.pid, signal.SIGKILL)]


@pytest.mark.parametrize("elapsed", [20, 31])
def test_re_adopt_preserves_stop_deadline(tmp_path, monkeypatch, elapsed):
    record = _record()
    record["phase"] = "stopping"
    started = datetime.fromisoformat(record["last_transition"])
    clock = [started + timedelta(seconds=elapsed)]
    registry = EngineRegistry(tmp_path / "engines.json")
    registry.save([record])
    launcher = _ControlledLauncher()
    launcher.stop_exits = False
    signals = []
    monkeypatch.setattr(runtime_module, "pid_is_alive", lambda _: True)
    monkeypatch.setattr(runtime_module, "process_group_is_alive", lambda _: True)
    monkeypatch.setattr(runtime_module, "process_start_time", lambda _: "123456")
    monkeypatch.setattr(
        runtime_module.os, "killpg", lambda pid, sig: signals.append((pid, sig))
    )

    runtime = ModelRuntime(launcher, registry=registry, now=lambda: clock[0])
    assert runtime.list_models()[0].last_transition == started
    if elapsed < 30:
        assert signals == []
        assert launcher.stopped == ["qwen"]
        clock[0] = started + timedelta(seconds=30)
        runtime.supervise_once()
    assert signals == [(4321, signal.SIGKILL)]
    assert registry.load()[0]["phase"] == "stopping"


def test_re_adopt_keeps_stop_failure_retryable(tmp_path, monkeypatch):
    record = _record()
    record["phase"] = "stopping"
    started = datetime.fromisoformat(record["last_transition"])
    registry = EngineRegistry(tmp_path / "engines.json")
    registry.save([record])
    monkeypatch.setattr(runtime_module, "pid_is_alive", lambda _: True)
    monkeypatch.setattr(runtime_module, "process_start_time", lambda _: "123456")

    def fail_killpg(pid, sig):
        raise PermissionError("signal denied")

    monkeypatch.setattr(runtime_module.os, "killpg", fail_killpg)
    runtime = ModelRuntime(
        _ControlledLauncher(),
        registry=registry,
        now=lambda: started + timedelta(seconds=31),
    )
    inst = runtime.list_models()[0]
    assert "signal denied" in inst.last_error
    assert inst.last_transition == started
    assert registry.load()[0]["last_error"] == inst.last_error
    signals = []
    monkeypatch.setattr(
        runtime_module.os, "killpg", lambda pid, sig: signals.append((pid, sig))
    )
    runtime.supervise_once()
    assert (4321, signal.SIGKILL) in signals


def test_stop_waits_for_surviving_process_group(stopping_runtime, monkeypatch):
    runtime, inst, _, registry, clock, signals = stopping_runtime
    runtime.deactivate("stuck")
    inst.proc.alive = False
    monkeypatch.setattr(runtime_module, "process_group_is_alive", lambda _: True)
    # A missing leader must not prevent stopping its surviving worker group.
    monkeypatch.setattr(runtime_module, "process_start_time", lambda _: None)
    clock[0] += timedelta(seconds=30)
    runtime.supervise_once()
    assert signals == [(inst.pid, signal.SIGKILL)]
    assert registry.load()[0]["phase"] == "stopping"
    monkeypatch.setattr(runtime_module, "process_group_is_alive", lambda _: False)
    runtime.supervise_once()
    assert registry.load() == []


def test_stop_does_not_signal_a_reused_pid(stopping_runtime, monkeypatch):
    runtime, inst, _, registry, clock, signals = stopping_runtime
    runtime.deactivate("stuck")
    clock[0] += timedelta(seconds=30)
    monkeypatch.setattr(runtime_module, "process_start_time", lambda _: "different")
    runtime.supervise_once()
    assert signals == []
    assert inst.last_error is not None
    assert registry.load()[0]["phase"] == "stopping"


@pytest.mark.parametrize("error", [ProcessLookupError, PermissionError])
def test_force_stop_handles_signal_races_and_retries(
    stopping_runtime, monkeypatch, error
):
    runtime, inst, _, registry, clock, signals = stopping_runtime
    runtime.deactivate("stuck")
    started = inst.last_transition
    clock[0] += timedelta(seconds=30)

    def fail_killpg(pid, sig):
        assert (pid, sig) == (inst.pid, signal.SIGKILL)
        if error is ProcessLookupError:
            inst.proc.alive = False
        raise error("signal failed")

    monkeypatch.setattr(runtime_module.os, "killpg", fail_killpg)
    runtime.supervise_once()
    assert inst.last_transition == started
    assert registry.load()[0]["phase"] == "stopping"
    if error is PermissionError:
        assert "signal failed" in inst.last_error
        monkeypatch.setattr(
            runtime_module.os, "killpg", lambda pid, sig: signals.append((pid, sig))
        )
        runtime.supervise_once()
        assert signals == [(inst.pid, signal.SIGKILL)]
        inst.proc.alive = False
    runtime.supervise_once()
    assert registry.load() == []

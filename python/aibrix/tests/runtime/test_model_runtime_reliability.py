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

import signal
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

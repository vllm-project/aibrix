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

"""Tests for runtime model lifecycle, using mock actuators (no GPU)."""

import os
import signal
import sys
import threading
import time
from types import SimpleNamespace

import pytest

from aibrix.runtime.model_runtime import (
    MockEngineLauncher,
    ModelRuntime,
)


def make_agent():
    return ModelRuntime(MockEngineLauncher())


def test_gpu_memory_snapshots_serializes_nvml_lifecycle(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    state_lock = threading.Lock()
    active = 0
    max_active = 0
    init_calls = 0
    shutdown_calls = 0

    def nvml_init():
        nonlocal active, init_calls, max_active
        with state_lock:
            init_calls += 1
            active += 1
            max_active = max(max_active, active)
        time.sleep(0.01)

    def nvml_shutdown():
        nonlocal active, shutdown_calls
        with state_lock:
            shutdown_calls += 1
            active -= 1

    nvml = SimpleNamespace(
        nvmlInit=nvml_init,
        nvmlShutdown=nvml_shutdown,
        nvmlDeviceGetCount=lambda: 1,
        nvmlDeviceGetHandleByIndex=lambda index: index,
        nvmlDeviceGetMemoryInfo=lambda handle: SimpleNamespace(total=100, free=50),
        nvmlDeviceGetUUID=lambda handle: f"GPU-{handle}",
    )
    monkeypatch.setitem(sys.modules, "pynvml", nvml)

    start = threading.Barrier(4)
    snapshots = []

    def observe():
        start.wait()
        snapshots.append(runtime_module.gpu_memory_snapshots())

    threads = [threading.Thread(target=observe) for _ in range(4)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()

    assert (
        snapshots
        == [[{"id": "GPU-0", "hbm_total_bytes": 100, "hbm_free_bytes": 50}]] * 4
    )
    assert init_calls == 4
    assert shutdown_calls == 4
    assert max_active == 1


def test_gpu_memory_observation_reports_process_memory_by_gpu(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    nvml = SimpleNamespace(
        nvmlInit=lambda: None,
        nvmlShutdown=lambda: None,
        nvmlDeviceGetCount=lambda: 2,
        nvmlDeviceGetHandleByIndex=lambda index: index,
        nvmlDeviceGetMemoryInfo=lambda handle: SimpleNamespace(
            total=1000, free=700 - (100 * handle)
        ),
        nvmlDeviceGetUUID=lambda handle: f"GPU-{handle}",
        nvmlDeviceGetComputeRunningProcesses=lambda handle: (
            [SimpleNamespace(pid=101, usedGpuMemory=111)]
            if handle == 0
            else [
                SimpleNamespace(pid=101, usedGpuMemory=222),
                SimpleNamespace(pid=202, usedGpuMemory=333),
            ]
        ),
    )
    monkeypatch.setitem(sys.modules, "pynvml", nvml)

    accelerators, process_hbm = runtime_module.gpu_memory_observation()

    assert accelerators == [
        {"id": "GPU-0", "hbm_total_bytes": 1000, "hbm_free_bytes": 700},
        {"id": "GPU-1", "hbm_total_bytes": 1000, "hbm_free_bytes": 600},
    ]
    assert process_hbm == {
        101: {"GPU-0": 111, "GPU-1": 222},
        202: {"GPU-1": 333},
    }


class _RecordingKVController:
    def __init__(self):
        self.limits = []

    def set_limit(self, ipc_name, limit_bytes):
        self.limits.append((ipc_name, limit_bytes))


def test_set_kv_limit_is_idempotent_by_operation_id():
    launcher = MockEngineLauncher()
    kv_controller = _RecordingKVController()
    agent = ModelRuntime(launcher, kv_controller=kv_controller)
    inst = agent.activate(
        model_name="qwen3-0.6b",
        artifact_url="hf://Qwen/Qwen3-0.6B",
        ipc_name="kvc_qwen3-0.6b",
    )

    first = agent.set_kv_limit(inst.model_name, 4096, operation_id="limit-1")
    duplicate = agent.set_kv_limit(inst.model_name, 4096, operation_id="limit-1")

    assert first.applied is True
    assert duplicate.applied is False
    assert kv_controller.limits == [("kvc_qwen3-0-6b", 4096)]


def test_failed_kv_limit_operation_is_retriable():
    class _FlakyKVController:
        def __init__(self):
            self.calls = 0

        def set_limit(self, ipc_name, limit_bytes):
            self.calls += 1
            if self.calls == 1:
                raise RuntimeError("kvctl failed")

    kv_controller = _FlakyKVController()
    agent = ModelRuntime(MockEngineLauncher(), kv_controller=kv_controller)
    inst = agent.activate(model_name="m1", artifact_url="hf://Org/M1")

    with pytest.raises(RuntimeError, match="kvctl failed"):
        agent.set_kv_limit("m1", 4096, operation_id="limit-1")
    retried = agent.set_kv_limit("m1", 4096, operation_id="limit-1")

    assert retried.applied is True
    assert kv_controller.calls == 2
    assert inst.completed_operation_ids["kv-limit"] == ["limit-1"]


def test_sleep_is_idempotent_by_operation_id():
    launcher = MockEngineLauncher()
    agent = ModelRuntime(launcher)
    inst = agent.activate(model_name="m1", artifact_url="hf://Org/M1")

    first = agent.sleep(inst.model_name, level=1, operation_id="sleep-1")
    duplicate = agent.sleep(inst.model_name, level=1, operation_id="sleep-1")

    assert first.applied is True
    assert duplicate.applied is False
    assert inst.phase == "sleeping"
    assert launcher.slept == [("m1", 1)]


def test_failed_sleep_operation_keeps_model_active_and_retriable():
    class _FlakySleepLauncher(MockEngineLauncher):
        def sleep(self, inst, level):
            super().sleep(inst, level)
            if len(self.slept) == 1:
                raise RuntimeError("vllm sleep failed")

    launcher = _FlakySleepLauncher()
    agent = ModelRuntime(launcher)
    inst = agent.activate(model_name="m1", artifact_url="hf://Org/M1")

    with pytest.raises(RuntimeError, match="vllm sleep failed"):
        agent.sleep("m1", level=1, operation_id="sleep-1")
    retried = agent.sleep("m1", level=1, operation_id="sleep-1")

    assert inst.phase == "sleeping"
    assert retried.applied is True
    assert launcher.slept == [("m1", 1), ("m1", 1)]
    assert inst.completed_operation_ids["sleep"] == ["sleep-1"]


def test_wake_restores_active_phase_and_readiness():
    from aibrix.runtime.model_runtime import instance_ready

    launcher = MockEngineLauncher()
    agent = ModelRuntime(launcher)
    inst = agent.activate(model_name="m1", artifact_url="hf://Org/M1")
    agent.sleep(inst.model_name, level=1, operation_id="sleep-1")

    assert instance_ready(inst) is False
    first = agent.wake(inst.model_name, operation_id="wake-1")
    duplicate = agent.wake(inst.model_name, operation_id="wake-1")

    assert first.applied is True
    assert duplicate.applied is False
    assert inst.phase == "active"
    assert instance_ready(inst) is True
    assert launcher.woken == ["m1"]


def test_kvctl_controller_uses_checked_limit_command(monkeypatch):
    import subprocess

    from aibrix.runtime.model_runtime import KvctlController

    calls = []
    monkeypatch.setattr(
        subprocess,
        "run",
        lambda *args, **kwargs: calls.append((args, kwargs)),
    )

    KvctlController().set_limit("kvc_m1", 4096)

    assert calls == [
        ((["kvctl", "limit", "kvc_m1", "4096"],), {"check": True, "timeout": 10})
    ]


def test_vllm_lifecycle_controls_use_checked_localhost_requests(monkeypatch):
    import httpx

    from aibrix.runtime.model_runtime import ModelInstance, SubprocessEngineLauncher

    calls = []

    class _Response:
        def raise_for_status(self):
            calls[-1]["checked"] = True

    def fake_post(url, timeout):
        calls.append({"url": url, "timeout": timeout})
        return _Response()

    monkeypatch.setattr(httpx, "post", fake_post)
    inst = ModelInstance(model_name="m1", port=30123, ipc_name="kvc_m1")
    launcher = SubprocessEngineLauncher()

    launcher.sleep(inst, level=2)
    launcher.wake(inst)

    assert calls == [
        {
            "url": "http://127.0.0.1:30123/sleep?level=2",
            "timeout": 10.0,
            "checked": True,
        },
        {
            "url": "http://127.0.0.1:30123/wake_up",
            "timeout": 10.0,
            "checked": True,
        },
    ]


def test_subprocess_launcher_stops_group_after_api_server_exits(monkeypatch):
    from aibrix.runtime.model_runtime import ModelInstance, SubprocessEngineLauncher

    calls = []
    monkeypatch.setattr(
        os,
        "killpg",
        lambda process_group_id, signum: calls.append((process_group_id, signum)),
    )
    inst = ModelInstance(
        model_name="m1",
        port=30123,
        ipc_name="kvc_m1",
        pid=1234,
        proc=SimpleNamespace(poll=lambda: 1),
    )

    SubprocessEngineLauncher().stop(inst)

    assert calls == [(1234, signal.SIGTERM)]


def test_completed_operation_ids_are_bounded():
    kv_controller = _RecordingKVController()
    agent = ModelRuntime(MockEngineLauncher(), kv_controller=kv_controller)
    inst = agent.activate(model_name="m1", artifact_url="hf://Org/M1")

    for index in range(130):
        agent.set_kv_limit("m1", index, operation_id=f"limit-{index}")

    assert len(inst.completed_operation_ids["kv-limit"]) == 128
    assert inst.completed_operation_ids["kv-limit"][0] == "limit-2"
    assert len(kv_controller.limits) == 130


def test_activate_assigns_ipc_port():
    agent = make_agent()
    inst = agent.activate(model_name="m1", artifact_url="hf://x")
    assert inst.ipc_name == "kvc_m1"
    assert 20000 <= inst.port < 21000
    assert inst.phase == "active"
    assert inst.pid is not None
    assert agent._launcher.launched == ["m1"]


def test_activate_is_idempotent():
    agent = make_agent()
    first = agent.activate(model_name="m1", artifact_url="hf://x")
    second = agent.activate(model_name="m1", artifact_url="hf://x")
    assert first.port == second.port
    assert agent._launcher.launched == ["m1"]  # launched only once


def test_activate_distinct_ports_and_ipc_per_model():
    agent = make_agent()
    a = agent.activate(model_name="m1", artifact_url="hf://x")
    b = agent.activate(model_name="m2", artifact_url="hf://y")
    assert a.port != b.port
    assert a.ipc_name != b.ipc_name
    assert {m.model_name for m in agent.list_models()} == {"m1", "m2"}


def test_engine_config_args_are_structured():
    from aibrix.runtime.model_runtime import _engine_args

    assert _engine_args(
        {"args": {"--max-model-len": "2048", "--enforce-eager": ""}},
        None,
    ) == {"--max-model-len": "2048", "--enforce-eager": ""}


def test_vllm_parallelism_defaults_and_combines_tp_pp():
    from aibrix.runtime.model_runtime import vllm_parallelism

    assert vllm_parallelism(None, None) == 1
    assert (
        vllm_parallelism(
            {"args": {"--tensor-parallel-size": "2", "--pipeline-parallel-size": "2"}},
            None,
        )
        == 4
    )


@pytest.mark.parametrize(
    "args",
    [
        {"--tensor-parallel-size": "0"},
        {"--pipeline-parallel-size": "not-a-number"},
        {"--data-parallel-size": "2"},
    ],
)
def test_vllm_parallelism_rejects_unsupported_or_invalid_args(args):
    from aibrix.runtime.model_runtime import vllm_parallelism

    with pytest.raises(ValueError):
        vllm_parallelism({"args": args}, None)


def test_activate_rejects_vllm_parallelism_that_does_not_match_visible_gpus(
    monkeypatch,
):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setattr(
        runtime_module,
        "gpu_memory_snapshots",
        lambda: [{"id": "GPU-0", "hbm_total_bytes": 1000, "hbm_free_bytes": 900}],
    )

    with pytest.raises(ValueError, match="must equal 1 GPU"):
        make_agent().activate(
            model_name="tp2",
            artifact_url="hf://Org/M1",
            engine_config={"args": {"--tensor-parallel-size": "2"}},
        )


def test_activate_accepts_vllm_tp_pp_matching_visible_gpus(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setattr(
        runtime_module,
        "gpu_memory_snapshots",
        lambda: [
            {"id": f"GPU-{index}", "hbm_total_bytes": 1000, "hbm_free_bytes": 900}
            for index in range(4)
        ],
    )

    inst = make_agent().activate(
        model_name="tp2pp2",
        artifact_url="hf://Org/M1",
        engine_config={
            "args": {"--tensor-parallel-size": "2", "--pipeline-parallel-size": "2"}
        },
    )

    assert inst.model_name == "tp2pp2"


def test_legacy_additional_config_engine_arg_prefix_is_supported():
    from aibrix.runtime.model_runtime import _engine_args

    assert _engine_args(None, {"engine-arg:--gpu-memory-utilization": "0.45"}) == {
        "--gpu-memory-utilization": "0.45"
    }


def test_explicit_port_and_ipc_respected():
    agent = make_agent()
    inst = agent.activate(
        model_name="m1", artifact_url="hf://x", port=20555, ipc_name="custom"
    )
    assert inst.port == 20555
    assert inst.ipc_name == "custom"


def test_activate_sanitizes_ipc_name():
    # kvcached normalizes the IPC name (dots/slashes -> '-'); the agent must do
    # the same so kvctl targets the segment the engine actually creates.
    agent = make_agent()
    inst = agent.activate(model_name="qwen3-0.6b", artifact_url="hf://x")
    assert inst.ipc_name == "kvc_qwen3-0-6b"


def test_deactivate_stop_removes_model():
    agent = make_agent()
    agent.activate(model_name="m1", artifact_url="hf://x")
    agent.deactivate("m1", mode="stop")
    assert agent.list_models() == []
    assert agent._launcher.stopped == ["m1"]


def test_deactivate_unknown_model_is_noop():
    agent = make_agent()
    agent.deactivate("ghost", mode="stop")  # must not raise


def test_deactivate_non_stop_mode_is_treated_as_stop():
    agent = make_agent()
    agent.activate(model_name="m1", artifact_url="hf://x")
    agent.deactivate("m1", mode="warm")
    assert agent.list_models() == []
    assert agent._launcher.stopped == ["m1"]


# --------------------------------------------------------------------------- #
# HTTP endpoint smoke test via FastAPI TestClient (mock agent)
# --------------------------------------------------------------------------- #
def _make_test_client():
    # Force the singleton agent into mock mode before the app imports it.
    os.environ["AIBRIX_MODEL_RUNTIME_MOCK"] = "1"
    import aibrix.runtime.model_runtime as pa

    pa._AGENT = None  # reset any agent created by an earlier import

    from fastapi import FastAPI
    from fastapi.testclient import TestClient

    from aibrix.app import router

    app = FastAPI()
    app.include_router(router)
    return TestClient(app)


def test_endpoints_activate_list_deactivate():
    client = _make_test_client()

    resp = client.post(
        "/v1/runtime/models/activate",
        json={"model_name": "ep1", "artifact_url": "hf://x", "engine": "vllm"},
    )
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["status"] == "success"
    assert body["ipc_name"] == "kvc_ep1"
    assert body["port"] >= 20000

    listed = client.get("/v1/runtime/models").json()
    assert any(m["model_name"] == "ep1" for m in listed["models"])

    resp = client.post(
        "/v1/runtime/models/deactivate", json={"model_name": "ep1", "mode": "stop"}
    )
    assert resp.status_code == 200

    listed = client.get("/v1/runtime/models").json()
    assert all(m["model_name"] != "ep1" for m in listed["models"])


def test_activate_endpoint_rejects_mismatched_vllm_parallelism(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setattr(
        runtime_module,
        "gpu_memory_observation",
        lambda: (
            [
                {
                    "id": "GPU-0",
                    "hbm_total_bytes": 1000,
                    "hbm_free_bytes": 700,
                }
            ],
            {},
        ),
    )
    client = _make_test_client()

    response = client.post(
        "/v1/runtime/models/activate",
        json={
            "model_name": "tp2-on-one-gpu",
            "artifact_url": "hf://Org/Model",
            "engine": "vllm",
            "engine_config": {"args": {"--tensor-parallel-size": "2"}},
        },
    )

    assert response.status_code == 400, response.text
    assert response.json()["status"] == "error"
    assert "must equal 1 GPU" in response.json()["message"]


def test_control_endpoints_apply_kv_sleep_and_wake():
    client = _make_test_client()
    activated = client.post(
        "/v1/runtime/models/activate",
        json={"model_name": "ep-control", "artifact_url": "hf://Org/Model"},
    )
    assert activated.status_code == 200, activated.text

    limited = client.post(
        "/v1/runtime/models/kv-limit",
        json={
            "model_name": "ep-control",
            "limit_bytes": 4096,
            "operation_id": "limit-1",
        },
    )
    assert limited.status_code == 200, limited.text
    assert limited.json()["applied"] is True

    sleeping = client.post(
        "/v1/runtime/models/sleep",
        json={"model_name": "ep-control", "level": 1, "operation_id": "sleep-1"},
    )
    assert sleeping.status_code == 200, sleeping.text
    assert sleeping.json() == {
        "status": "success",
        "model_name": "ep-control",
        "operation_id": "sleep-1",
        "applied": True,
        "phase": "sleeping",
    }
    listed = client.get("/v1/runtime/models").json()
    assert listed["models"] == [
        {
            "model_name": "ep-control",
            "port": activated.json()["port"],
            "ipc_name": "kvc_ep-control",
            "phase": "sleeping",
            "ready": False,
            "kv_used_bytes": 0,
            "kv_total_bytes": 0,
        }
    ]

    woken = client.post(
        "/v1/runtime/models/wake",
        json={"model_name": "ep-control", "operation_id": "wake-1"},
    )
    assert woken.status_code == 200, woken.text
    assert woken.json()["phase"] == "active"
    assert woken.json()["applied"] is True


def test_control_endpoints_reject_unknown_unsupported_and_invalid_requests():
    client = _make_test_client()

    missing = client.post(
        "/v1/runtime/models/kv-limit",
        json={"model_name": "missing", "limit_bytes": 4096, "operation_id": "x"},
    )
    assert missing.status_code == 404

    activated = client.post(
        "/v1/runtime/models/activate",
        json={
            "model_name": "ep-sglang",
            "artifact_url": "hf://Org/Model",
            "engine": "sglang",
        },
    )
    assert activated.status_code == 200, activated.text
    unsupported = client.post(
        "/v1/runtime/models/sleep",
        json={"model_name": "ep-sglang", "level": 1, "operation_id": "sleep-1"},
    )
    assert unsupported.status_code == 409

    invalid = client.post(
        "/v1/runtime/models/wake",
        json={"model_name": "ep-sglang", "operation_id": ""},
    )
    assert invalid.status_code == 422


def test_snapshot_reports_runtime_state(monkeypatch, tmp_path):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setenv("AIBRIX_WEIGHT_CACHE_DIR", str(tmp_path))
    agent = make_agent()
    agent.activate(
        model_name="qwen",
        artifact_url="hf://Qwen/Qwen3-0.6B",
        claim_ref={"namespace": "default", "name": "qwen", "uid": "claim-uid"},
    )
    monkeypatch.setattr(
        runtime_module,
        "gpu_memory_observation",
        lambda: (
            [
                {
                    "id": "GPU-0",
                    "hbm_total_bytes": 1000,
                    "hbm_free_bytes": 700,
                }
            ],
            {},
        ),
    )
    monkeypatch.setattr(
        runtime_module,
        "read_kv_segment",
        lambda ipc_name: (100, 20, 5),
    )
    monkeypatch.setattr(
        runtime_module,
        "engine_request_activity",
        lambda inst: runtime_module.EngineRequestActivity(
            observed=True,
            requests_running=2,
            requests_waiting=1,
            request_success_total=7,
        ),
    )

    snapshot = agent.snapshot()

    assert snapshot["accelerators"] == [
        {"id": "GPU-0", "hbm_total_bytes": 1000, "hbm_free_bytes": 700}
    ]
    assert snapshot["cached_artifacts"] == ["hf://Qwen/Qwen3-0.6B"]
    observed = snapshot["models"][0]
    assert observed.pop("last_transition").tzinfo is not None
    assert observed == {
        "model_name": "qwen",
        "artifact_url": "hf://Qwen/Qwen3-0.6B",
        "claim_ref": {
            "namespace": "default",
            "name": "qwen",
            "uid": "claim-uid",
        },
        "port": 20000,
        "ipc_name": "kvc_qwen",
        "phase": "active",
        "alive": True,
        "ready": True,
        "restart_count": 0,
        "last_error": None,
        "kv_used_bytes": 25,
        "kv_capacity_bytes": 100,
        "hbm_peak_bytes": 0,
        "hbm_reservation_fraction": 0.0,
        "request_metrics_observed": True,
        "requests_running": 2,
        "requests_waiting": 1,
        "request_success_total": 7,
    }
    assert snapshot["observed_at"]


def test_snapshot_reports_hbm_peak_for_engine_process_tree(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    agent = make_agent()
    inst = agent.activate(model_name="qwen", artifact_url="hf://Qwen/Qwen3-0.6B")
    assert inst.pid is not None
    monkeypatch.setattr(
        runtime_module,
        "gpu_memory_observation",
        lambda: (
            [
                {"id": "GPU-0", "hbm_total_bytes": 1000, "hbm_free_bytes": 700},
                {"id": "GPU-1", "hbm_total_bytes": 1000, "hbm_free_bytes": 800},
            ],
            {
                inst.pid: {"GPU-0": 10},
                20001: {"GPU-0": 120, "GPU-1": 80},
                30001: {"GPU-0": 999},
            },
        ),
    )
    monkeypatch.setattr(
        runtime_module,
        "process_tree_pids",
        lambda pid: {pid, 20001},
        raising=False,
    )

    snapshot = agent.snapshot()

    assert snapshot["accelerators"][0]["id"] == "GPU-0"
    assert snapshot["models"][0]["hbm_peak_bytes"] == 130


def test_engine_request_activity_accepts_vllm_metric_name_variants(monkeypatch):
    import httpx

    import aibrix.runtime.model_runtime as runtime_module

    class Response:
        text = """# HELP vllm:num_requests_running Running requests.
vllm:num_requests_running{model_name=\"m1\"} 2
vllm_num_requests_waiting{model_name=\"m1\"} 3
vllm:request_success_total{model_name=\"m1\",finished_reason=\"stop\"} 5
vllm:request_success_total{model_name=\"m1\",finished_reason=\"length\"} 7
"""

        def raise_for_status(self):
            return None

    monkeypatch.setattr(httpx, "get", lambda url, timeout: Response())
    inst = runtime_module.ModelInstance(
        model_name="m1",
        port=20000,
        ipc_name="kvc_m1",
        proc=object(),
    )

    activity = runtime_module.engine_request_activity(inst)

    assert activity.observed is True
    assert activity.requests_running == 2
    assert activity.requests_waiting == 3
    assert activity.request_success_total == 12


def test_engine_request_activity_ignores_other_models_from_shared_metrics(monkeypatch):
    import httpx

    import aibrix.runtime.model_runtime as runtime_module

    class Response:
        text = """vllm:num_requests_running{model_name=\"m1\"} 2
vllm:num_requests_running{model_name=\"m2\"} 11
vllm_num_requests_waiting{model_name=\"m1\"} 3
vllm_num_requests_waiting{model_name=\"m2\"} 13
vllm:request_success_total{model_name=\"m1\",finished_reason=\"stop\"} 5
vllm:request_success_total{model_name=\"m2\",finished_reason=\"stop\"} 17
"""

        def raise_for_status(self):
            return None

    monkeypatch.setattr(httpx, "get", lambda url, timeout: Response())
    inst = runtime_module.ModelInstance(
        model_name="m1",
        port=20000,
        ipc_name="kvc_m1",
        proc=object(),
    )

    assert runtime_module.engine_request_activity(
        inst
    ) == runtime_module.EngineRequestActivity(
        observed=True,
        requests_running=2,
        requests_waiting=3,
        request_success_total=5,
    )


def test_engine_request_activity_does_not_treat_scrape_failure_as_idle(monkeypatch):
    import httpx

    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setattr(
        httpx,
        "get",
        lambda url, timeout: (_ for _ in ()).throw(httpx.ConnectError("down")),
    )
    inst = runtime_module.ModelInstance(
        model_name="m1",
        port=20000,
        ipc_name="kvc_m1",
        proc=object(),
    )

    assert (
        runtime_module.engine_request_activity(inst)
        == runtime_module.EngineRequestActivity()
    )


def test_snapshot_handles_hosts_without_gpu(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setattr(runtime_module, "gpu_memory_observation", lambda: ([], {}))
    snapshot = make_agent().snapshot()

    assert snapshot["accelerators"] == []
    assert snapshot["models"] == []


def test_snapshot_collects_gpu_observation_once(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    calls = 0

    def observe():
        nonlocal calls
        calls += 1
        return [], {}

    monkeypatch.setattr(runtime_module, "gpu_memory_observation", observe)

    make_agent().snapshot()

    assert calls == 1


def test_snapshot_endpoint_returns_typed_runtime_state(monkeypatch, tmp_path):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setenv("AIBRIX_WEIGHT_CACHE_DIR", str(tmp_path))
    monkeypatch.setattr(
        runtime_module,
        "gpu_memory_observation",
        lambda: (
            [
                {
                    "id": "GPU-0",
                    "hbm_total_bytes": 1000,
                    "hbm_free_bytes": 700,
                }
            ],
            {},
        ),
    )
    client = _make_test_client()
    activated = client.post(
        "/v1/runtime/models/activate",
        json={
            "model_name": "ep-snapshot",
            "artifact_url": "hf://Org/Model",
            "claim_ref": {
                "namespace": "default",
                "name": "model-claim",
                "uid": "claim-uid",
            },
        },
    )
    assert activated.status_code == 200, activated.text

    response = client.get("/v1/runtime/snapshot")

    assert response.status_code == 200, response.text
    body = response.json()
    assert body["accelerators"][0]["id"] == "GPU-0"
    assert body["models"][0]["claim_ref"]["uid"] == "claim-uid"
    assert body["models"][0]["artifact_url"] == "hf://Org/Model"
    assert body["models"][0]["alive"] is True
    assert body["models"][0]["restart_count"] == 0
    assert body["models"][0]["last_error"] is None
    assert body["models"][0]["last_transition"]


def test_activate_endpoint_carries_hbm_reservation_fraction():
    client = _make_test_client()
    response = client.post(
        "/v1/runtime/models/activate",
        json={
            "model_name": "reservation-model",
            "artifact_url": "hf://Org/Model",
            "hbm_reservation_fraction": 0.45,
        },
    )
    assert response.status_code == 200, response.text

    snapshot = client.get("/v1/runtime/snapshot")

    assert snapshot.status_code == 200, snapshot.text
    assert snapshot.json()["models"][0]["hbm_reservation_fraction"] == 0.45


# --------------------------------------------------------------------------- #
# Cache markers and /dev/shm KV accounting
# --------------------------------------------------------------------------- #
def test_activate_writes_cache_marker(tmp_path, monkeypatch):
    import json

    monkeypatch.setenv("AIBRIX_WEIGHT_CACHE_DIR", str(tmp_path))
    agent = make_agent()
    agent.activate(model_name="m1", artifact_url="huggingface://Org/M1")
    marker = tmp_path / ".aibrix" / "served" / "m1.json"
    assert marker.exists(), "activation must record the model in the node cache"
    data = json.loads(marker.read_text())
    assert data == {"model_name": "m1", "artifact_url": "huggingface://Org/M1"}


def test_write_cache_marker_rejects_path_traversal(tmp_path):
    from aibrix.runtime.model_runtime import write_cache_marker

    path = write_cache_marker("../../etc/evil", "hf://x", cache_dir=str(tmp_path))
    assert path is None, "path traversal in model name must be rejected"
    assert not (tmp_path.parent / "etc" / "evil.json").exists()


def test_activate_marker_failure_does_not_block(monkeypatch):
    # Point the cache at an unwritable location: activation must still succeed.
    monkeypatch.setenv("AIBRIX_WEIGHT_CACHE_DIR", "/proc/definitely-not-writable")
    agent = make_agent()
    inst = agent.activate(model_name="m1", artifact_url="hf://x")
    assert inst.phase == "active"


def test_read_kv_segment_parses_meminfo(tmp_path):
    import struct

    from aibrix.runtime.model_runtime import read_kv_segment

    # kvcached MemInfoStruct: 3 little-endian int64 (total, used, prealloc).
    (tmp_path / "kvc_m1").write_bytes(struct.pack("<3q", 100, 40, 10))
    assert read_kv_segment("kvc_m1", shm_dir=str(tmp_path)) == (100, 40, 10)


def test_read_kv_segment_absent_or_short(tmp_path):
    from aibrix.runtime.model_runtime import read_kv_segment

    assert read_kv_segment("kvc_missing", shm_dir=str(tmp_path)) is None
    (tmp_path / "kvc_short").write_bytes(b"\x00" * 8)
    assert read_kv_segment("kvc_short", shm_dir=str(tmp_path)) is None


class _DeadProc:
    def poll(self):
        return 1  # exited


def test_activate_does_not_bypass_supervisor_for_dead_engine():
    agent = make_agent()
    inst = agent.activate(model_name="m1", artifact_url="hf://x")
    inst.proc = _DeadProc()  # engine died underneath the agent
    again = agent.activate(model_name="m1", artifact_url="hf://x")
    assert agent._launcher.launched == ["m1"]
    assert again is inst

    agent.supervise_once()

    assert inst.phase == "restarting"
    assert inst.restart_count == 1


def test_list_models_keeps_dead_engines_for_supervisor_visibility():
    agent = make_agent()
    a = agent.activate(model_name="m1", artifact_url="hf://x")
    agent.activate(model_name="m2", artifact_url="hf://y")
    a.proc = _DeadProc()
    names = {m.model_name for m in agent.list_models()}
    assert names == {"m1", "m2"}

    agent.supervise_once()

    assert a.phase == "restarting"


# --------------------------------------------------------------------------- #
# Readiness gate: engine_ready / instance_ready. The controller holds a model's
# warm-pod routing annotation at the parked marker (port 0) until instance_ready
# is True, so a still-booting engine is never routed to.
# --------------------------------------------------------------------------- #
class _LiveProc:
    def poll(self):
        return None  # still running


def test_instance_ready_mock_instance_is_ready():
    # A mock/handle-less instance has no engine process to probe and is ready.
    agent = make_agent()
    inst = agent.activate(model_name="m1", artifact_url="hf://x")
    from aibrix.runtime.model_runtime import instance_ready

    assert inst.proc is None
    assert instance_ready(inst) is True


def test_instance_ready_dead_process_not_ready():
    agent = make_agent()
    inst = agent.activate(model_name="m1", artifact_url="hf://x")
    inst.proc = _DeadProc()
    from aibrix.runtime.model_runtime import instance_ready

    assert instance_ready(inst) is False


def test_instance_ready_live_process_probes_health(monkeypatch):
    agent = make_agent()
    inst = agent.activate(model_name="m1", artifact_url="hf://x")
    inst.proc = _LiveProc()
    inst.port = 28123
    import aibrix.runtime.model_runtime as pa

    probed = {}

    def fake_engine_ready(port):
        probed["port"] = port
        return True

    monkeypatch.setattr(pa, "engine_ready", fake_engine_ready)
    assert pa.instance_ready(inst) is True
    assert probed["port"] == 28123, "a live engine must be probed on its port"

    monkeypatch.setattr(pa, "engine_ready", lambda port: False)
    assert pa.instance_ready(inst) is False, "live but unhealthy engine is not ready"


def test_engine_ready_health_200(monkeypatch):
    import httpx

    from aibrix.runtime.model_runtime import engine_ready

    class _Resp:
        status_code = 200

    monkeypatch.setattr(httpx, "get", lambda url, timeout: _Resp())
    assert engine_ready(29000) is True


def test_engine_ready_non_200(monkeypatch):
    import httpx

    from aibrix.runtime.model_runtime import engine_ready

    class _Resp:
        status_code = 503

    monkeypatch.setattr(httpx, "get", lambda url, timeout: _Resp())
    assert engine_ready(29000) is False


def test_engine_ready_connection_refused(monkeypatch):
    import httpx

    from aibrix.runtime.model_runtime import engine_ready

    def boom(url, timeout):
        raise httpx.ConnectError("connection refused")

    monkeypatch.setattr(httpx, "get", boom)
    assert engine_ready(29000) is False, "still-booting engine reads as not ready"

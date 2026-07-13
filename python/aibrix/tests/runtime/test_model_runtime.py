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

from aibrix.runtime.model_runtime import (
    MockEngineLauncher,
    ModelRuntime,
)


def make_agent():
    return ModelRuntime(MockEngineLauncher())


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
        "gpu_memory_snapshots",
        lambda: [
            {
                "id": "GPU-0",
                "hbm_total_bytes": 1000,
                "hbm_free_bytes": 700,
            }
        ],
    )
    monkeypatch.setattr(
        runtime_module,
        "read_kv_segment",
        lambda ipc_name: (100, 20, 5),
    )

    snapshot = agent.snapshot()

    assert snapshot["accelerators"] == [
        {"id": "GPU-0", "hbm_total_bytes": 1000, "hbm_free_bytes": 700}
    ]
    assert snapshot["cached_artifacts"] == ["hf://Qwen/Qwen3-0.6B"]
    assert snapshot["models"] == [
        {
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
            "ready": True,
            "kv_used_bytes": 25,
            "kv_capacity_bytes": 100,
        }
    ]
    assert snapshot["observed_at"]


def test_snapshot_handles_hosts_without_gpu(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setattr(runtime_module, "gpu_memory_snapshots", lambda: [])
    snapshot = make_agent().snapshot()

    assert snapshot["accelerators"] == []
    assert snapshot["models"] == []


def test_snapshot_endpoint_returns_typed_runtime_state(monkeypatch, tmp_path):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setenv("AIBRIX_WEIGHT_CACHE_DIR", str(tmp_path))
    monkeypatch.setattr(
        runtime_module,
        "gpu_memory_snapshots",
        lambda: [
            {
                "id": "GPU-0",
                "hbm_total_bytes": 1000,
                "hbm_free_bytes": 700,
            }
        ],
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


def test_activate_relaunches_dead_engine():
    agent = make_agent()
    inst = agent.activate(model_name="m1", artifact_url="hf://x")
    inst.proc = _DeadProc()  # engine died underneath the agent
    again = agent.activate(model_name="m1", artifact_url="hf://x")
    assert agent._launcher.launched == ["m1", "m1"], "dead instance must be relaunched"
    assert again.phase == "active"
    assert again.proc is None  # fresh mock launch


def test_list_models_reaps_dead_engines():
    agent = make_agent()
    a = agent.activate(model_name="m1", artifact_url="hf://x")
    agent.activate(model_name="m2", artifact_url="hf://y")
    a.proc = _DeadProc()
    names = {m.model_name for m in agent.list_models()}
    assert names == {"m2"}, "dead engine must be reaped from the listing"


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

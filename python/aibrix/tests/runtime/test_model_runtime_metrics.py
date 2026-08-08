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

import pytest

import aibrix.runtime.model_runtime as pa
from aibrix.runtime.model_runtime_metrics import ModelRuntimeKVCollector


def _families(collector):
    return {mf.name: mf for mf in collector.collect()}


def _sample(mf, **labels):
    for s in mf.samples:
        if all(s.labels.get(key) == value for key, value in labels.items()):
            return s.value
    return None


def _fresh_mock_agent(monkeypatch):
    monkeypatch.setenv("AIBRIX_MODEL_RUNTIME_MOCK", "1")
    pa._AGENT = None  # drop any agent built by an earlier test/import
    return pa.get_model_runtime()


def test_models_resident_counts_instances(monkeypatch):
    agent = _fresh_mock_agent(monkeypatch)
    agent.activate(model_name="m1", artifact_url="hf://x")
    agent.activate(model_name="m2", artifact_url="hf://y")

    fams = _families(ModelRuntimeKVCollector())
    # GaugeMetricFamily names drop the colon-suffix unit; match by prefix.
    resident = fams["aibrix:modelclaim_models_resident"]
    assert _sample(resident) == 2.0


def test_operational_metric_families_are_exported(monkeypatch):
    _fresh_mock_agent(monkeypatch)

    assert {
        "aibrix:modelclaim_engine_state",
        "aibrix:modelclaim_engine_restart_attempts",
        "aibrix:modelclaim_engine_restart_budget_exhausted",
        "aibrix:modelclaim_engine_re_adoptions",
        "aibrix:modelclaim_kv_limit_operations",
        "aibrix:modelclaim_kv_limit_requested_bytes",
        "aibrix:modelclaim_kv_limit_applied_bytes",
        "aibrix:modelclaim_lifecycle_operations",
        "aibrix:modelclaim_lifecycle_operation_duration_seconds",
    } <= _families(ModelRuntimeKVCollector()).keys()


def test_restart_and_re_adoption_metric_values_are_exported(monkeypatch):
    agent = _fresh_mock_agent(monkeypatch)
    snapshot = agent.snapshot_metrics()
    snapshot.restart_attempts["m1"] = 2
    snapshot.restart_budget_exhausted["m1"] = 1
    snapshot.re_adoption_outcomes["success"] = 1
    monkeypatch.setattr(agent, "snapshot_metrics", lambda: snapshot)

    fams = _families(ModelRuntimeKVCollector())

    assert _sample(fams["aibrix:modelclaim_engine_restart_attempts"], model="m1") == 2
    assert (
        _sample(fams["aibrix:modelclaim_engine_restart_budget_exhausted"], model="m1")
        == 1
    )
    assert _sample(fams["aibrix:modelclaim_engine_re_adoptions"], result="success") == 1


def test_engine_state_exports_fixed_phase_set(monkeypatch):
    agent = _fresh_mock_agent(monkeypatch)
    phases = ("active", "sleeping", "restarting", "failed")
    for index, phase in enumerate(phases):
        inst = agent.activate(model_name=f"m{index}", artifact_url="hf://x")
        inst.phase = phase

    state = _families(ModelRuntimeKVCollector())["aibrix:modelclaim_engine_state"]

    assert {sample.labels["phase"] for sample in state.samples} == set(phases)
    for phase in phases:
        assert _sample(state, phase=phase) == 1.0


def test_kv_bytes_from_segment(monkeypatch):
    agent = _fresh_mock_agent(monkeypatch)
    agent.activate(model_name="m1", artifact_url="hf://x")

    # (total, used, prealloc) — used metric reports used+prealloc.
    monkeypatch.setattr(pa, "read_kv_segment", lambda ipc, **kw: (1000, 400, 100))

    fams = _families(ModelRuntimeKVCollector())
    assert _sample(fams["aibrix:modelclaim_kv_used_bytes"], model="m1") == 500.0
    assert _sample(fams["aibrix:modelclaim_kv_total_bytes"], model="m1") == 1000.0


def test_kv_bytes_zero_when_segment_absent(monkeypatch):
    agent = _fresh_mock_agent(monkeypatch)
    agent.activate(model_name="m1", artifact_url="hf://x")
    monkeypatch.setattr(pa, "read_kv_segment", lambda ipc, **kw: None)

    fams = _families(ModelRuntimeKVCollector())
    assert _sample(fams["aibrix:modelclaim_kv_used_bytes"], model="m1") == 0.0
    assert _sample(fams["aibrix:modelclaim_kv_total_bytes"], model="m1") == 0.0


def test_hbm_peak_bytes_from_engine_process_tree(monkeypatch):
    agent = _fresh_mock_agent(monkeypatch)
    inst = agent.activate(model_name="m1", artifact_url="hf://x")
    assert inst.pid is not None
    monkeypatch.setattr(
        pa,
        "gpu_memory_observation",
        lambda: (
            [{"id": "GPU-0", "hbm_total_bytes": 1000, "hbm_free_bytes": 500}],
            {inst.pid: {"GPU-0": 125}, 20001: {"GPU-0": 375}},
        ),
    )
    monkeypatch.setattr(pa, "process_tree_pids", lambda pid: {pid, 20001})

    fams = _families(ModelRuntimeKVCollector())

    assert _sample(fams["aibrix:modelclaim_hbm_peak_bytes"], model="m1") == 500.0


def test_kv_limit_metrics_track_requested_applied_and_skipped(monkeypatch):
    agent = _fresh_mock_agent(monkeypatch)
    agent.activate(model_name="m1", artifact_url="hf://x")
    agent.set_kv_limit("m1", 4096, operation_id="limit-1")
    agent.set_kv_limit("m1", 8192, operation_id="limit-1")

    fams = _families(ModelRuntimeKVCollector())

    assert (
        _sample(fams["aibrix:modelclaim_kv_limit_requested_bytes"], model="m1")
        == 8192.0
    )
    assert (
        _sample(fams["aibrix:modelclaim_kv_limit_applied_bytes"], model="m1") == 4096.0
    )
    operations = fams["aibrix:modelclaim_kv_limit_operations"]
    assert _sample(operations, model="m1", result="applied") == 1
    assert _sample(operations, model="m1", result="skipped") == 1


def test_failed_kv_limit_metric_is_exported(monkeypatch):
    class _FailingKVController:
        def set_limit(self, ipc_name, limit_bytes):
            raise RuntimeError("kvctl failed")

    agent = pa.ModelRuntime(
        pa.MockEngineLauncher(), kv_controller=_FailingKVController()
    )
    monkeypatch.setattr(pa, "_AGENT", agent)
    agent.activate(model_name="m1", artifact_url="hf://x")

    with pytest.raises(RuntimeError, match="kvctl failed"):
        agent.set_kv_limit("m1", 4096, operation_id="limit-1")

    fams = _families(ModelRuntimeKVCollector())

    assert (
        _sample(
            fams["aibrix:modelclaim_kv_limit_operations"],
            model="m1",
            result="failed",
        )
        == 1
    )
    assert (
        _sample(fams["aibrix:modelclaim_kv_limit_requested_bytes"], model="m1") == 4096
    )
    assert _sample(fams["aibrix:modelclaim_kv_limit_applied_bytes"], model="m1") is None


def test_lifecycle_metrics_track_count_result_and_duration(monkeypatch):
    agent = _fresh_mock_agent(monkeypatch)
    agent.activate(model_name="m1", artifact_url="hf://x")
    agent.sleep("m1", level=1, operation_id="sleep-1")
    agent.sleep("m1", level=1, operation_id="sleep-1")
    agent.wake("m1", operation_id="wake-1")

    fams = _families(ModelRuntimeKVCollector())
    operations = fams["aibrix:modelclaim_lifecycle_operations"]
    duration = fams["aibrix:modelclaim_lifecycle_operation_duration_seconds"]

    assert _sample(operations, model="m1", action="sleep", result="applied") == 1
    assert _sample(operations, model="m1", action="sleep", result="skipped") == 1
    assert _sample(operations, model="m1", action="wake", result="applied") == 1
    assert _sample(duration, model="m1", action="sleep", result="applied") is not None
    assert {
        sample.labels["result"]
        for sample in operations.samples
        if sample.labels["model"] == "m1"
    } <= {"applied", "skipped", "failed"}


def test_kv_limit_gauges_disappear_after_deactivation(monkeypatch):
    agent = _fresh_mock_agent(monkeypatch)
    agent.activate(model_name="m1", artifact_url="hf://x")
    agent.set_kv_limit("m1", 4096, operation_id="limit-1")
    agent.deactivate("m1")

    fams = _families(ModelRuntimeKVCollector())

    assert (
        _sample(fams["aibrix:modelclaim_kv_limit_requested_bytes"], model="m1") is None
    )
    assert _sample(fams["aibrix:modelclaim_kv_limit_applied_bytes"], model="m1") is None


def test_collect_is_side_effect_free(monkeypatch):
    # A scrape must not reap or otherwise mutate agent state.
    agent = _fresh_mock_agent(monkeypatch)
    agent.activate(model_name="m1", artifact_url="hf://x")
    before = {m.model_name for m in agent.snapshot_models()}
    list(ModelRuntimeKVCollector().collect())
    after = {m.model_name for m in agent.snapshot_models()}
    assert before == after == {"m1"}


def test_collect_never_raises_without_agent(monkeypatch):
    # If the agent singleton blows up, a scrape still yields the resident gauge.
    monkeypatch.setattr(
        pa, "get_model_runtime", lambda: (_ for _ in ()).throw(RuntimeError("boom"))
    )
    fams = _families(ModelRuntimeKVCollector())
    assert _sample(fams["aibrix:modelclaim_models_resident"]) == 0.0

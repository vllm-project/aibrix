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

import aibrix.runtime.model_runtime as pa
from aibrix.runtime.model_runtime_metrics import ModelRuntimeKVCollector


def _families(collector):
    return {mf.name: mf for mf in collector.collect()}


def _sample(mf, model=None):
    for s in mf.samples:
        if model is None or s.labels.get("model") == model:
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

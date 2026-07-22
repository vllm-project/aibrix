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

"""ModelClaim operational and density metrics for the runtime sidecar.

A scrape-time Prometheus collector: how many models are resident on this pod,
each model's kvcached KV usage and observed peak HBM footprint, plus engine
state, restart/re-adoption, lifecycle, and KV-limit operation outcomes. This is
the agent (per-pod) plane of ModelClaim observability, complementing the
controller (lifecycle) and gateway (wake) planes.

Mounted on the agent's existing /metrics (the same REGISTRY HTTPCollector uses).
collect() is read-only — it takes a lock-protected snapshot and never reaps or
otherwise mutates agent state — so a scrape has no side effects.
"""

import logging

from prometheus_client.core import (
    CounterMetricFamily,
    GaugeMetricFamily,
    SummaryMetricFamily,
)
from prometheus_client.registry import Collector

logger = logging.getLogger(__name__)


_ENGINE_PHASES = ("active", "sleeping", "restarting", "failed")
_RE_ADOPTION_RESULTS = (
    "success",
    "restart_scheduled",
    "terminal",
    "stopping",
    "removed",
    "invalid",
    "duplicate",
)


class ModelRuntimeKVCollector(Collector):
    """Yields ModelClaim runtime metrics from one lock-consistent snapshot."""

    def collect(self):
        # Imported lazily so importing this module never drags in the agent
        # singleton (keeps the standalone/demo import chain light).
        from aibrix.runtime.model_runtime import (
            engine_hbm_peak_bytes,
            get_model_runtime,
            gpu_memory_observation,
            read_kv_segment,
        )

        try:
            runtime_metrics = get_model_runtime().snapshot_metrics()
            models = runtime_metrics.resident_models
        except Exception as exc:  # a scrape must never fail on agent state
            logger.warning("model runtime metrics collect failed: %s", exc)
            runtime_metrics = None
            models = []

        resident = GaugeMetricFamily(
            "aibrix:modelclaim_models_resident",
            "Number of models resident (engine processes) on this pod.",
        )
        resident.add_metric([], float(len(models)))
        yield resident

        used = GaugeMetricFamily(
            "aibrix:modelclaim_kv_used_bytes",
            "kvcached KV bytes in use by a resident model (0 when its /dev/shm "
            "segment is absent: mock engine, or engine still starting).",
            labels=["model"],
        )
        total = GaugeMetricFamily(
            "aibrix:modelclaim_kv_total_bytes",
            "kvcached KV pool total bytes visible to a resident model.",
            labels=["model"],
        )
        hbm_peak = GaugeMetricFamily(
            "aibrix:modelclaim_hbm_peak_bytes",
            "Largest GPU-memory footprint attributable to a resident engine on any "
            "visible accelerator (0 when NVML process observation is unavailable).",
            labels=["model"],
        )
        _, process_hbm = gpu_memory_observation() if models else ([], {})
        for m in models:
            seg = read_kv_segment(m.ipc_name)
            total_bytes, used_bytes, prealloc_bytes = seg if seg else (0, 0, 0)
            used.add_metric([m.model_name], float(used_bytes + prealloc_bytes))
            total.add_metric([m.model_name], float(total_bytes))
            hbm_peak.add_metric(
                [m.model_name], float(engine_hbm_peak_bytes(m, process_hbm))
            )
        yield used
        yield total
        yield hbm_peak

        engine_state = GaugeMetricFamily(
            "aibrix:modelclaim_engine_state",
            "Number of runtime engines in each operator-relevant phase.",
            labels=["phase"],
        )
        phase_counts = {phase: 0 for phase in _ENGINE_PHASES}
        if runtime_metrics is not None:
            for model in runtime_metrics.models:
                if model.phase in phase_counts:
                    phase_counts[model.phase] += 1
        for phase, count in phase_counts.items():
            engine_state.add_metric([phase], float(count))
        yield engine_state

        restart_attempts = CounterMetricFamily(
            "aibrix:modelclaim_engine_restart_attempts_total",
            "Engine relaunch attempts made by the local runtime supervisor.",
            labels=["model"],
        )
        restart_exhausted = CounterMetricFamily(
            "aibrix:modelclaim_engine_restart_budget_exhausted_total",
            "Engines that reached terminal failure after exhausting restart attempts.",
            labels=["model"],
        )
        re_adoptions = CounterMetricFamily(
            "aibrix:modelclaim_engine_re_adoptions_total",
            "Runtime-agent re-adoption outcomes, from a fixed result set.",
            labels=["result"],
        )
        if runtime_metrics is not None:
            for model, count in sorted(runtime_metrics.restart_attempts.items()):
                restart_attempts.add_metric([model], count)
            for model, count in sorted(
                runtime_metrics.restart_budget_exhausted.items()
            ):
                restart_exhausted.add_metric([model], count)
            for result in _RE_ADOPTION_RESULTS:
                re_adoptions.add_metric(
                    [result], runtime_metrics.re_adoption_outcomes.get(result, 0)
                )
        yield restart_attempts
        yield restart_exhausted
        yield re_adoptions

        kv_operations = CounterMetricFamily(
            "aibrix:modelclaim_kv_limit_operations_total",
            "KV-limit operation outcomes (applied, skipped, or failed).",
            labels=["model", "result"],
        )
        kv_requested = GaugeMetricFamily(
            "aibrix:modelclaim_kv_limit_requested_bytes",
            "Most recently requested kvcached limit for a resident model.",
            labels=["model"],
        )
        kv_applied = GaugeMetricFamily(
            "aibrix:modelclaim_kv_limit_applied_bytes",
            "Most recently applied kvcached limit for a resident model.",
            labels=["model"],
        )
        if runtime_metrics is not None:
            for (model, result), count in sorted(
                runtime_metrics.kv_limit_outcomes.items()
            ):
                kv_operations.add_metric([model, result], count)
            for model, value in sorted(
                runtime_metrics.kv_limit_requested_bytes.items()
            ):
                kv_requested.add_metric([model], value)
            for model, value in sorted(runtime_metrics.kv_limit_applied_bytes.items()):
                kv_applied.add_metric([model], value)
        yield kv_operations
        yield kv_requested
        yield kv_applied

        lifecycle_operations = CounterMetricFamily(
            "aibrix:modelclaim_lifecycle_operations_total",
            "Sleep and wake operation outcomes.",
            labels=["model", "action", "result"],
        )
        lifecycle_duration = SummaryMetricFamily(
            "aibrix:modelclaim_lifecycle_operation_duration_seconds",
            "Cumulative duration of sleep and wake operations.",
            labels=["model", "action", "result"],
        )
        if runtime_metrics is not None:
            for key, count in sorted(runtime_metrics.lifecycle_outcomes.items()):
                lifecycle_operations.add_metric(list(key), count)
            for key, count in sorted(runtime_metrics.lifecycle_duration_count.items()):
                lifecycle_duration.add_metric(
                    list(key),
                    count,
                    runtime_metrics.lifecycle_duration_sum[key],
                )
        yield lifecycle_operations
        yield lifecycle_duration

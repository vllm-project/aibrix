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

"""ModelClaim density metrics for the runtime sidecar.

A scrape-time Prometheus collector: how many models are resident on this pod,
each model's kvcached KV usage, and its observed peak HBM footprint. This is
the agent (per-pod) plane of ModelClaim observability, complementing the
controller (lifecycle) and gateway (wake) planes.

Mounted on the agent's existing /metrics (the same REGISTRY HTTPCollector uses).
collect() is read-only — it takes a lock-protected snapshot and never reaps or
otherwise mutates agent state — so a scrape has no side effects.
"""

import logging

from prometheus_client.core import GaugeMetricFamily
from prometheus_client.registry import Collector

logger = logging.getLogger(__name__)


class ModelRuntimeKVCollector(Collector):
    """Yields ModelClaim KV-density gauges for the models resident on this pod."""

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
            models = get_model_runtime().snapshot_models()
        except Exception as exc:  # a scrape must never fail on agent state
            logger.warning("KV metrics collect failed: %s", exc)
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

# Copyright 2026 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

from __future__ import annotations

import re
import threading
from typing import Any

from prometheus_client import CollectorRegistry, Counter, Gauge, Histogram

from aibrix.logger import init_logger
from aibrix.metadata.core.metrics.sink import Sink, Tag, normalize_tags

logger = init_logger(__name__)

_PROM_BUCKETS_MS = (
    1.0,
    5.0,
    10.0,
    25.0,
    50.0,
    100.0,
    250.0,
    500.0,
    1000.0,
    2500.0,
    5000.0,
    10000.0,
    30000.0,
    60000.0,
)


def sanitize_metric_name(name: str) -> str:
    sanitized = re.sub(r"[^a-zA-Z0-9_:]", "_", name.replace(".", "_"))
    if not sanitized:
        return "unnamed_metric"
    if sanitized[0].isdigit():
        return f"_{sanitized}"
    return sanitized


class PrometheusSink(Sink):
    def __init__(self, registry: CollectorRegistry):
        self.registry = registry
        self._lock = threading.Lock()
        self._metrics: dict[tuple[str, str], tuple[Any, tuple[str, ...]]] = {}

    def _metric(
        self, kind: str, name: str, documentation: str, tags: tuple[Tag, ...]
    ) -> Any | None:
        metric_name = sanitize_metric_name(name)
        label_names = tuple(tag.name for tag in tags)
        cache_key = (kind, metric_name)

        with self._lock:
            existing = self._metrics.get(cache_key)
            if existing is not None:
                metric, existing_labels = existing
                if existing_labels != label_names:
                    logger.warning(
                        "Prometheus metric label mismatch; dropping sample",
                        metric=metric_name,
                        existing_labels=existing_labels,
                        new_labels=label_names,
                    )
                    return None
                return metric

            if kind == "counter":
                metric = Counter(
                    metric_name,
                    documentation,
                    labelnames=label_names,
                    registry=self.registry,
                )
            elif kind == "gauge":
                metric = Gauge(
                    metric_name,
                    documentation,
                    labelnames=label_names,
                    registry=self.registry,
                )
            elif kind == "histogram":
                metric = Histogram(
                    metric_name,
                    documentation,
                    labelnames=label_names,
                    registry=self.registry,
                    buckets=_PROM_BUCKETS_MS,
                )
            else:
                raise ValueError(f"unsupported metric kind: {kind}")

            self._metrics[cache_key] = (metric, label_names)
            return metric

    @staticmethod
    def _label_values(tags: tuple[Tag, ...]) -> tuple[str, ...]:
        return tuple(tag.value for tag in tags)

    def counter(self, name: str, value: float, *tags: Tag) -> None:
        normalized = normalize_tags(tags)
        metric = self._metric("counter", name, f"Counter for {name}", normalized)
        if metric is None:
            return
        cast_metric = (
            metric.labels(*self._label_values(normalized)) if normalized else metric
        )
        cast_metric.inc(value)

    def gauge(self, name: str, value: float, *tags: Tag) -> None:
        normalized = normalize_tags(tags)
        metric = self._metric("gauge", name, f"Gauge for {name}", normalized)
        if metric is None:
            return
        cast_metric = (
            metric.labels(*self._label_values(normalized)) if normalized else metric
        )
        cast_metric.set(value)

    def timer(self, name: str, value: float, *tags: Tag) -> None:
        normalized = normalize_tags(tags)
        metric = self._metric("histogram", name, f"Histogram for {name}", normalized)
        if metric is None:
            return
        cast_metric = (
            metric.labels(*self._label_values(normalized)) if normalized else metric
        )
        cast_metric.observe(value)

    def store(self, name: str, value: float, *tags: Tag) -> None:
        self.gauge(name, value, *tags)

    def rate(self, name: str, value: float, *tags: Tag) -> None:
        self.counter(name, value, *tags)

    def close(self) -> None:
        return None

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

import gc
import math
import resource
import sys
import threading
from abc import ABC, abstractmethod
from dataclasses import dataclass
from time import perf_counter
from typing import Iterable

from aibrix.logger import init_logger
from aibrix.metadata.core.metrics.names import (
    METRIC_METADATA_GC_GEN0_COUNT,
    METRIC_METADATA_GC_GEN1_COUNT,
    METRIC_METADATA_GC_GEN2_COUNT,
    METRIC_METADATA_METRICS_EMIT_AVG_US,
    METRIC_METADATA_METRICS_EMIT_COUNT,
    METRIC_METADATA_METRICS_EMIT_P90_US,
    METRIC_METADATA_METRICS_EMIT_PER_SECOND,
    METRIC_METADATA_METRICS_EMIT_TELEMETRY_SUMMARY,
    METRIC_METADATA_METRICS_EMIT_TOTAL_MS,
    METRIC_METADATA_PROCESS_RSS_MB,
)

logger = init_logger(__name__)


@dataclass(frozen=True)
class Tag:
    name: str
    value: str


def T(name: str, value: str) -> Tag:
    return Tag(name=name, value=value)


LOG_CONTROL_TAGS = frozenset(
    {
        "flush",
        "log_context",
        "log_context_bool",
        "log_context_null_if_empty",
        "log_default_map",
        "log_default_null",
        "log_field",
        "log_group",
        "log_inner",
        "log_index",
        "log_key",
        "log_list",
        "log_map",
        "log_nested_map",
        "log_outer",
    }
)


def normalize_tags(tags: Iterable[Tag]) -> tuple[Tag, ...]:
    deduped: dict[str, str] = {}
    for tag in tags:
        deduped[tag.name] = tag.value
    return tuple(Tag(name=name, value=deduped[name]) for name in sorted(deduped))


def normalize_metric_tags(tags: Iterable[Tag]) -> tuple[Tag, ...]:
    """Normalize tags for time-series sinks, excluding LogSink directives."""
    return tuple(
        tag for tag in normalize_tags(tags) if tag.name not in LOG_CONTROL_TAGS
    )


@dataclass(frozen=True)
class MetricEmissionSnapshot:
    emit_count: int
    emit_per_second: float
    emit_total_ms: float
    emit_avg_us: float
    emit_p90_us: float | None
    process_rss_mb: float
    gc_counts: tuple[int, int, int]
    active_metric_backend: str


def _rss_mb() -> float:
    max_rss = float(resource.getrusage(resource.RUSAGE_SELF).ru_maxrss)
    if sys.platform == "darwin":
        return max_rss / (1024.0 * 1024.0)
    return max_rss / 1024.0


class SystemTelemetry:
    def __init__(
        self,
        *,
        sink: Sink,
        interval_seconds: float,
        active_metric_backend: str = "unknown",
        logger_name: str | None = None,
    ) -> None:
        self._sink = sink
        self._interval_seconds = max(interval_seconds, 0.1)
        self._active_metric_backend = active_metric_backend
        self._logger = logger if logger_name is None else init_logger(logger_name)
        self._lock = threading.Lock()
        self._stop_event = threading.Event()
        self._thread = threading.Thread(
            target=self._run,
            name="aibrix-system-telemetry",
            daemon=True,
        )
        self._window_start = perf_counter()
        self._emit_count = 0
        self._emit_total_us = 0.0
        self._samples_us: list[float] = []

    def start(self) -> None:
        self._thread.start()

    def stop(self) -> None:
        self._stop_event.set()
        if self._thread.is_alive():
            self._thread.join(timeout=max(self._interval_seconds, 1.0))
        snapshot = self.snapshot_and_reset()
        if snapshot.emit_count:
            self._handle_snapshot(snapshot)

    def record(self, metric_type: str, duration_us: float) -> None:
        _ = metric_type
        with self._lock:
            self._emit_count += 1
            self._emit_total_us += duration_us
            self._samples_us.append(duration_us)

    def snapshot_and_reset(self) -> MetricEmissionSnapshot:
        with self._lock:
            now = perf_counter()
            elapsed_seconds = max(now - self._window_start, 1e-9)
            emit_count = self._emit_count
            emit_total_us = self._emit_total_us
            samples_us = tuple(self._samples_us)
            self._window_start = now
            self._emit_count = 0
            self._emit_total_us = 0.0
            self._samples_us.clear()

        emit_per_second = emit_count / elapsed_seconds
        emit_avg_us = emit_total_us / emit_count if emit_count else 0.0
        emit_p90_us = None
        if samples_us:
            sorted_samples = sorted(samples_us)
            index = max(math.ceil(len(sorted_samples) * 0.90) - 1, 0)
            emit_p90_us = sorted_samples[index]

        return MetricEmissionSnapshot(
            emit_count=emit_count,
            emit_per_second=emit_per_second,
            emit_total_ms=emit_total_us / 1000.0,
            emit_avg_us=emit_avg_us,
            emit_p90_us=emit_p90_us,
            process_rss_mb=_rss_mb(),
            gc_counts=gc.get_count(),
            active_metric_backend=self._active_metric_backend,
        )

    def _handle_snapshot(self, snapshot: MetricEmissionSnapshot) -> None:
        try:
            self._publish_snapshot(snapshot)
        except Exception:
            self._logger.exception("Failed to publish metric emission snapshot")

    def _publish_snapshot(self, snapshot: MetricEmissionSnapshot) -> None:
        event = "Metric emission overhead snapshot"
        base_tags = (
            Tag("event", event),
            Tag("active_metric_backend", snapshot.active_metric_backend),
            Tag("log_context", "active_metric_backend"),
        )
        numeric_fields = {
            "metric_emit_count": (
                METRIC_METADATA_METRICS_EMIT_COUNT,
                float(snapshot.emit_count),
            ),
            "metric_emit_per_second": (
                METRIC_METADATA_METRICS_EMIT_PER_SECOND,
                snapshot.emit_per_second,
            ),
            "metric_emit_total_ms": (
                METRIC_METADATA_METRICS_EMIT_TOTAL_MS,
                snapshot.emit_total_ms,
            ),
            "metric_emit_avg_us": (
                METRIC_METADATA_METRICS_EMIT_AVG_US,
                snapshot.emit_avg_us,
            ),
            "process_rss_mb": (
                METRIC_METADATA_PROCESS_RSS_MB,
                snapshot.process_rss_mb,
            ),
            "gc_gen0_count": (
                METRIC_METADATA_GC_GEN0_COUNT,
                float(snapshot.gc_counts[0]),
            ),
            "gc_gen1_count": (
                METRIC_METADATA_GC_GEN1_COUNT,
                float(snapshot.gc_counts[1]),
            ),
            "gc_gen2_count": (
                METRIC_METADATA_GC_GEN2_COUNT,
                float(snapshot.gc_counts[2]),
            ),
        }
        if snapshot.emit_p90_us is not None:
            numeric_fields["metric_emit_p90_us"] = (
                METRIC_METADATA_METRICS_EMIT_P90_US,
                snapshot.emit_p90_us,
            )

        for metric_field, (metric_name, value) in numeric_fields.items():
            self._sink.gauge(
                metric_name,
                value,
                *base_tags,
                Tag("log_field", metric_field),
            )
        self._sink.gauge(
            METRIC_METADATA_METRICS_EMIT_TELEMETRY_SUMMARY,
            1.0,
            *base_tags,
            Tag("log_default_null", "metric_emit_p90_us"),
            Tag("flush", "true"),
        )

    def _run(self) -> None:
        while not self._stop_event.wait(self._interval_seconds):
            snapshot = self.snapshot_and_reset()
            if snapshot.emit_count:
                self._handle_snapshot(snapshot)


class Sink(ABC):
    @abstractmethod
    def counter(self, name: str, value: float, *tags: Tag) -> None:
        raise NotImplementedError

    @abstractmethod
    def gauge(self, name: str, value: float, *tags: Tag) -> None:
        raise NotImplementedError

    @abstractmethod
    def timer(self, name: str, value: float, *tags: Tag) -> None:
        raise NotImplementedError

    @abstractmethod
    def store(self, name: str, value: float, *tags: Tag) -> None:
        raise NotImplementedError

    @abstractmethod
    def rate(self, name: str, value: float, *tags: Tag) -> None:
        raise NotImplementedError

    @abstractmethod
    def close(self) -> None:
        raise NotImplementedError


class NoopSink(Sink):
    def counter(self, name: str, value: float, *tags: Tag) -> None:
        return None

    def gauge(self, name: str, value: float, *tags: Tag) -> None:
        return None

    def timer(self, name: str, value: float, *tags: Tag) -> None:
        return None

    def store(self, name: str, value: float, *tags: Tag) -> None:
        return None

    def rate(self, name: str, value: float, *tags: Tag) -> None:
        return None

    def close(self) -> None:
        return None


class FanoutSink(Sink):
    def __init__(self, sinks: Iterable[Sink]):
        self._sinks = tuple(sinks)

    def counter(self, name: str, value: float, *tags: Tag) -> None:
        self._emit("counter", name, value, *tags)

    def gauge(self, name: str, value: float, *tags: Tag) -> None:
        self._emit("gauge", name, value, *tags)

    def timer(self, name: str, value: float, *tags: Tag) -> None:
        self._emit("timer", name, value, *tags)

    def store(self, name: str, value: float, *tags: Tag) -> None:
        self._emit("store", name, value, *tags)

    def rate(self, name: str, value: float, *tags: Tag) -> None:
        self._emit("rate", name, value, *tags)

    def close(self) -> None:
        for sink in self._sinks:
            try:
                sink.close()
            except Exception:
                logger.exception(
                    "Metrics sink failed during fanout close",
                    sink=type(sink).__name__,
                )  # type: ignore[call-arg]

    def _emit(self, metric_type: str, name: str, value: float, *tags: Tag) -> None:
        for sink in self._sinks:
            try:
                getattr(sink, metric_type)(name, value, *tags)
            except Exception:
                logger.exception(
                    "Metrics sink failed during fanout emission",
                    sink=type(sink).__name__,
                    metric_type=metric_type,
                    metric=name,
                )  # type: ignore[call-arg]


def describe_sink(sink: Sink) -> str:
    if isinstance(sink, FanoutSink):
        return ",".join(describe_sink(child) for child in sink._sinks)
    return type(sink).__name__


class ObservedSink(Sink):
    def __init__(self, sink: Sink, observer: SystemTelemetry):
        self._sink = sink
        self._observer = observer
        self._observer.start()

    def counter(self, name: str, value: float, *tags: Tag) -> None:
        self._observe("counter", self._sink.counter, name, value, *tags)

    def gauge(self, name: str, value: float, *tags: Tag) -> None:
        self._observe("gauge", self._sink.gauge, name, value, *tags)

    def timer(self, name: str, value: float, *tags: Tag) -> None:
        self._observe("timer", self._sink.timer, name, value, *tags)

    def store(self, name: str, value: float, *tags: Tag) -> None:
        self._observe("store", self._sink.store, name, value, *tags)

    def rate(self, name: str, value: float, *tags: Tag) -> None:
        self._observe("rate", self._sink.rate, name, value, *tags)

    def close(self) -> None:
        self._sink.close()
        self._observer.stop()

    def _observe(self, metric_type: str, operation, *args: object) -> None:
        start = perf_counter()
        operation(*args)
        self._observer.record(metric_type, (perf_counter() - start) * 1_000_000.0)


class GlobalEmitter(Sink):
    def __init__(self) -> None:
        self._sink: Sink = NoopSink()

    def set_sink(self, sink: Sink) -> None:
        self._sink = sink

    def reset(self) -> None:
        self._sink = NoopSink()

    def counter(self, name: str, value: float, *tags: Tag) -> None:
        self._sink.counter(name, value, *tags)

    def gauge(self, name: str, value: float, *tags: Tag) -> None:
        self._sink.gauge(name, value, *tags)

    def timer(self, name: str, value: float, *tags: Tag) -> None:
        self._sink.timer(name, value, *tags)

    def store(self, name: str, value: float, *tags: Tag) -> None:
        self._sink.store(name, value, *tags)

    def rate(self, name: str, value: float, *tags: Tag) -> None:
        self._sink.rate(name, value, *tags)

    def close(self) -> None:
        self._sink.close()


Emitter = GlobalEmitter()


def duration_ms(sink: Sink, name: str, start_time: float, *tags: Tag) -> None:
    sink.timer(f"{name}.ms", (perf_counter() - start_time) * 1000.0, *tags)

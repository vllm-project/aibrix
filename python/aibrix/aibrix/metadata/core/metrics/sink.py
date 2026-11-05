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

logger = init_logger(__name__)


@dataclass(frozen=True)
class Tag:
    name: str
    value: str


def T(name: str, value: str) -> Tag:
    return Tag(name=name, value=value)


def normalize_tags(tags: Iterable[Tag]) -> tuple[Tag, ...]:
    deduped: dict[str, str] = {}
    for tag in tags:
        deduped[tag.name] = tag.value
    return tuple(Tag(name=name, value=deduped[name]) for name in sorted(deduped))


@dataclass(frozen=True)
class MetricEmissionSnapshot:
    emit_count: int
    emit_total_ms: float
    emit_avg_us: float
    emit_p95_us: float | None
    process_rss_mb: float
    gc_counts: tuple[int, int, int]
    active_metric_backend: str


def _rss_mb() -> float:
    max_rss = float(resource.getrusage(resource.RUSAGE_SELF).ru_maxrss)
    if sys.platform == "darwin":
        return max_rss / (1024.0 * 1024.0)
    return max_rss / 1024.0


class MetricEmissionObserver:
    def __init__(
        self,
        *,
        interval_seconds: float,
        active_metric_backend: str = "unknown",
        logger_name: str | None = None,
    ) -> None:
        self._interval_seconds = max(interval_seconds, 0.1)
        self._active_metric_backend = active_metric_backend
        self._logger = logger if logger_name is None else init_logger(logger_name)
        self._lock = threading.Lock()
        self._stop_event = threading.Event()
        self._thread = threading.Thread(
            target=self._run,
            name="aibrix-metric-emission-observer",
            daemon=True,
        )
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
            self.log_snapshot(snapshot)

    def record(self, metric_type: str, duration_us: float) -> None:
        _ = metric_type
        with self._lock:
            self._emit_count += 1
            self._emit_total_us += duration_us
            self._samples_us.append(duration_us)

    def snapshot_and_reset(self) -> MetricEmissionSnapshot:
        with self._lock:
            emit_count = self._emit_count
            emit_total_us = self._emit_total_us
            samples_us = tuple(self._samples_us)
            self._emit_count = 0
            self._emit_total_us = 0.0
            self._samples_us.clear()

        emit_avg_us = emit_total_us / emit_count if emit_count else 0.0
        emit_p95_us = None
        if samples_us:
            sorted_samples = sorted(samples_us)
            index = max(math.ceil(len(sorted_samples) * 0.95) - 1, 0)
            emit_p95_us = sorted_samples[index]

        return MetricEmissionSnapshot(
            emit_count=emit_count,
            emit_total_ms=emit_total_us / 1000.0,
            emit_avg_us=emit_avg_us,
            emit_p95_us=emit_p95_us,
            process_rss_mb=_rss_mb(),
            gc_counts=gc.get_count(),
            active_metric_backend=self._active_metric_backend,
        )

    def log_snapshot(self, snapshot: MetricEmissionSnapshot) -> None:
        self._logger.info(
            "Metric emission overhead snapshot",
            metric_emit_count=snapshot.emit_count,
            metric_emit_total_ms=snapshot.emit_total_ms,
            metric_emit_avg_us=snapshot.emit_avg_us,
            metric_emit_p95_us=snapshot.emit_p95_us,
            process_rss_mb=snapshot.process_rss_mb,
            gc_counts=snapshot.gc_counts,
            active_metric_backend=snapshot.active_metric_backend,
        )

    def _run(self) -> None:
        while not self._stop_event.wait(self._interval_seconds):
            snapshot = self.snapshot_and_reset()
            if snapshot.emit_count:
                self.log_snapshot(snapshot)


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
        for sink in self._sinks:
            sink.counter(name, value, *tags)

    def gauge(self, name: str, value: float, *tags: Tag) -> None:
        for sink in self._sinks:
            sink.gauge(name, value, *tags)

    def timer(self, name: str, value: float, *tags: Tag) -> None:
        for sink in self._sinks:
            sink.timer(name, value, *tags)

    def store(self, name: str, value: float, *tags: Tag) -> None:
        for sink in self._sinks:
            sink.store(name, value, *tags)

    def rate(self, name: str, value: float, *tags: Tag) -> None:
        for sink in self._sinks:
            sink.rate(name, value, *tags)

    def close(self) -> None:
        for sink in self._sinks:
            sink.close()


def describe_sink(sink: Sink) -> str:
    if isinstance(sink, FanoutSink):
        return ",".join(describe_sink(child) for child in sink._sinks)
    return type(sink).__name__


class ObservedSink(Sink):
    def __init__(self, sink: Sink, observer: MetricEmissionObserver):
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

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
"""Small JSON CPU-cost profiler for synchronous parse/decode hotspots."""

from __future__ import annotations

import time
from collections import Counter
from dataclasses import dataclass, field
from threading import Lock
from typing import Callable, Optional, ParamSpec, TypeVar

from aibrix import envs
from aibrix.logger import init_logger
from aibrix.metadata.core.metrics import Emitter, T

logger = init_logger(__name__)

P = ParamSpec("P")
R = TypeVar("R")


def _telemetry_enabled() -> bool:
    return envs.JSON_PROFILER_TELEMETRY_ENABLED


def _telemetry_interval_seconds() -> float:
    return envs.JSON_PROFILER_TELEMETRY_INTERVAL_SECONDS


def _percentile(samples: list[float], percentile: float) -> Optional[float]:
    if not samples:
        return None
    ordered = sorted(samples)
    index = min(len(ordered) - 1, max(0, int(len(ordered) * percentile + 0.999999) - 1))
    return ordered[index]


@dataclass(slots=True)
class JsonProfilerOperationSnapshot:
    operation: str
    started: int
    completed: int
    failed: int
    exception_types: dict[str, int]


@dataclass(slots=True)
class JsonProfilerSnapshot:
    started: int
    completed: int
    failed: int
    window_started: int
    window_completed: int
    window_failed: int
    p50_latency_seconds: Optional[float]
    p95_latency_seconds: Optional[float]
    exception_types: dict[str, int]
    operations: tuple[JsonProfilerOperationSnapshot, ...]


@dataclass(slots=True)
class _JsonProfilerOperationStats:
    started: int = 0
    completed: int = 0
    failed: int = 0
    exception_types: Counter[str] = field(default_factory=Counter)


@dataclass
class _JsonProfilerStats:
    lock: Lock = field(default_factory=Lock)
    started: int = 0
    completed: int = 0
    failed: int = 0
    window_started: int = 0
    window_completed: int = 0
    window_failed: int = 0
    _window_latencies_seconds: list[float] = field(default_factory=list)
    _window_exception_types: Counter[str] = field(default_factory=Counter)
    _operations: dict[str, _JsonProfilerOperationStats] = field(default_factory=dict)

    def record_start(self, operation: str) -> None:
        with self.lock:
            self.started += 1
            self.window_started += 1
            operation_stats = self._operations.setdefault(
                operation,
                _JsonProfilerOperationStats(),
            )
            operation_stats.started += 1

    def record_completion(
        self,
        *,
        operation: str,
        failed: bool,
        latency_seconds: float,
        exception_type: Optional[str] = None,
    ) -> None:
        with self.lock:
            self.completed += 1
            self.window_completed += 1
            if failed:
                self.failed += 1
                self.window_failed += 1
            self._window_latencies_seconds.append(max(latency_seconds, 0.0))
            if failed and exception_type is not None:
                self._window_exception_types[exception_type] += 1
            operation_stats = self._operations.setdefault(
                operation,
                _JsonProfilerOperationStats(),
            )
            operation_stats.completed += 1
            if failed:
                operation_stats.failed += 1
                if exception_type is not None:
                    operation_stats.exception_types[exception_type] += 1

    def snapshot(self, *, reset_window: bool) -> JsonProfilerSnapshot:
        with self.lock:
            snapshot = JsonProfilerSnapshot(
                started=self.started,
                completed=self.completed,
                failed=self.failed,
                window_started=self.window_started,
                window_completed=self.window_completed,
                window_failed=self.window_failed,
                p50_latency_seconds=_percentile(self._window_latencies_seconds, 0.50),
                p95_latency_seconds=_percentile(self._window_latencies_seconds, 0.95),
                exception_types=dict(sorted(self._window_exception_types.items())),
                operations=tuple(self._operation_snapshots()),
            )
            if reset_window:
                self.window_started = 0
                self.window_completed = 0
                self.window_failed = 0
                self._window_latencies_seconds = []
                self._window_exception_types = Counter()
                self._operations = {}
            return snapshot

    def _operation_snapshots(self) -> list[JsonProfilerOperationSnapshot]:
        snapshots: list[JsonProfilerOperationSnapshot] = []
        for operation, stats in sorted(
            self._operations.items(),
            key=lambda item: (item[1].failed, item[1].completed, item[0]),
            reverse=True,
        ):
            snapshots.append(
                JsonProfilerOperationSnapshot(
                    operation=operation,
                    started=stats.started,
                    completed=stats.completed,
                    failed=stats.failed,
                    exception_types=dict(sorted(stats.exception_types.items())),
                )
            )
        return snapshots


@dataclass
class _JsonProfilerPurposeState:
    stats: _JsonProfilerStats = field(default_factory=_JsonProfilerStats)
    last_telemetry_emitted_at: float = field(default_factory=time.perf_counter)
    emit_lock: Lock = field(default_factory=Lock)


_PURPOSE_STATES: dict[str, _JsonProfilerPurposeState] = {}


class JsonProfiler:
    """Records aggregated latency for synchronous JSON parse/decode operations."""

    def __init__(
        self,
        *,
        purpose: str,
        telemetry_enabled: Optional[bool] = None,
        telemetry_interval_seconds: Optional[float] = None,
    ) -> None:
        self.purpose = purpose
        self._telemetry_enabled = (
            telemetry_enabled if telemetry_enabled is not None else _telemetry_enabled()
        )
        self._telemetry_interval_seconds = (
            max(telemetry_interval_seconds, 0.0)
            if telemetry_interval_seconds is not None
            else _telemetry_interval_seconds()
        )
        self._purpose_state = _PURPOSE_STATES.setdefault(
            purpose,
            _JsonProfilerPurposeState(),
        )
        self._stats = self._purpose_state.stats

    def profile(
        self,
        operation: str,
        func: Callable[P, R],
        *args: P.args,
        **kwargs: P.kwargs,
    ) -> R:
        if not self._telemetry_enabled:
            return func(*args, **kwargs)

        start = time.perf_counter()
        self._stats.record_start(operation)
        failed = False
        exception_type: Optional[str] = None
        try:
            return func(*args, **kwargs)
        except Exception as exc:
            failed = True
            exception_type = type(exc).__name__
            raise
        finally:
            self._stats.record_completion(
                operation=operation,
                failed=failed,
                latency_seconds=time.perf_counter() - start,
                exception_type=exception_type,
            )
            self._emit_periodic_telemetry()

    def snapshot(self, *, reset_window: bool) -> JsonProfilerSnapshot:
        return self._stats.snapshot(reset_window=reset_window)

    def emit_telemetry(self, *, final: bool, elapsed: float) -> None:
        if not self._telemetry_enabled:
            return
        snapshot = self.snapshot(reset_window=True)
        elapsed = max(elapsed, 1e-9)
        event = "JSON profiler telemetry summary"
        base_tags = (
            T("event", event),
            T("purpose", self.purpose),
            T("final", str(final).lower()),
            T("log_context", "purpose,final"),
            T("log_context_bool", "final"),
        )
        numeric_fields = {
            "started_qps": round(snapshot.window_started / elapsed, 3),
            "completed_qps": round(snapshot.window_completed / elapsed, 3),
            "failed_qps": round(snapshot.window_failed / elapsed, 3),
            "started": snapshot.started,
            "completed": snapshot.completed,
            "failed": snapshot.failed,
            "window_started": snapshot.window_started,
            "window_completed": snapshot.window_completed,
            "window_failed": snapshot.window_failed,
        }
        if snapshot.p50_latency_seconds is not None:
            numeric_fields["p50_latency_seconds"] = round(
                snapshot.p50_latency_seconds,
                6,
            )
        if snapshot.p95_latency_seconds is not None:
            numeric_fields["p95_latency_seconds"] = round(
                snapshot.p95_latency_seconds,
                6,
            )
        for metric_field, value in numeric_fields.items():
            Emitter.gauge(
                f"metadata_json_telemetry_{metric_field}",
                float(value),
                *base_tags,
                T("log_field", metric_field),
            )
        for exception_type, exception_count in snapshot.exception_types.items():
            Emitter.gauge(
                "metadata_json_telemetry_exception_count",
                float(exception_count),
                *base_tags,
                T("log_map", "exception_types"),
                T("log_key", exception_type),
                T("exception_type", exception_type),
            )
        Emitter.gauge(
            "metadata_json_telemetry_summary",
            1.0,
            *base_tags,
            T("log_default_null", "p50_latency_seconds,p95_latency_seconds"),
            T("log_default_map", "exception_types"),
            T("flush", "true"),
        )
        for operation_snapshot in snapshot.operations:
            logger.debug(
                "JSON profiler telemetry by operation",
                purpose=self.purpose,
                final=final,
                operation=operation_snapshot.operation,
                started=operation_snapshot.started,
                completed=operation_snapshot.completed,
                failed=operation_snapshot.failed,
                exception_types=operation_snapshot.exception_types,
            )  # type: ignore[call-arg]

    def _emit_periodic_telemetry(self) -> None:
        if not self._telemetry_enabled or self._telemetry_interval_seconds <= 0:
            return
        now = time.perf_counter()
        with self._purpose_state.emit_lock:
            elapsed = now - self._purpose_state.last_telemetry_emitted_at
            if elapsed < self._telemetry_interval_seconds:
                return
            self._purpose_state.last_telemetry_emitted_at = now
        self.emit_telemetry(final=False, elapsed=elapsed)

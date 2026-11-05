import json
from unittest.mock import patch

import pytest

from aibrix.metadata.core.json_profiler import JsonProfiler
from aibrix.metadata.core.metrics.sink import T


def test_json_profiler_records_success_and_failure_by_shared_purpose():
    first = JsonProfiler(
        purpose="test-json-shared",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )
    second = JsonProfiler(
        purpose="test-json-shared",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )

    assert first.profile("json.loads", json.loads, '{"ok": true}') == {"ok": True}
    with pytest.raises(json.JSONDecodeError):
        second.profile("json.loads", json.loads, "{")

    assert first._stats is second._stats
    snapshot = first.snapshot(reset_window=False)
    assert snapshot.started == 2
    assert snapshot.completed == 2
    assert snapshot.failed == 1
    assert snapshot.operations[0].operation == "json.loads"
    assert snapshot.operations[0].started == 2
    assert snapshot.operations[0].failed == 1


def test_json_profiler_emits_summary_gauge_with_purpose():
    profiler = JsonProfiler(
        purpose="test-json-log",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )

    profiler.profile("json.loads", json.loads, '{"ok": true}')

    gauges = []
    with patch(
        "aibrix.metadata.core.json_profiler.Emitter.gauge",
        lambda name, value, *tags: gauges.append((name, value, tags)),
    ):
        profiler.emit_telemetry(final=False, elapsed=1.0)

    summary_gauge = next(
        tags for name, _, tags in gauges if name == "metadata_json_telemetry_summary"
    )
    assert T("purpose", "test-json-log") in summary_gauge
    assert T("flush", "true") in summary_gauge
    metric_values = {name: value for name, value, _ in gauges}
    assert metric_values["metadata_json_telemetry_started_qps"] == 1.0
    assert metric_values["metadata_json_telemetry_completed_qps"] == 1.0
    assert metric_values["metadata_json_telemetry_failed_qps"] == 0.0
    assert "metadata_json_telemetry_p50_latency_seconds" in metric_values


def test_json_profiler_emits_telemetry_numeric_metrics(monkeypatch):
    gauges = []

    monkeypatch.setattr(
        "aibrix.metadata.core.json_profiler.Emitter.gauge",
        lambda name, value, *tags: gauges.append((name, value, tags)),
    )
    profiler = JsonProfiler(
        purpose="batch_json",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )
    profiler.profile("decode_response", lambda: {"ok": True})

    profiler.emit_telemetry(final=False, elapsed=1.0)

    emitted_names = {name for name, _, _ in gauges}
    assert "metadata_json_telemetry_completed" in emitted_names
    assert "metadata_json_telemetry_completed_qps" in emitted_names
    assert "metadata_json_telemetry_summary" in emitted_names
    completed_tags = next(
        tags for name, _, tags in gauges if name == "metadata_json_telemetry_completed"
    )
    assert T("log_field", "completed") in completed_tags
    summary_gauges = [
        (value, tags)
        for name, value, tags in gauges
        if name == "metadata_json_telemetry_summary"
    ]
    assert summary_gauges
    assert summary_gauges[0][0] == 1.0
    assert T("flush", "true") in summary_gauges[0][1]
    assert T("event", "JSON profiler telemetry summary") in summary_gauges[0][1]


def test_json_profiler_shares_periodic_emit_clock_by_purpose():
    first = JsonProfiler(
        purpose="test-json-periodic-shared",
        telemetry_enabled=True,
        telemetry_interval_seconds=1.0,
    )
    second = JsonProfiler(
        purpose="test-json-periodic-shared",
        telemetry_enabled=True,
        telemetry_interval_seconds=1.0,
    )
    first._purpose_state.last_telemetry_emitted_at = 0.0

    gauges = []
    with (
        patch(
            "aibrix.metadata.core.json_profiler.time.perf_counter",
            side_effect=[2.0, 2.1, 2.1, 2.2, 2.2, 2.2],
        ),
        patch(
            "aibrix.metadata.core.json_profiler.Emitter.gauge",
            lambda name, value, *tags: gauges.append((name, value, tags)),
        ),
    ):
        first.profile("json.loads", json.loads, '{"first": true}')
        second.profile("json.loads", json.loads, '{"second": true}')

    summary_gauges = [
        tags for name, _, tags in gauges if name == "metadata_json_telemetry_summary"
    ]
    assert len(summary_gauges) == 1
    assert T("purpose", "test-json-periodic-shared") in summary_gauges[0]
    metric_values = {name: value for name, value, _ in gauges}
    assert metric_values["metadata_json_telemetry_window_completed"] == 1.0
    assert second.snapshot(reset_window=False).window_completed == 1

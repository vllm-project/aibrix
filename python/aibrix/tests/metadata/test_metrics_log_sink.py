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

import json
import logging

from aibrix.metadata.core.httpx_client import HTTPXClientWrapper
from aibrix.metadata.core.json_profiler import JsonProfiler
from aibrix.metadata.core.metrics import (
    FanoutSink,
    LogSink,
    MetricsConfig,
    metrics_names,
    setup_metrics,
    shutdown_metrics,
)
from aibrix.metadata.core.metrics.sink import T


def _payloads_for_metric(caplog, metric_name):
    payloads = []
    for record in caplog.records:
        payload = json.loads(record.message)
        if payload.get("metric_name") == metric_name:
            payloads.append(payload)
    return payloads


def test_log_sink_logs_included_metric(caplog):
    sink = LogSink(include_patterns=("batch_finalize_*",), exclude_patterns=())

    with caplog.at_level(logging.INFO):
        sink.timer("batch_finalize_stage_duration_ms", 12.5, T("stage", "total"))

    assert len(caplog.records) == 1
    payload = json.loads(caplog.records[0].message)
    assert payload["event"] == "Metric sink sample"
    assert payload["metric_type"] == "timer"
    assert payload["metric_name"] == "batch_finalize_stage_duration_ms"
    assert payload["metric_value"] == 12.5
    assert payload["metric_tags"] == {"stage": "total"}


def test_log_sink_aggregates_httpx_numeric_metrics(caplog):
    sink = LogSink(include_patterns=("metadata_http_client_telemetry_*",))
    base_tags = (
        T("event", "HTTPX client telemetry summary"),
        T("client_id", "client-1"),
        T("purpose", "batch_runtime_http"),
        T("final", "false"),
        T("is_closed", "false"),
        T("log_context", "client_id,purpose,final,is_closed"),
        T("log_context_bool", "final,is_closed"),
    )

    with caplog.at_level(logging.INFO):
        sink.gauge(
            "metadata_http_client_telemetry_started_qps",
            1.5,
            *base_tags,
            T("log_field", "started_qps"),
        )
        sink.gauge(
            "metadata_http_client_telemetry_exception_count",
            2,
            *base_tags,
            T("log_map", "exception_types"),
            T("log_key", "TimeoutError"),
        )
        sink.gauge(
            "metadata_http_client_telemetry_latency_bucket_count",
            3,
            *base_tags,
            T("log_list", "latency_histogram"),
            T("log_index", "1"),
        )
        sink.gauge(
            "metadata_http_client_telemetry_summary",
            1.0,
            *base_tags,
            T("flush", "true"),
        )

    payload = json.loads(caplog.records[-1].message)
    assert payload["event"] == "HTTPX client telemetry summary"
    assert payload["metric_name"] == "metadata_http_client_telemetry_summary"
    assert payload["client_id"] == "client-1"
    assert payload["purpose"] == "batch_runtime_http"
    assert payload["final"] is False
    assert payload["is_closed"] is False
    assert payload["started_qps"] == 1.5
    assert payload["exception_types"] == {"TimeoutError": 2}
    assert payload["latency_histogram"] == [0, 3]


def test_log_sink_converts_empty_context_tag_to_null(caplog):
    sink = LogSink(include_patterns=("metadata_http_client_telemetry_*",))

    with caplog.at_level(logging.INFO):
        sink.gauge(
            "metadata_http_client_telemetry_summary",
            1.0,
            T("event", "HTTPX client telemetry summary"),
            T("client_id", "client-1"),
            T("purpose", ""),
            T("log_context", "client_id,purpose"),
            T("log_context_null_if_empty", "purpose"),
            T("flush", "true"),
        )

    payload = json.loads(caplog.records[-1].message)
    assert payload["purpose"] is None


def test_log_sink_applies_flush_defaults(caplog):
    sink = LogSink(include_patterns=("metadata_json_telemetry_*",))

    with caplog.at_level(logging.INFO):
        sink.gauge(
            "metadata_json_telemetry_summary",
            1.0,
            T("event", "JSON profiler telemetry summary"),
            T("purpose", "json"),
            T("log_context", "purpose"),
            T("log_default_null", "p50_latency_seconds,p95_latency_seconds"),
            T("log_default_map", "exception_types"),
            T("flush", "true"),
        )

    payload = json.loads(caplog.records[-1].message)
    assert payload["p50_latency_seconds"] is None
    assert payload["p95_latency_seconds"] is None
    assert payload["exception_types"] == {}


def test_log_sink_aggregates_batch_finalize_numeric_metrics_once(caplog):
    sink = LogSink(include_patterns=("batch_finalize_*",))
    base_tags = (
        T("event", "Batch finalize telemetry summary"),
        T("job_id", "job-1"),
        T("log_context", "job_id"),
        T("log_group", "job-1"),
    )

    with caplog.at_level(logging.INFO):
        sink.timer(
            "batch_finalize_stage_duration_ms",
            12.5,
            *base_tags,
            T("stage", "list_metastore_keys"),
            T("log_map", "stage_duration_ms"),
            T("log_key", "list_metastore_keys"),
        )
        sink.gauge(
            "batch_finalize_stage_items",
            10.0,
            *base_tags,
            T("stage", "list_metastore_keys"),
            T("item", "keys"),
            T("log_nested_map", "stage_items"),
            T("log_outer", "list_metastore_keys"),
            T("log_inner", "keys"),
        )
        sink.gauge(
            "batch_finalize_stage_field",
            2.0,
            *base_tags,
            T("stage", "assemble_parts"),
            T("field", "output_parts"),
            T("log_nested_map", "stage_fields"),
            T("log_outer", "assemble_parts"),
            T("log_inner", "output_parts"),
        )
        sink.timer(
            "batch_finalize_job_total_duration_ms",
            20.0,
            *base_tags,
            T("failed_finalize", "false"),
            T("log_field", "job_total_duration_ms"),
        )
        sink.gauge(
            "batch_finalize_job_summary",
            1.0,
            *base_tags,
            T("failed_finalize", "false"),
            T("log_context", "job_id,failed_finalize"),
            T("flush", "true"),
        )

    assert len(caplog.records) == 1
    payload = json.loads(caplog.records[0].message)
    assert payload["event"] == "Batch finalize telemetry summary"
    assert payload["metric_name"] == "batch_finalize_job_summary"
    assert payload["job_id"] == "job-1"
    assert payload["failed_finalize"] == "false"
    assert payload["stage_duration_ms"] == {
        "list_metastore_keys": 12.5,
    }
    assert payload["stage_items"] == {
        "list_metastore_keys": {"keys": 10.0},
    }
    assert payload["stage_fields"] == {
        "assemble_parts": {"output_parts": 2.0},
    }
    assert payload["job_total_duration_ms"] == 20.0


def test_log_sink_aggregates_batch_job_latency_metrics_once(caplog):
    sink = LogSink(include_patterns=("metadata.batch.api.job.execution*ms",))
    base_tags = (
        T("event", "Batch job latency telemetry"),
        T("job_id", "job-1"),
        T("console_job_id", "console-1"),
        T("endpoint", "/v1/chat/completions"),
        T("completion_window", "86400"),
        T("log_context", "job_id,console_job_id,endpoint,completion_window"),
        T("log_group", "job-1"),
    )

    with caplog.at_level(logging.INFO):
        sink.timer(
            f"{metrics_names.METRIC_METADATA_BATCH_API_JOB_EXECUTION_PHASE_TIME}.ms",
            12.5,
            *base_tags,
            T("phase", "scheduling"),
            T("log_map", "phase_duration_ms"),
            T("log_key", "scheduling"),
        )
        sink.timer(
            f"{metrics_names.METRIC_METADATA_BATCH_API_JOB_EXECUTION_PHASE_TIME}.ms",
            30.0,
            *base_tags,
            T("phase", "task_execution"),
            T("log_map", "phase_duration_ms"),
            T("log_key", "task_execution"),
        )
        sink.timer(
            f"{metrics_names.METRIC_METADATA_BATCH_API_JOB_EXECUTION_PHASE_TIME}.ms",
            7.5,
            *base_tags,
            T("phase", "finalization"),
            T("log_map", "phase_duration_ms"),
            T("log_key", "finalization"),
        )
        sink.timer(
            f"{metrics_names.METRIC_METADATA_BATCH_API_JOB_EXECUTION_TIME}.ms",
            50.0,
            *base_tags,
            T("finalize_type", "completed"),
            T("error_code", "none"),
            T(
                "log_context",
                "job_id,console_job_id,endpoint,completion_window,finalize_type,error_code",
            ),
            T("log_field", "execution_duration_ms"),
            T("log_default_map", "phase_duration_ms"),
            T("flush", "true"),
        )

    assert len(caplog.records) == 1
    payload = json.loads(caplog.records[0].message)
    assert payload["event"] == "Batch job latency telemetry"
    assert payload["metric_name"] == (
        f"{metrics_names.METRIC_METADATA_BATCH_API_JOB_EXECUTION_TIME}.ms"
    )
    assert payload["job_id"] == "job-1"
    assert payload["console_job_id"] == "console-1"
    assert payload["finalize_type"] == "completed"
    assert payload["error_code"] == "none"
    assert payload["phase_duration_ms"] == {
        "scheduling": 12.5,
        "task_execution": 30.0,
        "finalization": 7.5,
    }
    assert payload["execution_duration_ms"] == 50.0


def test_setup_metrics_routes_httpx_summary_through_log_sink(caplog):
    setup_metrics(
        MetricsConfig(
            log_enabled=True,
            log_include_patterns=["metadata_http_client_telemetry_*"],
        )
    )
    try:
        wrapper = HTTPXClientWrapper(
            client_id="configured-httpx-client",
            purpose="metrics_log_sink_httpx",
            telemetry_enabled=True,
            telemetry_interval_seconds=0,
        )
        wrapper._telemetry_stats.record_start(call_site="setup.metrics.httpx")
        wrapper._telemetry_stats.record_completion(
            call_site="setup.metrics.httpx",
            failed=False,
            latency_seconds=0.123,
            exception_type=None,
        )

        with caplog.at_level(logging.INFO):
            wrapper._emit_telemetry(final=False, elapsed=1.0)

        payloads = _payloads_for_metric(
            caplog,
            "metadata_http_client_telemetry_summary",
        )
        assert len(payloads) == 1
        payload = payloads[0]
        assert payload["event"] == "HTTP client telemetry summary"
        assert payload["client_id"] == "configured-httpx-client"
        assert payload["purpose"] == "metrics_log_sink_httpx"
        assert payload["completed"] == 1
        assert payload["exception_types"] == {}
        assert "completed_qps" in payload
        assert payload["final"] is False
        assert payload["is_closed"] is True
    finally:
        shutdown_metrics()


def test_setup_metrics_routes_json_summary_through_log_sink(caplog):
    setup_metrics(
        MetricsConfig(
            log_enabled=True,
            log_include_patterns=["metadata_json_telemetry_*"],
        )
    )
    try:
        profiler = JsonProfiler(
            purpose="metrics_log_sink_json",
            telemetry_enabled=True,
            telemetry_interval_seconds=0,
        )
        profiler.profile("decode_response", lambda: {"ok": True})

        with caplog.at_level(logging.INFO):
            profiler.emit_telemetry(final=False, elapsed=1.0)

        payloads = _payloads_for_metric(caplog, "metadata_json_telemetry_summary")
        assert len(payloads) == 1
        payload = payloads[0]
        assert payload["event"] == "JSON profiler telemetry summary"
        assert payload["purpose"] == "metrics_log_sink_json"
        assert payload["completed"] >= 1
        assert payload["exception_types"] == {}
        assert "completed_qps" in payload
        assert payload["final"] is False
    finally:
        shutdown_metrics()


def test_setup_metrics_routes_empty_httpx_summary_defaults_through_log_sink(caplog):
    setup_metrics(
        MetricsConfig(
            log_enabled=True,
            log_include_patterns=["metadata_http_client_telemetry_*"],
        )
    )
    try:
        wrapper = HTTPXClientWrapper(
            client_id="empty-httpx-client",
            purpose=None,
            telemetry_enabled=True,
            telemetry_interval_seconds=0,
        )

        with caplog.at_level(logging.INFO):
            wrapper._emit_telemetry(final=True, elapsed=1.0)

        payloads = _payloads_for_metric(
            caplog,
            "metadata_http_client_telemetry_summary",
        )
        assert len(payloads) == 1
        payload = payloads[0]
        assert payload["purpose"] is None
        assert payload["final"] is True
        assert payload["is_closed"] is True
        assert payload["p50_latency_seconds"] is None
        assert payload["p95_latency_seconds"] is None
        assert payload["exception_types"] == {}
    finally:
        shutdown_metrics()


def test_setup_metrics_routes_empty_json_summary_defaults_through_log_sink(caplog):
    setup_metrics(
        MetricsConfig(
            log_enabled=True,
            log_include_patterns=["metadata_json_telemetry_*"],
        )
    )
    try:
        profiler = JsonProfiler(
            purpose="empty-json",
            telemetry_enabled=True,
            telemetry_interval_seconds=0,
        )

        with caplog.at_level(logging.INFO):
            profiler.emit_telemetry(final=True, elapsed=1.0)

        payloads = _payloads_for_metric(caplog, "metadata_json_telemetry_summary")
        assert len(payloads) == 1
        payload = payloads[0]
        assert payload["final"] is True
        assert payload["p50_latency_seconds"] is None
        assert payload["p95_latency_seconds"] is None
        assert payload["exception_types"] == {}
    finally:
        shutdown_metrics()


def test_log_sink_exclude_overrides_include(caplog):
    sink = LogSink(
        include_patterns=("batch_finalize_*",),
        exclude_patterns=("batch_finalize_stage_items",),
    )

    with caplog.at_level(logging.INFO):
        sink.gauge("batch_finalize_stage_items", 1000.0, T("stage", "delete"))

    assert caplog.records == []


def test_setup_metrics_attaches_log_sink():
    runtime = setup_metrics(
        MetricsConfig(
            log_enabled=True,
            log_include_patterns=["metadata_http_client_telemetry_*"],
        )
    )
    try:
        assert isinstance(runtime.sink, LogSink)
    finally:
        shutdown_metrics()


def test_setup_metrics_fans_out_prometheus_and_log_sink():
    runtime = setup_metrics(
        MetricsConfig(
            prometheus_enabled=True,
            log_enabled=True,
            log_include_patterns=["metadata_http_client_telemetry_*"],
        )
    )
    try:
        assert isinstance(runtime.sink, FanoutSink)
    finally:
        shutdown_metrics()

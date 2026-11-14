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

import logging
from unittest.mock import patch

import pytest

from aibrix.metadata.core.metrics import (
    MetricsConfig,
    T,
    setup_metrics,
    shutdown_metrics,
)
from aibrix.metadata.core.metrics.opentsdb import (
    _FAILURE_BACKOFF_SECONDS,
    OpenTSDBSink,
)
from aibrix.metadata.setting import load_metrics_config

_DEFAULT_OPENTSDB_MAX_TAGS = 8


class NoopThread:
    def __init__(self, *, target, name, daemon):
        self.target = target
        self.name = name
        self.daemon = daemon

    def start(self):
        return None

    def is_alive(self):
        return False

    def join(self, timeout=None):
        self.join_timeout = timeout


def _opentsdb_tag_count(payload: str) -> int:
    return len(payload.strip().split()[4:])


def test_opentsdb_sink_formats_telnet_put_line():
    sink = OpenTSDBSink(
        "opentsdb:4242",
        prefix="aibrix_metadata.prod",
        global_tags=(T("service", "aibrix-metadata"),),
    )

    with patch("aibrix.metadata.core.metrics.opentsdb.time.time", return_value=1234):
        payload = sink._format(
            "metadata.batch.api.job.incoming",
            1,
            (T("job_id", "job 1"), T("endpoint", "/v1/batch")),
        )

    try:
        assert payload == (
            "put aibrix_metadata.prod.metadata.batch.api.job.incoming 1234 1 "
            "endpoint=/v1/batch job_id=job_1 service=aibrix-metadata\n"
        )
    finally:
        sink.close()


@pytest.mark.parametrize(
    ("metric_name", "tags"),
    (
        (
            "metadata.http.client.telemetry.completed",
            (
                T("event", "HTTP client telemetry summary"),
                T("client_id", "metadata-client"),
                T("purpose", "batch_runtime_http"),
                T("backend", "httpx_client"),
                T("final", "false"),
                T("is_closed", "false"),
                T("log_context", "client_id,purpose,backend,final,is_closed"),
                T("log_context_bool", "final,is_closed"),
                T("log_context_null_if_empty", "purpose"),
                T("log_field", "completed"),
            ),
        ),
        (
            "metadata.http.client.telemetry.exception_count",
            (
                T("event", "HTTP client telemetry summary"),
                T("client_id", "metadata-client"),
                T("purpose", "batch_runtime_http"),
                T("backend", "httpx_client"),
                T("final", "false"),
                T("is_closed", "false"),
                T("log_context", "client_id,purpose,backend,final,is_closed"),
                T("log_context_bool", "final,is_closed"),
                T("log_context_null_if_empty", "purpose"),
                T("log_map", "exception_types"),
                T("log_key", "ConnectError"),
                T("exception_type", "ConnectError"),
            ),
        ),
        (
            "metadata.http.client.telemetry.latency_bucket_count",
            (
                T("event", "HTTP client telemetry summary"),
                T("client_id", "metadata-client"),
                T("purpose", "batch_runtime_http"),
                T("backend", "httpx_client"),
                T("final", "false"),
                T("is_closed", "false"),
                T("log_context", "client_id,purpose,backend,final,is_closed"),
                T("log_context_bool", "final,is_closed"),
                T("log_context_null_if_empty", "purpose"),
                T("log_list", "latency_histogram"),
                T("log_index", "0"),
                T("latency_histogram_bucket", "0"),
            ),
        ),
        (
            "metadata.http.client.telemetry.summary",
            (
                T("event", "HTTP client telemetry summary"),
                T("client_id", "metadata-client"),
                T("purpose", "batch_runtime_http"),
                T("backend", "httpx_client"),
                T("final", "true"),
                T("is_closed", "true"),
                T("log_context", "client_id,purpose,backend,final,is_closed"),
                T("log_context_bool", "final,is_closed"),
                T("log_context_null_if_empty", "purpose"),
                T("log_default_null", "avg_latency_seconds,p90_latency_seconds"),
                T("log_default_map", "exception_types"),
                T("flush", "true"),
            ),
        ),
        (
            "metadata.batch.api.job.execution_phase_time.ms",
            (
                T("job_id", "job-1"),
                T("console_job_id", "console-job-1"),
                T("endpoint", "/v1/batch"),
                T("completion_window", "24h"),
                T("phase", "finalization"),
                T("phase_order", "060"),
                T("event", "Batch job latency summary"),
                T("log_group", "job-1"),
                T("log_context", "job_id"),
                T("log_map", "phase_duration_ms"),
                T("log_key", "finalization"),
            ),
        ),
        (
            "batch_finalize_stage_field",
            (
                T("event", "Batch finalize telemetry summary"),
                T("job_id", "job-1"),
                T("stage", "mark_finished"),
                T("field", "attempts"),
                T("log_context", "job_id"),
                T("log_group", "job-1"),
                T("log_nested_map", "stage_fields"),
                T("log_outer", "mark_finished"),
                T("log_inner", "attempts"),
            ),
        ),
        (
            "metadata.metrics.emit.count",
            (
                T("event", "Metric emission overhead snapshot"),
                T("active_metric_backend", "OpenTSDBSink"),
                T("log_context", "active_metric_backend"),
                T("log_field", "metric_emit_count"),
            ),
        ),
    ),
)
def test_opentsdb_sink_strips_log_control_tags_to_fit_default_limit(
    metric_name,
    tags,
):
    sink = OpenTSDBSink(
        "opentsdb:4242",
        global_tags=(T("service", "aibrix-metadata"),),
    )

    with patch("aibrix.metadata.core.metrics.opentsdb.time.time", return_value=1234):
        payload = sink._format(metric_name, 1, tags)

    try:
        assert _opentsdb_tag_count(payload) <= _DEFAULT_OPENTSDB_MAX_TAGS
        assert " log_" not in payload
        assert " flush=" not in payload
        assert "service=aibrix-metadata" in payload
    finally:
        sink.close()


def test_opentsdb_sink_enqueues_without_network_io_on_caller_thread():
    class FakeThread:
        def __init__(self, *, target, name, daemon):
            self.target = target
            self.name = name
            self.daemon = daemon
            self.started = False

        def start(self):
            self.started = True

        def is_alive(self):
            return False

        def join(self, timeout=None):
            self.join_timeout = timeout

    with (
        patch("aibrix.metadata.core.metrics.opentsdb.threading.Thread", FakeThread),
        patch.object(
            OpenTSDBSink,
            "_destination",
            side_effect=AssertionError("network IO must stay on the worker"),
        ),
    ):
        sink = OpenTSDBSink("opentsdb:4242")
        sink.gauge("metadata.batch.api.job.incoming", 1, T("job_id", "job-1"))

        payload = sink._queue.get_nowait()
        assert payload.startswith("put metadata.batch.api.job.incoming ")
        assert "job_id=job-1" in payload

        sink.close()


def test_opentsdb_sink_drops_payloads_when_queue_is_full(caplog):
    with patch("aibrix.metadata.core.metrics.opentsdb.threading.Thread", NoopThread):
        sink = OpenTSDBSink("opentsdb:4242", queue_size=1)

        with caplog.at_level(logging.WARNING):
            sink.gauge("metric.one", 1)
            sink.gauge("metric.two", 2)

        assert sink._queue.qsize() == 1
        assert "Dropped OpenTSDB metrics payloads" in caplog.records[-1].message
        assert sink._dropped_payloads_total == 1
        sink.close()


def test_opentsdb_sink_rate_limits_queue_full_drop_warnings(caplog):
    with (
        patch("aibrix.metadata.core.metrics.opentsdb.threading.Thread", NoopThread),
        patch("aibrix.metadata.core.metrics.opentsdb.time.monotonic", return_value=100),
    ):
        sink = OpenTSDBSink("opentsdb:4242", queue_size=1)

        with caplog.at_level(logging.WARNING):
            sink.gauge("metric.one", 1)
            sink.gauge("metric.two", 2)
            sink.gauge("metric.three", 3)

        drop_logs = [
            record
            for record in caplog.records
            if "Dropped OpenTSDB metrics payloads" in record.message
        ]
        assert len(drop_logs) == 1
        assert sink._dropped_payloads_total == 2
        assert sink._dropped_payloads_since_log == 1
        sink.close()


def test_opentsdb_sink_enters_backoff_after_connect_failure():
    with (
        patch("aibrix.metadata.core.metrics.opentsdb.threading.Thread", NoopThread),
        patch("aibrix.metadata.core.metrics.opentsdb.time.monotonic", return_value=100),
    ):
        sink = OpenTSDBSink("opentsdb:4242")

        with patch.object(sink, "_destination", return_value=None):
            sink._send_payload("put metric 1234 1 tag=value\n")

        assert sink._backoff_until == 100 + _FAILURE_BACKOFF_SECONDS
        assert sink._dropped_payloads_total == 1
        sink.close()


def test_opentsdb_sink_drops_without_reconnect_during_backoff():
    with (
        patch("aibrix.metadata.core.metrics.opentsdb.threading.Thread", NoopThread),
        patch("aibrix.metadata.core.metrics.opentsdb.time.monotonic", return_value=100),
    ):
        sink = OpenTSDBSink("opentsdb:4242")
        sink._backoff_until = 101

        with patch.object(
            sink,
            "_destination",
            side_effect=AssertionError("must not reconnect during backoff"),
        ):
            sink._send_payload("put metric 1234 1 tag=value\n")

        assert sink._dropped_payloads_total == 1
        sink.close()


def test_setup_metrics_uses_opentsdb_sink_when_configured():
    runtime = setup_metrics(MetricsConfig(opentsdb_addr="opentsdb:4242"))

    try:
        assert isinstance(runtime.sink, OpenTSDBSink)
        assert runtime.registry is None
        assert runtime.metrics_app is None
    finally:
        shutdown_metrics()


def test_load_metrics_config_includes_opentsdb(monkeypatch):
    from aibrix import envs

    monkeypatch.setattr(envs, "METRICS_PROMETHEUS_ENABLED", False)
    monkeypatch.setattr(envs, "METRICS_STATSD_ADDR", "")
    monkeypatch.setattr(envs, "METRICS_STATSITE_ADDR", "")
    monkeypatch.setattr(envs, "METRICS_DOGSTATSD_ADDR", "")
    monkeypatch.setattr(envs, "METRICS_OPENTSDB_ADDR", "opentsdb:4242")
    monkeypatch.setattr(envs, "METRICS_OPENTSDB_PREFIX", "aibrix_metadata.prod")
    monkeypatch.setattr(envs, "METRICS_OPENTSDB_TIMEOUT_SECONDS", 0.5)
    monkeypatch.setattr(envs, "METRICS_DOGSTATSD_TAGS", [])
    monkeypatch.setattr(envs, "METRICS_LOG_ENABLED", False)
    monkeypatch.setattr(envs, "METRICS_LOG_INCLUDE", [])
    monkeypatch.setattr(envs, "METRICS_LOG_EXCLUDE", [])
    monkeypatch.setattr(envs, "METADATA_SERVICE_TELEMETRY_ENABLE", False)
    monkeypatch.setattr(envs, "METADATA_SERVICE_TELEMETRY_INTERVAL_SECONDS", 10.0)

    config = load_metrics_config()

    assert config is not None
    assert config.opentsdb_addr == "opentsdb:4242"
    assert config.opentsdb_prefix == "aibrix_metadata.prod"
    assert config.opentsdb_timeout_seconds == 0.5

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

from aibrix.metadata.core.metrics import (
    MetricsConfig,
    T,
    setup_metrics,
    shutdown_metrics,
)
from aibrix.metadata.core.metrics.opentsdb import OpenTSDBSink
from aibrix.metadata.setting import load_metrics_config


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

    assert payload == (
        "put aibrix_metadata.prod.metadata.batch.api.job.incoming 1234 1 "
        "endpoint=/v1/batch job_id=job_1 service=aibrix-metadata\n"
    )


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
    class FakeThread:
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

    with patch("aibrix.metadata.core.metrics.opentsdb.threading.Thread", FakeThread):
        sink = OpenTSDBSink("opentsdb:4242", queue_size=1)

        with caplog.at_level(logging.WARNING):
            sink.gauge("metric.one", 1)
            sink.gauge("metric.two", 2)

        assert sink._queue.qsize() == 1
        assert "sink queue is full" in caplog.records[-1].message
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
    monkeypatch.setattr(envs, "METRICS_EMIT_OBSERVATION_ENABLED", False)
    monkeypatch.setattr(envs, "METRICS_EMIT_OBSERVATION_INTERVAL_SECONDS", 10.0)

    config = load_metrics_config()

    assert config is not None
    assert config.opentsdb_addr == "opentsdb:4242"
    assert config.opentsdb_prefix == "aibrix_metadata.prod"
    assert config.opentsdb_timeout_seconds == 0.5

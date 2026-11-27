from aibrix.metadata.core.metrics import MetricsConfig, setup_metrics, shutdown_metrics
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
from aibrix.metadata.core.metrics.sink import (
    MetricEmissionSnapshot,
    SystemTelemetry,
)
from aibrix.metadata.setting import load_metrics_config


class RecordingSink:
    def __init__(self):
        self.gauges = []

    def counter(self, name, value, *tags):
        return None

    def gauge(self, name, value, *tags):
        self.gauges.append((name, value, tags))

    def timer(self, name, value, *tags):
        return None

    def store(self, name, value, *tags):
        return None

    def rate(self, name, value, *tags):
        return None

    def close(self):
        return None


def test_load_metrics_config_returns_config_when_metadata_service_telemetry_enabled(
    monkeypatch,
):
    from aibrix import envs

    monkeypatch.setattr(envs, "METRICS_PROMETHEUS_ENABLED", False)
    monkeypatch.setattr(envs, "METRICS_STATSD_ADDR", "")
    monkeypatch.setattr(envs, "METRICS_STATSITE_ADDR", "")
    monkeypatch.setattr(envs, "METRICS_DOGSTATSD_ADDR", "")
    monkeypatch.setattr(envs, "METADATA_SERVICE_TELEMETRY_ENABLE", True)
    monkeypatch.setattr(envs, "METADATA_SERVICE_TELEMETRY_INTERVAL_SECONDS", 7.5)

    config = load_metrics_config()

    assert config is not None
    assert config.prometheus_enabled is False
    assert config.system_telemetry_enabled is True
    assert config.system_telemetry_interval_seconds == 7.5


def test_setup_metrics_exposes_emit_observer_on_runtime():
    runtime = setup_metrics(
        MetricsConfig(
            prometheus_enabled=False,
            system_telemetry_enabled=True,
            system_telemetry_interval_seconds=5.0,
        )
    )

    try:
        assert runtime.observer is not None
        assert runtime.registry is None
    finally:
        shutdown_metrics()


def test_metric_emission_observer_snapshot_and_flush():
    sink = RecordingSink()
    telemetry = SystemTelemetry(
        sink=sink,
        interval_seconds=30.0,
    )

    telemetry.record("counter", 200.0)
    telemetry.record("timer", 400.0)

    snapshot = telemetry.snapshot_and_reset()

    assert snapshot.emit_count == 2
    assert snapshot.emit_per_second > 0
    assert snapshot.emit_total_ms == 0.6
    assert snapshot.emit_avg_us == 300.0
    assert snapshot.emit_p90_us == 400.0

    telemetry._handle_snapshot(snapshot)

    assert any(
        name == METRIC_METADATA_METRICS_EMIT_TELEMETRY_SUMMARY
        for name, _value, _tags in sink.gauges
    )


def test_system_telemetry_records_dashboard_gauges():
    sink = RecordingSink()
    snapshot = MetricEmissionSnapshot(
        emit_count=10,
        emit_per_second=2.0,
        emit_total_ms=2.5,
        emit_avg_us=250.0,
        emit_p90_us=400.0,
        process_rss_mb=128.0,
        gc_counts=(100, 10, 3),
        active_metric_backend="PrometheusSink",
    )

    telemetry = SystemTelemetry(sink=sink, interval_seconds=30.0)

    telemetry._handle_snapshot(snapshot)

    metric_map = {name: (value, tags) for name, value, tags in sink.gauges}

    assert metric_map[METRIC_METADATA_METRICS_EMIT_COUNT][0] == 10.0
    assert metric_map[METRIC_METADATA_METRICS_EMIT_PER_SECOND][0] == 2.0
    assert metric_map[METRIC_METADATA_METRICS_EMIT_TOTAL_MS][0] == 2.5
    assert metric_map[METRIC_METADATA_METRICS_EMIT_AVG_US][0] == 250.0
    assert metric_map[METRIC_METADATA_METRICS_EMIT_P90_US][0] == 400.0
    assert metric_map[METRIC_METADATA_PROCESS_RSS_MB][0] == 128.0
    assert metric_map[METRIC_METADATA_GC_GEN0_COUNT][0] == 100.0
    assert metric_map[METRIC_METADATA_GC_GEN1_COUNT][0] == 10.0
    assert metric_map[METRIC_METADATA_GC_GEN2_COUNT][0] == 3.0

    avg_tags = {
        tag.name: tag.value
        for tag in metric_map[METRIC_METADATA_METRICS_EMIT_AVG_US][1]
    }
    assert avg_tags["event"] == "Metric emission overhead snapshot"
    assert avg_tags["active_metric_backend"] == "PrometheusSink"
    assert avg_tags["log_context"] == "active_metric_backend"
    assert avg_tags["log_field"] == "metric_emit_avg_us"

    summary_value, summary_tags = metric_map[
        METRIC_METADATA_METRICS_EMIT_TELEMETRY_SUMMARY
    ]
    summary_tags_dict = {tag.name: tag.value for tag in summary_tags}
    assert summary_value == 1.0
    assert summary_tags_dict["flush"] == "true"
    assert summary_tags_dict["log_default_null"] == "metric_emit_p90_us"

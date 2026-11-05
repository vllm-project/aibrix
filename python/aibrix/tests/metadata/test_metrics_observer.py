import logging

from aibrix.metadata.core.metrics import MetricsConfig, setup_metrics, shutdown_metrics
from aibrix.metadata.core.metrics.sink import MetricEmissionObserver
from aibrix.metadata.setting import load_metrics_config


def test_load_metrics_config_returns_config_when_emit_observation_enabled(monkeypatch):
    from aibrix import envs

    monkeypatch.setattr(envs, "METRICS_PROMETHEUS_ENABLED", False)
    monkeypatch.setattr(envs, "METRICS_STATSD_ADDR", "")
    monkeypatch.setattr(envs, "METRICS_STATSITE_ADDR", "")
    monkeypatch.setattr(envs, "METRICS_DOGSTATSD_ADDR", "")
    monkeypatch.setattr(envs, "METRICS_EMIT_OBSERVATION_ENABLED", True)
    monkeypatch.setattr(envs, "METRICS_EMIT_OBSERVATION_INTERVAL_SECONDS", 7.5)

    config = load_metrics_config()

    assert config is not None
    assert config.prometheus_enabled is False
    assert config.emit_observation_enabled is True
    assert config.emit_observation_interval_seconds == 7.5


def test_setup_metrics_exposes_emit_observer_on_runtime():
    runtime = setup_metrics(
        MetricsConfig(
            prometheus_enabled=False,
            emit_observation_enabled=True,
            emit_observation_interval_seconds=5.0,
        )
    )

    try:
        assert runtime.observer is not None
        assert runtime.registry is None
    finally:
        shutdown_metrics()


def test_metric_emission_observer_snapshot_and_flush(caplog):
    observer = MetricEmissionObserver(interval_seconds=30.0, logger_name="test.metrics")

    observer.record("counter", 200.0)
    observer.record("timer", 400.0)

    snapshot = observer.snapshot_and_reset()

    assert snapshot.emit_count == 2
    assert snapshot.emit_total_ms == 0.6
    assert snapshot.emit_avg_us == 300.0
    assert snapshot.emit_p95_us == 400.0

    with caplog.at_level(logging.INFO, logger="test.metrics"):
        observer.log_snapshot(snapshot)

    assert "metric_emit_count" in caplog.text
    assert "process_rss_mb" in caplog.text

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

import argparse
import asyncio
import os
from unittest.mock import patch

from fastapi.testclient import TestClient
from prometheus_client import generate_latest

os.environ.setdefault("SECRET_KEY", "test-secret-key-for-testing")

from aibrix.metadata.app import build_app
from aibrix.metadata.core.metrics import MetricsConfig, setup_metrics, shutdown_metrics
from aibrix.metadata.setting import load_metrics_config, settings
from aibrix.metadata.store import RedisMetadataStore
from tests.fake.redis import FakeRedisClient


def _args(**overrides):
    defaults = {
        "enable_fastapi_docs": False,
        "enable_k8s_support": False,
        "disable_batch_api": True,
        "disable_file_api": True,
        "dry_run": False,
    }
    defaults.update(overrides)
    return argparse.Namespace(**defaults)


def test_metrics_endpoint_exposes_metadata_http_metrics(monkeypatch):
    monkeypatch.setattr(settings, "METRICS", MetricsConfig(prometheus_enabled=True))

    with patch(
        "aibrix.metadata.store.redis.get_redis_client",
        return_value=FakeRedisClient(ping_result=True),
    ):
        app = build_app(_args())
        with TestClient(app) as client:
            health_response = client.get("/healthz")
            assert health_response.status_code == 200

            metrics_response = client.get("/metrics")
            assert metrics_response.status_code == 200

    metrics_text = metrics_response.text
    assert "metadata_http_request_total" in metrics_text
    assert 'method="GET",route="/healthz",status="200"' in metrics_text
    assert "metadata_http_duration_ms_bucket" in metrics_text


def test_load_metrics_config_defaults_metrics_off(monkeypatch):
    from aibrix import envs

    monkeypatch.setattr(envs, "METRICS_PROMETHEUS_ENABLED", False)
    monkeypatch.setattr(envs, "METRICS_STATSD_ADDR", "")
    monkeypatch.setattr(envs, "METRICS_STATSITE_ADDR", "")
    monkeypatch.setattr(envs, "METRICS_DOGSTATSD_ADDR", "")

    assert load_metrics_config() is None


def test_build_app_uses_noop_metrics_runtime_when_disabled(monkeypatch):
    monkeypatch.setattr(settings, "METRICS", None)
    monkeypatch.setattr(
        "aibrix.metadata.core.metrics.setup.load_metrics_config", lambda: None
    )

    with patch(
        "aibrix.metadata.store.redis.get_redis_client",
        return_value=FakeRedisClient(ping_result=True),
    ):
        app = build_app(_args())
        assert app.state.metrics is not None
        assert app.state.metrics.registry is None
        with TestClient(app) as client:
            health_response = client.get("/healthz")
            assert health_response.status_code == 200

            metrics_response = client.get("/metrics")
            assert metrics_response.status_code == 404


def test_redis_metadata_store_emits_prometheus_metrics():
    runtime = setup_metrics(MetricsConfig(prometheus_enabled=True))

    try:
        with patch(
            "aibrix.metadata.store.redis.get_redis_client",
            return_value=FakeRedisClient(values={"user:1": b"demo-user"}),
        ):
            store = RedisMetadataStore()
            result = asyncio.run(store.get("user:1"))
            assert result == b"demo-user"
            asyncio.run(store.close())

        assert runtime.registry is not None
        metrics_text = generate_latest(runtime.registry).decode()
        assert "metadata_store_duration_ms_bucket" in metrics_text
        assert 'backend="redis"' in metrics_text
        assert 'operation="get"' in metrics_text
        assert 'operation="close"' in metrics_text
    finally:
        shutdown_metrics()

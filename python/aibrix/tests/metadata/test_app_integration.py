# Copyright 2024 The Aibrix Team.
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

import argparse
import os
from unittest.mock import patch

import pytest
from fastapi.testclient import TestClient

# Set required environment variable before importing
os.environ.setdefault("SECRET_KEY", "test-secret-key-for-testing")

from aibrix.metadata.app import build_app
from aibrix.metadata.setting import settings
from aibrix.storage import StorageType


def _args(**overrides):
    defaults = {
        "enable_fastapi_docs": False,
        "disable_k8s_support": False,
        "disable_batch_api": True,
        "disable_file_api": True,
        "disable_inference_endpoint": True,
        "enable_k8s_job": False,
        "enable_mongo_job": False,
        "enable_redis_job": False,
        "registry_provider": "configmap",
        "dry_run": False,
        "k8s_namespace": "default",
        "k8s_job_patch": None,
        "kopf_startup_timeout": 30.0,
        "kopf_shutdown_timeout": 10.0,
    }
    defaults.update(overrides)
    return argparse.Namespace(**defaults)


@pytest.fixture(autouse=True)
def _local_storage_settings():
    old_storage = settings.STORAGE_TYPE
    old_metastore = settings.METASTORE_TYPE
    settings.STORAGE_TYPE = StorageType.LOCAL
    settings.METASTORE_TYPE = StorageType.LOCAL
    try:
        yield
    finally:
        settings.STORAGE_TYPE = old_storage
        settings.METASTORE_TYPE = old_metastore


def test_build_app_without_k8s_job():
    """Test building app without K8s job support."""
    args = _args()

    app = build_app(args)

    # App should not have kopf operator wrapper
    assert not hasattr(app.state, "kopf_operator_wrapper")
    assert hasattr(app.state, "httpx_client_wrapper")


def test_build_app_with_k8s_job():
    """Test building app with K8s job support."""
    args = _args(
        disable_batch_api=False,
        enable_k8s_job=True,
        k8s_namespace="test-namespace",
        kopf_startup_timeout=5.0,
        kopf_shutdown_timeout=2.0,
    )

    # build_app constructs ConfigMap-backed template / profile registries
    # and calls reload() on each, which would hit the K8s API. Stub
    # them out here since this test only exercises wiring.
    with (
        patch("aibrix.metadata.app.JobCache"),
        patch("aibrix.metadata.app.k8s_client.CoreV1Api"),
        patch("aibrix.metadata.app.k8s_client.AppsV1Api"),
        patch("aibrix.metadata.app.k8s_template_registry"),
        patch("aibrix.metadata.app.k8s_profile_registry"),
    ):
        app = build_app(args)

    # App should have kopf operator wrapper
    assert hasattr(app.state, "kopf_operator_wrapper")
    assert hasattr(app.state, "httpx_client_wrapper")
    assert hasattr(app.state, "batch_driver")

    # Check kopf operator wrapper configuration
    kopf_wrapper = app.state.kopf_operator_wrapper
    assert kopf_wrapper.namespace == "test-namespace"
    assert kopf_wrapper.startup_timeout == 5.0
    assert kopf_wrapper.shutdown_timeout == 2.0


def test_build_app_with_redis_job(monkeypatch):
    args = _args(
        disable_batch_api=False,
        enable_redis_job=True,
        dry_run=True,
    )
    monkeypatch.setattr("aibrix.metadata.app.envs.STORAGE_REDIS_HOST", "redis-service")
    monkeypatch.setattr("aibrix.metadata.app.envs.STORAGE_REDIS_PORT", 6380)
    monkeypatch.setattr("aibrix.metadata.app.envs.STORAGE_REDIS_DB", 2)
    monkeypatch.setattr("aibrix.metadata.app.envs.STORAGE_REDIS_PASSWORD", "secret")
    monkeypatch.setattr("aibrix.metadata.app.envs.DB_REDIS_PREFIX", "batch_jobs_test:")

    with patch("aibrix.metadata.app.RedisJobCache") as redis_job_cache:
        app = build_app(args)

    assert hasattr(app.state, "batch_driver")
    redis_job_cache.assert_called_once_with(
        host="redis-service",
        port=6380,
        db=2,
        password="secret",
        key_prefix="batch_jobs_test:batch_jobs",
    )


def test_build_app_with_mongo_job(monkeypatch):
    args = _args(
        disable_batch_api=False,
        enable_mongo_job=True,
        dry_run=True,
    )
    monkeypatch.setattr(
        "aibrix.metadata.app.envs.DB_MONGO_URI", "mongodb://mongo:27017"
    )
    monkeypatch.setattr("aibrix.metadata.app.envs.DB_MONGO_DATABASE", "aibrix")
    monkeypatch.setattr("aibrix.metadata.app.envs.DB_MONGO_COLLECTION", "batch_jobs")

    with patch("aibrix.metadata.app.MongoJobCache") as mongo_job_cache:
        app = build_app(args)

    assert hasattr(app.state, "batch_driver")
    mongo_job_cache.assert_called_once_with(
        uri="mongodb://mongo:27017",
        database="aibrix",
        collection="batch_jobs",
    )


def test_build_app_with_redis_job_missing_env(monkeypatch):
    args = _args(
        disable_batch_api=False,
        enable_redis_job=True,
        dry_run=True,
    )
    monkeypatch.setattr("aibrix.metadata.app.envs.STORAGE_REDIS_HOST", None)
    monkeypatch.setattr("aibrix.metadata.app.envs.STORAGE_REDIS_PORT", None)
    monkeypatch.setattr("aibrix.metadata.app.envs.STORAGE_REDIS_DB", None)
    monkeypatch.setattr("aibrix.metadata.app.envs.STORAGE_REDIS_PASSWORD", None)
    monkeypatch.setattr("aibrix.metadata.app.envs.DB_REDIS_PREFIX", None)

    with pytest.raises(
        RuntimeError, match="REDIS_HOST environment variable is required"
    ):
        build_app(args)


def test_build_app_with_mongo_job_missing_env(monkeypatch):
    args = _args(
        disable_batch_api=False,
        enable_mongo_job=True,
        dry_run=True,
    )
    monkeypatch.setattr("aibrix.metadata.app.envs.DB_MONGO_URI", None)
    monkeypatch.setattr("aibrix.metadata.app.envs.DB_MONGO_DATABASE", None)
    monkeypatch.setattr("aibrix.metadata.app.envs.DB_MONGO_COLLECTION", None)

    with pytest.raises(
        RuntimeError, match="DB_MONGO_URI environment variable is required"
    ):
        build_app(args)


def test_status_endpoint_without_k8s():
    """Test /status endpoint without K8s support."""
    args = _args()

    app = build_app(args)
    client = TestClient(app)

    response = client.get("/status")
    assert response.status_code == 200

    data = response.json()
    assert "httpx_client" in data
    assert "kopf_operator" in data
    assert "batch_driver" in data

    assert data["httpx_client"]["available"] is True
    assert data["kopf_operator"]["available"] is False
    assert data["batch_driver"]["available"] is False


def test_status_endpoint_with_k8s():
    """Test /status endpoint with K8s support."""
    args = _args(
        disable_batch_api=False,
        enable_k8s_job=True,
        k8s_namespace="test-namespace",
        kopf_startup_timeout=5.0,
        kopf_shutdown_timeout=2.0,
    )

    with (
        patch("aibrix.metadata.app.JobCache"),
        patch("aibrix.metadata.app.k8s_client.CoreV1Api"),
        patch("aibrix.metadata.app.k8s_client.AppsV1Api"),
        patch("aibrix.metadata.app.k8s_template_registry"),
        patch("aibrix.metadata.app.k8s_profile_registry"),
    ):
        app = build_app(args)

    client = TestClient(app)

    response = client.get("/status")
    assert response.status_code == 200

    data = response.json()
    assert "httpx_client" in data
    assert "kopf_operator" in data
    assert "batch_driver" in data

    assert data["httpx_client"]["available"] is True
    assert data["kopf_operator"]["available"] is True
    assert data["batch_driver"]["available"] is True

    # Check kopf operator status details
    kopf_status = data["kopf_operator"]
    assert "is_running" in kopf_status
    assert "namespace" in kopf_status
    assert kopf_status["namespace"] == "test-namespace"
    assert kopf_status["startup_timeout"] == 5.0
    assert kopf_status["shutdown_timeout"] == 2.0


def test_healthz_endpoint():
    """Test /healthz endpoint."""
    args = _args()

    app = build_app(args)
    client = TestClient(app)

    response = client.get("/healthz")
    assert response.status_code == 200

    data = response.json()
    assert data["status"] == "ok"


def test_ready_endpoint():
    """Test /readyz endpoint."""
    args = _args()

    app = build_app(args)
    client = TestClient(app)

    response = client.get("/readyz")
    assert response.status_code == 200

    data = response.json()
    assert data["status"] == "ready"

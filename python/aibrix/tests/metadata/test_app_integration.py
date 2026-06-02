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
from unittest.mock import MagicMock, patch

import pytest
from fastapi.testclient import TestClient
from kubernetes import client, config

# Set required environment variable before importing
os.environ.setdefault("SECRET_KEY", "test-secret-key-for-testing")

from aibrix.metadata.app import build_app
from aibrix.metadata.cache.redis import RedisJobCache
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
        "enable_metastore_job": False,
        "enable_redis_job": False,
        "registry_provider": None,
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


@pytest.fixture
def _mock_k8s_config_loading():
    with (
        patch("aibrix.metadata.app.config.load_incluster_config") as load_incluster,
        patch("aibrix.metadata.app.config.load_kube_config") as load_kube,
    ):
        yield load_incluster, load_kube


@pytest.fixture
def _mock_k8s_runtime():
    with (
        patch("aibrix.metadata.app.k8s_client.CoreV1Api") as core_api,
        patch("aibrix.metadata.app.k8s_client.AppsV1Api") as apps_api,
        patch("aibrix.metadata.app.k8s_template_registry") as template_registry,
        patch("aibrix.metadata.app.k8s_profile_registry") as profile_registry,
    ):
        yield core_api, apps_api, template_registry, profile_registry


@pytest.fixture(scope="session")
def k8s_config():
    """Initialize Kubernetes client and test connectivity."""
    try:
        # Try to load in-cluster config first, then fallback to local config
        try:
            config.load_incluster_config()
        except config.ConfigException:
            try:
                config.load_kube_config()
            except config.ConfigException as e:
                pytest.skip(f"Kubernetes configuration not available: {e}")

        # Test API server accessibility with client-side request timeout.
        try:
            v1 = client.CoreV1Api()
            api_host = v1.api_client.configuration.host
            if not api_host:
                pytest.skip(
                    "Kubernetes configuration is invalid: no API server host found"
                )
            v1.list_namespace(limit=1, _request_timeout=(1, 2))

        except Exception as e:
            pytest.skip(f"Failed to create Kubernetes API client: {e}")

    except Exception as e:
        pytest.skip(f"Failed to initialize Kubernetes client: {e}")


def test_build_app_without_k8s_job(_mock_k8s_config_loading):
    """Test building app without K8s job support."""
    args = _args(
        disable_batch_api=True,
        disable_file_api=True,
        enable_k8s_job=False,
        disable_k8s_support=True,
        disable_inference_endpoint=True,
    )
    load_incluster, load_kube = _mock_k8s_config_loading

    app = build_app(args)

    # App should not load k8s config
    load_incluster.assert_not_called()
    load_kube.assert_not_called()

    # App should not have kopf operator wrapper
    assert not hasattr(app.state, "kopf_operator_wrapper")
    assert hasattr(app.state, "httpx_client_wrapper")


def test_build_app_disabled_batch_api_without_inference_endpoint(
    _mock_k8s_config_loading, monkeypatch
):
    """Regression for #2185: disabling the batch API must not require an
    inference endpoint. The inference client is only consumed by the batch
    API, so build_app should succeed (not sys.exit) when batch is off and no
    INFERENCE_ENGINE_ENDPOINT is configured."""
    monkeypatch.delenv("INFERENCE_ENGINE_ENDPOINT", raising=False)
    args = _args(
        disable_batch_api=True,
        disable_inference_endpoint=False,
        disable_k8s_support=True,
    )

    app = build_app(args)

    assert not hasattr(app.state, "batch_driver")
    assert hasattr(app.state, "httpx_client_wrapper")


def test_build_app_k8s_job_without_inference_endpoint(
    _mock_k8s_config_loading, monkeypatch
):
    """Regression for #2185: in k8s-job mode the worker pods bring their own
    engine endpoint, so build_app must not require INFERENCE_ENGINE_ENDPOINT
    even when --disable-inference-endpoint is not set. This is the default
    deployment mode for the Helm chart and kustomize manifests."""
    monkeypatch.delenv("INFERENCE_ENGINE_ENDPOINT", raising=False)
    args = _args(
        disable_batch_api=False,
        enable_k8s_job=True,
        disable_inference_endpoint=False,
    )

    with (
        patch("aibrix.metadata.app.JobCache"),
        patch("aibrix.metadata.app.k8s_client.CoreV1Api"),
        patch("aibrix.metadata.app.k8s_client.AppsV1Api"),
        patch("aibrix.metadata.app.k8s_template_registry"),
        patch("aibrix.metadata.app.k8s_profile_registry"),
    ):
        app = build_app(args)

    assert hasattr(app.state, "batch_driver")
    assert hasattr(app.state, "kopf_operator_wrapper")


def test_build_app_with_k8s_job(_mock_k8s_config_loading):
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
    load_incluster, load_kube = _mock_k8s_config_loading
    with patch("aibrix.metadata.app.JobCache"):
        app = build_app(args)

    # App should have kopf operator wrapper
    assert hasattr(app.state, "kopf_operator_wrapper")
    assert hasattr(app.state, "httpx_client_wrapper")
    assert hasattr(app.state, "batch_driver")
    load_incluster.assert_called_once_with()
    load_kube.assert_not_called()

    # Check kopf operator wrapper configuration
    kopf_wrapper = app.state.kopf_operator_wrapper
    assert kopf_wrapper.namespace == "test-namespace"
    assert kopf_wrapper.startup_timeout == 5.0
    assert kopf_wrapper.shutdown_timeout == 2.0


def test_load_batch_k8s_context_skips_registry_loading_when_provider_unset(
    k8s_config,
):
    args = _args(
        registry_provider=None,
        dry_run=False,
        disable_batch_api=False,
        enable_redis_job=True,
        disable_inference_endpoint=True,
    )
    redis_client = MagicMock(name="redis_client")

    with (
        patch("aibrix.metadata.app.config.load_incluster_config") as load_incluster,
        patch("aibrix.metadata.app.config.load_kube_config") as load_kube,
        patch("aibrix.metadata.app.k8s_client.CoreV1Api") as core_api,
        patch("aibrix.metadata.app.k8s_client.AppsV1Api") as apps_api,
        patch("aibrix.metadata.app.k8s_template_registry") as template_registry,
        patch("aibrix.metadata.app.k8s_profile_registry") as profile_registry,
        patch(
            "aibrix.metadata.app.redis.get_redis_client",
            return_value=redis_client,
        ) as get_redis_client,
    ):
        app = build_app(args)

    load_incluster.assert_called_once_with()
    load_kube.assert_not_called()
    core_api.assert_called_once_with()  # in _load_batch_k8s_context
    apps_api.assert_called_once_with()  # in _load_batch_k8s_context
    template_registry.assert_not_called()
    profile_registry.assert_not_called()
    assert args.disable_k8s_support is False
    assert app.state.template_registry is None
    assert app.state.profile_registry is None
    assert isinstance(app.state.batch_driver._job_entity_manager, RedisJobCache)
    assert app.state.batch_driver._job_entity_manager._client is redis_client
    get_redis_client.assert_called_once_with()


def test_load_batch_k8s_context_registry_loading_overrides_k8s_disabled(
    _mock_k8s_runtime,
    k8s_config,
):
    args = _args(
        disable_k8s_support=True,
        registry_provider="configmap",
        dry_run=False,
        disable_batch_api=False,
        enable_k8s_job=True,
        disable_inference_endpoint=True,
        k8s_namespace="test-namespace",
    )
    template_registry = MagicMock()
    profile_registry = MagicMock()
    core_api, apps_api, _, _ = _mock_k8s_runtime

    with (
        patch(
            "aibrix.metadata.app.k8s_template_registry",
            return_value=template_registry,
        ) as template_registry_factory,
        patch(
            "aibrix.metadata.app.k8s_profile_registry",
            return_value=profile_registry,
        ) as profile_registry_factory,
        patch("aibrix.metadata.app.JobCache"),
    ):
        app = build_app(args)

    core_api.assert_called_once_with()
    apps_api.assert_called_once_with()
    template_registry_factory.assert_called_once_with(
        core_api.return_value, namespace="test-namespace"
    )
    profile_registry_factory.assert_called_once_with(
        core_api.return_value, namespace="test-namespace"
    )
    template_registry.reload.assert_called_once_with()
    profile_registry.reload.assert_called_once_with()
    assert args.disable_k8s_support is False
    assert app.state.template_registry is template_registry
    assert app.state.profile_registry is profile_registry


def test_build_app_with_redis_job():
    args = _args(
        disable_k8s_support=True,
        disable_batch_api=False,
        enable_redis_job=True,
        dry_run=True,
    )
    redis_client = MagicMock(name="redis_client")

    with (
        patch(
            "aibrix.metadata.app.redis.get_redis_client",
            return_value=redis_client,
        ) as get_redis_client,
        patch("aibrix.metadata.app.RedisJobCache") as redis_job_cache,
    ):
        app = build_app(args)

    assert hasattr(app.state, "batch_driver")
    get_redis_client.assert_called_once_with()
    redis_job_cache.assert_called_once_with(redis_client)


def test_build_app_with_redis_job_missing_env():
    args = _args(
        disable_k8s_support=True,
        disable_batch_api=False,
        enable_redis_job=True,
        dry_run=True,
    )

    with (
        patch(
            "aibrix.metadata.app.redis.get_redis_client",
            side_effect=RuntimeError("REDIS_HOST environment variable is required"),
        ),
        pytest.raises(
            RuntimeError, match="REDIS_HOST environment variable is required"
        ),
    ):
        build_app(args)


def test_status_endpoint_without_k8s(_mock_k8s_config_loading):
    """Test /status endpoint without K8s support."""
    args = _args(
        disable_k8s_support=True,
        disable_batch_api=True,
        disable_file_api=True,
        enable_k8s_job=False,
        disable_inference_endpoint=True,
    )
    load_incluster, load_kube = _mock_k8s_config_loading

    app = build_app(args)

    # App should not load k8s config
    load_incluster.assert_not_called()
    load_kube.assert_not_called()

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


def test_status_endpoint_with_k8s(
    _mock_k8s_config_loading, _mock_k8s_runtime, k8s_config
):
    """Test /status endpoint with K8s support."""
    args = _args(
        disable_batch_api=False,
        enable_k8s_job=True,
        k8s_namespace="test-namespace",
        kopf_startup_timeout=5.0,
        kopf_shutdown_timeout=2.0,
    )

    load_incluster, load_kube = _mock_k8s_config_loading
    with patch("aibrix.metadata.app.JobCache"):
        app = build_app(args)

    # App should load k8s config
    load_incluster.assert_called_once_with()
    load_kube.assert_not_called()

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
    args = _args(
        disable_k8s_support=True,
        disable_batch_api=True,
        disable_file_api=True,
        enable_k8s_job=False,
        disable_inference_endpoint=True,
    )

    app = build_app(args)
    client = TestClient(app)

    response = client.get("/healthz")
    assert response.status_code == 200

    data = response.json()
    assert data["status"] == "ok"


def test_ready_endpoint():
    """Test /readyz endpoint."""
    args = _args(
        disable_k8s_support=True,
        disable_batch_api=True,
        disable_file_api=True,
        enable_k8s_job=False,
        disable_inference_endpoint=True,
    )

    app = build_app(args)
    client = TestClient(app)

    response = client.get("/readyz")
    assert response.status_code == 200

    data = response.json()
    assert data["status"] == "ready"

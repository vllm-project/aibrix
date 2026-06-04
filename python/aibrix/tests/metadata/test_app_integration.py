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

# Set required environment variable before importing
os.environ.setdefault("SECRET_KEY", "test-secret-key-for-testing")

from aibrix.batch.state import JobStore
from aibrix.metadata.app import build_app
from aibrix.metadata.setting import settings
from aibrix.storage import StorageType


def _args(**overrides):
    defaults = {
        "enable_fastapi_docs": False,
        "enable_k8s_support": True,
        "disable_batch_api": True,
        "disable_file_api": True,
        "job_store_provider": None,
        "dry_run": False,
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
    ):
        yield core_api, apps_api


def test_build_app_without_batch(_mock_k8s_config_loading):
    """Building the app with batch + k8s disabled wires only the HTTP layer."""
    args = _args(
        disable_batch_api=True,
        disable_file_api=True,
        enable_k8s_support=False,
    )
    load_incluster, load_kube = _mock_k8s_config_loading

    app = build_app(args)

    # App should not load k8s config
    load_incluster.assert_not_called()
    load_kube.assert_not_called()

    assert hasattr(app.state, "httpx_client_wrapper")
    assert not hasattr(app.state, "batch_driver")


def test_build_app_batch_no_global_inference_endpoint(
    _mock_k8s_config_loading, monkeypatch
):
    """With the batch API enabled, build_app succeeds without any global
    inference endpoint: jobs carry their own aibrix.runtime.target and the
    per-job runtime builds its own EndpointSource. The default entity manager
    is the metastore-backed JobStore."""
    monkeypatch.delenv("INFERENCE_ENGINE_ENDPOINT", raising=False)
    args = _args(disable_batch_api=False)

    app = build_app(args)

    assert hasattr(app.state, "batch_driver")
    assert isinstance(app.state.batch_driver.job_manager._job_entity_manager, JobStore)


def test_build_app_creates_k8s_clients_when_enabled(
    _mock_k8s_runtime, _mock_k8s_config_loading
):
    """--enable-k8s-support loads kube config and creates the CoreV1/AppsV1
    clients used for per-job k8s execution. The ConfigMap-driven template/
    profile registries were removed, so they stay None."""
    args = _args(disable_batch_api=False, enable_k8s_support=True)
    load_incluster, load_kube = _mock_k8s_config_loading
    core_api, apps_api = _mock_k8s_runtime

    app = build_app(args)

    load_incluster.assert_called_once_with()
    load_kube.assert_not_called()
    core_api.assert_called_once_with()  # in _load_batch_k8s_context
    apps_api.assert_called_once_with()  # in _load_batch_k8s_context
    assert app.state.template_registry is None
    assert app.state.profile_registry is None
    assert isinstance(app.state.batch_driver.job_manager._job_entity_manager, JobStore)


def test_build_app_skips_k8s_clients_when_disabled(
    _mock_k8s_runtime, _mock_k8s_config_loading
):
    """Without --enable-k8s-support (the default), build_app neither loads kube
    config nor creates k8s clients, even with the batch API enabled."""
    args = _args(disable_batch_api=False, enable_k8s_support=False)
    load_incluster, load_kube = _mock_k8s_config_loading
    core_api, apps_api = _mock_k8s_runtime

    app = build_app(args)

    load_incluster.assert_not_called()
    load_kube.assert_not_called()
    core_api.assert_not_called()
    apps_api.assert_not_called()
    assert hasattr(app.state, "batch_driver")


def test_build_app_with_redis_job_store():
    args = _args(
        enable_k8s_support=False,
        disable_batch_api=False,
        job_store_provider="redis",
    )
    redis_client = MagicMock(name="redis_client")

    with (
        patch(
            "aibrix.client.redis.get_redis_client",
            return_value=redis_client,
        ) as get_redis_client,
        patch("aibrix.batch.state.redis_job_store.RedisJobStore") as redis_job_store,
    ):
        app = build_app(args)

    assert hasattr(app.state, "batch_driver")
    get_redis_client.assert_called_once_with(require_check=True)
    redis_job_store.assert_called_once_with(redis_client)


def test_build_app_with_redis_job_store_during_dry_run():
    args = _args(
        enable_k8s_support=False,
        disable_batch_api=False,
        job_store_provider="redis",
        dry_run=True,
    )
    with pytest.raises(
        RuntimeError, match="Redis job store is not supported in dry run mode"
    ):
        build_app(args)


def test_build_app_with_redis_job_missing_env():
    args = _args(
        disable_k8s_support=True,
        disable_batch_api=False,
        job_store_provider="redis",
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
        enable_k8s_support=False,
        disable_batch_api=True,
        disable_file_api=True,
    )
    load_incluster, load_kube = _mock_k8s_config_loading

    app = build_app(args)

    load_incluster.assert_not_called()
    load_kube.assert_not_called()

    test_client = TestClient(app)

    response = test_client.get("/status")
    assert response.status_code == 200

    data = response.json()
    assert "httpx_client" in data
    assert "batch_driver" in data
    assert "kopf_operator" not in data

    assert data["httpx_client"]["available"] is True
    assert data["batch_driver"]["available"] is False


def test_healthz_endpoint():
    """Test /healthz endpoint."""
    args = _args(
        enable_k8s_support=False,
        disable_batch_api=True,
        disable_file_api=True,
    )

    app = build_app(args)
    test_client = TestClient(app)

    response = test_client.get("/healthz")
    assert response.status_code == 200

    data = response.json()
    assert data["status"] == "ok"


def test_ready_endpoint():
    """Test /readyz endpoint."""
    args = _args(
        enable_k8s_support=False,
        disable_batch_api=True,
        disable_file_api=True,
    )

    app = build_app(args)
    test_client = TestClient(app)

    response = test_client.get("/readyz")
    assert response.status_code == 200

    data = response.json()
    assert data["status"] == "ready"

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

from fastapi.testclient import TestClient

# Set required environment variable before importing
os.environ.setdefault("SECRET_KEY", "test-secret-key-for-testing")

from aibrix.metadata.app import build_app


def test_build_app_without_k8s_job():
    """Test building app without K8s job support."""
    args = argparse.Namespace(
        enable_fastapi_docs=False,
        disable_batch_api=True,
        disable_file_api=True,
        enable_k8s_job=False,
        e2e_test=False,
    )

    app = build_app(args)

    # App should not have kopf operator wrapper
    assert not hasattr(app.state, "kopf_operator_wrapper")
    assert hasattr(app.state, "httpx_client_wrapper")


def test_build_app_with_k8s_job():
    """Test building app with K8s job support."""
    args = argparse.Namespace(
        enable_fastapi_docs=False,
        disable_batch_api=False,
        disable_file_api=True,
        enable_k8s_job=True,
        k8s_namespace="test-namespace",
        k8s_job_patch=None,
        kopf_startup_timeout=5.0,
        kopf_shutdown_timeout=2.0,
        e2e_test=False,
    )

    with patch("aibrix.metadata.app.JobCache"):
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


def test_status_endpoint_without_k8s():
    """Test /status endpoint without K8s support."""
    args = argparse.Namespace(
        enable_fastapi_docs=False,
        disable_batch_api=True,
        disable_file_api=True,
        enable_k8s_job=False,
        e2e_test=False,
    )

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
    args = argparse.Namespace(
        enable_fastapi_docs=False,
        disable_batch_api=False,
        disable_file_api=True,
        enable_k8s_job=True,
        k8s_job_patch=None,
        k8s_namespace="test-namespace",
        kopf_startup_timeout=5.0,
        kopf_shutdown_timeout=2.0,
        e2e_test=False,
    )

    with patch("aibrix.metadata.app.JobCache"):
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
    args = argparse.Namespace(
        enable_fastapi_docs=False,
        disable_batch_api=True,
        disable_file_api=True,
        enable_k8s_job=False,
        e2e_test=False,
    )

    app = build_app(args)
    client = TestClient(app)

    response = client.get("/healthz")
    assert response.status_code == 200

    data = response.json()
    assert data["status"] == "ok"


def test_ready_endpoint():
    """Test /readyz endpoint."""
    args = argparse.Namespace(
        enable_fastapi_docs=False,
        disable_batch_api=True,
        disable_file_api=True,
        enable_k8s_job=False,
        e2e_test=False,
    )

    app = build_app(args)
    client = TestClient(app)

    response = client.get("/readyz")
    assert response.status_code == 200

    data = response.json()
    assert data["status"] == "ready"

# Copyright 2025 The Aibrix Team.
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

"""
Tests for the metadata models API.

Test Coverage:
- K8sModelDiscovery: Kubernetes-based model discovery without caching
- Models API: /v1/models endpoint for listing available models
"""

import os
from unittest.mock import MagicMock, patch

import pytest
from fastapi.testclient import TestClient
from kubernetes.client.models import V1ObjectMeta, V1Pod, V1PodStatus

# Set required environment variable before importing
os.environ.setdefault("SECRET_KEY", "test-secret-key-for-testing")

# Try importing, skip tests if dependencies missing
try:
    from aibrix.metadata.api.v1.models import K8sModelDiscovery
    from aibrix.metadata.app import build_app

    DEPENDENCIES_AVAILABLE = True
except ModuleNotFoundError as e:
    DEPENDENCIES_AVAILABLE = False
    SKIP_REASON = f"Missing dependency: {e}"

pytestmark = pytest.mark.skipif(
    not DEPENDENCIES_AVAILABLE,
    reason="Dependencies not available. Run: poetry install --with dev",
)


class TestK8sModelDiscovery:
    """Tests for K8sModelDiscovery class."""

    @patch("kubernetes.client.CoreV1Api")
    def test_get_base_models_from_pods(self, mock_core_v1):
        """Test getting base models from pods."""
        # Mock pods with model label
        mock_pod1 = V1Pod(
            metadata=V1ObjectMeta(
                name="model-pod-1",
                labels={
                    "model.aibrix.ai/name": "llama-3-8b",
                },
            ),
            status=V1PodStatus(phase="Running"),
        )
        mock_pod2 = V1Pod(
            metadata=V1ObjectMeta(
                name="model-pod-2",
                labels={
                    "model.aibrix.ai/name": "llama-3-8b",
                },
            ),
            status=V1PodStatus(phase="Running"),
        )
        mock_pod3 = V1Pod(
            metadata=V1ObjectMeta(
                name="model-pod-3",
                labels={
                    "model.aibrix.ai/name": "mistral-7b",
                },
            ),
            status=V1PodStatus(phase="Running"),
        )
        # Pod with pending status should be excluded
        mock_pod4 = V1Pod(
            metadata=V1ObjectMeta(
                name="model-pod-4",
                labels={
                    "model.aibrix.ai/name": "gpt-4",
                },
            ),
            status=V1PodStatus(phase="Pending"),
        )
        # Ray worker pod should be excluded
        mock_pod5 = V1Pod(
            metadata=V1ObjectMeta(
                name="ray-worker-1",
                labels={
                    "model.aibrix.ai/name": "ray-model",
                    "ray.io/node-type": "worker",
                },
            ),
            status=V1PodStatus(phase="Running"),
        )

        mock_core_v1.return_value.list_pod_for_all_namespaces.return_value.items = [
            mock_pod1,
            mock_pod2,
            mock_pod3,
            mock_pod4,
            mock_pod5,
        ]

        discovery = K8sModelDiscovery()
        models = discovery.get_base_models_from_pods()

        # Should only include Running pods, exclude Ray workers and duplicates
        assert models == {"llama-3-8b", "mistral-7b"}

    @patch("kubernetes.client.CustomObjectsApi")
    def test_get_adapter_models_from_crds(self, mock_custom_objects):
        """Test getting adapter models from ModelAdapter CRDs."""
        mock_adapters = {
            "items": [
                {
                    "metadata": {"name": "lora-adapter-1"},
                },
                {
                    "metadata": {"name": "lora-adapter-2"},
                },
            ]
        }

        mock_custom_objects.return_value.list_cluster_custom_object.return_value = (
            mock_adapters
        )

        discovery = K8sModelDiscovery()
        models = discovery.get_adapter_models_from_crds()

        assert models == {"lora-adapter-1", "lora-adapter-2"}

    @patch("kubernetes.client.CustomObjectsApi")
    @patch("kubernetes.client.CoreV1Api")
    def test_get_all_models(self, mock_core_v1, mock_custom_objects):
        """Test getting all models (base + adapters)."""
        # Mock base models
        mock_pod = V1Pod(
            metadata=V1ObjectMeta(
                name="model-pod",
                labels={"model.aibrix.ai/name": "llama-3-8b"},
            ),
            status=V1PodStatus(phase="Running"),
        )
        mock_core_v1.return_value.list_pod_for_all_namespaces.return_value.items = [
            mock_pod
        ]

        # Mock adapters
        mock_adapters = {
            "items": [
                {"metadata": {"name": "lora-adapter-1"}},
            ]
        }
        mock_custom_objects.return_value.list_cluster_custom_object.return_value = (
            mock_adapters
        )

        discovery = K8sModelDiscovery()
        models = discovery.get_all_models()

        # Should be sorted list of all models
        assert models == ["llama-3-8b", "lora-adapter-1"]


class TestModelsAPI:
    """Integration tests for /v1/models API."""

    @patch("kubernetes.config.load_kube_config")
    @patch("kubernetes.config.load_incluster_config")
    @patch("kubernetes.client.CustomObjectsApi")
    @patch("kubernetes.client.CoreV1Api")
    def test_list_models_endpoint(
        self,
        mock_core_v1,
        mock_custom_objects,
        mock_incluster_config,
        mock_kube_config,
    ):
        """Test /v1/models/ endpoint returns model list."""
        from argparse import Namespace
        from kubernetes import config as k8s_config

        # Mock config loading to avoid real K8s config
        mock_incluster_config.side_effect = k8s_config.ConfigException(
            "Not in cluster"
        )

        # Mock K8s responses
        mock_pod1 = V1Pod(
            metadata=V1ObjectMeta(
                name="llama-3-8b-pod",
                labels={"model.aibrix.ai/name": "llama-3-8b"},
            ),
            status=V1PodStatus(phase="Running"),
        )
        mock_pod2 = V1Pod(
            metadata=V1ObjectMeta(
                name="mistral-7b-pod",
                labels={"model.aibrix.ai/name": "mistral-7b"},
            ),
            status=V1PodStatus(phase="Running"),
        )
        mock_core_v1.return_value.list_pod_for_all_namespaces.return_value.items = [
            mock_pod1,
            mock_pod2,
        ]

        mock_adapters = {
            "items": [
                {"metadata": {"name": "lora-adapter-1"}},
            ]
        }
        mock_custom_objects.return_value.list_cluster_custom_object.return_value = (
            mock_adapters
        )

        # Build app without K8s job support for simpler testing
        args = Namespace(
            enable_fastapi_docs=False,
            disable_batch_api=True,
            disable_file_api=True,
            enable_k8s_job=False,
            e2e_test=False,
        )
        app = build_app(args)
        client = TestClient(app)

        # Test endpoint (note: API_V1_STR = "/v1" so endpoint is /v1/models/)
        response = client.get("/v1/models/")
        assert response.status_code == 200

        data = response.json()
        assert data["object"] == "list"
        assert len(data["data"]) == 3

        # Verify model structure
        model_ids = [model["id"] for model in data["data"]]
        assert "llama-3-8b" in model_ids
        assert "mistral-7b" in model_ids
        assert "lora-adapter-1" in model_ids

        for model in data["data"]:
            assert model["object"] == "model"
            assert "id" in model
            assert "created" in model
            assert "owned_by" in model
            assert model["owned_by"] == "aibrix"

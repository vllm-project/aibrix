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

from typing import List, Set

from fastapi import APIRouter, Request
from pydantic import BaseModel, Field

from aibrix.logger import init_logger

logger = init_logger(__name__)
router = APIRouter()


class ModelInfo(BaseModel):
    """Model information matching OpenAI format."""

    id: str = Field(description="Model identifier")
    created: int = Field(default=0, description="Creation timestamp")
    object: str = Field(default="model", description="Object type")
    owned_by: str = Field(default="aibrix", description="Owner")


class ModelListResponse(BaseModel):
    """Response model for listing models."""

    object: str = Field(default="list", description="Object type")
    data: List[ModelInfo] = Field(description="List of models")


class K8sModelDiscovery:
    """Kubernetes-based model discovery.

    Discovers models from two sources:
    1. Base models: From pod labels (model.aibrix.ai/name)
    2. Adapter models: From ModelAdapter CRD resources
    """

    # Label constants
    MODEL_LABEL = "model.aibrix.ai/name"
    NODE_TYPE_LABEL = "ray.io/node-type"
    NODE_WORKER_VALUE = "worker"

    def __init__(self):
        self.core_api = None
        self.custom_api = None

    def init_k8s_clients(self):
        """Initialize Kubernetes clients lazily."""
        if self.core_api is None:
            from kubernetes import client, config

            try:
                # Try in-cluster config first
                config.load_incluster_config()
                logger.info("Using in-cluster Kubernetes configuration")
            except config.ConfigException:
                # Fall back to kubeconfig
                config.load_kube_config()
                logger.info("Using kubeconfig for Kubernetes configuration")

            self.core_api = client.CoreV1Api()
            self.custom_api = client.CustomObjectsApi()

    def get_base_models_from_pods(self) -> Set[str]:
        """Get base model names from pod labels.

        Filters pods to only include:
        - Pods with label model.aibrix.ai/name
        - Pods in Running state
        - Excludes Ray worker pods (ray.io/node-type=worker)

        Returns:
            Set of base model names
        """
        self.init_k8s_clients()

        # List pods with model.aibrix.ai/name label
        pods = self.core_api.list_pod_for_all_namespaces(
            label_selector=self.MODEL_LABEL
        )

        models = set()
        for pod in pods.items:
            # Only consider Running pods
            if pod.status.phase != "Running":
                logger.debug(
                    f"Skipping pod {pod.metadata.name} in {pod.metadata.namespace}: "
                    f"phase={pod.status.phase}"
                )
                continue

            # Get labels
            labels = pod.metadata.labels or {}

            # Exclude Ray worker pods
            if labels.get(self.NODE_TYPE_LABEL) == self.NODE_WORKER_VALUE:
                logger.debug(
                    f"Skipping Ray worker pod {pod.metadata.name} "
                    f"in {pod.metadata.namespace}"
                )
                continue

            # Get base model name
            model_name = labels.get(self.MODEL_LABEL)
            if model_name:
                models.add(model_name)
                logger.debug(
                    f"Found base model: {model_name} from pod "
                    f"{pod.metadata.name} in {pod.metadata.namespace}"
                )

        logger.info(f"Discovered {len(models)} base models from pods")
        return models

    def get_adapter_models_from_crds(self) -> Set[str]:
        """Get adapter model names from ModelAdapter CRDs.

        Returns:
            Set of adapter model names
        """
        self.init_k8s_clients()

        models = set()
        try:
            # List ModelAdapter CRDs across all namespaces
            model_adapters = self.custom_api.list_cluster_custom_object(
                group="model.aibrix.ai", version="v1alpha1", plural="modeladapters"
            )

            for adapter in model_adapters.get("items", []):
                # Get adapter name from metadata
                adapter_name = adapter.get("metadata", {}).get("name")
                if adapter_name:
                    models.add(adapter_name)
                    namespace = adapter.get("metadata", {}).get("namespace", "unknown")
                    logger.debug(
                        f"Found adapter model: {adapter_name} in namespace {namespace}"
                    )

            logger.info(f"Discovered {len(models)} adapter models from CRDs")

        except Exception as e:
            logger.warning(f"Failed to list ModelAdapters: {e}")

        return models

    def get_all_models(self) -> List[str]:
        """Get all models (base + adapters).

        Returns:
            Sorted list of all model names
        """
        base_models = self.get_base_models_from_pods()
        adapter_models = self.get_adapter_models_from_crds()

        # Combine and sort
        all_models = base_models.union(adapter_models)
        return sorted(all_models)


# Global discovery instance
k8s_discovery = K8sModelDiscovery()


@router.get("/")
async def list_models(request: Request) -> ModelListResponse:
    """List all models from Kubernetes.

    This endpoint aggregates models from:
    1. Base models: Pods with label model.aibrix.ai/name (Running, non-worker)
    2. Adapter models: ModelAdapter CRD resources

    No caching - always returns fresh data from Kubernetes API.

    Returns:
        ModelListResponse with list of models in OpenAI format
    """
    try:
        # Get fresh model list from K8s
        model_names = k8s_discovery.get_all_models()

        # Build response
        model_list = [
            ModelInfo(id=model_name, created=0, object="model", owned_by="aibrix")
            for model_name in model_names
        ]

        logger.info(f"Listed {len(model_list)} models")
        return ModelListResponse(data=model_list)

    except Exception as e:
        logger.error(f"Failed to list models: {e}", exc_info=True)
        # Return empty list on error (matches Go behavior)
        return ModelListResponse(data=[])

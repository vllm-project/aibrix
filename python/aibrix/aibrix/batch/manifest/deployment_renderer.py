# Copyright 2026 The Aibrix Team.
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

from __future__ import annotations

import uuid
from copy import deepcopy
from typing import Any, Dict, Optional

from aibrix.batch.template import BatchProfile, ModelDeploymentTemplate
from aibrix.downloader.utils import infer_model_name

from .downloader_env import build_downloader_env
from .renderer import _RendererSupport

_AIBRIX_MODEL_NAME_KEY = "model.aibrix.ai/name"
_RUNTIME_CONTAINER_NAME = "aibrix-runtime"
_RUNTIME_IMAGE = "aibrix/runtime:nightly"
_RUNTIME_PORT = 8080
_RUNTIME_COMMAND = ["aibrix_runtime", "--port", str(_RUNTIME_PORT)]
_PROMETHEUS_PATH = "/metrics"
_PROMETHEUS_PORT = "8000"
_PROMETHEUS_SCRAPE = "true"
_DEFAULT_MIN_REPLICAS = "1"
_DSHM_VOLUME_NAME = "dshm"
_DSHM_MOUNT_PATH = "/dev/shm"
_DSHM_SIZE_LIMIT = "4Gi"
_MODEL_VOLUME_NAME = "model-hostpath"
_MODEL_MOUNT_PATH = "/models"


class DeploymentManifestRenderer(_RendererSupport):
    def render(
        self,
        template_name: str,
        profile_name: Optional[str] = None,
        deployment_name: Optional[str] = None,
        template_version: Optional[str] = None,
        replicas: Optional[int] = None,
        gpu_type: Optional[str] = None,
    ) -> Dict[str, Dict[str, Any]]:
        template = self._resolve_template(template_name, template_version)
        self._validate_template(template)
        # only require profile if model download is neeeded
        profile = (
            self._resolve_profile(profile_name)
            if self._needs_model_download(template)
            else None
        )

        # deployment_name use template.name + random 8hex
        deployment_name = deployment_name or f"{template.name}-{uuid.uuid4().hex[:8]}"
        replica_count = int(replicas or 1)
        port = self._resolve_engine_port(template)

        deployment = self._system_base(
            template,
            deployment_name,
            replica_count,
            port,
            profile,
        )
        self._apply_template(deployment, template)
        service = self._build_service(deployment, template)
        return {"deployment": deployment, "service": service}

    # ── Layer 1: system base ────────────────────────────────────────────────

    def _system_base(
        self,
        template: ModelDeploymentTemplate,
        deployment_name: str,
        replicas: int,
        port: int,
        profile: Optional[BatchProfile],
    ) -> Dict[str, Any]:
        labels = self._base_labels(template, deployment_name, port)
        volumes = [
            {
                "name": _DSHM_VOLUME_NAME,
                "emptyDir": {
                    "medium": "Memory",
                    "sizeLimit": _DSHM_SIZE_LIMIT,
                },
            }
        ]
        if self._needs_model_download(template):
            # TODO: Hardcoding the host path to /root/models makes the deployment manifest
            # dependent on a specific node filesystem layout, which may not be portable.
            # Consider making this base path configurable via the InfrastructureContext or BatchProfile.
            volumes.append(
                {
                    "name": _MODEL_VOLUME_NAME,
                    "hostPath": {
                        "path": f"/root{_MODEL_MOUNT_PATH}",
                        "type": "DirectoryOrCreate",
                    },
                }
            )
        return {
            "apiVersion": "apps/v1",
            "kind": "Deployment",
            "metadata": {
                "labels": labels["deployment"],
                "name": deployment_name,
                "namespace": "default",
            },
            "spec": {
                "replicas": replicas,
                "selector": {"matchLabels": labels["selector"]},
                "template": {
                    "metadata": {
                        "labels": labels["pod"],
                        "annotations": self._prometheus_annotations(),
                    },
                    "spec": {
                        "containers": (
                            [self._runtime_container(template, port)]
                            if self._needs_runtime_sidecar(template)
                            else []
                        ),
                        "initContainers": self._init_containers(template, profile),
                        "terminationGracePeriodSeconds": 300,
                        "volumes": volumes,
                    },
                },
            },
        }

    def _runtime_container(
        self,
        template: ModelDeploymentTemplate,
        engine_port: int,
    ) -> Dict[str, Any]:
        return {
            "name": _RUNTIME_CONTAINER_NAME,
            "image": _RUNTIME_IMAGE,
            "command": _RUNTIME_COMMAND,
            "env": [
                {
                    "name": "INFERENCE_ENGINE",
                    "value": template.spec.engine.type.value,
                },
                {
                    "name": "INFERENCE_ENGINE_ENDPOINT",
                    "value": f"http://localhost:{engine_port}",
                },
            ],
            "ports": [{"containerPort": _RUNTIME_PORT, "protocol": "TCP"}],
            "livenessProbe": {
                "httpGet": {"path": "/healthz", "port": _RUNTIME_PORT},
                "initialDelaySeconds": 3,
                "periodSeconds": 2,
            },
            "readinessProbe": {
                "httpGet": {"path": "/ready", "port": _RUNTIME_PORT},
                "initialDelaySeconds": 5,
                "periodSeconds": 10,
            },
        }

    def _init_containers(
        self, template: ModelDeploymentTemplate, profile: Optional[BatchProfile]
    ) -> list[Dict[str, Any]]:
        if not self._needs_model_download(template):
            return []
        if profile is None:
            raise ValueError("profile is required for remote model downloads")

        return [
            {
                "name": "init-model",
                "image": _RUNTIME_IMAGE,
                "command": [
                    "aibrix_download",
                    "--model-uri",
                    template.spec.model_source.uri,
                    "--local-dir",
                    f"{_MODEL_MOUNT_PATH}/",
                ],
                "env": build_downloader_env(template, profile),
                "volumeMounts": [
                    {
                        "mountPath": _MODEL_MOUNT_PATH,
                        "name": _MODEL_VOLUME_NAME,
                    }
                ],
            }
        ]

    # ── Layer 2: template ──────────────────────────────────────────────────

    def _apply_template(
        self, manifest: Dict[str, Any], template: ModelDeploymentTemplate
    ) -> Dict[str, Any]:
        """Add the engine container based on the template spec."""
        template_for_engine = (
            self._localize_model_source(template)
            if self._needs_model_download(template)
            else template
        )
        port = self._resolve_engine_port(template_for_engine)
        engine_container = self._build_engine_container(template_for_engine, port)
        volumes = engine_container.setdefault("volumeMounts", [])
        volumes.append({"name": _DSHM_VOLUME_NAME, "mountPath": _DSHM_MOUNT_PATH})
        if self._needs_model_download(template):
            volumes.append(
                {
                    "name": _MODEL_VOLUME_NAME,
                    "mountPath": _MODEL_MOUNT_PATH,
                }
            )

        # Append engine container to the pod spec containers list.
        containers = manifest["spec"]["template"]["spec"]["containers"]
        containers.append(engine_container)

        return manifest

    # ── service ──────────────────────────────────────────────────

    def _build_service(
        self,
        deployment: Dict[str, Any],
        template: ModelDeploymentTemplate,
    ) -> Dict[str, Any]:
        service_name = str(deployment["metadata"]["labels"][_AIBRIX_MODEL_NAME_KEY])
        return {
            "apiVersion": "v1",
            "kind": "Service",
            "metadata": {
                "labels": {"prometheus-discovery": "true"},
                "annotations": self._prometheus_annotations(),
                "name": service_name,
                "namespace": deployment["metadata"].get("namespace"),
            },
            "spec": {
                "selector": {_AIBRIX_MODEL_NAME_KEY: service_name},
                "ports": [
                    {
                        "name": "metrics",
                        "port": 8000,
                        "protocol": "TCP",
                        "targetPort": 8000,
                    }
                ],
                "type": "ClusterIP",
            },
        }

    def _base_labels(
        self,
        template: ModelDeploymentTemplate,
        deployment_name: str,
        port: int,
    ) -> Dict[str, Dict[str, str]]:
        # Use deployment as model name
        deployment_labels = {
            "adapter.model.aibrix.ai/enabled": "true",
            _AIBRIX_MODEL_NAME_KEY: deployment_name,
            "model.aibrix.ai/port": str(port),
            "model.aibrix.ai/min_replicas": _DEFAULT_MIN_REPLICAS,
        }
        selector_labels = {
            "adapter.model.aibrix.ai/enabled": "true",
            "app": deployment_name,
            _AIBRIX_MODEL_NAME_KEY: deployment_name,
        }
        return {
            "deployment": deployment_labels,
            "selector": selector_labels,
            "pod": deepcopy(selector_labels),
            "minimal": {"app": deployment_name},
        }

    def _prometheus_annotations(self) -> Dict[str, str]:
        return {
            "prometheus.io/path": _PROMETHEUS_PATH,
            "prometheus.io/port": _PROMETHEUS_PORT,
            "prometheus.io/scrape": _PROMETHEUS_SCRAPE,
        }

    def _needs_runtime_sidecar(self, template: ModelDeploymentTemplate) -> bool:
        return False

    def _localize_model_source(
        self,
        template: ModelDeploymentTemplate,
    ) -> ModelDeploymentTemplate:
        localized = template.model_copy(deep=True)
        localized.spec.model_source.uri = (
            f"{_MODEL_MOUNT_PATH}/{infer_model_name(template.spec.model_source.uri)}"
        )
        return localized

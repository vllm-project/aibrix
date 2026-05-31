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

"""ModelDeploymentTemplate and BatchProfile schema, registries, loaders."""

from .registry import (
    DEFAULT_NAMESPACE,
    PROFILES_CONFIGMAP_NAME,
    PROFILES_DATA_KEY,
    TEMPLATES_CONFIGMAP_NAME,
    TEMPLATES_DATA_KEY,
    K8sConfigMapSource,
    LoadError,
    LocalFileSource,
    ProfileRegistry,
    TemplateRegistry,
    TemplateSource,
    k8s_profile_registry,
    k8s_template_registry,
    local_profile_registry,
    local_template_registry,
)
from .schema import (
    SCHEMA_API_VERSION,
    AcceleratorSpec,
    BatchProfile,
    BatchProfileList,
    BatchProfileSpec,
    CompletionWindowOption,
    DeploymentMode,
    EngineArgsSpec,
    EngineInvocation,
    EngineSpec,
    EngineType,
    Interconnect,
    KvCacheQuantization,
    MetastoreBackend,
    MetastoreSpec,
    ModelDeploymentTemplate,
    ModelDeploymentTemplateList,
    ModelDeploymentTemplateSpec,
    ModelSourceSpec,
    ModelSourceType,
    ParallelismSpec,
    Priority,
    ProfileOverridesSpec,
    QuantizationSpec,
    QuotaSpec,
    ResolvedJobSpec,
    RetryPolicy,
    SchedulingSpec,
    StorageBackend,
    StorageSpec,
    TemplateOverridesSpec,
    TemplateStatus,
    WeightQuantization,
)

__all__ = [
    "SCHEMA_API_VERSION",
    # Registry
    "DEFAULT_NAMESPACE",
    "PROFILES_CONFIGMAP_NAME",
    "PROFILES_DATA_KEY",
    "TEMPLATES_CONFIGMAP_NAME",
    "TEMPLATES_DATA_KEY",
    "K8sConfigMapSource",
    "LoadError",
    "LocalFileSource",
    "ProfileRegistry",
    "TemplateRegistry",
    "TemplateSource",
    "k8s_profile_registry",
    "k8s_template_registry",
    "local_profile_registry",
    "local_template_registry",
    # Template
    "AcceleratorSpec",
    "DeploymentMode",
    "EngineArgsSpec",
    "EngineInvocation",
    "EngineSpec",
    "EngineType",
    "Interconnect",
    "KvCacheQuantization",
    "ModelDeploymentTemplate",
    "ModelDeploymentTemplateList",
    "ModelDeploymentTemplateSpec",
    "MetastoreBackend",
    "MetastoreSpec",
    "ModelSourceSpec",
    "ModelSourceType",
    "ParallelismSpec",
    "QuantizationSpec",
    "TemplateStatus",
    "WeightQuantization",
    # Profile
    "BatchProfile",
    "BatchProfileList",
    "BatchProfileSpec",
    "CompletionWindowOption",
    "Priority",
    "QuotaSpec",
    "RetryPolicy",
    "SchedulingSpec",
    "StorageBackend",
    "StorageSpec",
    # Overrides + resolved
    "TemplateOverridesSpec",
    "ProfileOverridesSpec",
    "ResolvedJobSpec",
]

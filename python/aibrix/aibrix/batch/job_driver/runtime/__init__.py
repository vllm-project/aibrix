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
"""Runtime: the single axis of the job lifecycle — *where compute lives*.

``base`` defines the contract (``Runtime`` / ``RuntimeBase``), the no-provision
runtimes (``ExternalRuntime`` / ``NoopRuntime``), and the string-keyed registry. A
backend is one module per provider that registers a factory under its
``ComputeProvider`` key:

    k8s_deployment  -> "Kubernetes"
    k8s_job         -> "KubernetesJob"
    lambda_cloud    -> "LambdaCloud"
    runpod          -> "RunPod"

These re-exports keep ``from aibrix.batch.job_driver.runtime import X`` stable.
"""

from aibrix.batch.job_driver.runtime.base import (
    _RUNTIME_FACTORIES,
    AsyncRuntimeSession,
    Completion,
    Endpoint,
    ExternalRuntime,
    NoopRuntime,
    Runtime,
    RuntimeBase,
    create_runtime,
    register_runtime,
    registered_runtimes,
)

__all__ = [
    "AsyncRuntimeSession",
    "Completion",
    "Endpoint",
    "ExternalRuntime",
    "NoopRuntime",
    "Runtime",
    "RuntimeBase",
    "create_runtime",
    "register_runtime",
    "registered_runtimes",
    "_RUNTIME_FACTORIES",
]

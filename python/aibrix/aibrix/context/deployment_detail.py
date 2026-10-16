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

"""Deployment detail provider registry"""

from typing import Any, Dict, Optional, Protocol, runtime_checkable

from aibrix.batch.job_entity import BatchJob
from aibrix.context.infra import InfrastructureContext


@runtime_checkable
class DeploymentDetailProvider(Protocol):
    """Side-channel deployment detail query interface.

    Each backend implements this to return environment-specific deployment
    info. The API layer wraps the returned dict into a DeploymentDetail
    (open envelope) and serializes it to JSON for the caller.
    """

    async def get_deployment_detail(
        self, context: InfrastructureContext, job: BatchJob
    ) -> Optional[Dict[str, Any]]:
        """Query deployment detail for a batch job.

        Returns a dict (not a typed model) so different backends can return
        completely different structures. Returns None on failure or when no
        info is available.
        """
        ...


# --- Registry (self-registration, keyed by runtime_target) ---
#
# Each provider module calls register_deployment_detail_provider(key, provider)
# at import time. The key is the runtime_target string (e.g. "Kubernetes").
# A single provider can register under multiple keys. Downstream backends
# register in their own modules — no edits to shared code required.

_DEPLOYMENT_DETAIL_PROVIDER_REGISTRY: Dict[str, DeploymentDetailProvider] = {}


def register_deployment_detail_provider(
    key: str, provider: DeploymentDetailProvider
) -> None:
    """Register a provider under a runtime_target key. Last write wins."""
    _DEPLOYMENT_DETAIL_PROVIDER_REGISTRY[key] = provider


def get_deployment_detail_provider(
    runtime_target: str,
) -> Optional[DeploymentDetailProvider]:
    """Look up a provider by runtime_target. Returns None if not found."""
    return _DEPLOYMENT_DETAIL_PROVIDER_REGISTRY.get(runtime_target)

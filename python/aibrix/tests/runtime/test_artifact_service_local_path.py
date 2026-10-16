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

import pytest

from aibrix.openapi.protocol import (
    LoadLoraAdapterRequest,
    LoadLoraAdapterRuntimeRequest,
)
from aibrix.runtime.artifact_service import ArtifactDelegationService


class _RecordingEngine:
    """Engine stand-in that records the load requests it receives."""

    def __init__(self):
        self.load_requests = []

    async def load_lora_adapter(self, request: LoadLoraAdapterRequest) -> str:
        self.load_requests.append(request)
        return "loaded"


@pytest.mark.asyncio
@pytest.mark.parametrize("artifact_url", ["/models/my-lora", "/models/my-lora/"])
async def test_local_path_adapter_is_loaded_from_its_path(tmp_path, artifact_url):
    """The controller's webhook accepts an artifactURL that starts with ``/``
    (an adapter already on a mounted volume) and sends it to the runtime
    unchanged. There is nothing to download: the engine loads it in place."""
    service = ArtifactDelegationService(local_dir=str(tmp_path))
    engine = _RecordingEngine()
    request = LoadLoraAdapterRuntimeRequest(
        lora_name="my-lora", artifact_url=artifact_url
    )

    response = await service.load_adapter_with_delegation(request, engine)

    assert response.status == "success", response.message
    assert response.local_path == artifact_url
    assert [(r.lora_name, r.lora_path) for r in engine.load_requests] == [
        ("my-lora", artifact_url)
    ]
    assert list(tmp_path.iterdir()) == []

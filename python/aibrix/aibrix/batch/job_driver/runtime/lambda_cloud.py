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
"""LambdaCloud runtime

Registers under the ``LambdaCloud`` provider key so selection works end-to-end;
the actual lease-a-VM + SSH-launch mechanics are not implemented yet. ``provision``
raises so a job routed here fails clearly instead of silently doing nothing.
"""

from __future__ import annotations

from typing import Any

from aibrix.batch.job_driver.runtime import RuntimeBase, register_runtime
from aibrix.batch.job_entity import BatchJob


class LambdaCloudRuntime(RuntimeBase):
    """Placeholder for the LambdaCloud backend (not implemented yet)."""

    provisions = True

    async def _provision(self, job: BatchJob, job_id: str) -> Any:
        raise NotImplementedError("LambdaCloud runtime is not implemented yet")


register_runtime("LambdaCloud", lambda **_: LambdaCloudRuntime())

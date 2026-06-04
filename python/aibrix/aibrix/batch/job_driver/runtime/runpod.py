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
"""RunPod runtime: leases an SSH box (RM) and launches vLLM over SSH."""

from __future__ import annotations

from aibrix.batch.job_driver.runtime import register_runtime
from aibrix.batch.job_driver.runtime.ssh_launch import (
    SSHConnInfo,
    SSHLaunchRuntime,
    build_launch_command,
)


class RunPodRuntime(SSHLaunchRuntime):
    """RunPod uses the shared SSH-launch flow; conn info comes from RM via
    runtime.options (host = publicIp, ssh_port = portMappings['22'],
    http_base_url = proxy URL). The pod image has vLLM preinstalled, so it
    launches the vLLM binary directly."""

    def _launch_command(self, info: SSHConnInfo) -> str:
        return build_launch_command(info)


register_runtime(
    "RunPod",
    lambda *, job=None, context=None, entity_manager=None, **_: RunPodRuntime(),
)

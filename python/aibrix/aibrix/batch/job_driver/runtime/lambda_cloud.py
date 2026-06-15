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
"""LambdaCloud runtime: leases a bare GPU VM (RM) and runs the vLLM container.

Unlike RunPod (whose pod IS the vLLM container), a LambdaCloud instance is a
bare Ubuntu+CUDA VM. Rather than pip-installing vLLM onto the host Python — which
forces vLLM/torch/transformers to resolve against the host and breaks across
driver versions — we run the model-template's vLLM **container** via ``docker``
(Lambda ships Docker + the NVIDIA Container Toolkit). The container carries a
self-consistent environment matched to its own CUDA, exactly like RunPod.

The container binds to the VM's localhost only; the control plane reaches it
over an SSH tunnel (Lambda's firewall denies inbound by default), so the vLLM
endpoint is never exposed on the public internet.
"""

from __future__ import annotations

import shlex
from typing import List

from aibrix.batch.job_driver.runtime import register_runtime
from aibrix.batch.job_driver.runtime.ssh_launch import SSHConnInfo, SSHLaunchRuntime

# Container name so teardown can target it deterministically.
CONTAINER_NAME = "aibrix-vllm"

# Fallback serving image when the job/options carry none (model template should
# normally supply it). Lambda's current driver runs the latest vLLM image.
DEFAULT_IMAGE = "vllm/vllm-openai:latest"


class LambdaCloudRuntime(SSHLaunchRuntime):
    """LambdaCloud runtime: leases a bare VM (RM) and ``docker run``s the vLLM
    serving image over SSH. The VM's default login user is ``ubuntu``."""

    # Lambda VMs log in as ubuntu, not root.
    default_ssh_user: str = "ubuntu"

    # The vLLM container binds the VM's 127.0.0.1:8000 and Lambda's firewall
    # denies inbound, so dispatch goes through an SSH local port-forward the
    # runtime opens over its own connection (no public exposure).
    uses_local_tunnel: bool = True

    def __init__(self, *, default_image: str = DEFAULT_IMAGE, **kwargs) -> None:
        super().__init__(**kwargs)
        self._default_image = default_image

    def _bootstrap_commands(self, info: SSHConnInfo) -> List[str]:
        """Verify Docker is usable before launch. Lambda ships Docker + the
        NVIDIA Container Toolkit, but the ``ubuntu`` user is not in the docker
        group, so commands run under (passwordless) sudo."""
        return ["sudo docker info >/dev/null 2>&1"]

    def _launch_command(self, info: SSHConnInfo) -> str:
        """Run the vLLM container, GPUs attached, bound to the VM's localhost:8000
        (reached via the SSH tunnel). Image comes from the model template. Uses
        sudo (ubuntu is not in the docker group on Lambda's stock image)."""
        image = info.image or self._default_image
        extra = " ".join(shlex.quote(a) for a in info.vllm_args)
        return (
            f"sudo docker rm -f {CONTAINER_NAME} >/dev/null 2>&1 || true; "
            f"sudo docker run -d --name {CONTAINER_NAME} --gpus all "
            f"-p 127.0.0.1:8000:8000 {shlex.quote(image)} "
            f"--model {shlex.quote(info.model)} --host 0.0.0.0 --port 8000 {extra}"
        ).strip()

    def _teardown_command(self) -> str:
        return f"sudo docker rm -f {CONTAINER_NAME} >/dev/null 2>&1 || true"


register_runtime(
    "LambdaCloud",
    lambda *, job=None, context=None, entity_manager=None, **_: LambdaCloudRuntime(),
)

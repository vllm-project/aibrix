# Copyright 2026 The Aibrix Team.
# Licensed under the Apache License, Version 2.0 (see repo LICENSE).
"""Shared SSH-launch runtime for bare-box GPU providers (RunPod, LambdaCloud).

RM leases an SSH-reachable box; this runtime SSHes in, launches `vllm serve`,
polls /health, and dispatches over the box's HTTP endpoint. The box itself is
released by RM, not here.
"""

from __future__ import annotations

import abc
import asyncio
import os
import shlex
from dataclasses import dataclass, field
from typing import Any, List, Optional

import httpx

from aibrix.batch.client.sources.static import GatewayEndpointSource
from aibrix.batch.job_driver.runtime import Endpoint, RuntimeBase
from aibrix.batch.job_entity import BatchJob


@dataclass(slots=True)
class SSHConnInfo:
    host: str
    ssh_port: int
    ssh_user: str
    http_base_url: str
    model: str
    vllm_args: List[str] = field(default_factory=list)
    # Serving container image (from the model template). Used by container-based
    # launches (LambdaCloud `docker run`); ignored by RunPod (its pod already is
    # the vLLM container and serves the binary directly).
    image: Optional[str] = None


def parse_conn_info(opts: dict, *, default_ssh_user: str = "root") -> SSHConnInfo:
    """Build SSHConnInfo from job.spec.aibrix.runtime.options.

    ``ssh_user`` is taken from options (RM surfaces it on the ProvisionResult);
    when absent it falls back to ``default_ssh_user`` (root for RunPod's
    container, "ubuntu" for LambdaCloud's bare VM)."""
    host = opts.get("host")
    http_base_url = opts.get("http_base_url")
    model = opts.get("model")
    if not host or not http_base_url or not model:
        raise ValueError(
            "ssh-launch runtime requires 'host', 'http_base_url', and 'model' options"
        )
    return SSHConnInfo(
        host=host,
        ssh_port=int(opts["ssh_port"]) if opts.get("ssh_port") is not None else 22,
        ssh_user=opts.get("ssh_user") or default_ssh_user,
        http_base_url=http_base_url,
        model=model,
        vllm_args=list(opts.get("vllm_args") or []),
        image=opts.get("image"),
    )


@dataclass(slots=True)
class SSHHandle:
    info: SSHConnInfo
    conn: Any = None  # asyncssh.SSHClientConnection
    # Effective base_url the control plane dispatches to. Equals info.http_base_url
    # for directly-reachable endpoints (RunPod proxy); for tunnelled providers
    # (LambdaCloud) it's the local end of an SSH port-forward.
    endpoint_url: Optional[str] = None
    listener: Any = None  # asyncssh SSHListener for the local port-forward


def build_launch_command(info: SSHConnInfo, *, log_path: str = "/tmp/vllm.log") -> str:
    """Background vLLM launch command. Detaches so the SSH session can close."""
    args = " ".join(shlex.quote(a) for a in info.vllm_args)
    serve = (
        f"vllm serve {shlex.quote(info.model)} " f"--host 0.0.0.0 --port 8000 {args}"
    ).strip()
    # Ensure pip-user installs (~/.local/bin, e.g. LambdaCloud bare VMs) are on
    # PATH; harmless when vllm lives in /usr/local/bin (RunPod image). asyncssh
    # runs a non-login shell, so ~/.profile's PATH tweak is not applied.
    return (
        'export PATH="$HOME/.local/bin:$PATH"; '
        f"nohup {serve} > {shlex.quote(log_path)} 2>&1 &"
    )


class SSHLaunchRuntime(RuntimeBase, abc.ABC):
    """Abstract dispatch-shape runtime: SSH in, launch a server, wait /health,
    dispatch. NOT usable on its own — instantiate a concrete provider subclass
    (RunPodRuntime / LambdaCloudRuntime). ``_launch_command`` is abstract so each
    provider must declare HOW it starts the server (RunPod runs the preinstalled
    vLLM binary; LambdaCloud ``docker run``s the template image), which both
    blocks direct use and keeps the per-provider launch strategy explicit.
    """

    provisions = True

    # Default SSH login user when options omit ssh_user. RunPod's container
    # logs in as root; LambdaCloud's bare VM overrides this to "ubuntu".
    default_ssh_user: str = "root"

    # When True, the server is bound to the box's localhost (not publicly
    # reachable), so the runtime opens an SSH local port-forward over its own
    # connection and dispatches through it. RunPod leaves this False (its proxy
    # URL is directly reachable); LambdaCloud sets it True (firewall denies
    # inbound + the vLLM container binds 127.0.0.1).
    uses_local_tunnel: bool = False

    # Internal port the server listens on inside the box (tunnel destination).
    server_port: int = 8000

    def __init__(
        self,
        *,
        ready_timeout_s: float = 1800.0,
        poll_interval_s: float = 5.0,
        private_key_path: Optional[str] = None,
        ssh_connect_timeout_s: float = 120.0,
        ssh_connect_retry_interval_s: float = 5.0,
    ) -> None:
        self._ready_timeout_s = ready_timeout_s
        self._poll_interval_s = poll_interval_s
        # Ephemeral boxes: read the disposable private key from env by default.
        self._private_key_path = private_key_path or os.environ.get(
            "AIBRIX_BATCH_SSH_KEY_FILE"
        )
        self._ssh_connect_timeout_s = ssh_connect_timeout_s
        self._ssh_connect_retry_interval_s = ssh_connect_retry_interval_s

    def _options(self, job: BatchJob) -> dict:
        aibrix = getattr(job.spec, "aibrix", None)
        runtime = getattr(aibrix, "runtime", None) if aibrix else None
        return dict(getattr(runtime, "options", {}) or {})

    def _base_url(self, handle: SSHHandle) -> str:
        return (handle.endpoint_url or handle.info.http_base_url).rstrip("/")

    async def _wait_ready(self, handle: SSHHandle) -> None:
        url = self._base_url(handle) + "/health"
        loop = asyncio.get_event_loop()
        deadline = loop.time() + self._ready_timeout_s
        async with httpx.AsyncClient(timeout=10.0) as client:
            while True:
                try:
                    resp = await client.get(url)
                    if resp.status_code == 200:
                        return
                except Exception:
                    pass
                if loop.time() >= deadline:
                    raise TimeoutError(
                        f"vLLM not ready at {url} after {self._ready_timeout_s}s"
                    )
                await asyncio.sleep(self._poll_interval_s)

    async def _connect(self, handle: SSHHandle) -> Endpoint:
        # Origin only (no path); HttpChannel does urljoin(base_url, path).
        # Gateway (capacity>1) so the engine drives vLLM's internal batching.
        return Endpoint(
            source=GatewayEndpointSource(self._base_url(handle)),
            model_name=handle.info.model,
        )

    async def _connect_ssh(self, connect_kwargs: dict) -> Any:
        import asyncssh

        loop = asyncio.get_event_loop()
        deadline = loop.time() + self._ssh_connect_timeout_s
        while True:
            try:
                return await asyncssh.connect(**connect_kwargs)
            except (OSError, TimeoutError, asyncssh.Error):
                if loop.time() >= deadline:
                    raise
                await asyncio.sleep(self._ssh_connect_retry_interval_s)

    @abc.abstractmethod
    def _launch_command(self, info: SSHConnInfo) -> str:
        """The shell command (run over SSH) that starts the server, detached.

        Abstract: each provider declares its launch strategy. RunPod runs the
        preinstalled vLLM binary (``build_launch_command``); LambdaCloud
        ``docker run``s the template image."""
        raise NotImplementedError

    def _teardown_command(self) -> str:
        """The shell command (run over SSH) that stops the server. Default kills
        the vLLM process; container providers override to remove the container."""
        return "pkill -f 'vllm serve' || true"

    def _bootstrap_commands(self, info: SSHConnInfo) -> List[str]:
        """Commands to run over SSH before launching vLLM.

        The default is empty: RunPod boxes use a vLLM Docker image that already
        has vLLM installed, so no bootstrap is needed. Bare-VM providers (e.g.
        LambdaCloud) override this to install vLLM before `vllm serve` runs.
        """
        return []

    async def _provision(self, job: BatchJob, job_id: str) -> SSHHandle:
        info = parse_conn_info(
            self._options(job), default_ssh_user=self.default_ssh_user
        )
        connect_kwargs: dict = {
            "host": info.host,
            "port": info.ssh_port,
            "username": info.ssh_user,
            "known_hosts": None,  # ephemeral boxes: no stable host key (TOFU/off)
            "timeout": 15.0,
        }
        if self._private_key_path:
            connect_kwargs["client_keys"] = [self._private_key_path]
        conn = await self._connect_ssh(connect_kwargs)
        handle = SSHHandle(info=info, conn=conn)
        # Bootstrap first (bare VMs install vLLM here); RunPod returns []. These
        # are blocking so vLLM is present before `vllm serve` is launched.
        for cmd in self._bootstrap_commands(info):
            await conn.run(cmd, check=True)
        await conn.run(self._launch_command(info), check=True)
        if self.uses_local_tunnel:
            # Server is bound to the box's localhost; forward a local port over
            # this SSH connection and dispatch through it (no public exposure).
            listener = await conn.forward_local_port(
                "127.0.0.1", 0, "127.0.0.1", self.server_port
            )
            handle.listener = listener
            handle.endpoint_url = f"http://127.0.0.1:{listener.get_port()}"
        else:
            handle.endpoint_url = info.http_base_url
        return handle

    async def _teardown(self, handle: Optional[SSHHandle]) -> None:
        if handle is None or handle.conn is None:
            return
        try:
            await handle.conn.run(self._teardown_command(), check=False)
        finally:
            if handle.listener is not None:
                handle.listener.close()
            handle.conn.close()

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

"""Engine lifecycle manager for the aibrix-runtime sidecar.

This is the engine side of the engine<->control-plane protocol. The ModelClaim
controller drives this runtime over HTTP to:

  * activate   a model as its own kvcached-enabled engine process on the GPU,
  * deactivate it by stopping the engine process, and
  * list the models currently resident on the pod.

The engine launcher is pluggable so the agent runs and is fully testable
without a GPU (MockEngineLauncher).
"""

from __future__ import annotations

import logging
import os
import re
import socket
import sys
import threading
from abc import ABC, abstractmethod
from dataclasses import dataclass, field
from typing import Dict, List, Optional


def sanitize_ipc_name(name: str) -> str:
    """Match kvcached's KVCACHED_IPC_NAME normalization: characters outside
    [A-Za-z0-9_-] become '-'. Verified on real hardware that kvcached rewrites
    e.g. "kvc_qwen3-0.6b" to "kvc_qwen3-0-6b"; without sanitizing on our side,
    kvctl operations would target a different segment than the engine created.
    """
    return re.sub(r"[^A-Za-z0-9_-]", "-", name)


logger = logging.getLogger(__name__)


@dataclass
class ModelInstance:
    """One engine process serving a model on the pod."""

    model_name: str
    port: int
    ipc_name: str
    engine: str = "vllm"
    phase: str = "active"
    pid: Optional[int] = None
    # Underlying process handle (None for mock); excluded from repr.
    proc: Optional[object] = field(default=None, repr=False)


# --------------------------------------------------------------------------- #
# Pluggable actuators
# --------------------------------------------------------------------------- #
class EngineLauncher(ABC):
    """Launches/stops an engine process for a model."""

    @abstractmethod
    def launch(
        self,
        inst: ModelInstance,
        artifact_url: str,
        engine_config: Optional[Dict],
        additional_config: Optional[Dict[str, str]],
    ) -> int:
        """Start the engine process and return its pid."""

    @abstractmethod
    def stop(self, inst: ModelInstance) -> None:
        """Terminate the engine process."""


# --------------------------------------------------------------------------- #
# Real implementations (require a GPU + kvcached + engine at runtime)
# --------------------------------------------------------------------------- #
def kvcached_env(
    ipc_name: str, extra: Optional[Dict[str, str]] = None
) -> Dict[str, str]:
    """Build the environment that activates kvcached for an engine process.

    A distinct KVCACHED_IPC_NAME per model is mandatory: the default name keys
    on the process group id, so without it agent-spawned children would collide
    on one /dev/shm segment and defeat per-model KV budgets.
    """
    env = dict(os.environ)
    env["ENABLE_KVCACHED"] = "true"
    env["KVCACHED_AUTOPATCH"] = "1"
    env["KVCACHED_IPC_NAME"] = ipc_name
    if extra:
        env.update(extra)
    return env


# --------------------------------------------------------------------------- #
# Node weight-cache markers + kvcached /dev/shm accounting
# --------------------------------------------------------------------------- #
def weight_cache_dir() -> str:
    """The node-shared weight cache directory (the warm pods on a node mount the
    same host dir here, so weights downloaded by one pod serve them all)."""
    return os.environ.get(
        "AIBRIX_WEIGHT_CACHE_DIR", os.path.expanduser("~/.cache/huggingface")
    )


def write_cache_marker(
    model_name: str, artifact_url: str, cache_dir: Optional[str] = None
) -> Optional[str]:
    """Record that a served model's weights live in the node cache.

    Markers live under ``<cache>/.aibrix/served/<served-name>.json`` and OUTLIVE
    the engine instance: the weights stay on the node's disk after deactivation,
    and the node-state reporter scans these markers to advertise the node as
    "warm" for the model — which is what makes locality-aware placement prefer
    this node when the model is claimed again. Best-effort; returns the marker
    path or None.
    """
    import json

    base = cache_dir or weight_cache_dir()
    try:
        marker_dir = os.path.join(base, ".aibrix", "served")
        os.makedirs(marker_dir, exist_ok=True)
        path = os.path.join(marker_dir, f"{model_name}.json")
        with open(path, "w") as f:
            json.dump({"model_name": model_name, "artifact_url": artifact_url}, f)
        return path
    except Exception as exc:  # cache may be read-only/absent; never block activate
        logger.warning("could not write cache marker for %s: %s", model_name, exc)
        return None


def engine_ready(port: int) -> bool:
    """Whether the engine on this port can serve, via a short /health probe.
    Connection-refused (engine still starting) and timeouts read as not-ready.
    """
    try:
        import httpx

        resp = httpx.get(f"http://127.0.0.1:{port}/health", timeout=1.0)
        return resp.status_code == 200
    except Exception:
        return False


def instance_ready(inst: "ModelInstance") -> bool:
    """Whether a model instance is ready to serve. A mock/handle-less instance
    (no real engine process) is ready by definition; a dead one is not; a live
    real engine is probed via /health. The controller gates routability on this:
    a model's warm-pod annotation stays at the non-routable marker (port 0)
    until ready, so requests never route to an engine that is still
    booting/compiling."""
    proc = inst.proc
    poll = getattr(proc, "poll", None)
    if proc is None or poll is None:
        return True  # mock launcher: nothing to probe
    if poll() is not None:
        return False  # process exited
    return engine_ready(inst.port)


def read_kv_segment(ipc_name: str, shm_dir: str = "/dev/shm"):
    """Read a model's kvcached MemInfoStruct: 3 little-endian int64s
    (total_size, used_size, prealloc_size) at the start of /dev/shm/<ipc_name>
    (layout per kvcached/cli/utils.py MemInfoStruct). Returns the tuple, or
    None when the segment does not exist (mock engine / engine still starting).
    """
    import struct

    path = os.path.join(shm_dir, ipc_name)
    try:
        with open(path, "rb") as f:
            raw = f.read(24)
        if len(raw) < 24:
            return None
        return struct.unpack("<3q", raw)
    except OSError:
        return None


class SubprocessEngineLauncher(EngineLauncher):
    """Spawns a real vLLM/SGLang process with kvcached enabled, each in its own
    session (process group) so the kvcached IPC names do not collide."""

    def launch(self, inst, artifact_url, engine_config, additional_config):
        import subprocess

        model_path = _resolve_model_path(artifact_url)
        # Serve under the logical model name (not the artifact path) so the
        # gateway routes the claimed served name to this engine.
        if inst.engine == "sglang":
            cmd = [
                sys.executable,
                "-m",
                "sglang.launch_server",
                "--model-path",
                model_path,
                "--served-model-name",
                inst.model_name,
                "--host",
                "0.0.0.0",
                "--port",
                str(inst.port),
                "--enable-memory-saver",
            ]
        else:  # vllm
            cmd = [
                sys.executable,
                "-m",
                "vllm.entrypoints.openai.api_server",
                "--model",
                model_path,
                "--served-model-name",
                inst.model_name,
                "--host",
                "0.0.0.0",
                "--port",
                str(inst.port),
                "--enable-sleep-mode",
            ]
        env = kvcached_env(inst.ipc_name)
        env.setdefault("VLLM_SERVER_DEV_MODE", "1")  # enable /sleep, /wake_up
        for key, value in _engine_args(engine_config, additional_config).items():
            cmd.append(key)
            if value:
                cmd.append(value)

        logger.info(
            "launching %s engine for %s: %s",
            inst.engine,
            inst.model_name,
            " ".join(cmd),
        )
        proc = subprocess.Popen(cmd, env=env, start_new_session=True)
        inst.proc = proc
        return proc.pid

    def stop(self, inst):
        import signal
        import subprocess

        proc = inst.proc
        if isinstance(proc, subprocess.Popen) and proc.poll() is None:
            try:
                os.killpg(os.getpgid(proc.pid), signal.SIGTERM)
            except ProcessLookupError:
                pass


def _resolve_model_path(artifact_url: str) -> str:
    """Resolve a weight artifact URL to a path/id the engine can load.

    - Local paths and bare model names pass through unchanged.
    - ``huggingface://`` / ``hf://`` hand the bare repo id to the engine, which
      downloads from the Hugging Face Hub natively (no aibrix downloader needed).
    - Other remote stores (``s3://``, ``gcs://``, ``tos://``, ...) that the
      engine can't fetch are staged locally via the aibrix downloader, falling
      back to the raw path if the downloader/deps are unavailable.
    """
    if "://" not in artifact_url:
        return artifact_url
    scheme, rest = artifact_url.split("://", 1)
    if scheme in ("huggingface", "hf"):
        return rest
    try:
        from aibrix.downloader import download_model  # type: ignore

        return download_model(artifact_url)
    except Exception:  # pragma: no cover - depends on runtime deps
        return rest


def _engine_args(
    engine_config: Optional[Dict],
    additional_config: Optional[Dict[str, str]],
) -> Dict[str, str]:
    """Extract engine CLI args from the structured config.

    additional_config is kept only for compatibility with earlier test scripts
    that used engine-arg:<flag> keys before engine_config.args existed.
    """
    args: Dict[str, str] = {}
    if engine_config:
        args.update(engine_config.get("args") or {})
    for key, value in (additional_config or {}).items():
        if key.startswith("engine-arg:"):
            args[key[len("engine-arg:") :]] = value
    return args


# --------------------------------------------------------------------------- #
# Mock implementations (no GPU, no process, no network) for local testing
# --------------------------------------------------------------------------- #
class MockEngineLauncher(EngineLauncher):
    def __init__(self) -> None:
        self.launched: List[str] = []
        self.stopped: List[str] = []
        self._pid = 10000

    def launch(self, inst, artifact_url, engine_config, additional_config):
        self._pid += 1
        self.launched.append(inst.model_name)
        return self._pid

    def stop(self, inst):
        self.stopped.append(inst.model_name)


# --------------------------------------------------------------------------- #
# The agent
# --------------------------------------------------------------------------- #
class ModelRuntime:
    """Manages the set of kvcached-enabled engine processes on one pod."""

    def __init__(
        self,
        launcher: EngineLauncher,
        port_range: tuple = (20000, 21000),
    ) -> None:
        self._launcher = launcher
        self._models: Dict[str, ModelInstance] = {}
        self._lock = threading.RLock()
        self._port_lo, self._port_hi = port_range

    @staticmethod
    def _instance_alive(inst: ModelInstance) -> bool:
        """Whether the instance's engine process is still running. Instances
        without a process handle (mock launcher) count as alive."""
        proc = inst.proc
        poll = getattr(proc, "poll", None)
        if proc is None or poll is None:
            return True
        return poll() is None

    def activate(
        self,
        *,
        model_name: str,
        artifact_url: str,
        engine: str = "vllm",
        port: int = 0,
        ipc_name: str = "",
        engine_config: Optional[Dict] = None,
        additional_config: Optional[Dict[str, str]] = None,
    ) -> ModelInstance:
        with self._lock:
            existing = self._models.get(model_name)
            if existing is not None:
                if self._instance_alive(existing):
                    return existing  # idempotent
                # The engine died underneath us (observed live: a woken engine
                # left a defunct child while the agent kept advertising it,
                # wedging routing on a dead port). Drop the corpse and fall
                # through to a fresh launch so a re-activate self-heals.
                logger.warning(
                    "engine for %s (port %d) is dead; relaunching",
                    model_name,
                    existing.port,
                )
                self._models.pop(model_name, None)
            inst = ModelInstance(
                model_name=model_name,
                port=port or self._pick_port(),
                ipc_name=sanitize_ipc_name(ipc_name or f"kvc_{model_name}"),
                engine=engine,
            )
            inst.pid = self._launcher.launch(
                inst, artifact_url, engine_config, additional_config
            )
            inst.phase = "active"
            self._models[model_name] = inst
            # The engine downloads weights into the node-shared cache; the marker
            # makes that cache state visible to the node reporter (and survives
            # deactivation, so re-claims prefer this node).
            write_cache_marker(model_name, artifact_url)
            return inst

    def deactivate(self, model_name: str, mode: str = "stop") -> None:
        with self._lock:
            inst = self._models.get(model_name)
            if inst is None:
                return
            if mode != "stop":
                logger.info("deactivate mode %s is treated as stop", mode)
            self._launcher.stop(inst)
            self._models.pop(model_name, None)

    def list_models(self) -> List[ModelInstance]:
        """Resident instances, with dead engines reaped first so the listing
        (and everything built on it: node reporting, the reclaim loop, the
        controller's view) reflects reality rather than stale bookkeeping."""
        with self._lock:
            dead = [
                name
                for name, inst in self._models.items()
                if not self._instance_alive(inst)
            ]
            for name in dead:
                inst = self._models.pop(name)
                logger.warning("reaping dead engine for %s (port %d)", name, inst.port)
            return list(self._models.values())

    def snapshot_models(self) -> List[ModelInstance]:
        """Thread-safe read-only snapshot of resident instances (no reaping),
        so the metrics collector never mutates agent state on a scrape."""
        with self._lock:
            return list(self._models.values())

    def _pick_port(self) -> int:
        used = {m.port for m in self._models.values()}
        for port in range(self._port_lo, self._port_hi):
            if port in used:
                continue
            if self._port_free(port):
                return port
        raise RuntimeError("no free port available in agent range")

    @staticmethod
    def _port_free(port: int) -> bool:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
            return sock.connect_ex(("127.0.0.1", port)) != 0


# --------------------------------------------------------------------------- #
# Singleton wiring for the FastAPI runtime app
# --------------------------------------------------------------------------- #
_AGENT: Optional[ModelRuntime] = None


def get_model_runtime() -> ModelRuntime:
    """Return the process-wide ModelRuntime.

    Set AIBRIX_MODEL_RUNTIME_MOCK=1 to use the mock actuators (no GPU/process),
    which is what tests and dry-run deployments use.
    """
    global _AGENT
    if _AGENT is None:
        if os.environ.get("AIBRIX_MODEL_RUNTIME_MOCK") == "1":
            _AGENT = ModelRuntime(MockEngineLauncher())
        else:
            _AGENT = ModelRuntime(SubprocessEngineLauncher())
    return _AGENT

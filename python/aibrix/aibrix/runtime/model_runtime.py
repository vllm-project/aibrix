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
from datetime import datetime, timezone
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
    artifact_url: str = ""
    claim_ref: Optional[Dict[str, str]] = None
    phase: str = "active"
    pid: Optional[int] = None
    completed_operation_ids: Dict[str, List[str]] = field(
        default_factory=dict, repr=False
    )
    # Underlying process handle (None for mock); excluded from repr.
    proc: Optional[object] = field(default=None, repr=False)


@dataclass(frozen=True)
class RuntimeOperationResult:
    """Result of an idempotent runtime control operation."""

    model_name: str
    operation_id: str
    applied: bool
    phase: str


class ModelNotFoundError(LookupError):
    """Raised when a control request targets an absent runtime model."""


class UnsupportedModelControlError(RuntimeError):
    """Raised when an engine does not implement a requested lifecycle control."""


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

    @abstractmethod
    def sleep(self, inst: ModelInstance, level: int) -> None:
        """Put an engine into its supported sleep state."""

    @abstractmethod
    def wake(self, inst: ModelInstance) -> None:
        """Wake an engine that was previously put to sleep."""


class KVController(ABC):
    """Applies kvcached controls to a model's shared-memory segment."""

    @abstractmethod
    def set_limit(self, ipc_name: str, limit_bytes: int) -> None:
        """Set the kvcached limit for one normalized IPC segment."""


class KvctlController(KVController):
    """Production kvcached control plane backed by the kvctl CLI."""

    def set_limit(self, ipc_name: str, limit_bytes: int) -> None:
        import subprocess

        subprocess.run(
            ["kvctl", "limit", ipc_name, str(limit_bytes)],
            check=True,
            timeout=10,
        )


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


def cached_artifacts(cache_dir: Optional[str] = None) -> List[str]:
    """Return artifact URLs marked as resident in this runtime's shared cache.

    A marker is intentionally only a locality hint. A missing, malformed, or
    stale file must never make an otherwise valid placement fail.
    """
    import json
    from pathlib import Path

    marker_dir = Path(cache_dir or weight_cache_dir()) / ".aibrix" / "served"
    artifacts = set()
    try:
        for marker in marker_dir.glob("*.json"):
            try:
                data = json.loads(marker.read_text())
            except (OSError, ValueError):
                continue
            artifact_url = data.get("artifact_url")
            if isinstance(artifact_url, str) and artifact_url:
                artifacts.add(artifact_url)
    except OSError as exc:
        logger.debug("could not inspect model cache markers: %s", exc)
    return sorted(artifacts)


def gpu_memory_snapshots() -> List[Dict[str, object]]:
    """Read memory state for GPUs visible to this pod via NVML.

    The sidecar also runs in CPU-only test and development environments. NVML
    is therefore optional: lack of the module, driver, or visible device means
    the controller receives an empty accelerator list and falls back safely.
    """
    initialized = False
    try:
        import pynvml  # type: ignore[import-not-found]

        pynvml.nvmlInit()
        initialized = True
        snapshots = []
        for index in range(pynvml.nvmlDeviceGetCount()):
            handle = pynvml.nvmlDeviceGetHandleByIndex(index)
            info = pynvml.nvmlDeviceGetMemoryInfo(handle)
            device_id = pynvml.nvmlDeviceGetUUID(handle)
            if isinstance(device_id, bytes):
                device_id = device_id.decode()
            snapshots.append(
                {
                    "id": str(device_id),
                    "hbm_total_bytes": int(info.total),
                    "hbm_free_bytes": int(info.free),
                }
            )
        return snapshots
    except Exception as exc:
        logger.debug("NVML GPU observation is unavailable: %s", exc)
        return []
    finally:
        if initialized:
            try:
                pynvml.nvmlShutdown()  # type: ignore[name-defined]
            except Exception:
                pass


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
        marker_dir = os.path.abspath(os.path.join(base, ".aibrix", "served"))
        os.makedirs(marker_dir, exist_ok=True)
        path = os.path.abspath(os.path.join(marker_dir, f"{model_name}.json"))
        if os.path.commonpath([marker_dir, path]) != marker_dir:
            raise ValueError(f"path traversal detected in model name: {model_name!r}")
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


def _vllm_control_post(port: int, path: str) -> None:
    """Invoke a vLLM development lifecycle endpoint through localhost only."""
    import httpx

    response = httpx.post(f"http://127.0.0.1:{port}{path}", timeout=10.0)
    response.raise_for_status()


def instance_ready(inst: "ModelInstance") -> bool:
    """Whether a model instance is ready to serve. A mock/handle-less instance
    (no real engine process) is ready by definition; a dead one is not; a live
    real engine is probed via /health. The controller gates routability on this:
    a model's warm-pod annotation stays at the non-routable marker (port 0)
    until ready, so requests never route to an engine that is still
    booting/compiling."""
    if inst.phase == "sleeping":
        return False
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
            except OSError:
                pass

    def sleep(self, inst, level):
        _vllm_control_post(inst.port, f"/sleep?level={level}")

    def wake(self, inst):
        _vllm_control_post(inst.port, "/wake_up")


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


def _positive_engine_arg(args: Dict[str, str], name: str) -> int:
    raw = args.get(name, "1")
    try:
        value = int(raw)
    except (TypeError, ValueError) as exc:
        raise ValueError(f"{name} must be a positive integer") from exc
    if value < 1:
        raise ValueError(f"{name} must be a positive integer")
    return value


def vllm_parallelism(
    engine_config: Optional[Dict], additional_config: Optional[Dict[str, str]]
) -> int:
    """Return the fixed GPU group size required by vLLM TP and PP."""
    args = _engine_args(engine_config, additional_config)
    tensor = _positive_engine_arg(args, "--tensor-parallel-size")
    pipeline = _positive_engine_arg(args, "--pipeline-parallel-size")
    data = _positive_engine_arg(args, "--data-parallel-size")
    if data != 1:
        raise ValueError(
            f"--data-parallel-size={data} is unsupported by fixed topology pools"
        )
    return tensor * pipeline


def validate_vllm_parallelism(
    engine_config: Optional[Dict], additional_config: Optional[Dict[str, str]]
) -> None:
    """Require TP * PP to match the GPUs visible to this warm runtime pod."""
    parallelism = vllm_parallelism(engine_config, additional_config)
    visible_gpus = len(gpu_memory_snapshots())
    if visible_gpus == 0:
        if parallelism > 1:
            raise RuntimeError(
                "cannot validate vLLM TP/PP topology because no GPUs are visible to the runtime"
            )
        return
    if visible_gpus != parallelism:
        raise ValueError(
            f"vLLM parallelism {parallelism} must equal {visible_gpus} GPU(s) visible to runtime"
        )


# --------------------------------------------------------------------------- #
# Mock implementations (no GPU, no process, no network) for local testing
# --------------------------------------------------------------------------- #
class MockEngineLauncher(EngineLauncher):
    def __init__(self) -> None:
        self.launched: List[str] = []
        self.stopped: List[str] = []
        self.slept: List[tuple[str, int]] = []
        self.woken: List[str] = []
        self._pid = 10000

    def launch(self, inst, artifact_url, engine_config, additional_config):
        self._pid += 1
        self.launched.append(inst.model_name)
        return self._pid

    def stop(self, inst):
        self.stopped.append(inst.model_name)

    def sleep(self, inst, level):
        self.slept.append((inst.model_name, level))

    def wake(self, inst):
        self.woken.append(inst.model_name)


class MockKVController(KVController):
    """In-memory KV actuator for runtime mock mode and unit tests."""

    def __init__(self) -> None:
        self.limits: List[tuple[str, int]] = []

    def set_limit(self, ipc_name: str, limit_bytes: int) -> None:
        self.limits.append((ipc_name, limit_bytes))


# --------------------------------------------------------------------------- #
# The agent
# --------------------------------------------------------------------------- #
class ModelRuntime:
    """Manages the set of kvcached-enabled engine processes on one pod."""

    def __init__(
        self,
        launcher: EngineLauncher,
        port_range: tuple = (20000, 21000),
        kv_controller: Optional[KVController] = None,
    ) -> None:
        self._launcher = launcher
        self._kv_controller = kv_controller or KvctlController()
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
        claim_ref: Optional[Dict[str, str]] = None,
    ) -> ModelInstance:
        with self._lock:
            if engine == "vllm":
                validate_vllm_parallelism(engine_config, additional_config)
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
                artifact_url=artifact_url,
                claim_ref=dict(claim_ref) if claim_ref else None,
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

    def set_kv_limit(
        self, model_name: str, limit_bytes: int, *, operation_id: str
    ) -> RuntimeOperationResult:
        """Apply a kvcached limit once for an idempotent controller operation."""
        if limit_bytes < 0:
            raise ValueError("limit_bytes must be non-negative")
        if not operation_id:
            raise ValueError("operation_id must not be empty")
        with self._lock:
            inst = self._require_instance(model_name)
            if self._operation_completed(inst, "kv-limit", operation_id):
                return RuntimeOperationResult(
                    model_name=model_name,
                    operation_id=operation_id,
                    applied=False,
                    phase=inst.phase,
                )
            self._kv_controller.set_limit(inst.ipc_name, limit_bytes)
            self._remember_operation(inst, "kv-limit", operation_id)
            return RuntimeOperationResult(
                model_name=model_name,
                operation_id=operation_id,
                applied=True,
                phase=inst.phase,
            )

    def sleep(
        self, model_name: str, *, level: int, operation_id: str
    ) -> RuntimeOperationResult:
        """Put a vLLM model to sleep once for a controller operation ID."""
        if level not in (1, 2):
            raise ValueError("sleep level must be 1 or 2")
        if not operation_id:
            raise ValueError("operation_id must not be empty")
        with self._lock:
            inst = self._require_instance(model_name)
            if inst.engine != "vllm":
                raise UnsupportedModelControlError(
                    f"sleep is unsupported for engine {inst.engine!r}"
                )
            if self._operation_completed(inst, "sleep", operation_id):
                return RuntimeOperationResult(
                    model_name=model_name,
                    operation_id=operation_id,
                    applied=False,
                    phase=inst.phase,
                )
            if inst.phase == "sleeping":
                self._remember_operation(inst, "sleep", operation_id)
                return RuntimeOperationResult(
                    model_name=model_name,
                    operation_id=operation_id,
                    applied=False,
                    phase=inst.phase,
                )
            self._launcher.sleep(inst, level)
            inst.phase = "sleeping"
            self._remember_operation(inst, "sleep", operation_id)
            return RuntimeOperationResult(
                model_name=model_name,
                operation_id=operation_id,
                applied=True,
                phase=inst.phase,
            )

    def wake(self, model_name: str, *, operation_id: str) -> RuntimeOperationResult:
        """Wake a vLLM model once for a controller operation ID."""
        if not operation_id:
            raise ValueError("operation_id must not be empty")
        with self._lock:
            inst = self._require_instance(model_name)
            if inst.engine != "vllm":
                raise UnsupportedModelControlError(
                    f"wake is unsupported for engine {inst.engine!r}"
                )
            if self._operation_completed(inst, "wake", operation_id):
                return RuntimeOperationResult(
                    model_name=model_name,
                    operation_id=operation_id,
                    applied=False,
                    phase=inst.phase,
                )
            if inst.phase != "sleeping":
                self._remember_operation(inst, "wake", operation_id)
                return RuntimeOperationResult(
                    model_name=model_name,
                    operation_id=operation_id,
                    applied=False,
                    phase=inst.phase,
                )
            self._launcher.wake(inst)
            inst.phase = "active"
            self._remember_operation(inst, "wake", operation_id)
            return RuntimeOperationResult(
                model_name=model_name,
                operation_id=operation_id,
                applied=True,
                phase=inst.phase,
            )

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

    def snapshot(self) -> Dict[str, object]:
        """Return the sidecar's point-in-time scheduling observation.

        Instance bookkeeping is copied while holding the runtime lock. The
        potentially slow hardware, filesystem, and readiness reads happen
        afterwards so an observation never blocks activate/deactivate.
        """
        instances = self.list_models()
        models = []
        for inst in instances:
            segment = read_kv_segment(inst.ipc_name)
            total, used, prealloc = segment if segment else (0, 0, 0)
            models.append(
                {
                    "model_name": inst.model_name,
                    "artifact_url": inst.artifact_url,
                    "claim_ref": dict(inst.claim_ref) if inst.claim_ref else None,
                    "port": inst.port,
                    "ipc_name": inst.ipc_name,
                    "phase": inst.phase,
                    "ready": instance_ready(inst),
                    "kv_used_bytes": used + prealloc,
                    "kv_capacity_bytes": total,
                }
            )
        return {
            "observed_at": datetime.now(timezone.utc),
            "accelerators": gpu_memory_snapshots(),
            "models": models,
            "cached_artifacts": cached_artifacts(),
        }

    @staticmethod
    def _operation_completed(
        inst: ModelInstance, action: str, operation_id: str
    ) -> bool:
        return operation_id in inst.completed_operation_ids.get(action, [])

    @staticmethod
    def _remember_operation(
        inst: ModelInstance, action: str, operation_id: str
    ) -> None:
        completed = inst.completed_operation_ids.setdefault(action, [])
        completed.append(operation_id)
        if len(completed) > 128:
            del completed[:-128]

    def _require_instance(self, model_name: str) -> ModelInstance:
        inst = self._models.get(model_name)
        if inst is None:
            raise ModelNotFoundError(f"model {model_name!r} is not active")
        return inst

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
        try:
            with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
                sock.bind(("127.0.0.1", port))
                return True
        except OSError:
            return False


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
            _AGENT = ModelRuntime(
                MockEngineLauncher(), kv_controller=MockKVController()
            )
        else:
            _AGENT = ModelRuntime(
                SubprocessEngineLauncher(), kv_controller=KvctlController()
            )
    return _AGENT

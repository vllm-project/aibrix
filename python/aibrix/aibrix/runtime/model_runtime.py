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
import signal
import socket
import sys
import threading
from abc import ABC, abstractmethod
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from typing import Any, Callable, Dict, List, Optional

from aibrix.runtime.engine_registry import EngineRegistry


def sanitize_ipc_name(name: str) -> str:
    """Match kvcached's KVCACHED_IPC_NAME normalization: characters outside
    [A-Za-z0-9_-] become '-'. Verified on real hardware that kvcached rewrites
    e.g. "kvc_qwen3-0.6b" to "kvc_qwen3-0-6b"; without sanitizing on our side,
    kvctl operations would target a different segment than the engine created.
    """
    return re.sub(r"[^A-Za-z0-9_-]", "-", name)


logger = logging.getLogger(__name__)

# NVML owns a process-global initialization state. Snapshot requests can run
# concurrently, so init/query/shutdown must be one uninterrupted operation.
_nvml_lock = threading.Lock()


@dataclass
class ModelInstance:
    """One engine process serving a model on the pod."""

    model_name: str
    port: int
    ipc_name: str
    engine: str = "vllm"
    artifact_url: str = ""
    claim_ref: Optional[Dict[str, str]] = None
    engine_config: Optional[Dict[str, Any]] = field(default=None, repr=False)
    additional_config: Optional[Dict[str, str]] = field(default=None, repr=False)
    phase: str = "active"
    pid: Optional[int] = None
    pid_start_time: Optional[str] = None
    # Controller-derived vLLM GPU reservation. It is an engine envelope, not a
    # ModelClaim resource field, and survives in the sidecar snapshot so
    # capacity admission can reconstruct after controller restart.
    hbm_reservation_fraction: float = 0.0
    restart_count: int = 0
    last_error: Optional[str] = None
    last_transition: datetime = field(
        default_factory=lambda: datetime.now(timezone.utc)
    )
    next_restart_at: Optional[datetime] = None
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


def _nvml_compute_processes(pynvml, handle):
    """Return NVML compute-process records across pynvml API versions."""
    for name in (
        "nvmlDeviceGetComputeRunningProcesses_v3",
        "nvmlDeviceGetComputeRunningProcesses_v2",
        "nvmlDeviceGetComputeRunningProcesses",
    ):
        getter = getattr(pynvml, name, None)
        if getter is not None:
            return getter(handle)
    return []


def gpu_memory_observation() -> tuple[
    List[Dict[str, object]], Dict[int, Dict[str, int]]
]:
    """Read visible-GPU memory and per-process allocations through NVML.

    The sidecar also runs in CPU-only test and development environments. NVML
    is therefore optional: lack of the module, driver, or visible device means
    the controller receives no accelerator or process observations and falls
    back safely.

    The second return value is keyed by process ID and GPU UUID. It keeps the
    raw NVML attribution separate from ModelRuntime's process-tree ownership
    logic, which is necessary because vLLM uses worker child processes.
    """
    with _nvml_lock:
        initialized = False
        try:
            import pynvml  # type: ignore[import-not-found]

            pynvml.nvmlInit()
            initialized = True
            snapshots = []
            process_hbm: Dict[int, Dict[str, int]] = {}
            for index in range(pynvml.nvmlDeviceGetCount()):
                handle = pynvml.nvmlDeviceGetHandleByIndex(index)
                info = pynvml.nvmlDeviceGetMemoryInfo(handle)
                device_id = pynvml.nvmlDeviceGetUUID(handle)
                if isinstance(device_id, bytes):
                    device_id = device_id.decode()
                device_id = str(device_id)
                snapshots.append(
                    {
                        "id": device_id,
                        "hbm_total_bytes": int(info.total),
                        "hbm_free_bytes": int(info.free),
                    }
                )
                for process in _nvml_compute_processes(pynvml, handle):
                    pid = getattr(process, "pid", None)
                    used = getattr(process, "usedGpuMemory", None)
                    if pid is None or used is None:
                        continue
                    used = int(used)
                    unavailable = getattr(pynvml, "NVML_VALUE_NOT_AVAILABLE", None)
                    if used < 0 or (unavailable is not None and used == unavailable):
                        continue
                    process_hbm.setdefault(int(pid), {})[device_id] = used
            return snapshots, process_hbm
        except Exception as exc:
            logger.debug("NVML GPU observation is unavailable: %s", exc)
            return [], {}
        finally:
            if initialized:
                try:
                    pynvml.nvmlShutdown()  # type: ignore[name-defined]
                except Exception:
                    pass


def gpu_memory_snapshots() -> List[Dict[str, object]]:
    """Read memory state for GPUs visible to this pod via NVML."""
    snapshots, _ = gpu_memory_observation()
    return snapshots


def process_tree_pids(root_pid: Optional[int]) -> set[int]:
    """Return a process and its descendants from the container's procfs.

    vLLM's GPU allocations normally live in worker children rather than in the
    API-server parent launched by the runtime. Procfs makes the attribution
    dependency-free and intentionally best-effort: a process that exits while
    the tree is scanned simply contributes no observation.
    """
    if root_pid is None or root_pid <= 0:
        return set()
    seen = {root_pid}
    pending = [root_pid]
    while pending:
        pid = pending.pop()
        try:
            with open(f"/proc/{pid}/task/{pid}/children") as children:
                child_pids = children.read().split()
        except OSError:
            continue
        for raw_pid in child_pids:
            try:
                child_pid = int(raw_pid)
            except ValueError:
                continue
            if child_pid not in seen:
                seen.add(child_pid)
                pending.append(child_pid)
    return seen


def engine_hbm_peak_bytes(
    inst: "ModelInstance", process_hbm: Dict[int, Dict[str, int]]
) -> int:
    """Return this engine's largest per-GPU observed process allocation."""
    by_accelerator: Dict[str, int] = {}
    for pid in process_tree_pids(inst.pid):
        for accelerator, used in process_hbm.get(pid, {}).items():
            by_accelerator[accelerator] = by_accelerator.get(accelerator, 0) + used
    return max(by_accelerator.values(), default=0)


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


def pid_is_alive(pid: Optional[int]) -> bool:
    """Best-effort liveness check that treats a zombie as exited."""
    if pid is None or pid <= 0:
        return False
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True

    try:
        with open(f"/proc/{pid}/stat", encoding="utf-8") as stat_file:
            _, fields = stat_file.read().rsplit(")", 1)
        return not fields.split() or fields.split()[0] != "Z"
    except OSError:
        # /proc is unavailable on some development hosts. os.kill above is
        # still a valid liveness signal there.
        return True


def process_start_time(pid: Optional[int]) -> Optional[str]:
    """Return Linux procfs start-time ticks, guarding against PID reuse."""
    if pid is None or pid <= 0:
        return None
    try:
        with open(f"/proc/{pid}/stat", encoding="utf-8") as stat_file:
            _, fields = stat_file.read().rsplit(")", 1)
        # procfs field 22 is starttime. After removing the comm field (2), it
        # is at offset 19 in the remaining whitespace-separated fields.
        return fields.split()[19]
    except (IndexError, OSError):
        return None


def process_group_is_alive(process_group_id: Optional[int]) -> bool:
    """Whether a Linux process group still has at least one member.

    Engines run in a dedicated session whose process-group ID initially equals
    the API-server PID.  That leader can exit while vLLM worker children keep
    the group, CUDA context, and HBM allocation alive.  ``killpg(..., 0)`` is
    the least invasive way to check that condition without relying on a
    parent/child relationship that no longer exists after orphaning.
    """
    if process_group_id is None or process_group_id <= 0:
        return False
    try:
        os.killpg(process_group_id, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    except OSError:
        return False
    return True


class AdoptedProcess:
    """Small Popen-compatible liveness handle for an orphaned engine PID."""

    def __init__(self, pid: Optional[int], expected_start_time: Optional[str]):
        self.pid = pid
        self.expected_start_time = expected_start_time

    def poll(self) -> Optional[int]:
        if not pid_is_alive(self.pid):
            return 1
        current_start_time = process_start_time(self.pid)
        if (
            self.expected_start_time is not None
            and current_start_time is not None
            and current_start_time != self.expected_start_time
        ):
            return 1
        return None


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
    if inst.phase in {"sleeping", "restarting", "failed", "stopping"}:
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

        if inst.pid is None:
            return
        try:
            # ``start_new_session=True`` makes the API-server PID the group
            # ID. Do not derive it with os.getpgid(inst.pid): when the API
            # server has already crashed, that lookup fails even though its
            # vLLM worker children can still own GPU memory in the group.
            os.killpg(inst.pid, signal.SIGTERM)
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
def _utcnow() -> datetime:
    return datetime.now(timezone.utc)


class ModelRuntime:
    """Manages the set of kvcached-enabled engine processes on one pod."""

    def __init__(
        self,
        launcher: EngineLauncher,
        port_range: tuple = (20000, 21000),
        kv_controller: Optional[KVController] = None,
        registry: Optional[EngineRegistry] = None,
        now: Callable[[], datetime] = _utcnow,
        max_restarts: int = 5,
        restart_base_delay_seconds: float = 2.0,
        restart_max_delay_seconds: float = 60.0,
        start_supervisor: bool = False,
        supervisor_interval_seconds: float = 1.0,
    ) -> None:
        if max_restarts < 1:
            raise ValueError("max_restarts must be positive")
        if restart_base_delay_seconds <= 0:
            raise ValueError("restart_base_delay_seconds must be positive")
        if restart_max_delay_seconds < restart_base_delay_seconds:
            raise ValueError(
                "restart_max_delay_seconds must be >= restart_base_delay_seconds"
            )
        if supervisor_interval_seconds <= 0:
            raise ValueError("supervisor_interval_seconds must be positive")
        self._launcher = launcher
        self._kv_controller = kv_controller or KvctlController()
        self._models: Dict[str, ModelInstance] = {}
        self._lock = threading.RLock()
        self._port_lo, self._port_hi = port_range
        self._registry = registry
        self._now = now
        self._max_restarts = max_restarts
        self._restart_base_delay_seconds = restart_base_delay_seconds
        self._restart_max_delay_seconds = restart_max_delay_seconds
        self._supervisor_interval_seconds = supervisor_interval_seconds
        self._supervisor_stop = threading.Event()
        self._supervisor_thread: Optional[threading.Thread] = None
        if self._registry is not None:
            self.re_adopt()
        if start_supervisor:
            self.start_supervisor()

    @staticmethod
    def _instance_alive(inst: ModelInstance) -> bool:
        """Whether the instance's engine process is still running. Instances
        without a process handle (mock launcher) count as alive."""
        if inst.phase == "failed":
            return False
        proc = inst.proc
        poll = getattr(proc, "poll", None)
        if proc is None or poll is None:
            return True
        return poll() is None

    def start_supervisor(self) -> None:
        """Start the per-engine supervisor once for this runtime instance."""
        with self._lock:
            if (
                self._supervisor_thread is not None
                and self._supervisor_thread.is_alive()
            ):
                return
            self._supervisor_stop.clear()
            self._supervisor_thread = threading.Thread(
                target=self._supervisor_loop,
                name="aibrix-engine-supervisor",
                daemon=True,
            )
            self._supervisor_thread.start()

    def stop_supervisor(self) -> None:
        """Stop a background supervisor, primarily for controlled shutdown."""
        self._supervisor_stop.set()
        thread = self._supervisor_thread
        if thread is not None and thread is not threading.current_thread():
            thread.join(timeout=self._supervisor_interval_seconds * 2)

    def _supervisor_loop(self) -> None:
        while not self._supervisor_stop.wait(self._supervisor_interval_seconds):
            try:
                self.supervise_once()
            except Exception:
                logger.exception("engine supervisor iteration failed")

    def re_adopt(self) -> None:
        """Restore locally-owned engines after the runtime agent restarts."""
        if self._registry is None:
            return
        with self._lock:
            changed = False
            for record in self._registry.load():
                inst = self._instance_from_registry_record(record)
                if inst is None or inst.model_name in self._models:
                    continue
                self._models[inst.model_name] = inst

                if inst.phase == "failed":
                    # A terminal record is never routed or relaunched. If a
                    # stale process unexpectedly remains, clean it on recovery.
                    self._terminate_stale_process_group(inst)
                    continue
                if inst.phase == "stopping":
                    if self._process_handle_alive(inst) or process_group_is_alive(
                        inst.pid
                    ):
                        self._launcher.stop(inst)
                    else:
                        self._models.pop(inst.model_name, None)
                        changed = True
                    continue

                if self._process_handle_alive(inst):
                    if inst.phase != "sleeping":
                        if instance_ready(inst):
                            if inst.phase != "active" or inst.last_error is not None:
                                self._transition(inst, "active")
                                inst.last_error = None
                                changed = True
                        elif inst.phase != "booting":
                            self._transition(inst, "booting")
                            changed = True
                    continue

                if inst.phase != "restarting" or inst.next_restart_at is None:
                    self._schedule_restart(
                        inst, "engine unavailable during runtime re-adoption"
                    )
                    changed = True

            if changed:
                self._persist_locked()

    @staticmethod
    def _process_handle_alive(inst: ModelInstance) -> bool:
        proc = inst.proc
        poll = getattr(proc, "poll", None)
        if proc is None or poll is None:
            return True
        return poll() is None

    def _instance_from_registry_record(
        self, record: Dict[str, Any]
    ) -> Optional[ModelInstance]:
        try:
            model_name = record["model_name"]
            port = int(record["port"])
            ipc_name = record["ipc_name"]
            engine = record["engine"]
            artifact_url = record["artifact_url"]
            if (
                not isinstance(model_name, str)
                or not model_name
                or not isinstance(ipc_name, str)
                or not ipc_name
                or not isinstance(engine, str)
                or not engine
                or not isinstance(artifact_url, str)
                or not artifact_url
                or port <= 0
                or port > 65535
            ):
                raise ValueError("missing or invalid required engine metadata")
            raw_pid = record.get("pid")
            pid = int(raw_pid) if raw_pid is not None else None
            if pid is not None and pid <= 0:
                raise ValueError("invalid pid")
            raw_engine_config = record.get("engine_config")
            if raw_engine_config is not None and not isinstance(
                raw_engine_config, dict
            ):
                raise ValueError("invalid engine_config")
            raw_additional_config = record.get("additional_config")
            if raw_additional_config is not None and not isinstance(
                raw_additional_config, dict
            ):
                raise ValueError("invalid additional_config")
            raw_claim_ref = record.get("claim_ref")
            if raw_claim_ref is not None and not isinstance(raw_claim_ref, dict):
                raise ValueError("invalid claim_ref")
            phase = record.get("phase", "booting")
            if phase not in {
                "active",
                "booting",
                "sleeping",
                "restarting",
                "failed",
                "stopping",
            }:
                raise ValueError("invalid engine phase")
            raw_start_time = record.get("pid_start_time")
            pid_start_time = str(raw_start_time) if raw_start_time is not None else None
            restart_count = int(record.get("restart_count", 0))
            if restart_count < 0:
                raise ValueError("invalid restart_count")
            hbm_reservation_fraction = float(
                record.get("hbm_reservation_fraction", 0.0)
            )
            if not 0.0 <= hbm_reservation_fraction <= 1.0:
                raise ValueError("invalid hbm_reservation_fraction")
        except (KeyError, TypeError, ValueError) as exc:
            logger.warning("ignoring invalid engine registry record: %s", exc)
            return None

        return ModelInstance(
            model_name=model_name,
            port=port,
            ipc_name=ipc_name,
            engine=engine,
            artifact_url=artifact_url,
            claim_ref=dict(raw_claim_ref) if raw_claim_ref else None,
            engine_config=dict(raw_engine_config) if raw_engine_config else None,
            additional_config=(
                dict(raw_additional_config) if raw_additional_config else None
            ),
            phase=phase,
            pid=pid,
            pid_start_time=pid_start_time,
            hbm_reservation_fraction=hbm_reservation_fraction,
            restart_count=restart_count,
            last_error=(
                record.get("last_error")
                if isinstance(record.get("last_error"), str)
                else None
            ),
            last_transition=self._registry_timestamp(
                record.get("last_transition"), self._now()
            ),
            next_restart_at=self._registry_timestamp(
                record.get("next_restart_at"), None
            ),
            proc=AdoptedProcess(pid, pid_start_time),
        )

    def _registry_timestamp(
        self, value: Any, fallback: Optional[datetime]
    ) -> Optional[datetime]:
        if value is None:
            return fallback
        if not isinstance(value, str):
            return fallback
        try:
            timestamp = datetime.fromisoformat(value.replace("Z", "+00:00"))
        except ValueError:
            return fallback
        if timestamp.tzinfo is None:
            return timestamp.replace(tzinfo=timezone.utc)
        return timestamp

    def _registry_record(self, inst: ModelInstance) -> Dict[str, Any]:
        return {
            "model_name": inst.model_name,
            "port": inst.port,
            "ipc_name": inst.ipc_name,
            "pid": inst.pid,
            "pid_start_time": inst.pid_start_time,
            "engine": inst.engine,
            "artifact_url": inst.artifact_url,
            "engine_config": dict(inst.engine_config) if inst.engine_config else None,
            "additional_config": (
                dict(inst.additional_config) if inst.additional_config else None
            ),
            "claim_ref": dict(inst.claim_ref) if inst.claim_ref else None,
            "hbm_reservation_fraction": inst.hbm_reservation_fraction,
            "phase": inst.phase,
            "restart_count": inst.restart_count,
            "last_error": inst.last_error,
            "last_transition": inst.last_transition.isoformat(),
            "next_restart_at": (
                inst.next_restart_at.isoformat() if inst.next_restart_at else None
            ),
        }

    def _persist_locked(self) -> None:
        if self._registry is not None:
            self._registry.save(
                [self._registry_record(inst) for inst in self._models.values()]
            )

    def _transition(self, inst: ModelInstance, phase: str) -> bool:
        if inst.phase == phase:
            return False
        inst.phase = phase
        inst.last_transition = self._now()
        return True

    def _terminate_stale_process_group(self, inst: ModelInstance) -> bool:
        """Terminate surviving vLLM workers after their API parent is gone.

        A vLLM API server is the process-group leader created by
        ``start_new_session=True``. Its EngineCore workers can outlive it and
        retain CUDA contexts, so checking only the parent's Popen handle is
        insufficient. This path is intentionally idempotent: a group that
        already exited is considered clean, and a stubborn group is retried by
        the next supervisor iteration before a replacement is launched.
        """
        process_group_id = inst.pid
        if not process_group_is_alive(process_group_id):
            return True
        try:
            self._launcher.stop(inst)
        except Exception:
            logger.exception(
                "could not gracefully stop stale engine process group",
                extra={"model_name": inst.model_name, "pid": process_group_id},
            )
        if process_group_is_alive(process_group_id):
            try:
                os.killpg(process_group_id, signal.SIGKILL)
            except OSError:
                pass
        return not process_group_is_alive(process_group_id)

    def _schedule_restart(self, inst: ModelInstance, error: str) -> None:
        # Release worker-owned HBM as soon as the API parent is observed dead;
        # the exponential backoff governs relaunches, not resource cleanup.
        self._terminate_stale_process_group(inst)
        inst.last_error = error
        if inst.restart_count >= self._max_restarts:
            self._transition(inst, "failed")
            inst.last_error = "engine exited; restart budget exhausted"
            inst.next_restart_at = None
            return
        inst.restart_count += 1
        delay_seconds = min(
            self._restart_base_delay_seconds * 2 ** (inst.restart_count - 1),
            self._restart_max_delay_seconds,
        )
        self._transition(inst, "restarting")
        inst.next_restart_at = self._now() + timedelta(seconds=delay_seconds)

    def _restart_instance(self, inst: ModelInstance) -> None:
        if not self._terminate_stale_process_group(inst):
            inst.last_error = "waiting for stale engine process group to exit"
            inst.next_restart_at = self._now() + timedelta(
                seconds=self._supervisor_interval_seconds
            )
            return

        inst.proc = None
        inst.pid = None
        inst.pid_start_time = None
        try:
            inst.pid = self._launcher.launch(
                inst,
                inst.artifact_url,
                inst.engine_config,
                inst.additional_config,
            )
        except Exception as exc:
            inst.proc = AdoptedProcess(None, None)
            self._schedule_restart(inst, f"restart launch failed: {exc}")
            return
        inst.pid_start_time = process_start_time(inst.pid)
        inst.next_restart_at = None
        self._transition(inst, "booting" if inst.proc is not None else "active")

    def supervise_once(self) -> None:
        """Advance each engine independently through its local failure state."""
        with self._lock:
            changed = False
            remove = []
            now = self._now()
            for model_name, inst in self._models.items():
                if inst.phase == "failed":
                    continue
                if inst.phase == "stopping":
                    if self._process_handle_alive(inst) or process_group_is_alive(
                        inst.pid
                    ):
                        try:
                            self._launcher.stop(inst)
                        except Exception as exc:
                            inst.last_error = f"stop failed: {exc}"
                            inst.last_transition = now
                            changed = True
                    else:
                        remove.append(model_name)
                        changed = True
                    continue

                if self._process_handle_alive(inst):
                    if inst.phase != "sleeping":
                        if instance_ready(inst):
                            if inst.phase != "active" or inst.last_error is not None:
                                self._transition(inst, "active")
                                inst.last_error = None
                                changed = True
                        elif inst.phase != "booting":
                            self._transition(inst, "booting")
                            changed = True
                    continue

                if inst.phase == "restarting" and inst.next_restart_at is not None:
                    if now < inst.next_restart_at:
                        continue
                    self._restart_instance(inst)
                    changed = True
                    continue

                self._schedule_restart(inst, "engine exited")
                changed = True

            for model_name in remove:
                self._models.pop(model_name, None)
            if changed:
                self._persist_locked()

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
        hbm_reservation_fraction: Optional[float] = None,
        claim_ref: Optional[Dict[str, str]] = None,
    ) -> ModelInstance:
        with self._lock:
            if hbm_reservation_fraction is not None and not (
                0 < hbm_reservation_fraction <= 1
            ):
                raise ValueError("hbm_reservation_fraction must be in (0, 1]")
            if engine == "vllm":
                validate_vllm_parallelism(engine_config, additional_config)
            existing = self._models.get(model_name)
            if existing is not None:
                if existing.phase == "failed":
                    raise RuntimeError(
                        f"model {model_name!r} exhausted its local restart budget; "
                        "deactivate it before activating again"
                    )
                return existing  # idempotent; the supervisor owns recovery
            inst = ModelInstance(
                model_name=model_name,
                port=port or self._pick_port(),
                ipc_name=sanitize_ipc_name(ipc_name or f"kvc_{model_name}"),
                engine=engine,
                artifact_url=artifact_url,
                engine_config=dict(engine_config) if engine_config else None,
                additional_config=(
                    dict(additional_config) if additional_config else None
                ),
                hbm_reservation_fraction=hbm_reservation_fraction or 0.0,
                claim_ref=dict(claim_ref) if claim_ref else None,
                phase="booting",
                last_transition=self._now(),
            )
            self._models[model_name] = inst
            try:
                # Persist the intent before spawning, so an agent crash cannot
                # silently lose ownership of a startup attempt.
                self._persist_locked()
            except Exception:
                self._models.pop(model_name, None)
                raise
            try:
                inst.pid = self._launcher.launch(
                    inst, artifact_url, engine_config, additional_config
                )
            except Exception:
                self._models.pop(model_name, None)
                self._persist_locked()
                raise
            inst.pid_start_time = process_start_time(inst.pid)
            self._transition(inst, "booting" if inst.proc is not None else "active")
            try:
                self._persist_locked()
            except Exception:
                self._launcher.stop(inst)
                self._models.pop(model_name, None)
                try:
                    self._persist_locked()
                except Exception:
                    logger.exception(
                        "could not clear engine registry after failed activation",
                        extra={"model_name": model_name},
                    )
                raise
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
            self._transition(inst, "stopping")
            self._persist_locked()
            try:
                self._launcher.stop(inst)
            except Exception as exc:
                inst.last_error = f"stop failed: {exc}"
                inst.last_transition = self._now()
                self._persist_locked()
                raise
            # A real SIGTERM is asynchronous. Retain the stopping record until
            # the process exits so an agent crash in this interval can still
            # re-adopt and finish the stop instead of losing an orphan engine.
            if inst.proc is None or (
                not self._process_handle_alive(inst)
                and not process_group_is_alive(inst.pid)
            ):
                self._models.pop(model_name, None)
                self._persist_locked()

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
            self._transition(inst, "sleeping")
            self._remember_operation(inst, "sleep", operation_id)
            self._persist_locked()
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
            self._transition(inst, "booting" if inst.proc is not None else "active")
            self._remember_operation(inst, "wake", operation_id)
            self._persist_locked()
            return RuntimeOperationResult(
                model_name=model_name,
                operation_id=operation_id,
                applied=True,
                phase=inst.phase,
            )

    def list_models(self) -> List[ModelInstance]:
        """Return all lifecycle records without dropping failed engines.

        A dead process remains visible as ``restarting`` or ``failed`` until
        the local supervisor reaches a terminal decision. Removing it here
        would erase the controller's only explanation for a de-routed model.
        """
        with self._lock:
            return list(self._models.values())

    def snapshot_models(self) -> List[ModelInstance]:
        """Thread-safe snapshot of live resident engines for metrics only."""
        with self._lock:
            return [
                inst for inst in self._models.values() if self._instance_alive(inst)
            ]

    def snapshot(self) -> Dict[str, object]:
        """Return the sidecar's point-in-time scheduling observation.

        Instance bookkeeping is copied while holding the runtime lock. The
        potentially slow hardware, filesystem, and readiness reads happen
        afterwards so an observation never blocks activate/deactivate.
        """
        with self._lock:
            instances = list(self._models.values())
        accelerators, process_hbm = gpu_memory_observation()
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
                    "alive": self._instance_alive(inst),
                    "ready": instance_ready(inst),
                    "restart_count": inst.restart_count,
                    "last_error": inst.last_error,
                    "last_transition": inst.last_transition,
                    "kv_used_bytes": used + prealloc,
                    "kv_capacity_bytes": total,
                    "hbm_peak_bytes": engine_hbm_peak_bytes(inst, process_hbm),
                    "hbm_reservation_fraction": inst.hbm_reservation_fraction,
                }
            )
        return {
            "observed_at": datetime.now(timezone.utc),
            "accelerators": accelerators,
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
_DEFAULT_ENGINE_REGISTRY_PATH = "/var/run/aibrix/engines.json"


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
                SubprocessEngineLauncher(),
                kv_controller=KvctlController(),
                registry=EngineRegistry(
                    os.environ.get(
                        "AIBRIX_ENGINE_REGISTRY_PATH", _DEFAULT_ENGINE_REGISTRY_PATH
                    )
                ),
                start_supervisor=True,
            )
    return _AGENT

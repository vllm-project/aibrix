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

"""Tests for runtime model lifecycle, using mock actuators (no GPU)."""

import os
import signal
import sys
import threading
import time
from types import SimpleNamespace

import pytest

from aibrix.runtime.model_runtime import (
    MockEngineLauncher,
    ModelRuntime,
)


def make_agent():
    return ModelRuntime(MockEngineLauncher())


def test_gpu_memory_snapshots_serializes_nvml_lifecycle(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    state_lock = threading.Lock()
    active = 0
    max_active = 0
    init_calls = 0
    shutdown_calls = 0

    def nvml_init():
        nonlocal active, init_calls, max_active
        with state_lock:
            init_calls += 1
            active += 1
            max_active = max(max_active, active)
        time.sleep(0.01)

    def nvml_shutdown():
        nonlocal active, shutdown_calls
        with state_lock:
            shutdown_calls += 1
            active -= 1

    nvml = SimpleNamespace(
        nvmlInit=nvml_init,
        nvmlShutdown=nvml_shutdown,
        nvmlDeviceGetCount=lambda: 1,
        nvmlDeviceGetHandleByIndex=lambda index: index,
        nvmlDeviceGetMemoryInfo=lambda handle: SimpleNamespace(total=100, free=50),
        nvmlDeviceGetUUID=lambda handle: f"GPU-{handle}",
    )
    monkeypatch.setitem(sys.modules, "pynvml", nvml)

    start = threading.Barrier(4)
    snapshots = []

    def observe():
        start.wait()
        snapshots.append(runtime_module.gpu_memory_snapshots())

    threads = [threading.Thread(target=observe) for _ in range(4)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()

    assert (
        snapshots
        == [
            [
                {
                    "id": "GPU-0",
                    "hbm_total_bytes": 100,
                    "hbm_free_bytes": 50,
                    "hbm_usable_bytes": -1,
                }
            ]
        ]
        * 4
    )
    assert init_calls == 4
    assert shutdown_calls == 4
    assert max_active == 1


def test_gpu_memory_observation_reports_process_memory_by_gpu(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    nvml = SimpleNamespace(
        nvmlInit=lambda: None,
        nvmlShutdown=lambda: None,
        nvmlDeviceGetCount=lambda: 2,
        nvmlDeviceGetHandleByIndex=lambda index: index,
        nvmlDeviceGetMemoryInfo=lambda handle: SimpleNamespace(
            total=1000, free=700 - (100 * handle)
        ),
        nvmlDeviceGetUUID=lambda handle: f"GPU-{handle}",
        nvmlDeviceGetComputeRunningProcesses=lambda handle: (
            [SimpleNamespace(pid=101, usedGpuMemory=111)]
            if handle == 0
            else [
                SimpleNamespace(pid=101, usedGpuMemory=222),
                SimpleNamespace(pid=202, usedGpuMemory=333),
            ]
        ),
    )
    monkeypatch.setitem(sys.modules, "pynvml", nvml)

    accelerators, process_hbm = runtime_module.gpu_memory_observation()

    assert accelerators == [
        {
            "id": "GPU-0",
            "hbm_total_bytes": 1000,
            "hbm_free_bytes": 700,
            "hbm_usable_bytes": -1,
        },
        {
            "id": "GPU-1",
            "hbm_total_bytes": 1000,
            "hbm_free_bytes": 600,
            "hbm_usable_bytes": -1,
        },
    ]
    assert process_hbm == {
        101: {"GPU-0": 111, "GPU-1": 222},
        202: {"GPU-1": 333},
    }


def _nvml_with_v2(memory_info, count=1):
    """A pynvml stand-in whose v2 memory query is the only place the driver's
    own reservation shows up."""
    return SimpleNamespace(
        nvmlInit=lambda: None,
        nvmlShutdown=lambda: None,
        nvmlDeviceGetCount=lambda: count,
        nvmlDeviceGetHandleByIndex=lambda index: index,
        nvmlDeviceGetMemoryInfo=memory_info,
        nvmlDeviceGetUUID=lambda handle: f"GPU-{handle}",
        nvmlMemory_v2=2,
    )


def test_usable_memory_is_the_total_less_the_driver_reservation(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setattr(runtime_module, "_hbm_usable_by_device", {})

    def memory_info(handle, version=None):
        if version is None:
            return SimpleNamespace(total=1000, free=400)
        return SimpleNamespace(total=1000, free=400, reserved=40)

    monkeypatch.setitem(sys.modules, "pynvml", _nvml_with_v2(memory_info))

    accelerators, _ = runtime_module.gpu_memory_observation()

    assert accelerators == [
        {
            "id": "GPU-0",
            "hbm_total_bytes": 1000,
            "hbm_free_bytes": 400,
            "hbm_usable_bytes": 960,
        }
    ]


def test_usable_memory_is_measured_once_per_card(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setattr(runtime_module, "_hbm_usable_by_device", {})
    v2_calls = 0

    def memory_info(handle, version=None):
        nonlocal v2_calls
        if version is None:
            return SimpleNamespace(total=1000, free=400)
        v2_calls += 1
        return SimpleNamespace(total=1000, free=400, reserved=40)

    monkeypatch.setitem(sys.modules, "pynvml", _nvml_with_v2(memory_info))

    first, _ = runtime_module.gpu_memory_observation()
    second, _ = runtime_module.gpu_memory_observation()

    assert v2_calls == 1
    assert first[0]["hbm_usable_bytes"] == 960
    assert second[0]["hbm_usable_bytes"] == 960


def test_a_card_that_could_not_be_measured_is_tried_again(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setattr(runtime_module, "_hbm_usable_by_device", {})
    attempts = 0

    def memory_info(handle, version=None):
        nonlocal attempts
        if version is None:
            return SimpleNamespace(total=1000, free=400)
        attempts += 1
        if attempts == 1:
            raise RuntimeError("the driver is not ready yet")
        return SimpleNamespace(total=1000, free=400, reserved=40)

    monkeypatch.setitem(sys.modules, "pynvml", _nvml_with_v2(memory_info))

    failed, _ = runtime_module.gpu_memory_observation()
    recovered, _ = runtime_module.gpu_memory_observation()

    assert failed[0]["hbm_usable_bytes"] == runtime_module.HBM_USABLE_UNKNOWN
    assert recovered[0]["hbm_usable_bytes"] == 960


class _RecordingKVController:
    def __init__(self):
        self.limits = []

    def set_limit(self, ipc_name, limit_bytes):
        self.limits.append((ipc_name, limit_bytes))


def test_set_kv_limit_is_idempotent_by_operation_id():
    launcher = MockEngineLauncher()
    kv_controller = _RecordingKVController()
    agent = ModelRuntime(launcher, kv_controller=kv_controller)
    inst = agent.activate(
        model_name="qwen3-0.6b",
        artifact_url="hf://Qwen/Qwen3-0.6B",
        ipc_name="kvc_qwen3-0.6b",
    )
    inst.kv_initial_capacity_bytes = 4096

    first = agent.set_kv_limit(inst.model_name, 4096, operation_id="limit-1")
    duplicate = agent.set_kv_limit(inst.model_name, 4096, operation_id="limit-1")

    assert first.applied is True
    assert duplicate.applied is False
    assert kv_controller.limits == [("kvc_qwen3-0-6b", 4096)]


def test_failed_kv_limit_operation_is_retriable():
    class _FlakyKVController:
        def __init__(self):
            self.calls = 0

        def set_limit(self, ipc_name, limit_bytes):
            self.calls += 1
            if self.calls == 1:
                raise RuntimeError("kvctl failed")

    kv_controller = _FlakyKVController()
    agent = ModelRuntime(MockEngineLauncher(), kv_controller=kv_controller)
    inst = agent.activate(model_name="m1", artifact_url="hf://Org/M1")
    inst.kv_initial_capacity_bytes = 4096

    with pytest.raises(RuntimeError, match="kvctl failed"):
        agent.set_kv_limit("m1", 4096, operation_id="limit-1")
    retried = agent.set_kv_limit("m1", 4096, operation_id="limit-1")

    assert retried.applied is True
    assert kv_controller.calls == 2
    assert inst.completed_operation_ids["kv-limit"] == ["limit-1"]
    metrics = agent.snapshot_metrics()
    assert metrics.kv_limit_outcomes == {
        ("m1", "failed"): 1,
        ("m1", "applied"): 1,
    }
    assert metrics.kv_limit_requested_bytes == {"m1": 4096}
    assert metrics.kv_limit_applied_bytes == {"m1": 4096}


def test_sleep_is_idempotent_by_operation_id():
    launcher = MockEngineLauncher()
    agent = ModelRuntime(launcher)
    inst = agent.activate(model_name="m1", artifact_url="hf://Org/M1")

    first = agent.sleep(inst.model_name, level=1, operation_id="sleep-1")
    duplicate = agent.sleep(inst.model_name, level=1, operation_id="sleep-1")

    assert first.applied is True
    assert duplicate.applied is False
    assert inst.phase == "sleeping"
    assert launcher.slept == [("m1", 1)]


def test_failed_sleep_operation_keeps_model_active_and_retriable():
    class _FlakySleepLauncher(MockEngineLauncher):
        def sleep(self, inst, level):
            super().sleep(inst, level)
            if len(self.slept) == 1:
                raise RuntimeError("vllm sleep failed")

    launcher = _FlakySleepLauncher()
    agent = ModelRuntime(launcher)
    inst = agent.activate(model_name="m1", artifact_url="hf://Org/M1")

    with pytest.raises(RuntimeError, match="vllm sleep failed"):
        agent.sleep("m1", level=1, operation_id="sleep-1")
    retried = agent.sleep("m1", level=1, operation_id="sleep-1")

    assert inst.phase == "sleeping"
    assert retried.applied is True
    assert launcher.slept == [("m1", 1), ("m1", 1)]
    assert inst.completed_operation_ids["sleep"] == ["sleep-1"]
    assert agent.snapshot_metrics().lifecycle_outcomes == {
        ("m1", "sleep", "failed"): 1,
        ("m1", "sleep", "applied"): 1,
    }


def test_wake_restores_active_phase_and_readiness():
    from aibrix.runtime.model_runtime import instance_ready

    launcher = MockEngineLauncher()
    agent = ModelRuntime(launcher)
    inst = agent.activate(model_name="m1", artifact_url="hf://Org/M1")
    agent.sleep(inst.model_name, level=1, operation_id="sleep-1")

    assert instance_ready(inst) is False
    first = agent.wake(inst.model_name, operation_id="wake-1")
    duplicate = agent.wake(inst.model_name, operation_id="wake-1")

    assert first.applied is True
    assert duplicate.applied is False
    assert inst.phase == "active"
    assert instance_ready(inst) is True
    assert launcher.woken == ["m1"]


GIB = 1 << 30


def _readings(*maps):
    """Stand in for gpu_memory_observation, giving these per-process readings
    in turn, and the last one from then on."""
    calls = []

    def observe():
        calls.append(None)
        return [], maps[min(len(calls), len(maps)) - 1]

    return observe


def test_sleeping_footprint_matches_the_engines_own_processes():
    from aibrix.runtime.model_runtime import sleeping_footprint_bytes

    before = {100: {"GPU-0": 19 * GIB}, 200: {"GPU-0": 18 * GIB}}
    after = {100: {"GPU-0": 2 * GIB}, 200: {"GPU-0": 18 * GIB}}

    assert sleeping_footprint_bytes({100}, before, after) == 2 * GIB


def test_sleeping_footprint_finds_the_engine_by_its_drop_under_host_pids():
    from aibrix.runtime.model_runtime import sleeping_footprint_bytes

    # NVML reports host PIDs, so none of the engine's own PIDs appear. The
    # neighbour on the card mapped one more KV page at the same moment.
    before = {900: {"GPU-0": 19 * GIB}, 901: {"GPU-0": 18 * GIB}}
    after = {900: {"GPU-0": 2 * GIB}, 901: {"GPU-0": 18 * GIB + 128 * 2**20}}

    assert sleeping_footprint_bytes({100, 101}, before, after) == 2 * GIB


def test_sleeping_footprint_is_unknown_when_no_drop_stands_out():
    from aibrix.runtime.model_runtime import sleeping_footprint_bytes

    before = {900: {"GPU-0": 19 * GIB}, 901: {"GPU-0": 18 * GIB}}
    after = {900: {"GPU-0": 2 * GIB}, 901: {"GPU-0": 6 * GIB}}

    assert sleeping_footprint_bytes({100}, before, after) is None


def test_sleeping_footprint_is_unknown_for_a_small_drop_or_a_zero_reading():
    from aibrix.runtime.model_runtime import sleeping_footprint_bytes

    small = sleeping_footprint_bytes(
        {100}, {100: {"GPU-0": 2 * GIB}}, {100: {"GPU-0": 2 * GIB - 2**20}}
    )
    # Some drivers report zero for every process in a container.
    zero = sleeping_footprint_bytes(
        {100},
        {100: {"GPU-0": 0}, 900: {"GPU-0": 0}},
        {100: {"GPU-0": 0}, 900: {"GPU-0": 0}},
    )

    assert small is None
    assert zero is None


def test_sleeping_footprint_never_takes_another_process_drop_for_the_engines():
    from aibrix.runtime.model_runtime import sleeping_footprint_bytes

    # The engine's own process is found, but its memory has not fallen like a
    # sleep yet. Another process fell far more at the same moment. Its figure
    # would charge the engine too little, so there is none.
    before = {100: {"GPU-0": 6 * GIB}, 900: {"GPU-0": 19 * GIB}}
    after = {100: {"GPU-0": 6 * GIB - 512 * 2**20}, 900: {"GPU-0": 2 * GIB}}

    assert sleeping_footprint_bytes({100}, before, after) is None


def test_sleeping_footprint_is_unknown_when_the_engines_process_is_gone():
    from aibrix.runtime.model_runtime import sleeping_footprint_bytes

    before = {100: {"GPU-0": 19 * GIB}, 900: {"GPU-0": 18 * GIB}}
    after = {900: {"GPU-0": 2 * GIB}}

    assert sleeping_footprint_bytes({100}, before, after) is None


def test_sleeping_footprint_of_an_engine_on_two_cards_is_its_heaviest_card():
    from aibrix.runtime.model_runtime import sleeping_footprint_bytes

    # One worker on each card, and a neighbour engine on both.
    before = {
        100: {"GPU-0": 19 * GIB},
        101: {"GPU-1": 19 * GIB},
        200: {"GPU-0": 9 * GIB},
        201: {"GPU-1": 9 * GIB},
    }
    after = {
        100: {"GPU-0": 2 * GIB},
        101: {"GPU-1": 3 * GIB},
        200: {"GPU-0": 9 * GIB},
        201: {"GPU-1": 9 * GIB},
    }

    assert sleeping_footprint_bytes({100, 101}, before, after) == 3 * GIB


def test_sleeping_footprint_of_an_engine_on_two_cards_under_host_pids():
    from aibrix.runtime.model_runtime import sleeping_footprint_bytes

    before = {
        900: {"GPU-0": 19 * GIB},
        901: {"GPU-1": 19 * GIB},
        902: {"GPU-0": 9 * GIB},
        903: {"GPU-1": 9 * GIB},
    }
    after = {
        900: {"GPU-0": 2 * GIB},
        901: {"GPU-1": 3 * GIB},
        902: {"GPU-0": 9 * GIB + 128 * 2**20},
        903: {"GPU-1": 9 * GIB},
    }

    assert sleeping_footprint_bytes({100, 101}, before, after) == 3 * GIB


def test_sleeping_footprint_is_unknown_when_one_of_two_cards_did_not_fall():
    from aibrix.runtime.model_runtime import sleeping_footprint_bytes

    before = {100: {"GPU-0": 19 * GIB}, 101: {"GPU-1": 19 * GIB}}
    after = {100: {"GPU-0": 2 * GIB}, 101: {"GPU-1": 19 * GIB}}
    host_before = {900: {"GPU-0": 19 * GIB}, 901: {"GPU-1": 19 * GIB}}
    host_after = {900: {"GPU-0": 2 * GIB}, 901: {"GPU-1": 19 * GIB}}

    assert sleeping_footprint_bytes({100, 101}, before, after) is None
    assert sleeping_footprint_bytes({100, 101}, host_before, host_after) is None


def test_sleep_records_the_footprint_and_wake_forgets_it(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    agent = make_agent()
    inst = agent.activate(model_name="m1", artifact_url="hf://Org/M1")
    monkeypatch.setattr(
        runtime_module,
        "gpu_memory_observation",
        _readings({inst.pid: {"GPU-0": 19 * GIB}}, {inst.pid: {"GPU-0": 2 * GIB}}),
    )
    monkeypatch.setattr(runtime_module, "process_tree_pids", lambda pid: {pid})
    monkeypatch.setattr(runtime_module, "read_kv_segment", lambda ipc_name: None)
    monkeypatch.setattr(
        runtime_module,
        "engine_request_activity",
        lambda inst: runtime_module.EngineRequestActivity(),
    )

    agent.sleep("m1", level=1, operation_id="sleep-1")

    assert inst.sleeping_footprint_bytes == 2 * GIB
    assert agent.snapshot()["models"][0]["sleeping_footprint_bytes"] == 2 * GIB

    agent.wake("m1", operation_id="wake-1")

    assert inst.sleeping_footprint_bytes is None
    assert agent.snapshot()["models"][0]["sleeping_footprint_bytes"] is None


def test_a_failed_wake_forgets_the_footprint(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    class _FailingWakeLauncher(MockEngineLauncher):
        def wake(self, inst):
            raise RuntimeError("vllm wake failed")

    agent = ModelRuntime(_FailingWakeLauncher())
    inst = agent.activate(model_name="m1", artifact_url="hf://Org/M1")
    monkeypatch.setattr(
        runtime_module,
        "gpu_memory_observation",
        _readings({inst.pid: {"GPU-0": 19 * GIB}}, {inst.pid: {"GPU-0": 2 * GIB}}),
    )
    monkeypatch.setattr(runtime_module, "process_tree_pids", lambda pid: {pid})
    agent.sleep("m1", level=1, operation_id="sleep-1")

    with pytest.raises(RuntimeError, match="vllm wake failed"):
        agent.wake("m1", operation_id="wake-1")

    # vLLM may have taken memory back before it failed.
    assert inst.phase == "sleeping"
    assert inst.sleeping_footprint_bytes is None


def test_registry_keeps_the_footprint_of_a_sleeping_engine():
    agent = make_agent()
    inst = agent.activate(model_name="m1", artifact_url="hf://Org/M1")
    inst.phase = "sleeping"
    inst.sleeping_footprint_bytes = 2 * GIB

    record = agent._registry_record(inst)
    restored = agent._instance_from_registry_record(record)

    assert record["sleeping_footprint_bytes"] == 2 * GIB
    assert restored is not None
    assert restored.sleeping_footprint_bytes == 2 * GIB
    # A reading that cannot be used is dropped, and the engine is kept.
    for unusable in ("2147483648", -1, 0, True):
        restored = agent._instance_from_registry_record(
            {**record, "sleeping_footprint_bytes": unusable}
        )
        assert restored is not None
        assert restored.sleeping_footprint_bytes is None
    awake = agent._instance_from_registry_record({**record, "phase": "active"})
    assert awake is not None
    assert awake.sleeping_footprint_bytes is None


def test_write_kv_limit_sets_the_limit_and_keeps_what_the_engine_wrote(tmp_path):
    import struct

    from aibrix.runtime.model_runtime import read_kv_segment, write_kv_limit

    (tmp_path / "kvc_m1").write_bytes(struct.pack("<3q", 100, 40, 10))

    assert write_kv_limit("kvc_m1", 4096, shm_dir=str(tmp_path))
    assert read_kv_segment("kvc_m1", shm_dir=str(tmp_path)) == (4096, 40, 10)


def test_write_kv_limit_leaves_a_missing_segment_alone(tmp_path):
    from aibrix.runtime.model_runtime import write_kv_limit

    assert not write_kv_limit("kvc_missing", 4096, shm_dir=str(tmp_path))
    assert not (tmp_path / "kvc_missing").exists()


def test_write_kv_limit_waits_for_the_lock_the_engine_takes(tmp_path):
    import fcntl
    import struct

    from aibrix.runtime.model_runtime import read_kv_segment, write_kv_limit

    segment = tmp_path / "kvc_m1"
    segment.write_bytes(struct.pack("<3q", 100, 40, 10))
    written = threading.Event()

    def write():
        if write_kv_limit("kvc_m1", 4096, shm_dir=str(tmp_path)):
            written.set()

    with open(segment, "r+b") as engine:
        # kvcached rewrites the whole struct under an exclusive flock.
        fcntl.flock(engine, fcntl.LOCK_EX)
        writer = threading.Thread(target=write)
        writer.start()
        assert not written.wait(0.2)
        fcntl.flock(engine, fcntl.LOCK_UN)
    writer.join(timeout=5)

    assert written.is_set()
    assert read_kv_segment("kvc_m1", shm_dir=str(tmp_path)) == (4096, 40, 10)


def test_write_kv_limit_gives_up_on_a_lock_that_stays_held(tmp_path, monkeypatch):
    import fcntl
    import struct

    import aibrix.runtime.model_runtime as runtime_module
    from aibrix.runtime.model_runtime import read_kv_segment, write_kv_limit

    monkeypatch.setattr(runtime_module, "KV_LIMIT_LOCK_TIMEOUT_SECONDS", 0.1)
    segment = tmp_path / "kvc_m1"
    segment.write_bytes(struct.pack("<3q", 100, 40, 10))
    with open(segment, "r+b") as engine:
        fcntl.flock(engine, fcntl.LOCK_EX)
        with pytest.raises(TimeoutError, match="kvc_m1"):
            write_kv_limit("kvc_m1", 4096, shm_dir=str(tmp_path))

    assert read_kv_segment("kvc_m1", shm_dir=str(tmp_path)) == (100, 40, 10)


def test_vllm_lifecycle_controls_use_checked_localhost_requests(monkeypatch):

    import aibrix.runtime.model_runtime as runtime_module
    from aibrix.runtime.model_runtime import ModelInstance, SubprocessEngineLauncher

    calls = []

    class _Response:
        def raise_for_status(self):
            calls[-1]["checked"] = True

    def fake_post(url, timeout):
        calls.append({"url": url, "timeout": timeout})
        return _Response()

    monkeypatch.setattr(
        runtime_module, "_localhost", lambda: SimpleNamespace(post=fake_post)
    )
    inst = ModelInstance(model_name="m1", port=30123, ipc_name="kvc_m1")
    launcher = SubprocessEngineLauncher()

    launcher.sleep(inst, level=2)
    launcher.wake(inst)

    assert calls == [
        {
            "url": "http://127.0.0.1:30123/sleep?level=0&mode=wait",
            "timeout": 1.0,
            "checked": True,
        },
        {
            "url": "http://127.0.0.1:30123/sleep?level=2",
            "timeout": 50.0,
            "checked": True,
        },
        {
            "url": "http://127.0.0.1:30123/wake_up",
            "timeout": 50.0,
            "checked": True,
        },
    ]


class _VLLMSleeps:
    """A vLLM that answers each sleep request with the next status code given,
    or raises the next exception given."""

    def __init__(self, *answers):
        self.answers = list(answers)
        self.asked = []

    def post(self, url, timeout):
        import httpx

        self.asked.append(url)
        answer = self.answers.pop(0)
        if isinstance(answer, Exception):
            raise answer
        return httpx.Response(answer, request=httpx.Request("POST", url))


def _launcher_asking(monkeypatch, vllm):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setattr(runtime_module, "_localhost", lambda: vllm)
    monkeypatch.setattr(runtime_module.time, "sleep", lambda seconds: None)
    inst = runtime_module.ModelInstance(model_name="m1", port=30123, ipc_name="kvc_m1")
    return runtime_module.SubprocessEngineLauncher(), inst


_DRAIN = "http://127.0.0.1:30123/sleep?level=0&mode=wait"
_SLEEP = "http://127.0.0.1:30123/sleep?level=1"


def test_a_sleep_lets_the_requests_an_engine_serves_finish_first(monkeypatch):
    vllm = _VLLMSleeps(200, 200)
    launcher, inst = _launcher_asking(monkeypatch, vllm)

    launcher.sleep(inst, level=1)

    assert vllm.asked == [_DRAIN, _SLEEP]


def test_an_engine_that_still_serves_is_not_put_to_sleep(monkeypatch):
    import httpx

    from aibrix.runtime.model_runtime import EngineServingError

    vllm = _VLLMSleeps(httpx.ReadTimeout("still serving"), 200)
    launcher, inst = _launcher_asking(monkeypatch, vllm)

    with pytest.raises(EngineServingError):
        launcher.sleep(inst, level=1)

    assert vllm.asked == [_DRAIN, "http://127.0.0.1:30123/wake_up?tags=scheduling"]


def test_an_engine_that_cannot_pause_is_put_to_sleep(monkeypatch):
    vllm = _VLLMSleeps(400, 200)
    launcher, inst = _launcher_asking(monkeypatch, vllm)

    launcher.sleep(inst, level=1)

    assert vllm.asked == [_DRAIN, _SLEEP]


def test_a_refused_sleep_is_a_conflict():
    from fastapi import HTTPException

    from aibrix.runtime.model_runtime import EngineServingError
    from aibrix.runtime.model_runtime_api import _control_error

    with pytest.raises(HTTPException) as refused:
        _control_error(EngineServingError("model m1 still serves requests"))

    assert refused.value.status_code == 409


def test_a_sleep_vllm_failed_is_asked_for_again(monkeypatch):
    vllm = _VLLMSleeps(200, 500, 200)
    launcher, inst = _launcher_asking(monkeypatch, vllm)

    launcher.sleep(inst, level=1)

    assert vllm.asked == [_DRAIN, _SLEEP, _SLEEP]


def test_a_sleep_vllm_keeps_failing_fails(monkeypatch):
    import httpx

    import aibrix.runtime.model_runtime as runtime_module

    vllm = _VLLMSleeps(200, *[500] * runtime_module.VLLM_SLEEP_ATTEMPTS)
    launcher, inst = _launcher_asking(monkeypatch, vllm)

    with pytest.raises(httpx.HTTPStatusError):
        launcher.sleep(inst, level=1)
    assert vllm.asked == [_DRAIN] + [_SLEEP] * runtime_module.VLLM_SLEEP_ATTEMPTS


def test_a_sleep_vllm_did_not_answer_is_not_asked_for_again(monkeypatch):
    import httpx

    vllm = _VLLMSleeps(200, httpx.ReadTimeout("no answer"))
    launcher, inst = _launcher_asking(monkeypatch, vllm)

    with pytest.raises(httpx.ReadTimeout):
        launcher.sleep(inst, level=1)
    assert vllm.asked == [_DRAIN, _SLEEP]


def test_subprocess_launcher_stops_group_after_api_server_exits(monkeypatch):
    from aibrix.runtime.model_runtime import ModelInstance, SubprocessEngineLauncher

    calls = []
    monkeypatch.setattr(
        os,
        "killpg",
        lambda process_group_id, signum: calls.append((process_group_id, signum)),
    )
    inst = ModelInstance(
        model_name="m1",
        port=30123,
        ipc_name="kvc_m1",
        pid=1234,
        proc=SimpleNamespace(poll=lambda: 1),
    )

    SubprocessEngineLauncher().stop(inst)

    assert calls == [(1234, signal.SIGTERM)]


def test_completed_operation_ids_are_bounded():
    kv_controller = _RecordingKVController()
    agent = ModelRuntime(MockEngineLauncher(), kv_controller=kv_controller)
    inst = agent.activate(model_name="m1", artifact_url="hf://Org/M1")
    inst.kv_initial_capacity_bytes = 4096

    for index in range(130):
        agent.set_kv_limit("m1", index, operation_id=f"limit-{index}")

    assert len(inst.completed_operation_ids["kv-limit"]) == 128
    assert inst.completed_operation_ids["kv-limit"][0] == "limit-2"
    assert len(kv_controller.limits) == 130


def test_activate_assigns_ipc_port():
    agent = make_agent()
    inst = agent.activate(model_name="m1", artifact_url="hf://x")
    assert inst.ipc_name == "kvc_m1"
    assert 20000 <= inst.port < 21000
    assert inst.phase == "active"
    assert inst.pid is not None
    assert agent._launcher.launched == ["m1"]


def test_activate_is_idempotent():
    agent = make_agent()
    first = agent.activate(model_name="m1", artifact_url="hf://x")
    second = agent.activate(model_name="m1", artifact_url="hf://x")
    assert first.port == second.port
    assert agent._launcher.launched == ["m1"]  # launched only once


def test_activate_distinct_ports_and_ipc_per_model():
    agent = make_agent()
    a = agent.activate(model_name="m1", artifact_url="hf://x")
    b = agent.activate(model_name="m2", artifact_url="hf://y")
    assert a.port != b.port
    assert a.ipc_name != b.ipc_name
    assert {m.model_name for m in agent.list_models()} == {"m1", "m2"}


def test_engine_config_args_are_structured():
    from aibrix.runtime.model_runtime import _engine_args

    assert _engine_args(
        {"args": {"--max-model-len": "2048", "--enforce-eager": ""}},
        None,
    ) == {"--max-model-len": "2048", "--enforce-eager": ""}


def test_vllm_parallelism_defaults_and_combines_tp_pp():
    from aibrix.runtime.model_runtime import vllm_parallelism

    assert vllm_parallelism(None, None) == 1
    assert (
        vllm_parallelism(
            {"args": {"--tensor-parallel-size": "2", "--pipeline-parallel-size": "2"}},
            None,
        )
        == 4
    )


@pytest.mark.parametrize(
    "args",
    [
        {"--tensor-parallel-size": "0"},
        {"--pipeline-parallel-size": "not-a-number"},
        {"--data-parallel-size": "2"},
    ],
)
def test_vllm_parallelism_rejects_unsupported_or_invalid_args(args):
    from aibrix.runtime.model_runtime import vllm_parallelism

    with pytest.raises(ValueError):
        vllm_parallelism({"args": args}, None)


def test_activate_rejects_vllm_parallelism_that_does_not_match_visible_gpus(
    monkeypatch,
):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setattr(
        runtime_module,
        "gpu_memory_snapshots",
        lambda: [{"id": "GPU-0", "hbm_total_bytes": 1000, "hbm_free_bytes": 900}],
    )

    with pytest.raises(ValueError, match="must equal 1 GPU"):
        make_agent().activate(
            model_name="tp2",
            artifact_url="hf://Org/M1",
            engine_config={"args": {"--tensor-parallel-size": "2"}},
        )


def test_activate_accepts_vllm_tp_pp_matching_visible_gpus(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setattr(
        runtime_module,
        "gpu_memory_snapshots",
        lambda: [
            {"id": f"GPU-{index}", "hbm_total_bytes": 1000, "hbm_free_bytes": 900}
            for index in range(4)
        ],
    )

    inst = make_agent().activate(
        model_name="tp2pp2",
        artifact_url="hf://Org/M1",
        engine_config={
            "args": {"--tensor-parallel-size": "2", "--pipeline-parallel-size": "2"}
        },
    )

    assert inst.model_name == "tp2pp2"


def test_legacy_additional_config_engine_arg_prefix_is_supported():
    from aibrix.runtime.model_runtime import _engine_args

    assert _engine_args(None, {"engine-arg:--gpu-memory-utilization": "0.45"}) == {
        "--gpu-memory-utilization": "0.45"
    }


def test_explicit_port_and_ipc_respected():
    agent = make_agent()
    inst = agent.activate(
        model_name="m1", artifact_url="hf://x", port=20555, ipc_name="custom"
    )
    assert inst.port == 20555
    assert inst.ipc_name == "custom"


def test_activate_sanitizes_ipc_name():
    # kvcached normalizes the IPC name (dots/slashes -> '-'); the agent must do
    # the same so limit writes target the segment the engine actually creates.
    agent = make_agent()
    inst = agent.activate(model_name="qwen3-0.6b", artifact_url="hf://x")
    assert inst.ipc_name == "kvc_qwen3-0-6b"


def test_deactivate_stop_removes_model():
    agent = make_agent()
    agent.activate(model_name="m1", artifact_url="hf://x")
    agent.deactivate("m1", mode="stop")
    assert agent.list_models() == []
    assert agent._launcher.stopped == ["m1"]


def test_deactivate_unknown_model_is_noop():
    agent = make_agent()
    agent.deactivate("ghost", mode="stop")  # must not raise


def test_deactivate_non_stop_mode_is_treated_as_stop():
    agent = make_agent()
    agent.activate(model_name="m1", artifact_url="hf://x")
    agent.deactivate("m1", mode="warm")
    assert agent.list_models() == []
    assert agent._launcher.stopped == ["m1"]


# --------------------------------------------------------------------------- #
# HTTP endpoint smoke test via FastAPI TestClient (mock agent)
# --------------------------------------------------------------------------- #
def _make_test_client():
    # Force the singleton agent into mock mode before the app imports it.
    os.environ["AIBRIX_MODEL_RUNTIME_MOCK"] = "1"
    import aibrix.runtime.model_runtime as pa

    pa._AGENT = None  # reset any agent created by an earlier import

    from fastapi import FastAPI
    from fastapi.testclient import TestClient

    from aibrix.app import router

    app = FastAPI()
    app.include_router(router)
    return TestClient(app)


def test_endpoints_activate_list_deactivate():
    client = _make_test_client()

    resp = client.post(
        "/v1/runtime/models/activate",
        json={"model_name": "ep1", "artifact_url": "hf://x", "engine": "vllm"},
    )
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["status"] == "success"
    assert body["ipc_name"] == "kvc_ep1"
    assert body["port"] >= 20000

    listed = client.get("/v1/runtime/models").json()
    assert any(m["model_name"] == "ep1" for m in listed["models"])

    resp = client.post(
        "/v1/runtime/models/deactivate", json={"model_name": "ep1", "mode": "stop"}
    )
    assert resp.status_code == 200

    listed = client.get("/v1/runtime/models").json()
    assert all(m["model_name"] != "ep1" for m in listed["models"])


def test_a_deactivate_that_waits_does_not_stall_the_other_endpoints():
    # A deactivate waits for the runtime's operation lock, which an activation
    # that downloads weights, or a sleep, can hold for a minute. Waiting on the
    # event loop would hold up every other endpoint until the lock is released,
    # the controller's snapshot reads included.
    import asyncio

    import httpx

    client = _make_test_client()
    import aibrix.runtime.model_runtime as runtime_module

    agent = runtime_module.get_model_runtime()
    agent.activate(model_name="m1", artifact_url="hf://x")

    held = threading.Event()
    released = threading.Event()

    def hold_operation_lock():
        with agent._operation_lock:
            held.set()
            released.wait(10)

    holder = threading.Thread(target=hold_operation_lock)
    holder.start()
    assert held.wait(5)
    # The lock is let go after a while either way, so a deactivate that blocks
    # the event loop makes this test fail rather than hang.
    letting_go = threading.Timer(5.0, released.set)
    letting_go.start()

    async def deactivate_while_listing():
        transport = httpx.ASGITransport(app=client.app)
        async with httpx.AsyncClient(
            transport=transport, base_url="http://runtime"
        ) as http:
            stopping = asyncio.create_task(
                http.post(
                    "/v1/runtime/models/deactivate",
                    json={"model_name": "m1", "mode": "stop"},
                )
            )
            # Give the deactivate time to reach the lock and wait there.
            await asyncio.sleep(0.2)
            listed = await http.get("/v1/runtime/models")
            listed_while_held = not released.is_set()
            released.set()
            return listed, listed_while_held, await stopping

    try:
        listed, listed_while_held, stopped = asyncio.run(deactivate_while_listing())
    finally:
        released.set()
        letting_go.cancel()
        holder.join(5)

    assert listed.status_code == 200
    assert listed_while_held, "the listing waited for the deactivate's lock"
    assert stopped.status_code == 200
    assert agent.list_models() == []


def test_wake_endpoint_reports_a_wake_that_failed(monkeypatch):
    client = _make_test_client()
    import aibrix.runtime.model_runtime as runtime_module

    def wake_that_fails(model_name, operation_id):
        raise RuntimeError("the engine did not come back")

    monkeypatch.setattr(runtime_module.get_model_runtime(), "wake", wake_that_fails)

    resp = client.post(
        "/v1/runtime/models/wake",
        json={"model_name": "ep1", "operation_id": "op-1"},
    )

    # The controller tells this report from a bare server error, which a proxy
    # on the way could send as well.
    assert resp.status_code == 500
    body = resp.json()
    assert body["status"] == "error"
    assert body["model_name"] == "ep1"
    assert "did not come back" in body["message"]


def test_wake_endpoint_still_refuses_a_model_it_does_not_run():
    client = _make_test_client()

    resp = client.post(
        "/v1/runtime/models/wake",
        json={"model_name": "nobody-runs-this", "operation_id": "op-2"},
    )

    assert resp.status_code == 404, resp.text


def test_activate_endpoint_rejects_mismatched_vllm_parallelism(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setattr(
        runtime_module,
        "gpu_memory_observation",
        lambda: (
            [
                {
                    "id": "GPU-0",
                    "hbm_total_bytes": 1000,
                    "hbm_free_bytes": 700,
                }
            ],
            {},
        ),
    )
    client = _make_test_client()

    response = client.post(
        "/v1/runtime/models/activate",
        json={
            "model_name": "tp2-on-one-gpu",
            "artifact_url": "hf://Org/Model",
            "engine": "vllm",
            "engine_config": {"args": {"--tensor-parallel-size": "2"}},
        },
    )

    assert response.status_code == 400, response.text
    assert response.json()["status"] == "error"
    assert "must equal 1 GPU" in response.json()["message"]


def test_activate_endpoint_rejects_gpu_memory_utilization_for_kvcached():
    client = _make_test_client()

    response = client.post(
        "/v1/runtime/models/activate",
        json={
            "model_name": "invalid-kv-budget",
            "artifact_url": "hf://Org/Model",
            "engine": "vllm",
            "engine_config": {"args": {"--gpu-memory-utilization": "0.45"}},
        },
    )

    assert response.status_code == 400, response.text
    assert response.json()["status"] == "error"
    assert (
        "--gpu-memory-utilization is incompatible with kvcached"
        in response.json()["message"]
    )


def test_control_endpoints_apply_kv_sleep_and_wake():
    client = _make_test_client()
    activated = client.post(
        "/v1/runtime/models/activate",
        json={"model_name": "ep-control", "artifact_url": "hf://Org/Model"},
    )
    assert activated.status_code == 200, activated.text
    limited = client.post(
        "/v1/runtime/models/kv-limit",
        json={
            "model_name": "ep-control",
            "limit_bytes": 4096,
            "operation_id": "limit-1",
        },
    )
    assert limited.status_code == 200, limited.text
    assert limited.json()["applied"] is True

    sleeping = client.post(
        "/v1/runtime/models/sleep",
        json={"model_name": "ep-control", "level": 1, "operation_id": "sleep-1"},
    )
    assert sleeping.status_code == 200, sleeping.text
    assert sleeping.json() == {
        "status": "success",
        "model_name": "ep-control",
        "operation_id": "sleep-1",
        "applied": True,
        "phase": "sleeping",
    }
    listed = client.get("/v1/runtime/models").json()
    assert listed["models"] == [
        {
            "model_name": "ep-control",
            "port": activated.json()["port"],
            "ipc_name": "kvc_ep-control",
            "phase": "sleeping",
            "ready": False,
            "kv_used_bytes": 0,
            "kv_total_bytes": 0,
        }
    ]

    woken = client.post(
        "/v1/runtime/models/wake",
        json={"model_name": "ep-control", "operation_id": "wake-1"},
    )
    assert woken.status_code == 200, woken.text
    assert woken.json()["phase"] == "active"
    assert woken.json()["applied"] is True


def test_mock_mode_kv_limit_api_without_captured_capacity(monkeypatch, tmp_path):
    import json

    import aibrix.runtime.model_runtime as runtime_module
    import aibrix.runtime.model_runtime_api as api
    from aibrix.openapi.protocol import (
        ActivateRuntimeModelRequest,
        SetRuntimeModelKVLimitRequest,
    )

    monkeypatch.setenv("AIBRIX_MODEL_RUNTIME_MOCK", "1")
    monkeypatch.setenv("AIBRIX_WEIGHT_CACHE_DIR", str(tmp_path))
    monkeypatch.setattr(runtime_module, "_AGENT", None)
    monkeypatch.setattr(runtime_module, "gpu_memory_observation", lambda: ([], {}))
    activated = api.activate_runtime_model(
        ActivateRuntimeModelRequest(
            model_name="mock-kv-no-segment",
            artifact_url="hf://Org/Model",
            port=20000,
        )
    )
    assert activated.status_code == 200
    ipc_name = json.loads(activated.body)["ipc_name"]
    agent = runtime_module.get_model_runtime()
    assert isinstance(agent._kv_controller, runtime_module.MockKVController)
    assert runtime_module.read_kv_segment(ipc_name) is None

    limits = (4096, 8192, 2048)
    for index, limit in enumerate(limits):
        observed = agent.snapshot()["models"][0]
        assert observed["ready"] is True
        assert observed["kv_capacity_bytes"] == -1
        assert observed["kv_initial_capacity_bytes"] is None
        response = api.set_runtime_model_kv_limit(
            SetRuntimeModelKVLimitRequest(
                model_name=observed["model_name"],
                limit_bytes=limit,
                operation_id=f"limit-{index}",
            )
        )
        assert response.status_code == 200
        assert json.loads(response.body)["applied"] is True
        assert agent._kv_controller.limits == [
            (ipc_name, value) for value in limits[: index + 1]
        ]


def test_control_endpoints_reject_unknown_unsupported_and_invalid_requests():
    client = _make_test_client()

    missing = client.post(
        "/v1/runtime/models/kv-limit",
        json={"model_name": "missing", "limit_bytes": 4096, "operation_id": "x"},
    )
    assert missing.status_code == 404

    activated = client.post(
        "/v1/runtime/models/activate",
        json={
            "model_name": "ep-sglang",
            "artifact_url": "hf://Org/Model",
            "engine": "sglang",
        },
    )
    assert activated.status_code == 200, activated.text
    unsupported = client.post(
        "/v1/runtime/models/sleep",
        json={"model_name": "ep-sglang", "level": 1, "operation_id": "sleep-1"},
    )
    assert unsupported.status_code == 409

    invalid = client.post(
        "/v1/runtime/models/wake",
        json={"model_name": "ep-sglang", "operation_id": ""},
    )
    assert invalid.status_code == 422


def test_snapshot_reports_runtime_state(monkeypatch, tmp_path):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setenv("AIBRIX_WEIGHT_CACHE_DIR", str(tmp_path))
    agent = make_agent()
    agent.activate(
        model_name="qwen",
        artifact_url="hf://Qwen/Qwen3-0.6B",
        claim_ref={"namespace": "default", "name": "qwen", "uid": "claim-uid"},
    )
    monkeypatch.setattr(
        runtime_module,
        "gpu_memory_observation",
        lambda: (
            [
                {
                    "id": "GPU-0",
                    "hbm_total_bytes": 1000,
                    "hbm_free_bytes": 700,
                }
            ],
            {},
        ),
    )
    monkeypatch.setattr(
        runtime_module,
        "read_kv_segment",
        lambda ipc_name: (100, 20, 5),
    )
    monkeypatch.setattr(
        runtime_module,
        "engine_request_activity",
        lambda inst: runtime_module.EngineRequestActivity(
            observed=True,
            requests_running=2,
            requests_waiting=1,
            request_success_total=7,
        ),
    )

    snapshot = agent.snapshot()

    assert snapshot["accelerators"] == [
        {"id": "GPU-0", "hbm_total_bytes": 1000, "hbm_free_bytes": 700}
    ]
    assert snapshot["cached_artifacts"] == ["hf://Qwen/Qwen3-0.6B"]
    observed = snapshot["models"][0]
    assert observed.pop("last_transition").tzinfo is not None
    assert observed == {
        "model_name": "qwen",
        "artifact_url": "hf://Qwen/Qwen3-0.6B",
        "claim_ref": {
            "namespace": "default",
            "name": "qwen",
            "uid": "claim-uid",
        },
        "port": 20000,
        "ipc_name": "kvc_qwen",
        "phase": "active",
        "alive": True,
        "ready": True,
        "restart_count": 0,
        "last_error": None,
        "kv_used_bytes": 25,
        "kv_capacity_bytes": 100,
        "kv_initial_capacity_bytes": 100,
        "hbm_peak_bytes": 0,
        "sleeping_footprint_bytes": None,
        "request_metrics_observed": True,
        "requests_running": 2,
        "requests_waiting": 1,
        "request_success_total": 7,
    }
    assert snapshot["observed_at"]


def test_snapshot_reports_hbm_peak_for_engine_process_tree(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    agent = make_agent()
    inst = agent.activate(model_name="qwen", artifact_url="hf://Qwen/Qwen3-0.6B")
    assert inst.pid is not None
    monkeypatch.setattr(
        runtime_module,
        "gpu_memory_observation",
        lambda: (
            [
                {"id": "GPU-0", "hbm_total_bytes": 1000, "hbm_free_bytes": 700},
                {"id": "GPU-1", "hbm_total_bytes": 1000, "hbm_free_bytes": 800},
            ],
            {
                inst.pid: {"GPU-0": 10},
                20001: {"GPU-0": 120, "GPU-1": 80},
                30001: {"GPU-0": 999},
            },
        ),
    )
    monkeypatch.setattr(
        runtime_module,
        "process_tree_pids",
        lambda pid: {pid, 20001},
        raising=False,
    )

    snapshot = agent.snapshot()

    assert snapshot["accelerators"][0]["id"] == "GPU-0"
    assert snapshot["models"][0]["hbm_peak_bytes"] == 130


def test_engine_request_activity_accepts_vllm_metric_name_variants(monkeypatch):

    import aibrix.runtime.model_runtime as runtime_module

    class Response:
        text = """# HELP vllm:num_requests_running Running requests.
vllm:num_requests_running{model_name=\"m1\"} 2
vllm_num_requests_waiting{model_name=\"m1\"} 3
vllm:request_success_total{model_name=\"m1\",finished_reason=\"stop\"} 5
vllm:request_success_total{model_name=\"m1\",finished_reason=\"length\"} 7
"""

        def raise_for_status(self):
            return None

    monkeypatch.setattr(
        runtime_module,
        "_localhost",
        lambda: SimpleNamespace(get=lambda url, timeout: Response()),
    )
    inst = runtime_module.ModelInstance(
        model_name="m1",
        port=20000,
        ipc_name="kvc_m1",
        proc=object(),
    )

    activity = runtime_module.engine_request_activity(inst)

    assert activity.observed is True
    assert activity.requests_running == 2
    assert activity.requests_waiting == 3
    assert activity.request_success_total == 12


def test_engine_request_activity_finds_its_metrics_among_all_others(monkeypatch):

    import aibrix.runtime.model_runtime as runtime_module

    class Response:
        text = """# HELP vllm:time_to_first_token_seconds Histogram of TTFT.
# TYPE vllm:time_to_first_token_seconds histogram
vllm:time_to_first_token_seconds_bucket{le=\"0.1\",model_name=\"m1\"} 4.0
vllm:time_to_first_token_seconds_bucket{le=\"+Inf\",model_name=\"m1\"} 9.0
vllm:time_to_first_token_seconds_count{model_name=\"m1\"} 9.0
vllm:time_to_first_token_seconds_sum{model_name=\"m1\"} 1.5
# HELP vllm:num_requests_running Number of requests in model execution batches.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{engine=\"0\",model_name=\"m1\"} 2.0
# HELP vllm:kv_cache_usage_perc KV-cache usage.
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine=\"0\",model_name=\"m1\"} 0.25
# HELP vllm:num_requests_waiting Number of requests waiting to be processed.
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine=\"0\",model_name=\"m1\"} 1.0
# HELP vllm:request_success_total Count of successfully processed requests.
# TYPE vllm:request_success_total counter
vllm:request_success_total{finished_reason=\"stop\",model_name=\"m1\"} 5.0
vllm:request_success_total{finished_reason=\"length\",model_name=\"m1\"} 7.0
vllm:request_success_created{finished_reason=\"stop\",model_name=\"m1\"} 1.7e9
"""

        def raise_for_status(self):
            return None

    monkeypatch.setattr(
        runtime_module,
        "_localhost",
        lambda: SimpleNamespace(get=lambda url, timeout: Response()),
    )
    inst = runtime_module.ModelInstance(
        model_name="m1", port=20000, ipc_name="kvc_m1", proc=object()
    )

    activity = runtime_module.engine_request_activity(inst)

    assert activity.observed is True
    assert activity.requests_running == 2
    assert activity.requests_waiting == 1
    assert activity.request_success_total == 12


def test_snapshot_asks_the_engines_side_by_side(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    agent = make_agent()
    agent.activate(model_name="m1", artifact_url="hf://Org/M1")
    agent.activate(model_name="m2", artifact_url="hf://Org/M2")
    monkeypatch.setattr(runtime_module, "gpu_memory_observation", lambda: ([], {}))
    monkeypatch.setattr(runtime_module, "read_kv_segment", lambda ipc_name: None)
    both_asked = threading.Barrier(2, timeout=2)

    def answer_once_both_are_asked(inst):
        # Asked one after another, the first engine waits alone and gives up.
        try:
            both_asked.wait()
        except threading.BrokenBarrierError:
            return runtime_module.EngineRequestActivity()
        return runtime_module.EngineRequestActivity(
            observed=True, requests_running=1, requests_waiting=0
        )

    monkeypatch.setattr(
        runtime_module, "engine_request_activity", answer_once_both_are_asked
    )

    models = agent.snapshot()["models"]

    assert [m["model_name"] for m in models] == ["m1", "m2"]
    assert all(m["request_metrics_observed"] for m in models)
    assert all(m["requests_running"] == 1 for m in models)


def test_the_engines_on_a_pod_are_asked_through_one_client(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setattr(runtime_module, "_LOCALHOST", None)

    assert runtime_module._localhost() is runtime_module._localhost()


class _SlowSleepLauncher(MockEngineLauncher):
    """Holds a sleep until the test lets it finish."""

    def __init__(self):
        super().__init__()
        self.falling_asleep = threading.Event()
        self.let_it_finish = threading.Event()

    def sleep(self, inst, level):
        self.falling_asleep.set()
        assert self.let_it_finish.wait(5)
        super().sleep(inst, level)


def _agent_with_a_slow_sleep(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setattr(runtime_module, "gpu_memory_observation", lambda: ([], {}))
    monkeypatch.setattr(runtime_module, "read_kv_segment", lambda ipc_name: None)
    launcher = _SlowSleepLauncher()
    agent = ModelRuntime(launcher)
    agent.activate(model_name="m1", artifact_url="hf://Org/M1")
    sleeper = threading.Thread(
        target=lambda: agent.sleep("m1", level=1, operation_id="op-1")
    )
    sleeper.start()
    assert launcher.falling_asleep.wait(5)
    return agent, launcher, sleeper


def test_a_snapshot_does_not_wait_for_an_engine_to_fall_asleep(monkeypatch):
    agent, launcher, sleeper = _agent_with_a_slow_sleep(monkeypatch)
    taken = threading.Event()
    threading.Thread(target=lambda: (agent.snapshot(), taken.set())).start()
    try:
        assert taken.wait(2), "a snapshot waited for the sleep"
    finally:
        launcher.let_it_finish.set()
    sleeper.join(5)

    assert agent.list_models()[0].phase == "sleeping"


def test_a_change_of_an_engine_waits_for_its_sleep(monkeypatch):
    agent, launcher, sleeper = _agent_with_a_slow_sleep(monkeypatch)
    stopped = threading.Event()
    stopper = threading.Thread(target=lambda: (agent.deactivate("m1"), stopped.set()))
    stopper.start()
    try:
        assert not stopped.wait(0.2), "a deactivate ran while the engine fell asleep"
    finally:
        launcher.let_it_finish.set()
    sleeper.join(5)
    stopper.join(5)

    assert stopped.is_set()
    assert launcher.slept == [("m1", 1)]
    assert launcher.stopped == ["m1"]
    assert agent.list_models() == []


def test_a_snapshot_does_not_wait_for_a_kv_limit_write(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setattr(runtime_module, "gpu_memory_observation", lambda: ([], {}))
    monkeypatch.setattr(runtime_module, "read_kv_segment", lambda ipc_name: None)
    writing, let_it_finish = threading.Event(), threading.Event()

    class _SlowKVController:
        def set_limit(self, ipc_name, limit_bytes):
            writing.set()
            assert let_it_finish.wait(5)

    agent = ModelRuntime(MockEngineLauncher(), kv_controller=_SlowKVController())
    inst = agent.activate(model_name="m1", artifact_url="hf://Org/M1")
    inst.kv_initial_capacity_bytes = 4096
    writer = threading.Thread(
        target=lambda: agent.set_kv_limit("m1", 4096, operation_id="limit-1")
    )
    writer.start()
    assert writing.wait(5)
    taken = threading.Event()
    threading.Thread(target=lambda: (agent.snapshot(), taken.set())).start()
    try:
        assert taken.wait(2), "a snapshot waited for a KV limit write"
    finally:
        let_it_finish.set()
    writer.join(5)

    assert agent.snapshot_metrics().kv_limit_applied_bytes == {"m1": 4096}


def test_a_snapshot_does_not_wait_for_a_health_probe(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setattr(runtime_module, "gpu_memory_observation", lambda: ([], {}))
    monkeypatch.setattr(runtime_module, "read_kv_segment", lambda ipc_name: None)
    agent = make_agent()
    agent.activate(model_name="m1", artifact_url="hf://Org/M1")
    probing, answer, calls = threading.Event(), threading.Event(), []

    def first_probe_is_slow(inst):
        calls.append(inst.model_name)
        if len(calls) == 1:
            probing.set()
            answer.wait(5)
        return True

    monkeypatch.setattr(runtime_module, "instance_ready", first_probe_is_slow)
    supervisor = threading.Thread(target=agent.supervise_once)
    supervisor.start()
    assert probing.wait(5)
    taken = threading.Event()
    threading.Thread(target=lambda: (agent.snapshot(), taken.set())).start()
    try:
        assert taken.wait(2), "a snapshot waited for the supervisor's health probe"
    finally:
        answer.set()
    supervisor.join(5)


def test_engine_request_activity_scrapes_external_runtime_mock(monkeypatch):

    import aibrix.runtime.model_runtime as runtime_module

    class Response:
        text = """vllm:num_requests_running{model_name=\"m1\"} 0
vllm:num_requests_waiting{model_name=\"m1\"} 0
vllm:request_success_total{model_name=\"m1\"} 7
"""

        def raise_for_status(self):
            return None

    monkeypatch.setenv("AIBRIX_MODEL_RUNTIME_MOCK", "1")
    monkeypatch.setenv("AIBRIX_MODEL_RUNTIME_MOCK_EXTERNAL_ENGINES", "1")
    monkeypatch.setattr(
        runtime_module,
        "_localhost",
        lambda: SimpleNamespace(get=lambda url, timeout: Response()),
    )
    inst = runtime_module.ModelInstance(
        model_name="m1",
        port=20000,
        ipc_name="kvc_m1",
        pid=10001,
    )

    activity = runtime_module.engine_request_activity(inst)

    assert activity.observed is True
    assert activity.requests_running == 0
    assert activity.requests_waiting == 0
    assert activity.request_success_total == 7


def test_engine_request_activity_ignores_other_models_from_shared_metrics(monkeypatch):

    import aibrix.runtime.model_runtime as runtime_module

    class Response:
        text = """vllm:num_requests_running{model_name=\"m1\"} 2
vllm:num_requests_running{model_name=\"m2\"} 11
vllm_num_requests_waiting{model_name=\"m1\"} 3
vllm_num_requests_waiting{model_name=\"m2\"} 13
vllm:request_success_total{model_name=\"m1\",finished_reason=\"stop\"} 5
vllm:request_success_total{model_name=\"m2\",finished_reason=\"stop\"} 17
"""

        def raise_for_status(self):
            return None

    monkeypatch.setattr(
        runtime_module,
        "_localhost",
        lambda: SimpleNamespace(get=lambda url, timeout: Response()),
    )
    inst = runtime_module.ModelInstance(
        model_name="m1",
        port=20000,
        ipc_name="kvc_m1",
        proc=object(),
    )

    assert runtime_module.engine_request_activity(
        inst
    ) == runtime_module.EngineRequestActivity(
        observed=True,
        requests_running=2,
        requests_waiting=3,
        request_success_total=5,
    )


def test_engine_request_activity_does_not_treat_scrape_failure_as_idle(monkeypatch):
    import httpx

    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setattr(
        httpx,
        "get",
        lambda url, timeout: (_ for _ in ()).throw(httpx.ConnectError("down")),
    )
    inst = runtime_module.ModelInstance(
        model_name="m1",
        port=20000,
        ipc_name="kvc_m1",
        proc=object(),
    )

    assert (
        runtime_module.engine_request_activity(inst)
        == runtime_module.EngineRequestActivity()
    )


def test_snapshot_handles_hosts_without_gpu(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setattr(runtime_module, "gpu_memory_observation", lambda: ([], {}))
    snapshot = make_agent().snapshot()

    assert snapshot["accelerators"] == []
    assert snapshot["models"] == []


def test_snapshot_can_expose_single_gpu_for_runtime_mock(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setenv("AIBRIX_MODEL_RUNTIME_MOCK", "1")
    monkeypatch.setenv("AIBRIX_MODEL_RUNTIME_MOCK_EXTERNAL_ENGINES", "1")
    monkeypatch.setattr(runtime_module, "gpu_memory_observation", lambda: ([], {}))

    snapshot = make_agent().snapshot()

    assert snapshot["accelerators"] == [
        {
            "id": "mock-gpu-0",
            "hbm_total_bytes": 0,
            "hbm_free_bytes": 0,
        }
    ]


def test_external_runtime_mock_claims_prebound_engine_ports(monkeypatch):
    monkeypatch.setenv("AIBRIX_MODEL_RUNTIME_MOCK", "1")
    monkeypatch.setenv("AIBRIX_MODEL_RUNTIME_MOCK_EXTERNAL_ENGINES", "1")
    agent = make_agent()
    monkeypatch.setattr(agent, "_port_free", lambda port: False)

    first = agent.activate(model_name="m1", artifact_url="hf://m1")
    second = agent.activate(model_name="m2", artifact_url="hf://m2")

    assert first.port == 20000
    assert second.port == 20001


def test_snapshot_collects_gpu_observation_once(monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    calls = 0

    def observe():
        nonlocal calls
        calls += 1
        return [], {}

    monkeypatch.setattr(runtime_module, "gpu_memory_observation", observe)

    make_agent().snapshot()

    assert calls == 1


def test_snapshot_endpoint_returns_typed_runtime_state(monkeypatch, tmp_path):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setenv("AIBRIX_WEIGHT_CACHE_DIR", str(tmp_path))
    monkeypatch.setattr(
        runtime_module,
        "gpu_memory_observation",
        lambda: (
            [
                {
                    "id": "GPU-0",
                    "hbm_total_bytes": 1000,
                    "hbm_free_bytes": 700,
                }
            ],
            {},
        ),
    )
    client = _make_test_client()
    activated = client.post(
        "/v1/runtime/models/activate",
        json={
            "model_name": "ep-snapshot",
            "artifact_url": "hf://Org/Model",
            "claim_ref": {
                "namespace": "default",
                "name": "model-claim",
                "uid": "claim-uid",
            },
        },
    )
    assert activated.status_code == 200, activated.text

    response = client.get("/v1/runtime/snapshot")

    assert response.status_code == 200, response.text
    body = response.json()
    assert body["accelerators"][0]["id"] == "GPU-0"
    assert body["models"][0]["claim_ref"]["uid"] == "claim-uid"
    assert body["models"][0]["artifact_url"] == "hf://Org/Model"
    assert body["models"][0]["alive"] is True
    assert body["models"][0]["restart_count"] == 0
    assert body["models"][0]["last_error"] is None
    assert body["models"][0]["last_transition"]


def test_activate_endpoint_rejects_hbm_reservation_fraction():
    client = _make_test_client()
    response = client.post(
        "/v1/runtime/models/activate",
        json={
            "model_name": "reservation-model",
            "artifact_url": "hf://Org/Model",
            "hbm_reservation_fraction": 0.45,
        },
    )
    assert response.status_code == 422, response.text


# --------------------------------------------------------------------------- #
# Cache markers and /dev/shm KV accounting
# --------------------------------------------------------------------------- #
def test_activate_writes_cache_marker(tmp_path, monkeypatch):
    import json

    monkeypatch.setenv("AIBRIX_WEIGHT_CACHE_DIR", str(tmp_path))
    agent = make_agent()
    agent.activate(model_name="m1", artifact_url="huggingface://Org/M1")
    marker = tmp_path / ".aibrix" / "served" / "m1.json"
    assert marker.exists(), "activation must record the model in the node cache"
    data = json.loads(marker.read_text())
    assert data == {"model_name": "m1", "artifact_url": "huggingface://Org/M1"}


def test_write_cache_marker_rejects_path_traversal(tmp_path):
    from aibrix.runtime.model_runtime import write_cache_marker

    path = write_cache_marker("../../etc/evil", "hf://x", cache_dir=str(tmp_path))
    assert path is None, "path traversal in model name must be rejected"
    assert not (tmp_path.parent / "etc" / "evil.json").exists()


def test_activate_marker_failure_does_not_block(monkeypatch):
    # Point the cache at an unwritable location: activation must still succeed.
    monkeypatch.setenv("AIBRIX_WEIGHT_CACHE_DIR", "/proc/definitely-not-writable")
    agent = make_agent()
    inst = agent.activate(model_name="m1", artifact_url="hf://x")
    assert inst.phase == "active"


def test_read_kv_segment_parses_meminfo(tmp_path):
    import struct

    from aibrix.runtime.model_runtime import read_kv_segment

    # kvcached MemInfoStruct: 3 little-endian int64 (total, used, prealloc).
    (tmp_path / "kvc_m1").write_bytes(struct.pack("<3q", 100, 40, 10))
    assert read_kv_segment("kvc_m1", shm_dir=str(tmp_path)) == (100, 40, 10)


def test_read_kv_segment_absent_or_short(tmp_path):
    from aibrix.runtime.model_runtime import read_kv_segment

    assert read_kv_segment("kvc_missing", shm_dir=str(tmp_path)) is None
    (tmp_path / "kvc_short").write_bytes(b"\x00" * 8)
    assert read_kv_segment("kvc_short", shm_dir=str(tmp_path)) is None


class _DeadProc:
    def poll(self):
        return 1  # exited


def test_activate_does_not_bypass_supervisor_for_dead_engine():
    agent = make_agent()
    inst = agent.activate(model_name="m1", artifact_url="hf://x")
    inst.proc = _DeadProc()  # engine died underneath the agent
    again = agent.activate(model_name="m1", artifact_url="hf://x")
    assert agent._launcher.launched == ["m1"]
    assert again is inst

    agent.supervise_once()

    assert inst.phase == "restarting"
    assert inst.restart_count == 1


def test_list_models_keeps_dead_engines_for_supervisor_visibility():
    agent = make_agent()
    a = agent.activate(model_name="m1", artifact_url="hf://x")
    agent.activate(model_name="m2", artifact_url="hf://y")
    a.proc = _DeadProc()
    names = {m.model_name for m in agent.list_models()}
    assert names == {"m1", "m2"}

    agent.supervise_once()

    assert a.phase == "restarting"


# --------------------------------------------------------------------------- #
# Readiness gate: engine_ready / instance_ready. The controller holds a model's
# warm-pod routing annotation at the parked marker (port 0) until instance_ready
# is True, so a still-booting engine is never routed to.
# --------------------------------------------------------------------------- #
class _LiveProc:
    def poll(self):
        return None  # still running


def test_instance_ready_mock_instance_is_ready():
    # A mock/handle-less instance has no engine process to probe and is ready.
    agent = make_agent()
    inst = agent.activate(model_name="m1", artifact_url="hf://x")
    from aibrix.runtime.model_runtime import instance_ready

    assert inst.proc is None
    assert instance_ready(inst) is True


def test_instance_ready_dead_process_not_ready():
    agent = make_agent()
    inst = agent.activate(model_name="m1", artifact_url="hf://x")
    inst.proc = _DeadProc()
    from aibrix.runtime.model_runtime import instance_ready

    assert instance_ready(inst) is False


def test_instance_ready_live_process_probes_health(monkeypatch):
    agent = make_agent()
    inst = agent.activate(model_name="m1", artifact_url="hf://x")
    inst.proc = _LiveProc()
    inst.port = 28123
    import aibrix.runtime.model_runtime as pa

    probed = {}

    def fake_engine_ready(port):
        probed["port"] = port
        return True

    monkeypatch.setattr(pa, "engine_ready", fake_engine_ready)
    assert pa.instance_ready(inst) is True
    assert probed["port"] == 28123, "a live engine must be probed on its port"

    monkeypatch.setattr(pa, "engine_ready", lambda port: False)
    assert pa.instance_ready(inst) is False, "live but unhealthy engine is not ready"


def test_engine_ready_health_200(monkeypatch):

    import aibrix.runtime.model_runtime as runtime_module
    from aibrix.runtime.model_runtime import engine_ready

    class _Resp:
        status_code = 200

    monkeypatch.setattr(
        runtime_module,
        "_localhost",
        lambda: SimpleNamespace(get=lambda url, timeout: _Resp()),
    )
    assert engine_ready(29000) is True


def test_engine_ready_non_200(monkeypatch):

    import aibrix.runtime.model_runtime as runtime_module
    from aibrix.runtime.model_runtime import engine_ready

    class _Resp:
        status_code = 503

    monkeypatch.setattr(
        runtime_module,
        "_localhost",
        lambda: SimpleNamespace(get=lambda url, timeout: _Resp()),
    )
    assert engine_ready(29000) is False


def test_engine_ready_connection_refused(monkeypatch):
    import httpx

    import aibrix.runtime.model_runtime as runtime_module
    from aibrix.runtime.model_runtime import engine_ready

    def boom(url, timeout):
        raise httpx.ConnectError("connection refused")

    monkeypatch.setattr(runtime_module, "_localhost", lambda: SimpleNamespace(get=boom))
    assert engine_ready(29000) is False, "still-booting engine reads as not ready"


def test_snapshot_reports_unknown_kv_before_the_segment_exists(monkeypatch, tmp_path):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setenv("AIBRIX_WEIGHT_CACHE_DIR", str(tmp_path))
    agent = make_agent()
    agent.activate(
        model_name="qwen",
        artifact_url="hf://Qwen/Qwen3-0.6B",
        claim_ref={"namespace": "default", "name": "qwen", "uid": "claim-uid"},
    )
    monkeypatch.setattr(runtime_module, "gpu_memory_observation", lambda: ([], {}))
    monkeypatch.setattr(runtime_module, "read_kv_segment", lambda ipc_name: None)

    observed = agent.snapshot()["models"][0]

    assert observed["kv_used_bytes"] == runtime_module.KV_UNKNOWN
    assert observed["kv_capacity_bytes"] == runtime_module.KV_UNKNOWN


@pytest.fixture
def initial_kv(monkeypatch, tmp_path):
    import aibrix.runtime.model_runtime as runtime_module

    monkeypatch.setenv("AIBRIX_WEIGHT_CACHE_DIR", str(tmp_path))
    monkeypatch.setattr(runtime_module, "gpu_memory_observation", lambda: ([], {}))
    monkeypatch.setattr(
        runtime_module,
        "engine_request_activity",
        lambda inst: runtime_module.EngineRequestActivity(),
    )
    read, write = runtime_module.read_kv_segment, runtime_module.write_kv_limit
    monkeypatch.setattr(
        runtime_module, "read_kv_segment", lambda name: read(name, str(tmp_path))
    )
    monkeypatch.setattr(
        runtime_module,
        "write_kv_limit",
        lambda name, size: write(name, size, str(tmp_path)),
    )
    agent = make_agent()
    inst = agent.activate(model_name="m1", artifact_url="hf://x", port=20000)
    return agent, inst, tmp_path / inst.ipc_name


def test_initial_kv_capture_after_ready(initial_kv, monkeypatch):
    import struct

    import aibrix.runtime.model_runtime as runtime_module
    from aibrix.openapi.protocol import RuntimeSnapshotResponse

    agent, inst, segment = initial_kv
    segment.write_bytes(struct.pack("<3q", 100, 20, 5))
    monkeypatch.setattr(runtime_module, "instance_ready", lambda inst: False)
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] is None
    monkeypatch.setattr(runtime_module, "instance_ready", lambda inst: True)
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] == 100
    agent.set_kv_limit(inst.model_name, 200, operation_id="grow")
    model = RuntimeSnapshotResponse(**agent.snapshot()).models[0]
    assert model.kv_capacity_bytes == 100
    assert model.kv_initial_capacity_bytes == 100


@pytest.mark.parametrize("replace", [False, True])
def test_initial_kv_rejects_detached_instance(initial_kv, monkeypatch, replace):
    from dataclasses import replace as replace_instance

    import aibrix.runtime.model_runtime as runtime_module

    agent, inst, segment = initial_kv

    def detached_read(name):
        # Construct detachment: deactivate() also changes phase to stopping,
        # which would independently block capture and mask the identity guard.
        monkeypatch.delitem(agent._models, inst.model_name)
        if replace:
            monkeypatch.setitem(agent._models, inst.model_name, replace_instance(inst))
        return (100, 0, 0)

    monkeypatch.setattr(runtime_module, "read_kv_segment", detached_read)
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] is None
    assert inst.kv_initial_capacity_bytes is None


def test_initial_kv_rejects_capture_after_stop_starts(initial_kv, monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    agent, inst, segment = initial_kv

    def failed_stop(inst):
        raise RuntimeError("stop failed")

    def stopping_read(name):
        with pytest.raises(RuntimeError, match="stop failed"):
            agent.deactivate(inst.model_name)
        return (100, 0, 0)

    monkeypatch.setattr(agent._launcher, "stop", failed_stop)
    monkeypatch.setattr(runtime_module, "read_kv_segment", stopping_read)
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] is None
    assert inst.phase == "stopping"


def test_initial_kv_is_not_recaptured_after_segment_changes(initial_kv):
    import struct

    agent, inst, segment = initial_kv
    segment.write_bytes(struct.pack("<3q", 100, 0, 0))
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] == 100
    # Change the engine's segment without a runtime limit write, which would
    # independently mark startup capacity unavailable and mask recapture.
    segment.write_bytes(struct.pack("<3q", 80, 0, 0))
    observed = agent.snapshot()["models"][0]
    assert observed["kv_capacity_bytes"] == 80
    assert observed["kv_initial_capacity_bytes"] == 100


@pytest.mark.parametrize(
    "raw",
    [None, "unreadable", b"short", b"", (-1).to_bytes(8, "little", signed=True) * 3],
)
def test_initial_kv_retries_unknown_segment(initial_kv, raw):
    import struct

    agent, inst, segment = initial_kv
    if raw == "unreadable":
        segment.mkdir()
    elif raw is not None:
        segment.write_bytes(raw)
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] is None
    if segment.is_dir():
        segment.rmdir()
    segment.write_bytes(struct.pack("<3q", 0, 0, 0))
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] == 0


def test_initial_kv_write_before_capture_and_restart(initial_kv):
    import struct

    agent, inst, segment = initial_kv
    segment.write_bytes(struct.pack("<3q", 100, 0, 0))
    with pytest.raises(RuntimeError, match="startup KV capacity"):
        agent.set_kv_limit(inst.model_name, 200, operation_id="grow")
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] == 100
    agent._schedule_restart(inst, "engine exited")
    agent._now = lambda: inst.next_restart_at
    agent.supervise_once()
    segment.write_bytes(struct.pack("<3q", 80, 0, 0))
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] == 80
    agent._schedule_restart(inst, "replacement exited")
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] is None


@pytest.mark.parametrize("fail", [False, True])
def test_initial_kv_survives_sleep_wake(initial_kv, monkeypatch, fail):
    import struct

    agent, inst, segment = initial_kv
    segment.write_bytes(struct.pack("<3q", 100, 0, 0))
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] == 100
    agent.sleep(inst.model_name, level=1, operation_id="sleep")
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] == 100
    if fail:

        def failed_wake(inst):
            raise RuntimeError("wake failed")

        monkeypatch.setattr(agent._launcher, "wake", failed_wake)
        with pytest.raises(RuntimeError, match="wake failed"):
            agent.wake(inst.model_name, operation_id="wake")
    else:
        agent.wake(inst.model_name, operation_id="wake")
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] == 100


def test_initial_kv_rejects_a_stale_restart_read(initial_kv, monkeypatch):
    import aibrix.runtime.model_runtime as runtime_module

    agent, inst, segment = initial_kv

    def stale_read(name):
        agent._schedule_restart(inst, "engine exited during observation")
        agent._now = lambda: inst.next_restart_at
        agent.supervise_once()
        return (100, 0, 0)

    monkeypatch.setattr(runtime_module, "read_kv_segment", stale_read)
    model = agent.snapshot()["models"][0]
    assert model["kv_initial_capacity_bytes"] is None
    assert model["ready"] is False
    assert model["kv_capacity_bytes"] == model["kv_used_bytes"] == -1
    monkeypatch.setattr(runtime_module, "read_kv_segment", lambda name: (80, 0, 0))
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] == 80


def test_initial_kv_missing_write_does_not_prevent_capture(initial_kv):
    import struct

    agent, inst, segment = initial_kv
    with pytest.raises(RuntimeError, match="startup KV capacity"):
        agent.set_kv_limit(inst.model_name, 200, operation_id="no-segment")
    segment.write_bytes(struct.pack("<3q", 100, 0, 0))
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] == 100


@pytest.mark.parametrize("pending_restart", [False, True])
def test_initial_kv_recovered_restart(initial_kv, monkeypatch, pending_restart):
    import struct
    from datetime import timedelta

    import aibrix.runtime.model_runtime as runtime_module
    from aibrix.runtime.engine_registry import EngineRegistry

    agent, inst, segment = initial_kv
    record = agent._registry_record(inst)
    record.pop("kv_initial_capacity_bytes", None)  # Pre-field registry format.
    if pending_restart:
        record.update(
            phase="restarting",
            restart_count=1,
            next_restart_at=(agent._now() - timedelta(seconds=1)).isoformat(),
        )
    registry = EngineRegistry(segment.parent / "engines.json")
    registry.save([record])
    monkeypatch.setattr(runtime_module, "pid_is_alive", lambda pid: not pending_restart)
    monkeypatch.setattr(
        runtime_module, "process_start_time", lambda pid: inst.pid_start_time
    )
    monkeypatch.setattr(runtime_module, "process_group_is_alive", lambda pid: False)
    monkeypatch.setattr(runtime_module, "instance_ready", lambda inst: True)
    launcher = MockEngineLauncher()
    recovered = ModelRuntime(launcher, registry=registry)
    recovered.supervise_once()
    segment.write_bytes(struct.pack("<3q", 80, 0, 0))

    model = recovered.snapshot()["models"][0]
    assert model["phase"] == "active"
    if pending_restart:
        assert launcher.launched == [inst.model_name]
        assert model["kv_initial_capacity_bytes"] == 80
    else:
        assert launcher.launched == []
        assert model["kv_initial_capacity_bytes"] is None


@pytest.mark.parametrize("capacity", [0, 100])
def test_initial_kv_persisted_across_agent_restart(initial_kv, monkeypatch, capacity):
    import struct
    from unittest.mock import Mock

    import aibrix.runtime.model_runtime as runtime_module
    from aibrix.runtime.engine_registry import EngineRegistry

    agent, inst, segment = initial_kv
    registry = EngineRegistry(segment.parent / "engines.json")
    agent._registry = registry
    inst.pid_start_time = "start"
    assert agent._registry_record(inst)["kv_initial_capacity_bytes"] is None
    monkeypatch.setattr(registry, "save", Mock(wraps=registry.save))
    segment.write_bytes(struct.pack("<3q", capacity, 0, 0))
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] == capacity
    assert registry.load()[0]["kv_initial_capacity_bytes"] == capacity
    agent.snapshot()
    registry.save.assert_called_once()
    monkeypatch.setattr(runtime_module, "pid_is_alive", lambda pid: True)
    monkeypatch.setattr(
        runtime_module, "process_start_time", lambda pid: inst.pid_start_time
    )
    monkeypatch.setattr(runtime_module, "instance_ready", lambda inst: True)
    recovered = ModelRuntime(MockEngineLauncher(), registry=registry)
    assert recovered.snapshot()["models"][0]["kv_initial_capacity_bytes"] == capacity
    segment.write_bytes(struct.pack("<3q", 80, 0, 0))
    model = recovered.snapshot()["models"][0]
    assert model["kv_capacity_bytes"] == 80
    assert model["kv_initial_capacity_bytes"] == capacity
    assert recovered._models[inst.model_name].kv_initial_capacity_unavailable
    assert recovered._launcher.launched == []
    registry.save.assert_called_once()


@pytest.mark.parametrize("phase", ["active", "sleeping"])
@pytest.mark.parametrize(
    "saved,current",
    [
        (None, "start"),
        ("start", None),
        (None, None),
        ("start", "start"),
        ("old", "new"),
    ],
)
def test_initial_kv_registry_requires_identity(
    initial_kv, monkeypatch, phase, saved, current
):
    import struct
    from unittest.mock import Mock

    import aibrix.runtime.model_runtime as runtime_module
    from aibrix.runtime.engine_registry import EngineRegistry

    agent, inst, segment = initial_kv
    registry = EngineRegistry(segment.parent / "engines.json")
    registry.save(
        [
            {
                **agent._registry_record(inst),
                "phase": phase,
                "pid_start_time": saved,
                "kv_initial_capacity_bytes": 100,
            }
        ]
    )
    monkeypatch.setattr(registry, "save", Mock(wraps=registry.save))
    monkeypatch.setattr(runtime_module, "pid_is_alive", lambda pid: True)
    monkeypatch.setattr(runtime_module, "process_start_time", lambda pid: current)
    monkeypatch.setattr(runtime_module, "process_group_is_alive", lambda pid: False)
    monkeypatch.setattr(runtime_module, "instance_ready", lambda inst: True)
    recovered = ModelRuntime(MockEngineLauncher(), registry=registry)
    expected = 100 if saved is not None and saved == current else None
    segment.write_bytes(struct.pack("<3q", 80, 0, 0))
    model = recovered.snapshot()["models"][0]
    assert model["kv_initial_capacity_bytes"] == expected
    assert registry.load()[0]["kv_initial_capacity_bytes"] == expected
    assert recovered._launcher.launched == []
    if saved is not None and current is not None and saved != current:
        assert model["phase"] == "restarting"
    else:
        assert model["phase"] == phase
        assert recovered._models[inst.model_name].kv_initial_capacity_unavailable
    assert registry.save.call_count == (expected is None)


@pytest.mark.parametrize("phase", ["failed", "stopping"])
def test_initial_kv_registry_early_branches(initial_kv, monkeypatch, phase):
    from unittest.mock import Mock

    import aibrix.runtime.model_runtime as runtime_module
    from aibrix.runtime.engine_registry import EngineRegistry

    agent, inst, segment = initial_kv
    registry = EngineRegistry(segment.parent / "engines.json")
    registry.save(
        [
            {
                **agent._registry_record(inst),
                "phase": phase,
                "pid_start_time": "start",
                "kv_initial_capacity_bytes": 100,
            }
        ]
    )
    # Even matching identity must not retain a terminal failed capacity.
    monkeypatch.setattr(runtime_module, "pid_is_alive", lambda pid: phase == "failed")
    monkeypatch.setattr(runtime_module, "process_start_time", lambda pid: "start")
    monkeypatch.setattr(
        runtime_module, "process_group_is_alive", lambda pid: phase == "stopping"
    )
    launcher = MockEngineLauncher()
    monkeypatch.setattr(launcher, "stop", Mock())
    recovered = ModelRuntime(launcher, registry=registry)
    model = recovered.snapshot()["models"][0]
    assert model["kv_initial_capacity_bytes"] is None
    assert registry.load()[0]["kv_initial_capacity_bytes"] is None
    assert model["phase"] == phase
    assert not model["alive"]
    assert recovered._models[inst.model_name].kv_initial_capacity_unavailable
    assert launcher.stop.call_count == (phase == "stopping")
    assert launcher.launched == []


def test_initial_kv_capture_save_retried(initial_kv, monkeypatch, caplog):
    import struct
    from unittest.mock import Mock

    import aibrix.runtime.model_runtime as runtime_module
    from aibrix.runtime.engine_registry import EngineRegistry

    agent, inst, segment = initial_kv
    registry = EngineRegistry(segment.parent / "engines.json")
    agent._registry = registry
    inst.pid_start_time = "start"
    agent._persist_locked()
    save = registry.save
    monkeypatch.setattr(
        registry, "save", Mock(side_effect=OSError("transient failure"))
    )
    segment.write_bytes(struct.pack("<3q", 100, 0, 0))
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] == 100
    assert "could not persist startup KV capacity" in caplog.text
    assert registry.load()[0]["kv_initial_capacity_bytes"] is None
    registry.save.assert_called_once()
    segment.write_bytes(struct.pack("<3q", 80, 0, 0))
    monkeypatch.setattr(registry, "save", Mock(wraps=save))
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] == 100
    assert registry.load()[0]["kv_initial_capacity_bytes"] == 100
    agent.snapshot()
    registry.save.assert_called_once()
    monkeypatch.setattr(runtime_module, "pid_is_alive", lambda pid: True)
    monkeypatch.setattr(runtime_module, "process_start_time", lambda pid: "start")
    monkeypatch.setattr(runtime_module, "instance_ready", lambda inst: True)
    recovered = ModelRuntime(MockEngineLauncher(), registry=registry)
    assert recovered.snapshot()["models"][0]["kv_initial_capacity_bytes"] == 100
    assert recovered._launcher.launched == []
    registry.save.assert_called_once()


@pytest.mark.parametrize("replacement", ["gone", "pid_reused", "pending_restart"])
def test_initial_kv_registry_capacity_cleared_for_replacement(
    initial_kv, monkeypatch, replacement
):
    import struct

    import aibrix.runtime.model_runtime as runtime_module
    from aibrix.runtime.engine_registry import EngineRegistry

    agent, inst, segment = initial_kv
    inst.pid_start_time = "old"
    record = {**agent._registry_record(inst), "kv_initial_capacity_bytes": 100}
    if replacement == "pending_restart":
        record.update(phase="restarting", restart_count=1, next_restart_at="2000-01-01")
    registry = EngineRegistry(segment.parent / "engines.json")
    registry.save([record])
    monkeypatch.setattr(
        runtime_module, "pid_is_alive", lambda pid: replacement == "pid_reused"
    )
    monkeypatch.setattr(runtime_module, "process_start_time", lambda pid: "new")
    monkeypatch.setattr(runtime_module, "process_group_is_alive", lambda pid: False)
    recovered = ModelRuntime(MockEngineLauncher(), registry=registry)
    assert recovered.snapshot()["models"][0]["kv_initial_capacity_bytes"] is None
    restored = recovered._models[inst.model_name]
    restart_at = restored.next_restart_at
    recovered._now = lambda: restart_at
    recovered.supervise_once()
    assert recovered._launcher.launched == [inst.model_name]
    assert recovered.snapshot()["models"][0]["kv_initial_capacity_bytes"] is None
    segment.write_bytes(struct.pack("<3q", 80, 0, 0))
    assert recovered.snapshot()["models"][0]["kv_initial_capacity_bytes"] == 80


@pytest.mark.parametrize("value", [None, -1, True, False, "100", 1.5, [], {}, 0, 100])
def test_initial_kv_registry_validates_optional_capacity(initial_kv, value):
    agent, inst, _ = initial_kv
    record = {**agent._registry_record(inst), "kv_initial_capacity_bytes": value}
    restored = agent._instance_from_registry_record(record)
    assert restored is not None
    expected = value if type(value) is int and value >= 0 else None
    assert restored.kv_initial_capacity_bytes == expected
    assert restored.kv_initial_capacity_unavailable


def test_initial_kv_restart_after_backoff_write(initial_kv, monkeypatch):
    import struct

    import aibrix.runtime.model_runtime as runtime_module

    agent, inst, segment = initial_kv
    segment.write_bytes(struct.pack("<3q", 100, 0, 0))
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] == 100
    monkeypatch.setattr(runtime_module, "process_group_is_alive", lambda pid: False)
    inst.proc = SimpleNamespace(poll=lambda: 1)
    agent.supervise_once()
    assert inst.phase == "restarting"
    assert inst.kv_initial_capacity_bytes is None
    with pytest.raises(RuntimeError, match="startup KV capacity"):
        agent.set_kv_limit(inst.model_name, 200, operation_id="backoff-write")
    assert not inst.kv_initial_capacity_unavailable
    restart_at = inst.next_restart_at
    agent._now = lambda: restart_at
    agent.supervise_once()
    assert agent._launcher.launched == [inst.model_name, inst.model_name]
    segment.write_bytes(struct.pack("<3q", 80, 0, 0))
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] == 80


@pytest.mark.parametrize("failure", [None, "sleep", "wake"])
def test_initial_kv_sleep_before_capture(initial_kv, monkeypatch, failure):
    import struct

    import aibrix.runtime.model_runtime as runtime_module

    agent, inst, segment = initial_kv
    monkeypatch.setattr(runtime_module, "process_group_is_alive", lambda pid: False)
    segment.write_bytes(struct.pack("<3q", 100, 0, 0))

    def failed_operation(inst, *args):
        raise RuntimeError(f"{failure} failed")

    if failure:
        original_operation = getattr(agent._launcher, failure)
        monkeypatch.setattr(agent._launcher, failure, failed_operation)
    if failure == "sleep":
        with pytest.raises(RuntimeError, match="sleep failed"):
            agent.sleep(inst.model_name, level=1, operation_id="sleep")
    else:
        agent.sleep(inst.model_name, level=1, operation_id="sleep")
        if failure == "wake":
            with pytest.raises(RuntimeError, match="wake failed"):
                agent.wake(inst.model_name, operation_id="wake")
            monkeypatch.setattr(agent._launcher, "wake", original_operation)
            agent.wake(inst.model_name, operation_id="wake-retry")
        else:
            agent.wake(inst.model_name, operation_id="wake")
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] is None

    agent.set_kv_limit(inst.model_name, 200, operation_id="unavailable-growth")
    assert struct.unpack("<3q", segment.read_bytes())[0] == 200

    # Only a replacement generation restores startup provenance.
    inst.proc = SimpleNamespace(poll=lambda: 1)
    inst.phase = "active"
    agent.supervise_once()
    restart_at = inst.next_restart_at
    agent._now = lambda: restart_at
    agent.supervise_once()
    segment.write_bytes(struct.pack("<3q", 80, 0, 0))
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] == 80


def test_initial_kv_capture_during_sleep(initial_kv, monkeypatch):
    import struct

    agent, inst, segment = initial_kv
    segment.write_bytes(struct.pack("<3q", 100, 0, 0))
    during_sleep = []

    def observe_sleep(inst, level):
        during_sleep.append(agent.snapshot()["models"][0]["kv_initial_capacity_bytes"])

    monkeypatch.setattr(agent._launcher, "sleep", observe_sleep)
    agent.sleep(inst.model_name, level=1, operation_id="sleep")
    assert during_sleep == [None]
    agent.wake(inst.model_name, operation_id="wake")
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] is None


@pytest.mark.parametrize("captured", [False, True])
def test_initial_kv_delayed_generation_request(initial_kv, captured):
    import struct

    from aibrix.runtime.model_runtime import UnsupportedModelControlError

    agent, inst, segment = initial_kv
    segment.write_bytes(struct.pack("<3q", 100, 0, 0))
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] == 100
    agent.set_kv_limit(inst.model_name, 60, operation_id="shrink")
    agent._schedule_restart(inst, "old generation exited")
    agent._now = lambda: inst.next_restart_at
    agent.supervise_once()
    segment.write_bytes(struct.pack("<3q", 80, 0, 0))
    if captured:
        assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] == 80
    else:
        with pytest.raises(UnsupportedModelControlError, match="startup KV capacity"):
            agent.set_kv_limit(inst.model_name, 100, operation_id="delayed-grow")
        assert inst.kv_initial_capacity_bytes is None
        assert not inst.kv_initial_capacity_unavailable
        assert struct.unpack("<3q", segment.read_bytes())[0] == 80
        assert "delayed-grow" not in inst.completed_operation_ids.get("kv-limit", [])
        assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] == 80
    result = agent.set_kv_limit(inst.model_name, 100, operation_id="delayed-grow")
    assert struct.unpack("<3q", segment.read_bytes())[0] == 80
    assert result.limit_bytes == 80
    assert agent.snapshot_metrics().kv_limit_applied_bytes[inst.model_name] == 80


@pytest.mark.parametrize("captured", [False, True])
def test_initial_kv_direct_api_ceiling(initial_kv, monkeypatch, captured):
    import json
    import struct

    from fastapi import HTTPException

    import aibrix.runtime.model_runtime_api as api
    from aibrix.openapi.protocol import SetRuntimeModelKVLimitRequest

    agent, inst, segment = initial_kv
    segment.write_bytes(struct.pack("<3q", 100, 0, 0))
    monkeypatch.setattr(api, "get_model_runtime", lambda: agent)
    request = SetRuntimeModelKVLimitRequest(
        model_name=inst.model_name, limit_bytes=200, operation_id="direct"
    )
    if not captured:
        with pytest.raises(HTTPException) as exc:
            api.set_runtime_model_kv_limit(request)
        assert exc.value.status_code == 409
        assert "startup KV capacity" in exc.value.detail
        assert struct.unpack("<3q", segment.read_bytes())[0] == 100
        return
    assert agent.snapshot()["models"][0]["kv_initial_capacity_bytes"] == 100
    response = api.set_runtime_model_kv_limit(request)
    assert response.status_code == 200
    assert struct.unpack("<3q", segment.read_bytes())[0] == 100
    assert json.loads(response.body)["limit_bytes"] == 100
    assert agent.snapshot_metrics().kv_limit_applied_bytes[inst.model_name] == 100

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
"""Unit tests for the Runtime seam + registry (job lifecycle axis A)."""

import asyncio
import inspect
from datetime import datetime, timezone
from types import SimpleNamespace

import pytest

from aibrix.batch.job_driver.driver import TerminateResult
from aibrix.batch.job_driver.runtime import (
    RUNTIME_WAIT_MODE_PROVISION,
    RUNTIME_WAIT_MODE_RECONNECT,
    Endpoint,
    ExternalRuntime,
    NoopRuntime,
    Runtime,
    RuntimeBase,
    create_runtime,
    register_runtime,
    registered_runtimes,
)
from aibrix.batch.job_driver.runtime import base as runtime_base_mod
from aibrix.batch.job_entity import (
    BatchJobError,
    BatchJobErrorCode,
    BatchJobState,
    BatchJobStatus,
    BatchJobStatusCopy,
    JobRuntimeRef,
    RequestCountStats,
)
from aibrix.context import InfrastructureContext


class _FakeProgressManager:
    async def get_job(self, job_id: str) -> object | None:
        del job_id
        return None

    async def update_job_local_status(
        self, job_id: str, worker_id: str, status, update_keys=None
    ) -> object:
        del worker_id, update_keys
        return SimpleNamespace(job_id=job_id, status=status)


def _fake_worker_id_generator(owner_ref: str) -> str:
    return f"{owner_ref}-worker-1"


def _make_test_job(
    job_id: str = "j",
    *,
    status: BatchJobStatus | None = None,
    execution: dict[str, JobRuntimeRef] | None = None,
    request_counts: RequestCountStats | None = None,
    usage=None,
    opts: dict[str, object] | None = None,
) -> object:
    return SimpleNamespace(
        job_id=job_id,
        status=status
        or BatchJobStatus(
            jobID=job_id,
            state=BatchJobState.CREATED,
            createdAt=datetime.now(timezone.utc),
            requestCounts=request_counts or RequestCountStats(),
            usage=usage,
            execution=execution,
        ),
        spec=SimpleNamespace(opts=dict(opts or {})),
    )


def _make_deleted_job(job_id: str, *, cancelled: bool = False) -> object:
    return SimpleNamespace(
        job_id=job_id,
        status=SimpleNamespace(check_condition=lambda *_: cancelled),
    )


async def _maybe_await(result):
    if inspect.isawaitable(result):
        return await result
    return result


class _R(RuntimeBase):
    provisions = True

    def __init__(
        self,
        *,
        provision=None,
        wait_ready=None,
        check_liveness=None,
        connect=None,
        teardown=None,
    ) -> None:
        super().__init__(InfrastructureContext())
        self._provision_hook = provision
        self._wait_ready_hook = wait_ready
        self._check_liveness_hook = check_liveness
        self._connect_hook = connect
        self._teardown_hook = teardown

    async def _provision(self, job, job_id):
        if self._provision_hook is None:
            return "handle"
        return await _maybe_await(self._provision_hook(job, job_id))

    async def _wait_ready(self, handle, wait_mode="provision"):
        if self._wait_ready_hook is None:
            return None
        parameters = inspect.signature(self._wait_ready_hook).parameters
        if "wait_mode" in parameters:
            return await _maybe_await(
                self._wait_ready_hook(handle, wait_mode=wait_mode)
            )
        return await _maybe_await(self._wait_ready_hook(handle))

    async def _check_liveness(self, handle):
        if self._check_liveness_hook is None:
            return await super()._check_liveness(handle)
        return await _maybe_await(self._check_liveness_hook(handle))

    async def _connect(self, handle):
        if self._connect_hook is None:
            return Endpoint(source=None, model_name="m")
        return await _maybe_await(self._connect_hook(handle))

    async def _teardown(self, handle):
        if self._teardown_hook is None:
            return None
        return await _maybe_await(self._teardown_hook(handle))

    def _build_runtime_ref(self, job):
        del job
        return None


@pytest.mark.asyncio
async def test_runtime_base_session_runs_four_phases_in_order():
    calls: list[str] = []

    async def _provision(job, job_id):
        del job, job_id
        calls.append("provision")
        return "handle"

    async def _wait_ready(handle):
        assert handle == "handle"
        calls.append("wait_ready")

    async def _connect(handle):
        del handle
        calls.append("connect")
        return Endpoint(source=None, model_name="m")

    async def _teardown(handle):
        del handle
        calls.append("teardown")

    runtime = _R(
        provision=_provision,
        wait_ready=_wait_ready,
        connect=_connect,
        teardown=_teardown,
    )
    async with runtime.session(
        job=_make_test_job(),
        job_id="j",
        progress_manager=_FakeProgressManager(),
        worker_id_generator=_fake_worker_id_generator,
    ) as endpoint:
        calls.append("body")
        assert endpoint.model_name == "m"
        assert endpoint.source is None

    assert calls == ["provision", "wait_ready", "connect", "body", "teardown"]


@pytest.mark.asyncio
async def test_runtime_base_session_forwards_provision_wait_mode_on_initial_provision():
    wait_modes: list[str] = []

    async def _wait_ready(handle, *, wait_mode="provision"):
        del handle
        wait_modes.append(wait_mode)

    runtime = _R(wait_ready=_wait_ready)

    async with runtime.session(
        job=_make_test_job(),
        job_id="j",
        progress_manager=_FakeProgressManager(),
        worker_id_generator=_fake_worker_id_generator,
    ):
        pass

    assert wait_modes == [RUNTIME_WAIT_MODE_PROVISION]


@pytest.mark.asyncio
async def test_runtime_base_teardown_runs_even_when_body_raises():
    torn_down = []

    runtime = _R(
        provision=lambda job, job_id: "h",
        teardown=lambda handle: torn_down.append(handle),
    )
    with pytest.raises(ValueError):
        async with runtime.session(
            job=_make_test_job(),
            job_id="j",
            progress_manager=_FakeProgressManager(),
            worker_id_generator=_fake_worker_id_generator,
        ):
            raise ValueError("boom")

    assert torn_down == ["h"]


@pytest.mark.asyncio
async def test_runtime_base_terminate_before_session_start_short_circuits_session():
    calls: list[str] = []
    runtime = _R(
        provision=lambda job, job_id: calls.append(f"provision:{job_id}"),
        wait_ready=lambda handle: calls.append(f"wait_ready:{handle}"),
        connect=lambda handle: calls.append(f"connect:{handle}"),
        teardown=lambda handle: calls.append(f"teardown:{handle}"),
    )
    job = _make_test_job(job_id="job-1")

    deleted = await runtime.terminate(SimpleNamespace(job_id="job-1"))

    assert deleted == TerminateResult.ACCEPTED
    with pytest.raises(asyncio.CancelledError):
        async with runtime.session(
            job=job,
            job_id="job-1",
            progress_manager=_FakeProgressManager(),
            worker_id_generator=_fake_worker_id_generator,
        ):
            pytest.fail("session body should not run after pre-start termination")

    assert calls == []
    assert runtime.cancelled() is True


@pytest.mark.asyncio
async def test_runtime_base_session_with_new_id_clears_previous_stop_state():
    runtime = _R()

    deleted = await runtime.terminate(_make_deleted_job("job-1"))

    assert deleted == TerminateResult.ACCEPTED
    async with runtime.session(
        job=_make_test_job(job_id="job-2"),
        job_id="job-2",
        progress_manager=_FakeProgressManager(),
        worker_id_generator=_fake_worker_id_generator,
    ) as endpoint:
        assert endpoint.model_name == "m"
        assert endpoint.source is None

    assert runtime.cancelled() is False


@pytest.mark.asyncio
async def test_runtime_base_terminate_different_id_outside_session_after_previous_outside_terminate():
    runtime = _R()

    deleted_first = await runtime.terminate(_make_deleted_job("job-1"))
    deleted_second = await runtime.terminate(_make_deleted_job("job-2"))

    assert deleted_first == TerminateResult.ACCEPTED
    assert deleted_second == TerminateResult.ACCEPTED


@pytest.mark.asyncio
async def test_runtime_base_terminate_different_id_outside_session_after_session_succeeds():
    runtime = _R()

    async with runtime.session(
        job=_make_test_job(job_id="job-1"),
        job_id="job-1",
        progress_manager=_FakeProgressManager(),
        worker_id_generator=_fake_worker_id_generator,
    ):
        pass

    deleted = await runtime.terminate(_make_deleted_job("job-2"))

    assert deleted == TerminateResult.ACCEPTED


@pytest.mark.asyncio
async def test_runtime_base_terminate_different_id_outside_session_after_terminated_session_succeeds():
    wait_entered = asyncio.Event()
    runtime = _R()

    async def _wait_ready(handle):
        del handle
        wait_entered.set()
        await runtime._stop_requested.wait()
        raise asyncio.CancelledError

    runtime._wait_ready_hook = _wait_ready

    async def _run_session():
        async with runtime.session(
            job=_make_test_job(job_id="job-1"),
            job_id="job-1",
            progress_manager=_FakeProgressManager(),
            worker_id_generator=_fake_worker_id_generator,
        ):
            pytest.fail("session body should not run after in-session termination")

    task = asyncio.create_task(_run_session())
    await asyncio.wait_for(wait_entered.wait(), timeout=1)
    deleted_inside = await runtime.terminate(_make_deleted_job("job-1"))

    assert deleted_inside == TerminateResult.ACCEPTED
    with pytest.raises(asyncio.CancelledError):
        await task

    deleted_outside = await runtime.terminate(_make_deleted_job("job-2"))

    assert deleted_outside == TerminateResult.ACCEPTED


@pytest.mark.asyncio
async def test_runtime_base_terminate_same_id_outside_session_after_terminated_session_is_duplicate():
    wait_entered = asyncio.Event()
    runtime = _R()

    async def _wait_ready(handle):
        del handle
        wait_entered.set()
        await runtime._stop_requested.wait()
        raise asyncio.CancelledError

    runtime._wait_ready_hook = _wait_ready

    async def _run_session():
        async with runtime.session(
            job=_make_test_job(job_id="job-1"),
            job_id="job-1",
            progress_manager=_FakeProgressManager(),
            worker_id_generator=_fake_worker_id_generator,
        ):
            pytest.fail("session body should not run after in-session termination")

    task = asyncio.create_task(_run_session())
    await asyncio.wait_for(wait_entered.wait(), timeout=1)
    deleted_inside = await runtime.terminate(_make_deleted_job("job-1"))

    assert deleted_inside == TerminateResult.ACCEPTED
    with pytest.raises(asyncio.CancelledError):
        await task

    deleted_outside = await runtime.terminate(_make_deleted_job("job-1"))

    assert deleted_outside == TerminateResult.ALREADY_REQUESTED


@pytest.mark.asyncio
async def test_runtime_base_terminate_same_id_inside_session_succeeds():
    wait_entered = asyncio.Event()
    runtime = _R()

    async def _wait_ready(handle):
        del handle
        wait_entered.set()
        await runtime._stop_requested.wait()
        raise asyncio.CancelledError

    runtime._wait_ready_hook = _wait_ready

    async def _run_session():
        async with runtime.session(
            job=_make_test_job(job_id="job-1"),
            job_id="job-1",
            progress_manager=_FakeProgressManager(),
            worker_id_generator=_fake_worker_id_generator,
        ):
            pytest.fail("session body should not run after in-session termination")

    task = asyncio.create_task(_run_session())
    await asyncio.wait_for(wait_entered.wait(), timeout=1)

    deleted = await runtime.terminate(_make_deleted_job("job-1"))

    assert deleted == TerminateResult.ACCEPTED
    with pytest.raises(asyncio.CancelledError):
        await task
    assert runtime.cancelled() is True


@pytest.mark.asyncio
async def test_runtime_base_terminate_different_id_inside_session_is_rejected():
    wait_entered = asyncio.Event()
    allow_ready = asyncio.Event()
    runtime = _R()

    async def _wait_ready(handle):
        del handle
        wait_entered.set()
        await allow_ready.wait()

    runtime._wait_ready_hook = _wait_ready

    async def _run_session():
        async with runtime.session(
            job=_make_test_job(job_id="job-1"),
            job_id="job-1",
            progress_manager=_FakeProgressManager(),
            worker_id_generator=_fake_worker_id_generator,
        ) as endpoint:
            assert endpoint.model_name == "m"

    task = asyncio.create_task(_run_session())
    await asyncio.wait_for(wait_entered.wait(), timeout=1)

    deleted = await runtime.terminate(_make_deleted_job("job-2"))

    assert deleted == TerminateResult.REJECTED
    allow_ready.set()
    await task
    assert runtime.cancelled() is False


@pytest.mark.asyncio
async def test_runtime_base_session_passes_reconnect_wait_mode_on_reconnect_attempt():
    execution = JobRuntimeRef(driverType="base", reconnectPayload={})
    job = _make_test_job(job_id="job-1", execution={"base": execution})
    wait_calls: list[tuple[str, str]] = []

    class _ReconnectRuntime(_R):
        async def _load_handle(self, job, job_id, runtime_ref):
            del job, job_id
            assert runtime_ref is execution
            return "reconnected-handle"

    async def _wait_ready(handle, *, wait_mode=RUNTIME_WAIT_MODE_PROVISION):
        wait_calls.append((handle, wait_mode))

    runtime = _ReconnectRuntime(wait_ready=_wait_ready)

    async with runtime.session(
        job=job,
        job_id="job-1",
        progress_manager=_FakeProgressManager(),
        worker_id_generator=_fake_worker_id_generator,
    ) as endpoint:
        assert endpoint.model_name == "m"

    assert wait_calls == [("reconnected-handle", RUNTIME_WAIT_MODE_RECONNECT)]


@pytest.mark.asyncio
async def test_runtime_base_session_retries_provision_with_exponential_backoff(
    monkeypatch,
):
    calls: list[object] = []
    sleeps: list[float] = []

    async def _fake_sleep(delay: float) -> None:
        sleeps.append(delay)

    monkeypatch.setattr(runtime_base_mod.asyncio, "sleep", _fake_sleep)

    class _TemporaryProvisionError(RuntimeError):
        pass

    attempt = 0

    async def _provision(job, job_id):
        nonlocal attempt
        del job, job_id
        attempt += 1
        calls.append(("provision", attempt))
        if attempt < 4:
            raise _TemporaryProvisionError("temporary failure")
        return f"handle-{attempt}"

    runtime = _R(
        provision=_provision,
        wait_ready=lambda handle: calls.append(("wait_ready", handle)),
        connect=lambda handle: (
            calls.append(("connect", handle)),
            Endpoint(source=None, model_name="m"),
        )[1],
        teardown=lambda handle: calls.append(("teardown", handle)),
    )
    async with runtime.session(
        job=_make_test_job(),
        job_id="j",
        progress_manager=_FakeProgressManager(),
        worker_id_generator=_fake_worker_id_generator,
    ) as endpoint:
        calls.append("body")
        assert endpoint.model_name == "m"

    assert sleeps == [2.0, 4.0, 8.0]
    assert calls == [
        ("provision", 1),
        ("provision", 2),
        ("provision", 3),
        ("provision", 4),
        ("wait_ready", "handle-4"),
        ("connect", "handle-4"),
        "body",
        ("teardown", "handle-4"),
    ]


@pytest.mark.asyncio
async def test_runtime_base_session_retries_wait_ready_batch_job_not_found(
    monkeypatch,
):
    calls: list[tuple[str, str]] = []
    sleeps: list[float] = []

    async def _fake_sleep(delay: float) -> None:
        sleeps.append(delay)

    monkeypatch.setattr(runtime_base_mod.asyncio, "sleep", _fake_sleep)

    attempt = 0

    async def _provision(job, job_id):
        nonlocal attempt
        del job, job_id
        attempt += 1
        handle = f"handle-{attempt}"
        calls.append(("provision", handle))
        return handle

    async def _wait_ready(handle):
        calls.append(("wait_ready", handle))
        if handle == "handle-1":
            raise BatchJobError(
                code=BatchJobErrorCode.RESOURCE_NOTFOUND_ERROR,
                message="runtime not found",
            )

    runtime = _R(
        provision=_provision,
        wait_ready=_wait_ready,
        connect=lambda handle: (
            calls.append(("connect", handle)),
            Endpoint(source=None, model_name="m"),
        )[1],
        teardown=lambda handle: calls.append(("teardown", handle)),
    )
    async with runtime.session(
        job=_make_test_job(),
        job_id="j",
        progress_manager=_FakeProgressManager(),
        worker_id_generator=_fake_worker_id_generator,
    ) as endpoint:
        assert endpoint.model_name == "m"

    assert sleeps == [2.0]
    assert calls == [
        ("provision", "handle-1"),
        ("wait_ready", "handle-1"),
        ("provision", "handle-2"),
        ("wait_ready", "handle-2"),
        ("connect", "handle-2"),
        ("teardown", "handle-2"),
    ]


@pytest.mark.asyncio
async def test_runtime_base_session_ignores_teardown_failure_during_retry(
    monkeypatch,
):
    calls: list[tuple[str, str]] = []
    sleeps: list[float] = []
    warnings: list[dict[str, object]] = []

    async def _fake_sleep(delay: float) -> None:
        sleeps.append(delay)

    def _fake_warning(message: str, **kwargs) -> None:
        warnings.append({"message": message, **kwargs})

    monkeypatch.setattr(runtime_base_mod.asyncio, "sleep", _fake_sleep)
    monkeypatch.setattr(runtime_base_mod.logger, "warning", _fake_warning)

    class _RetryableReadyError(RuntimeError):
        pass

    attempt = 0

    async def _provision(job, job_id):
        nonlocal attempt
        del job, job_id
        attempt += 1
        handle = f"handle-{attempt}"
        calls.append(("provision", handle))
        return handle

    async def _wait_ready(handle):
        calls.append(("wait_ready", handle))
        if handle == "handle-1":
            raise _RetryableReadyError("try again")

    async def _teardown(handle):
        calls.append(("teardown", handle))
        if handle == "handle-1":
            raise RuntimeError("teardown failed")

    class _RetryRuntime(_R):
        def _should_teardown_failed_wait_ready(self, exc: Exception) -> bool:
            del exc
            return True

    runtime = _RetryRuntime(
        provision=_provision,
        wait_ready=_wait_ready,
        connect=lambda handle: (
            calls.append(("connect", handle)),
            Endpoint(source=None, model_name="m"),
        )[1],
        teardown=_teardown,
    )
    async with runtime.session(
        job=_make_test_job(job_id="job-1"),
        job_id="job-1",
        progress_manager=_FakeProgressManager(),
        worker_id_generator=_fake_worker_id_generator,
    ) as endpoint:
        assert endpoint.model_name == "m"

    assert sleeps == [2.0]
    assert calls == [
        ("provision", "handle-1"),
        ("wait_ready", "handle-1"),
        ("teardown", "handle-1"),
        ("provision", "handle-2"),
        ("wait_ready", "handle-2"),
        ("connect", "handle-2"),
        ("teardown", "handle-2"),
    ]
    assert warnings == [
        {
            "message": "Runtime teardown failed during retry recovery; continuing with retry",
            "job_id": "job-1",
            "phase": "wait_ready",
            "handle": "'handle-1'",
            "error": "teardown failed",
        }
    ]


@pytest.mark.asyncio
async def test_runtime_base_session_periodically_checks_liveness_and_surfaces_failure():
    third_check_started = asyncio.Event()
    calls: list[tuple[str, str]] = []

    async def _check_liveness(handle):
        calls.append(("check_liveness", handle))
        if len(calls) == 3:
            third_check_started.set()
        raise RuntimeError("runtime lost")

    async def _teardown(handle):
        calls.append(("teardown", handle))

    class _LivenessRuntime(_R):
        session_liveness_check_interval_s = 0.01

    runtime = _LivenessRuntime(
        check_liveness=_check_liveness,
        teardown=_teardown,
    )

    with pytest.raises(RuntimeError, match="runtime lost"):
        async with runtime.session(
            job=_make_test_job(job_id="job-1"),
            job_id="job-1",
            progress_manager=_FakeProgressManager(),
            worker_id_generator=_fake_worker_id_generator,
        ):
            await asyncio.wait_for(third_check_started.wait(), timeout=1)
            await asyncio.sleep(1)

    assert calls == [
        ("check_liveness", "handle"),
        ("check_liveness", "handle"),
        ("check_liveness", "handle"),
        ("teardown", "handle"),
    ]


@pytest.mark.asyncio
async def test_runtime_base_session_liveness_uses_runtime_failure_threshold_override():
    second_check_started = asyncio.Event()
    calls: list[tuple[str, str]] = []

    async def _check_liveness(handle):
        calls.append(("check_liveness", handle))
        if len(calls) == 2:
            second_check_started.set()
        raise RuntimeError("runtime lost")

    async def _teardown(handle):
        calls.append(("teardown", handle))

    class _LivenessRuntime(_R):
        session_liveness_check_interval_s = 0.01
        session_liveness_failure_threshold = 2

    runtime = _LivenessRuntime(
        check_liveness=_check_liveness,
        teardown=_teardown,
    )

    with pytest.raises(RuntimeError, match="runtime lost"):
        async with runtime.session(
            job=_make_test_job(job_id="job-1"),
            job_id="job-1",
            progress_manager=_FakeProgressManager(),
            worker_id_generator=_fake_worker_id_generator,
        ):
            await asyncio.wait_for(second_check_started.wait(), timeout=1)
            await asyncio.sleep(1)

    assert calls == [
        ("check_liveness", "handle"),
        ("check_liveness", "handle"),
        ("teardown", "handle"),
    ]


@pytest.mark.asyncio
async def test_runtime_base_session_liveness_uses_global_failure_threshold(
    monkeypatch,
):
    second_check_started = asyncio.Event()
    calls: list[tuple[str, str]] = []

    async def _check_liveness(handle):
        calls.append(("check_liveness", handle))
        if len(calls) == 2:
            second_check_started.set()
        raise RuntimeError("runtime lost")

    async def _teardown(handle):
        calls.append(("teardown", handle))

    class _LivenessRuntime(_R):
        session_liveness_check_interval_s = 0.01

    monkeypatch.setattr(
        runtime_base_mod.envs, "BATCH_SESSION_LIVENESS_FAILURE_THRESHOLD", 2
    )
    runtime = _LivenessRuntime(
        check_liveness=_check_liveness,
        teardown=_teardown,
    )

    with pytest.raises(RuntimeError, match="runtime lost"):
        async with runtime.session(
            job=_make_test_job(job_id="job-1"),
            job_id="job-1",
            progress_manager=_FakeProgressManager(),
            worker_id_generator=_fake_worker_id_generator,
        ):
            await asyncio.wait_for(second_check_started.wait(), timeout=1)
            await asyncio.sleep(1)

    assert calls == [
        ("check_liveness", "handle"),
        ("check_liveness", "handle"),
        ("teardown", "handle"),
    ]


@pytest.mark.asyncio
async def test_runtime_base_session_preserves_liveness_error_when_teardown_fails():
    liveness_failure_seen = asyncio.Event()

    async def _check_liveness(handle):
        del handle
        liveness_failure_seen.set()
        raise RuntimeError("runtime lost")

    async def _teardown(handle):
        del handle
        raise RuntimeError("teardown failed")

    class _LivenessRuntime(_R):
        session_liveness_check_interval_s = 0.01

    runtime = _LivenessRuntime(
        check_liveness=_check_liveness,
        teardown=_teardown,
    )

    with pytest.raises(RuntimeError, match="runtime lost"):
        async with runtime.session(
            job=_make_test_job(job_id="job-1"),
            job_id="job-1",
            progress_manager=_FakeProgressManager(),
            worker_id_generator=_fake_worker_id_generator,
        ):
            await asyncio.wait_for(liveness_failure_seen.wait(), timeout=1)
            await asyncio.sleep(1)


@pytest.mark.asyncio
async def test_runtime_base_session_reprovisions_after_reconnect_not_found(
    monkeypatch,
):
    calls: list[tuple[str, str]] = []
    sleeps: list[float] = []

    async def _fake_sleep(delay: float) -> None:
        sleeps.append(delay)

    monkeypatch.setattr(runtime_base_mod.asyncio, "sleep", _fake_sleep)

    execution = JobRuntimeRef(driverType="base")
    job = _make_test_job(job_id="job-1", execution={"base": execution})

    async def _load_handle(job, job_id, runtime_ref):
        del job
        assert runtime_ref is execution
        calls.append(("reconnect", job_id))
        return "reconnected-handle"

    async def _persist_runtime_ref(
        job,
        *,
        progress_manager=None,
        worker_id_generator=None,
    ):
        assert progress_manager is not None
        assert worker_id_generator is not None
        assert worker_id_generator("base") == "base-worker-1"
        calls.append(("persist", job.job_id))
        return job, "base-worker-1"

    async def _wait_ready(handle):
        calls.append(("wait_ready", handle))
        if handle == "reconnected-handle":
            raise BatchJobError(
                code=BatchJobErrorCode.RESOURCE_NOTFOUND_ERROR,
                message="runtime not found",
            )

    class _ReconnectRuntime(_R):
        async def _load_handle(self, job, job_id, runtime_ref):
            return await _load_handle(job, job_id, runtime_ref)

        async def _persist_runtime_ref(
            self,
            job,
            *,
            progress_manager=None,
            worker_id_generator=None,
        ):
            return await _persist_runtime_ref(
                job,
                progress_manager=progress_manager,
                worker_id_generator=worker_id_generator,
            )

    runtime = _ReconnectRuntime(
        provision=lambda job, job_id: (
            calls.append(("provision", job_id)),
            "fresh-handle",
        )[1],
        wait_ready=_wait_ready,
        connect=lambda handle: (
            calls.append(("connect", handle)),
            Endpoint(source=None, model_name="m"),
        )[1],
        teardown=lambda handle: calls.append(("teardown", handle)),
    )
    async with runtime.session(
        job=job,
        job_id="job-1",
        progress_manager=_FakeProgressManager(),
        worker_id_generator=_fake_worker_id_generator,
    ) as endpoint:
        assert endpoint.model_name == "m"

    assert sleeps == [2.0]
    assert calls == [
        ("reconnect", "job-1"),
        ("persist", "job-1"),
        ("wait_ready", "reconnected-handle"),
        ("provision", "job-1"),
        ("persist", "job-1"),
        ("wait_ready", "fresh-handle"),
        ("connect", "fresh-handle"),
    ]


@pytest.mark.asyncio
async def test_runtime_base_session_raises_if_execution_tracking_not_bound():
    job = _make_test_job(job_id="job-1")

    class _PersistingRuntime(_R):
        def _build_runtime_ref(self, job):
            existing = self._load_runtime_ref(job)
            now = datetime.now(timezone.utc)
            return JobRuntimeRef(
                driverType="base",
                attempt=existing.attempt if existing is not None else 1,
                ownerRef="owner-ref",
                reconnectPayload={},
                connectedAt=existing.connected_at if existing is not None else now,
                heartbeatAt=now,
            )

    runtime = _PersistingRuntime(provision=lambda job, job_id: "handle")
    with pytest.raises(
        RuntimeError,
        match="Execution tracking is not configured for session\\(\\)",
    ):
        async with runtime.session(job=job, job_id="job-1"):
            pytest.fail("session should fail before entering body")


@pytest.mark.asyncio
async def test_persist_runtime_ref_updates_execution_only():
    class _CapturingProgressManager:
        def __init__(self) -> None:
            self.last_status: BatchJobStatus | None = None
            self.last_worker_id: str | None = None
            self.last_update_keys = None
            self.execution: dict[str, JobRuntimeRef] | None = None
            self.status_copy: BatchJobStatusCopy | None = None

        async def update_job_local_status(
            self,
            job_id: str,
            worker_id: str,
            status: BatchJobStatus,
            update_keys=None,
        ):
            assert job_id == "job-1"
            self.last_worker_id = worker_id
            self.last_status = status
            self.last_update_keys = update_keys
            self.execution = status.execution
            self.status_copy = BatchJobStatusCopy.from_status(status)
            return SimpleNamespace(job_id=job_id, status=status)

    class _PersistingRuntime(_R):
        def _build_runtime_ref(self, job):
            del job
            return JobRuntimeRef(
                driverType="base",
                ownerRef="owner-ref",
            )

    progress_manager = _CapturingProgressManager()
    job = _make_test_job(
        job_id="job-1",
        request_counts=RequestCountStats(
            total=10000,
            launched=3166,
            completed=3152,
            failed=0,
        ),
        usage={"total_tokens": 1},
    )
    runtime = _PersistingRuntime()

    persisted, worker_id = await runtime._persist_runtime_ref(
        job,
        progress_manager=progress_manager,
        worker_id_generator=_fake_worker_id_generator,
    )

    assert worker_id == "owner-ref-worker-1"
    assert progress_manager.last_worker_id == "owner-ref-worker-1"
    assert progress_manager.execution is not None
    assert progress_manager.status_copy is not None
    assert progress_manager.execution["base"].owner_worker_id == "owner-ref-worker-1"
    assert progress_manager.status_copy.request_counts.total == 0
    assert progress_manager.status_copy.request_counts.launched == 0
    assert progress_manager.status_copy.request_counts.completed == 0
    assert progress_manager.status_copy.request_counts.failed == 0
    assert progress_manager.status_copy.usage is None
    assert progress_manager.last_update_keys == {"execution"}
    assert persisted.status is progress_manager.last_status


@pytest.mark.asyncio
async def test_persist_runtime_ref_clears_delete_started_marker():
    class _CapturingProgressManager:
        def __init__(self) -> None:
            self.execution = None

        async def update_job_local_status(
            self, job_id: str, worker_id: str, status, update_keys=None
        ):
            del job_id, worker_id, update_keys
            self.execution = status.execution
            return SimpleNamespace(job_id="job-1", status=status)

    now = datetime.now(timezone.utc)
    existing = JobRuntimeRef(
        driverType="base",
        ownerRef="owner-ref",
        ownerWorkerId="owner-ref-worker-1",
        deleteStartedAt=now,
        heartbeatAt=now,
    )

    class _PersistingRuntime(_R):
        def _build_runtime_ref(self, job):
            existing_ref = self._load_runtime_ref(job)
            assert existing_ref is not None
            return existing_ref.model_copy(deep=True)

    progress_manager = _CapturingProgressManager()
    job = _make_test_job(job_id="job-1", execution={"base": existing})
    runtime = _PersistingRuntime()

    await runtime._persist_runtime_ref(
        job,
        progress_manager=progress_manager,
        worker_id_generator=_fake_worker_id_generator,
    )

    assert progress_manager.execution is not None
    persisted_ref = progress_manager.execution["base"]
    assert persisted_ref.delete_started_at is None


@pytest.mark.asyncio
async def test_session_leaves_runtime_ref_persisted_on_exit_without_cas_clear():
    class _CapturingProgressManager:
        def __init__(self) -> None:
            self.calls: list[tuple[str, BatchJobStatus, object]] = []
            self.current_job: object | None = None

        async def get_job(self, job_id: str):
            del job_id
            return self.current_job

        async def update_job_local_status(
            self,
            job_id: str,
            worker_id: str,
            status: BatchJobStatus,
            update_keys=None,
        ):
            self.current_job = SimpleNamespace(job_id=job_id, status=status)
            self.calls.append((worker_id, status.model_copy(deep=True), update_keys))
            return SimpleNamespace(job_id=job_id, status=status)

    class _PersistingRuntime(_R):
        def _build_runtime_ref(self, job):
            del job
            return JobRuntimeRef(
                driverType="base",
                ownerRef="owner-ref",
            )

    progress_manager = _CapturingProgressManager()
    runtime = _PersistingRuntime()
    job = _make_test_job(job_id="job-1")

    async with runtime.session(
        job=job,
        job_id="job-1",
        progress_manager=progress_manager,
        worker_id_generator=_fake_worker_id_generator,
    ):
        pass

    assert len(progress_manager.calls) == 2
    first_worker_id, first_status, first_update_keys = progress_manager.calls[0]
    assert first_worker_id == "owner-ref-worker-1"
    first_ref = first_status.get_runtime_ref("base")
    assert first_ref is not None
    assert first_ref.delete_started_at is None
    assert first_update_keys == {"execution"}
    second_worker_id, second_status, second_update_keys = progress_manager.calls[1]
    assert second_worker_id == "owner-ref-worker-1"
    second_ref = second_status.get_runtime_ref("base")
    assert second_ref is not None
    assert second_ref.delete_started_at is not None
    assert second_ref.deleted_at is None
    assert second_update_keys == {"execution"}


def test_runtime_ref_lease_uses_double_liveness_interval():
    runtime = _R()
    runtime.session_liveness_check_interval_s = 7.5

    assert runtime._runtime_ref_lease_seconds() == 15.0


def test_touch_runtime_ref_heartbeat_only_updates_matching_runtime_key():
    runtime = _R()
    now = datetime.now(timezone.utc)
    target_ref = JobRuntimeRef(
        driverType="base",
        ownerRef="owner-ref",
        ownerWorkerId="owner-ref-worker-1",
    )
    other_ref = JobRuntimeRef(
        driverType="other-runtime",
        ownerRef="other-ref",
        ownerWorkerId="owner-ref-worker-1",
    )
    status = BatchJobStatus(
        jobID="job-1",
        state=BatchJobState.IN_PROGRESS,
        createdAt=now,
        execution={
            "base": target_ref,
            "other-runtime": other_ref,
        },
    )

    runtime.touch_runtime_ref_heartbeat(
        _make_test_job(job_id="job-1"),
        status,
        worker_id="owner-ref-worker-1",
        now=now,
    )

    assert status.get_runtime_ref("base").heartbeat_at == now
    assert status.get_runtime_ref("other-runtime").heartbeat_at is None


@pytest.mark.asyncio
async def test_refresh_runtime_ref_heartbeat_skips_recent_heartbeat():
    now = datetime.now(timezone.utc)
    existing = JobRuntimeRef(
        driverType="base",
        ownerRef="owner-ref",
        ownerWorkerId="owner-ref-worker-1",
        heartbeatAt=now,
    )
    job = _make_test_job(job_id="job-1", execution={"base": existing})

    class _CapturingProgressManager:
        def __init__(self) -> None:
            self.updated = False

        async def get_job(self, job_id: str):
            del job_id
            return job

        async def update_job_local_status(
            self, job_id: str, worker_id: str, status, update_keys=None
        ):
            del job_id, worker_id, status, update_keys
            self.updated = True
            return job

    runtime = _R()
    runtime.session_liveness_check_interval_s = 30.0
    progress_manager = _CapturingProgressManager()

    await runtime._refresh_runtime_ref_heartbeat(
        "job-1",
        progress_manager=progress_manager,
        worker_id="owner-ref-worker-1",
    )

    assert progress_manager.updated is False


@pytest.mark.asyncio
async def test_session_waits_for_delete_started_runtime_to_disappear_before_provision():
    now = datetime.now(timezone.utc)
    delete_started_ref = JobRuntimeRef(
        driverType="base",
        ownerRef="owner-ref",
        deleteStartedAt=now,
        heartbeatAt=now,
    )
    provision_calls: list[str] = []
    liveness_checks: list[str] = []

    class _ProgressManager(_FakeProgressManager):
        def __init__(self, current_job) -> None:
            self.current_job = current_job

        async def get_job(self, job_id: str):
            del job_id
            return self.current_job

    class _DeleteAwareRuntime(_R):
        session_retry_attempts = 2
        session_retry_base_delay_s = 0.01

        async def _load_handle(self, job, job_id, runtime_ref):
            del job, job_id
            assert runtime_ref is delete_started_ref
            return "old-runtime"

        async def _check_liveness(self, handle):
            assert handle == "old-runtime"
            liveness_checks.append(handle)
            if len(liveness_checks) == 1:
                return None
            raise BatchJobError(
                code=BatchJobErrorCode.RESOURCE_NOTFOUND_ERROR,
                message="runtime deleted",
            )

        async def _provision(self, job, job_id):
            del job
            provision_calls.append(job_id)
            return "new-runtime"

    job = _make_test_job(job_id="job-1", execution={"base": delete_started_ref})
    progress_manager = _ProgressManager(job)
    runtime = _DeleteAwareRuntime()

    async with runtime.session(
        job=job,
        job_id="job-1",
        progress_manager=progress_manager,
        worker_id_generator=_fake_worker_id_generator,
    ) as endpoint:
        assert endpoint.model_name == "m"

    assert liveness_checks == ["old-runtime", "old-runtime"]
    assert provision_calls == ["job-1"]


@pytest.mark.asyncio
async def test_session_retries_reconnect_until_owner_lease_expires():
    now = datetime.now(timezone.utc)
    execution = JobRuntimeRef(
        driverType="base",
        ownerRef="owner-ref",
        ownerWorkerId="owner-ref-worker-2",
        reconnectPayload={},
        heartbeatAt=now,
    )
    job = _make_test_job(job_id="job-1", execution={"base": execution})
    reconnect_calls: list[str] = []

    class _ReconnectRuntime(_R):
        session_retry_attempts = 2
        session_retry_base_delay_s = 0.01
        session_liveness_check_interval_s = 0.05

        async def _load_handle(self, job, job_id, runtime_ref):
            del job
            assert runtime_ref is execution
            reconnect_calls.append(job_id)
            return "reconnected-handle"

    runtime = _ReconnectRuntime()

    async with runtime.session(
        job=job,
        job_id="job-1",
        progress_manager=_FakeProgressManager(),
        worker_id_generator=_fake_worker_id_generator,
    ) as endpoint:
        assert endpoint.model_name == "m"

    assert reconnect_calls == ["job-1", "job-1"]


@pytest.mark.asyncio
async def test_session_skips_teardown_and_runtime_ref_clear_after_reownership():
    class _CapturingProgressManager:
        def __init__(self) -> None:
            self.calls: list[tuple[str, BatchJobStatus, object]] = []
            self.current_job: object | None = None

        async def get_job(self, job_id: str):
            del job_id
            return self.current_job

        async def update_job_local_status(
            self,
            job_id: str,
            worker_id: str,
            status: BatchJobStatus,
            update_keys=None,
        ):
            self.current_job = SimpleNamespace(job_id=job_id, status=status)
            self.calls.append((worker_id, status.model_copy(deep=True), update_keys))
            return SimpleNamespace(job_id=job_id, status=status)

    class _PersistingRuntime(_R):
        def _build_runtime_ref(self, job):
            del job
            return JobRuntimeRef(
                driverType="base",
                ownerRef="owner-ref",
            )

    teardown_calls: list[str] = []
    progress_manager = _CapturingProgressManager()
    runtime = _PersistingRuntime(teardown=lambda handle: teardown_calls.append(handle))
    job = _make_test_job(job_id="job-1")

    async with runtime.session(
        job=job,
        job_id="job-1",
        progress_manager=progress_manager,
        worker_id_generator=_fake_worker_id_generator,
    ):
        execution_ref = progress_manager.current_job.status.get_runtime_ref("base")
        assert execution_ref is not None
        stolen_ref = execution_ref.model_copy(deep=True)
        stolen_ref.owner_worker_id = "owner-ref-worker-2"
        stolen_ref.heartbeat_at = datetime.now(timezone.utc)
        stolen_status = progress_manager.current_job.status.model_copy(deep=True)
        stolen_status.set_runtime_ref("base", stolen_ref)
        progress_manager.current_job = SimpleNamespace(
            job_id="job-1",
            status=stolen_status,
        )

    assert teardown_calls == []
    assert len(progress_manager.calls) == 1
    persisted_ref = progress_manager.current_job.status.get_runtime_ref("base")
    assert persisted_ref is not None
    assert persisted_ref.owner_worker_id == "owner-ref-worker-2"


@pytest.mark.asyncio
async def test_local_runtime_yields_injected_source():
    sentinel_source = object()
    runtime = ExternalRuntime(sentinel_source)  # type: ignore[arg-type]
    assert runtime.provisions is False
    async with runtime.session(job=_make_test_job(), job_id="j") as endpoint:
        assert endpoint.source is sentinel_source


@pytest.mark.asyncio
async def test_noop_runtime_yields_no_endpoint():
    runtime = NoopRuntime()
    async with runtime.session(job=_make_test_job(), job_id="j") as endpoint:
        assert endpoint.source is None
        assert endpoint.model_name is None


def test_builtin_runtimes_are_registered():
    keys = registered_runtimes()
    assert "External" in keys
    assert "noop" in keys


def test_runtime_target_enum_values_are_registered():
    """Every public RuntimeTarget must resolve to a registered runtime, so a
    valid request never fails selection. Guards key<->enum drift."""
    import aibrix.batch.job_driver  # noqa: F401  triggers provider registration
    from aibrix.batch.job_entity import RuntimeTarget

    registered = set(registered_runtimes())
    missing = {p.value for p in RuntimeTarget} - registered
    assert not missing, f"RuntimeTarget values not registered: {missing}"


def test_registry_create_and_unknown_key():
    assert isinstance(create_runtime("noop"), NoopRuntime)
    src = object()
    external = create_runtime("External", endpoint_source=src)
    assert isinstance(external, ExternalRuntime)
    with pytest.raises(KeyError, match="unknown runtime 'nope'"):
        create_runtime("nope")


def test_registry_allows_downstream_registration():
    class _Custom(RuntimeBase):
        provisions = True

    register_runtime(
        "custom-test-runtime",
        lambda: _Custom(InfrastructureContext()),
    )
    try:
        runtime = create_runtime("custom-test-runtime")
        assert isinstance(runtime, _Custom)
        assert isinstance(runtime, Runtime)  # structural Protocol check
    finally:
        # keep the global registry clean for other tests
        from aibrix.batch.job_driver import runtime as runtime_mod

        runtime_mod._RUNTIME_FACTORIES.pop("custom-test-runtime", None)


def test_cloud_runtimes_registered_as_ssh_launch():
    import aibrix.batch.job_driver  # noqa: F401  triggers provider registration
    from aibrix.batch.job_driver.runtime.lambda_cloud import LambdaCloudRuntime
    from aibrix.batch.job_driver.runtime.runpod import RunPodRuntime
    from aibrix.batch.job_driver.runtime.ssh_launch import SSHLaunchRuntime

    assert "LambdaCloud" in registered_runtimes()
    assert "RunPod" in registered_runtimes()

    assert isinstance(create_runtime("LambdaCloud"), LambdaCloudRuntime)
    assert isinstance(create_runtime("RunPod"), RunPodRuntime)

    for key in ("LambdaCloud", "RunPod"):
        runtime = create_runtime(key)
        assert isinstance(runtime, SSHLaunchRuntime)
        assert runtime.provisions is True

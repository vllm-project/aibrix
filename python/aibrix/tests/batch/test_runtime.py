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
from aibrix.batch.job_entity import BatchJobError, BatchJobErrorCode, JobRuntimeRef
from aibrix.context import InfrastructureContext


class _FakeStatus:
    def __init__(self, execution: dict[str, JobRuntimeRef] | None = None) -> None:
        self._execution = dict(execution or {})

    def get_runtime_ref(self, execution_key: str) -> JobRuntimeRef | None:
        return self._execution.get(execution_key)

    def model_copy(self, deep: bool = True) -> "_FakeStatus":
        del deep
        return _FakeStatus(self._execution)

    def set_runtime_ref(self, execution_key: str, execution_ref: JobRuntimeRef) -> None:
        self._execution[execution_key] = execution_ref


class _FakeProgressManager:
    async def update_job_local_status(
        self, job_id: str, worker_id: str, status
    ) -> object:
        del worker_id
        return SimpleNamespace(job_id=job_id, status=status)


def _fake_worker_id_generator(owner_ref: str) -> str:
    return f"{owner_ref}-worker-1"


def _make_test_job(
    job_id: str = "j",
    *,
    execution: dict[str, JobRuntimeRef] | None = None,
    opts: dict[str, object] | None = None,
) -> object:
    return SimpleNamespace(
        job_id=job_id,
        status=_FakeStatus(execution),
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
        connect=None,
        teardown=None,
    ) -> None:
        super().__init__(InfrastructureContext())
        self._provision_hook = provision
        self._wait_ready_hook = wait_ready
        self._connect_hook = connect
        self._teardown_hook = teardown

    async def _provision(self, job, job_id):
        if self._provision_hook is None:
            return "handle"
        return await _maybe_await(self._provision_hook(job, job_id))

    async def _wait_ready(self, handle):
        if self._wait_ready_hook is None:
            return None
        return await _maybe_await(self._wait_ready_hook(handle))

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
        def _should_retry_wait_ready(self, exc: Exception) -> bool:
            return isinstance(exc, _RetryableReadyError)

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

    async def _reconnect(job, job_id, runtime_ref):
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
        return job

    async def _wait_ready(handle):
        calls.append(("wait_ready", handle))
        if handle == "reconnected-handle":
            raise BatchJobError(
                code=BatchJobErrorCode.RESOURCE_NOTFOUND_ERROR,
                message="runtime not found",
            )

    class _ReconnectRuntime(_R):
        async def _reconnect(self, job, job_id, runtime_ref):
            return await _reconnect(job, job_id, runtime_ref)

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
        ("teardown", "fresh-handle"),
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

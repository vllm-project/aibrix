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

import pytest

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
from aibrix.batch.job_entity import BatchJobError, BatchJobErrorCode


@pytest.mark.asyncio
async def test_runtime_base_session_runs_four_phases_in_order():
    calls: list[str] = []

    class _R(RuntimeBase):
        provisions = True

        async def _provision(self, job, job_id):
            calls.append("provision")
            return "handle"

        async def _wait_ready(self, handle):
            assert handle == "handle"
            calls.append("wait_ready")

        async def _connect(self, handle):
            calls.append("connect")
            return Endpoint(source=None, model_name="m")

        async def _teardown(self, handle):
            calls.append("teardown")

    runtime = _R()
    async with runtime.session(job=None, job_id="j") as endpoint:
        calls.append("body")
        assert endpoint.model_name == "m"
        assert endpoint.source is None

    assert calls == ["provision", "wait_ready", "connect", "body", "teardown"]


@pytest.mark.asyncio
async def test_runtime_base_teardown_runs_even_when_body_raises():
    torn_down = []

    class _R(RuntimeBase):
        async def _provision(self, job, job_id):
            return "h"

        async def _teardown(self, handle):
            torn_down.append(handle)

    runtime = _R()
    with pytest.raises(ValueError):
        async with runtime.session(job=None, job_id="j"):
            raise ValueError("boom")

    assert torn_down == ["h"]


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

    class _R(RuntimeBase):
        provisions = True

        def __init__(self) -> None:
            self._attempt = 0

        async def _provision(self, job, job_id):
            self._attempt += 1
            calls.append(("provision", self._attempt))
            if self._attempt < 4:
                raise _TemporaryProvisionError("temporary failure")
            return f"handle-{self._attempt}"

        async def _wait_ready(self, handle):
            calls.append(("wait_ready", handle))

        async def _connect(self, handle):
            calls.append(("connect", handle))
            return Endpoint(source=None, model_name="m")

        async def _teardown(self, handle):
            calls.append(("teardown", handle))

    runtime = _R()
    async with runtime.session(job=None, job_id="j") as endpoint:
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

    class _R(RuntimeBase):
        provisions = True

        def __init__(self) -> None:
            self._attempt = 0

        async def _provision(self, job, job_id):
            self._attempt += 1
            handle = f"handle-{self._attempt}"
            calls.append(("provision", handle))
            return handle

        async def _wait_ready(self, handle):
            calls.append(("wait_ready", handle))
            if handle == "handle-1":
                raise BatchJobError(
                    code=BatchJobErrorCode.RESOURCE_NOTFOUND_ERROR,
                    message="runtime not found",
                )

        async def _connect(self, handle):
            calls.append(("connect", handle))
            return Endpoint(source=None, model_name="m")

        async def _teardown(self, handle):
            calls.append(("teardown", handle))

    runtime = _R()
    async with runtime.session(job=None, job_id="j") as endpoint:
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

    class _R(RuntimeBase):
        provisions = True

        def __init__(self) -> None:
            self._attempt = 0

        def _should_retry_wait_ready(self, exc: Exception) -> bool:
            return isinstance(exc, _RetryableReadyError)

        def _should_teardown_failed_wait_ready(self, exc: Exception) -> bool:
            return True

        async def _provision(self, job, job_id):
            self._attempt += 1
            handle = f"handle-{self._attempt}"
            calls.append(("provision", handle))
            return handle

        async def _wait_ready(self, handle):
            calls.append(("wait_ready", handle))
            if handle == "handle-1":
                raise _RetryableReadyError("try again")

        async def _connect(self, handle):
            calls.append(("connect", handle))
            return Endpoint(source=None, model_name="m")

        async def _teardown(self, handle):
            calls.append(("teardown", handle))
            if handle == "handle-1":
                raise RuntimeError("teardown failed")

    runtime = _R()
    async with runtime.session(job=None, job_id="job-1") as endpoint:
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
async def test_local_runtime_yields_injected_source():
    sentinel_source = object()
    runtime = ExternalRuntime(sentinel_source)  # type: ignore[arg-type]
    assert runtime.provisions is False
    async with runtime.session(job=None, job_id="j") as endpoint:
        assert endpoint.source is sentinel_source


@pytest.mark.asyncio
async def test_noop_runtime_yields_no_endpoint():
    runtime = NoopRuntime()
    async with runtime.session(job=None, job_id="j") as endpoint:
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

    register_runtime("custom-test-runtime", lambda: _Custom())
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

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


def test_compute_provider_enum_values_are_registered():
    """Every public ComputeProvider must resolve to a registered runtime, so a
    valid request never fails selection. Guards key<->enum drift."""
    import aibrix.batch.job_driver  # noqa: F401  triggers provider registration
    from aibrix.batch.job_entity import ComputeProvider

    registered = set(registered_runtimes())
    missing = {p.value for p in ComputeProvider} - registered
    assert not missing, f"ComputeProvider values not registered: {missing}"


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


@pytest.mark.asyncio
async def test_cloud_runtimes_registered_as_placeholders():
    import aibrix.batch.job_driver  # noqa: F401  triggers provider registration
    from aibrix.batch.job_driver.runtime.lambda_cloud import LambdaCloudRuntime
    from aibrix.batch.job_driver.runtime.runpod import RunPodRuntime

    assert "LambdaCloud" in registered_runtimes()
    assert "RunPod" in registered_runtimes()

    assert isinstance(create_runtime("LambdaCloud"), LambdaCloudRuntime)
    assert isinstance(create_runtime("RunPod"), RunPodRuntime)

    # Placeholders: provisioning is not implemented, so the session fails clearly.
    for key in ("LambdaCloud", "RunPod"):
        runtime = create_runtime(key)
        assert runtime.provisions is True
        with pytest.raises(NotImplementedError):
            async with runtime.session(job=None, job_id="j"):
                pass

# Copyright 2024 The Aibrix Team.
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

"""
End-to-end tests for abnormal job behavior scenarios.

These tests cover various failure modes and edge cases in the batch job lifecycle:
- Validation failures
- Processing failures during different stages
- Job cancellation at various points
- Job expiration scenarios
"""

import asyncio
import threading
import warnings
from contextlib import asynccontextmanager
from datetime import datetime, timezone
from typing import Any, Dict, List, Optional, Tuple, TypeVar, Union, cast
from unittest.mock import patch

import pytest
from fastapi.testclient import TestClient

import aibrix.batch.constant as constant
from aibrix.batch.batch_manager import BatchManager
from aibrix.batch.job_driver import BaseJobDriver, JobDriver, TerminateResult
from aibrix.batch.job_entity import (
    AibrixMetadata,
    BatchJob,
    BatchJobError,
    BatchJobErrorCode,
    BatchJobSpec,
    BatchJobState,
    ResourceAllocation,
)
from aibrix.context import InfrastructureContext
from tests.batch.conftest import (
    backend_has_feature,
    build_batch_request,
    build_e2e_test_app,
    create_batch_job,
    create_test_client,
    get_e2e_backend,
    select_e2e_backends,
    upload_batch_input_file,
)

T = TypeVar("T")
TEST_OPTS_PENDING_AFTER_N_REQUESTS = "pending_after_n_requests"
TEST_OPTS_DELAY_BEFORE_FINALIZE_SHUTDOWN_SECONDS = (
    "delay_before_finalize_shutdown_seconds"
)
TEST_OPTS_DELAY_AFTER_FINALIZE_SHUTDOWN_SECONDS = (
    "delay_after_finalize_shutdown_seconds"
)
_ORIGINAL_RUNTIME_DELAY_SEND: Any = None
_PATCHED_RUNTIME_DELAY_SECONDS = 0.0


def _backend(test_backend):
    return get_e2e_backend(test_backend)


def _backend_provider(test_backend) -> Optional[str]:
    return _backend(test_backend).request_kwargs.get("provider")


def backend_uses_tce_provider(test_backend) -> bool:
    return _backend_provider(test_backend) == "tce"


def backend_uses_deployment_provider(test_backend) -> bool:
    return _backend_provider(test_backend) == "deployment"


def backend_uses_kubernetes_provider(test_backend) -> bool:
    return _backend_provider(test_backend) == "kubernetes"


def backend_supports_restart_phase_recovery(test_backend) -> bool:
    return not backend_uses_kubernetes_provider(test_backend)


def backend_expect_runtime_teardown(test_backend) -> bool:
    return _backend(test_backend).fake_runtime


def backend_expect_exact_request_counts(test_backend) -> bool:
    return _backend(test_backend).max_concurrency == 1


def backend_has_runtime_debug_state(test_backend) -> bool:
    return _backend(test_backend).runtime_debug_config is not None


def get_runtime_debug_handles(
    e2e_test_app, test_backend: str
) -> Optional[Dict[str, Any]]:
    backend = _backend(test_backend)
    runtime_debug_config = backend.runtime_debug_config
    if runtime_debug_config is None:
        return None

    debug_values = e2e_test_app.state.batch_driver._context.values
    runtime_create_target = debug_values[
        runtime_debug_config["runtime_create_target_key"]
    ]
    runtime_delete_target = debug_values[
        runtime_debug_config["runtime_delete_target_key"]
    ]
    return {
        "teardown_calls": debug_values[runtime_debug_config["teardown_calls_key"]],
        "inference_client_builds": debug_values[
            runtime_debug_config["endpoint_source_builds_key"]
        ],
        "runtime_create_calls": getattr(
            runtime_create_target, runtime_debug_config["runtime_create_attr"]
        ),
        "runtime_delete_calls": getattr(
            runtime_delete_target, runtime_debug_config["runtime_delete_attr"]
        ),
    }


def backend_runtime_driver_class(test_backend):
    if backend_uses_deployment_provider(test_backend):
        from aibrix.batch.job_driver.base import BaseJobDriver

        return BaseJobDriver

    from aibrix.batch.job_driver.base import BaseJobDriver

    return BaseJobDriver


def backend_runtime_create_patch(test_backend):
    if backend_uses_deployment_provider(test_backend):
        from aibrix.batch.job_driver.runtime.k8s_deployment import DeploymentRuntime

        return (
            "aibrix.batch.job_driver.runtime.k8s_deployment."
            "DeploymentRuntime._provision",
            DeploymentRuntime._provision,
        )
    raise ValueError(f"Backend {test_backend} does not have a runtime create hook")


def backend_uses_fake_provisioning_runtime(test_backend) -> bool:
    return _backend(test_backend).has_feature("fake_provisioning_runtime")


def backend_observes_restart_validation_runtime_delta(test_backend) -> bool:
    return _backend(test_backend).has_feature(
        "restart_validation_runtime_delta_observable"
    )


# Suppress metaservice manual crash log
pytestmark = pytest.mark.filterwarnings(
    "ignore::pytest.PytestUnraisableExceptionWarning"
)


def pytest_generate_tests(metafunc):
    select_e2e_backends(
        metafunc,
        [
            "local_metastore_job_parallel",
            "local_job_using_deployment",
            "k8s_job",
        ],
        default_backend="local_metastore_job",
    )


async def swap_job_manager(app, new_manager: BatchManager) -> BatchManager:
    original_manager = app.state.batch_driver.job_manager
    new_manager._context = original_manager._context
    new_manager._registry = original_manager._registry
    new_manager._job_entity_manager = original_manager._job_entity_manager
    new_manager._bridge = original_manager._bridge
    new_manager._endpoint_source = original_manager._endpoint_source
    scheduler = original_manager._job_scheduler
    if scheduler is not None:
        new_manager.set_scheduler(scheduler)
        scheduler._job_progress_manager = new_manager
    app.state.batch_driver._batch_manager = new_manager
    return original_manager


def restore_job_manager(app, original_manager: BatchManager) -> None:
    scheduler = original_manager._job_scheduler
    if scheduler is not None:
        scheduler._job_progress_manager = original_manager
    app.state.batch_driver._batch_manager = original_manager


def skip_if_runtime_unavailable(test_backend) -> None:
    if not get_e2e_backend(test_backend).support_runtime:
        pytest.skip("This test requires a runtime-backed driver")


def inject_job_creation_opts(monkeypatch, app, injected_opts: Dict[str, str]) -> None:
    manager = app.state.batch_driver.job_manager
    original_create_job_with_spec = manager.create_job_with_spec

    async def patched_create_job_with_spec(
        session_id: str,
        job_spec: BatchJobSpec,
        timeout: float = 30.0,
        initial_state: BatchJobState = BatchJobState.CREATED,
        request_count: int = 0,
    ) -> str:
        opts = dict(job_spec.opts or {})
        opts.update({key: str(value) for key, value in injected_opts.items()})
        if opts:
            job_spec.opts = opts
        return await original_create_job_with_spec(
            session_id, job_spec, timeout, initial_state, request_count
        )

    monkeypatch.setattr(manager, "create_job_with_spec", patched_create_job_with_spec)


def inject_runtime_deadline(monkeypatch, app, offset_seconds: float) -> None:
    manager = app.state.batch_driver.job_manager
    original_create_job_with_spec = manager.create_job_with_spec

    async def patched_create_job_with_spec(
        session_id: str,
        job_spec: BatchJobSpec,
        timeout: float = 30.0,
        initial_state: BatchJobState = BatchJobState.CREATED,
        request_count: int = 0,
    ) -> str:
        aibrix = job_spec.aibrix or AibrixMetadata()
        allocation = aibrix.resource_allocation or ResourceAllocation()
        allocation.provision_resource_deadline = int(
            datetime.now(timezone.utc).timestamp() + offset_seconds
        )
        aibrix.resource_allocation = allocation
        job_spec.aibrix = aibrix
        return await original_create_job_with_spec(
            session_id, job_spec, timeout, initial_state, request_count
        )

    monkeypatch.setattr(manager, "create_job_with_spec", patched_create_job_with_spec)


def force_batch_driver_crash_on_shutdown(monkeypatch, app) -> None:
    driver = app.state.batch_driver
    original_stop = driver.stop
    import _pytest.unraisableexception as pytest_unraisableexception

    from aibrix.batch.job_driver.runtime.base import RuntimeBase
    from aibrix.batch.job_driver.runtime.base import logger as runtime_logger

    warnings.simplefilter("ignore", pytest.PytestUnraisableExceptionWarning)
    warnings.filterwarnings("ignore", category=pytest.PytestUnraisableExceptionWarning)
    pytest_unraisableexception.warnings.warn = lambda *args, **kwargs: None

    @asynccontextmanager
    async def patched_runtime_session(
        self,
        job,
        job_id,
        *,
        progress_manager=None,
        worker_id_generator=None,
    ):
        self._bind_active_session(job_id, progress_manager)
        try:
            runtimeRef = self._load_runtime_ref(job)
            handle = None
            max_attempts = self.session_retry_attempts + 1
            try:
                for attempt in range(max_attempts):
                    try:
                        phase = (
                            "reconnect"
                            if attempt == 0 and runtimeRef is not None
                            else "provision"
                        )
                        if phase == "reconnect":
                            assert runtimeRef is not None
                            handle = await self._reconnect(job, job_id, runtimeRef)
                            if handle is None:
                                phase = "provision"
                        if handle is None:
                            self._maybe_fail_runtime_initialization(job)
                            handle = await self._provision(job, job_id)
                        job = await self._persist_runtime_ref(
                            job,
                            progress_manager=progress_manager,
                            worker_id_generator=worker_id_generator,
                        )
                        phase = "wait_ready"
                        await self._wait_ready(handle)
                        break
                    except Exception as exc:
                        should_retry = False
                        should_teardown = handle is not None

                        if phase in {"reconnect", "provision"}:
                            should_retry = attempt + 1 < max_attempts
                        elif phase == "wait_ready":
                            should_retry = (
                                self._should_retry_wait_ready(exc)
                                and attempt + 1 < max_attempts
                            )
                            should_teardown = self._should_teardown_failed_wait_ready(
                                exc
                            )

                        if should_teardown and handle is not None:
                            try:
                                await self._teardown(handle)
                            except Exception as teardown_exc:
                                runtime_logger.warning(
                                    "Runtime teardown failed during retry recovery; continuing with retry",
                                    job_id=job_id,
                                    phase=phase,
                                    handle=repr(handle),
                                    error=str(teardown_exc),
                                )  # type: ignore[call-arg]
                        elif not should_retry:
                            handle = None
                        if not should_retry:
                            raise
                        handle = None
                        runtimeRef = None
                        await self._sleep_before_session_retry(attempt)
                yield await self._connect(handle)
            finally:
                if handle is not None:
                    opts = job.spec.opts or {}
                    before_delay = float(
                        opts.get(TEST_OPTS_DELAY_BEFORE_FINALIZE_SHUTDOWN_SECONDS, 0.0)
                        or 0.0
                    )
                    if (
                        before_delay > 0
                        and progress_manager is not None
                        and job.job_id is not None
                    ):
                        await progress_manager.mark_job_finalizing(job.job_id)
                        await asyncio.sleep(before_delay)
                if handle is not None and not self._context.values.get(
                    "simulate_crash_shutdown", False
                ):
                    await self._teardown(handle)
                elif handle is not None and hasattr(self, "_active_handle"):
                    self._active_handle = None
                self._unbind_active_session()
        except asyncio.CancelledError:
            raise

    monkeypatch.setattr(RuntimeBase, "session", patched_runtime_session)

    async def patched_stop() -> None:
        async_thread_loop = getattr(driver, "_async_thread_loop", None)
        loop = getattr(async_thread_loop, "loop", None)
        driver._context.values["simulate_crash_shutdown"] = True
        if loop is None:
            try:
                loop = asyncio.get_running_loop()
            except RuntimeError:
                loop = None

        if loop is not None:
            try:
                running_loop = asyncio.get_running_loop()
            except RuntimeError:
                running_loop = None
            if loop is running_loop:
                loop.set_exception_handler(lambda _loop, _context: None)
            else:
                loop.call_soon_threadsafe(
                    loop.set_exception_handler,
                    lambda _loop, _context: None,
                )
        await original_stop()

    monkeypatch.setattr(driver, "stop", patched_stop)


def install_runtime_finalize_shutdown_delay(monkeypatch, test_backend) -> None:
    runtime_driver_class = backend_runtime_driver_class(test_backend)
    if hasattr(runtime_driver_class, "_shutdown_runtime_before_finalize"):
        original_shutdown_runtime_before_finalize = (
            runtime_driver_class._shutdown_runtime_before_finalize
        )

        async def patched_shutdown_runtime_before_finalize(self, job):
            opts = job.spec.opts or {}
            before_delay = float(
                opts.get(TEST_OPTS_DELAY_BEFORE_FINALIZE_SHUTDOWN_SECONDS, 0.0) or 0.0
            )
            after_delay = float(
                opts.get(TEST_OPTS_DELAY_AFTER_FINALIZE_SHUTDOWN_SECONDS, 0.0) or 0.0
            )
            if before_delay > 0:
                await asyncio.sleep(before_delay)
            job = await original_shutdown_runtime_before_finalize(self, job)
            if after_delay > 0:
                await asyncio.sleep(after_delay)
            return job

        monkeypatch.setattr(
            runtime_driver_class,
            "_shutdown_runtime_before_finalize",
            patched_shutdown_runtime_before_finalize,
        )
        return

    original_finalize_job = runtime_driver_class.finalize_job

    async def patched_finalize_job(self, job):
        opts = job.spec.opts or {}
        after_delay = float(
            opts.get(TEST_OPTS_DELAY_AFTER_FINALIZE_SHUTDOWN_SECONDS, 0.0) or 0.0
        )

        # Shared-driver runtimes now model the before-shutdown window inside the
        # RuntimeBase session wrapper; only keep the after-shutdown pause here.
        if after_delay > 0:
            job = await self._progress_manager.mark_job_finalizing(job.job_id)
        if after_delay > 0:
            await asyncio.sleep(after_delay)
        return await original_finalize_job(self, job)

    monkeypatch.setattr(runtime_driver_class, "finalize_job", patched_finalize_job)


def install_pending_after_n_requests_barrier(
    monkeypatch,
    e2e_test_app,
    test_backend,
    *,
    wait_for_release: bool = False,
):
    entered_pending = threading.Event()
    release_pending = threading.Event()
    blocked_jobs: set[str] = set()
    job_manager = e2e_test_app.state.batch_driver.job_manager

    def apply_barrier_patch(target: Any, attribute: str, replacement: Any) -> None:
        monkeypatch.setattr(target, attribute, replacement)

    def get_threshold(job: Any) -> Optional[int]:
        opts = job.spec.opts or {}
        threshold_value = opts.get(TEST_OPTS_PENDING_AFTER_N_REQUESTS)
        if threshold_value is None:
            return None
        try:
            return int(threshold_value)
        except (TypeError, ValueError):
            return None

    completed_counts: Dict[str, int] = {}
    target_driver = backend_runtime_driver_class(test_backend)
    original_sync_completed_request_tasks = target_driver._sync_completed_request_tasks

    async def block_after_completed_requests(
        original_method, self, job, completed_request_tasks
    ):
        job, completed = await original_method(self, job, completed_request_tasks)
        if job.status.state != BatchJobState.IN_PROGRESS or job.status.finished:
            return job, completed

        threshold = get_threshold(job)
        if threshold is None:
            return job, completed

        completed_counts[job.job_id] = completed_counts.get(job.job_id, 0) + completed
        if completed_counts[job.job_id] >= threshold and job.job_id not in blocked_jobs:
            # Block after persisted completions, not before dispatch. This keeps
            # restart and cancellation tests aligned with the shared pass loop.
            blocked_jobs.add(job.job_id)
            entered_pending.set()
            if wait_for_release:
                # Cancellation releases the barrier after the API call, then we
                # re-read the job snapshot so the driver sees the latest state.
                await asyncio.to_thread(release_pending.wait, 30)
                latest_job = await job_manager.get_job(job.job_id)
                if latest_job is not None:
                    return latest_job, completed
            else:
                await asyncio.sleep(3600)
        return job, completed

    async def blocked_sync_completed_request_tasks(self, job, completed_request_tasks):
        return await block_after_completed_requests(
            original_sync_completed_request_tasks,
            self,
            job,
            completed_request_tasks,
        )

    apply_barrier_patch(
        target_driver,
        "_sync_completed_request_tasks",
        blocked_sync_completed_request_tasks,
    )
    return entered_pending, release_pending


def pin_job_concurrency_to_serial(
    monkeypatch, test_backend, *, per_request_delay: float = 0.0
) -> None:
    """Make request-persistence barriers deterministic for timing tests."""
    target_driver = backend_runtime_driver_class(test_backend)

    async def serial_execute_worker(self, job_id):
        if self._engine is None:
            raise RuntimeError(
                "JobDriver was constructed without an engine; execute_worker "
                "requires one."
            )

        job, stopped = await self._is_job_stopped(job_id)
        if stopped:
            return await self._finish_stopped_job(job)
        return await self._execute_worker_serial(job)

    monkeypatch.setattr(
        target_driver,
        "execute_worker",
        serial_execute_worker,
    )

    if per_request_delay <= 0:
        return

    from aibrix.batch.client.engine import DispatchEngine

    original_send = DispatchEngine._send_with_failover

    async def delayed_send(self, request):
        await asyncio.sleep(per_request_delay)
        return await original_send(self, request)

    monkeypatch.setattr(DispatchEngine, "_send_with_failover", delayed_send)


async def wait_for_status(
    client: TestClient,
    batch_id: str,
    expected_status: str,
    extra_expected_fields: Optional[Union[str, List[str]]] = None,
    max_polls: int = 20,
    poll_interval: float = 0.5,
) -> Dict[str, Any]:
    """Wait for batch job to reach expected status."""
    for attempt in range(max_polls):
        response = client.get(f"/v1/batches/{batch_id}")
        assert response.status_code == 200, f"Status check failed: {response.text}"

        result = response.json()
        current_status = result["status"]

        if current_status == expected_status:
            if extra_expected_fields is None:
                return result
            if isinstance(extra_expected_fields, str):
                extra_expected_fields = [extra_expected_fields]
            if all(result.get(field) is not None for field in extra_expected_fields):
                return result
        elif current_status in ["failed", "cancelled", "expired", "completed"]:
            # Terminal states
            if current_status != expected_status:
                return result  # Return actual final state

        await asyncio.sleep(poll_interval)

    # Return last known status if timeout
    return result


def validate_batch_response(
    response: Dict[str, Any],
    *,
    # Required fields (default checked for not None)
    expected_id: Optional[str] = None,
    expected_object: str = "batch",
    expected_endpoint: Optional[str] = None,
    expected_input_file_id: Optional[str] = None,
    expected_completion_window: str = "24h",
    expected_status: Optional[str] = None,
    # Optional fields (default checked for None)
    expected_errors: Optional[Union[bool, str, List[str]]] = None,
    expected_output_file_id: Optional[bool] = None,
    expected_error_file_id: Optional[bool] = None,
    expected_in_progress_at: Optional[bool] = None,
    expected_finalizing_at: Optional[bool] = None,
    expected_completed_at: Optional[bool] = None,
    expected_failed_at: Optional[bool] = None,
    expected_expired_at: Optional[bool] = None,
    expected_cancelling_at: Optional[bool] = None,
    expected_cancelled_at: Optional[bool] = None,
    expected_request_counts: Optional[Union[bool, Dict[str, int]]] = None,
    expected_metadata: Optional[Dict[str, str]] = None,
) -> None:
    """Validate batch response fields according to BatchResponse schema.

    This function validates all fields as defined in api/v1/batch.py::BatchResponse.

    Args:
        response: The batch response dict to validate
        expected_*: Expected values or presence checks for each field
                   - For required fields: actual expected value or None to skip check
                   - For optional fields: True = not None, False = None, None = skip check
    """
    # Verify all BatchResponse fields are present
    required_fields = [
        "id",
        "object",
        "endpoint",
        "input_file_id",
        "completion_window",
        "status",
        "created_at",
        "expires_at",
    ]
    optional_fields: List[Tuple[str, Any, type]] = [
        # Pass bool to skip equal test.
        ("errors", True if expected_errors else False, Dict),
        ("output_file_id", expected_output_file_id, str),
        ("error_file_id", expected_error_file_id, str),
        ("in_progress_at", expected_in_progress_at, int),
        ("finalizing_at", expected_finalizing_at, int),
        ("completed_at", expected_completed_at, int),
        ("failed_at", expected_failed_at, int),
        ("expired_at", expected_expired_at, int),
        ("cancelling_at", expected_cancelling_at, int),
        ("cancelled_at", expected_cancelled_at, int),
        ("request_counts", expected_request_counts, Dict),
        ("metadata", expected_metadata, Dict),
    ]

    # Required fields must be present; optional fields may be omitted and are treated
    # the same as explicit null in some backends.
    for field in required_fields:
        assert field in response, f"Field '{field}' missing from response"
    response = dict(response)
    for field, _, _ in optional_fields:
        response.setdefault(field, None)

    # Validate required fields
    if expected_id is not None:
        assert response["id"] == expected_id, (
            f"Expected id '{expected_id}', got '{response['id']}'"
        )
    else:
        assert response["id"] is not None, "Required field 'id' should not be None"

    assert response["object"] == expected_object, (
        f"Expected object '{expected_object}', got '{response['object']}'"
    )

    if expected_endpoint is not None:
        assert response["endpoint"] == expected_endpoint, (
            f"Expected endpoint '{expected_endpoint}', got '{response['endpoint']}'"
        )
    else:
        assert response["endpoint"] is not None, (
            "Required field 'endpoint' should not be None"
        )

    if expected_input_file_id is not None:
        assert response["input_file_id"] == expected_input_file_id, (
            f"Expected input_file_id '{expected_input_file_id}', got '{response['input_file_id']}'"
        )
    else:
        assert response["input_file_id"] is not None, (
            "Required field 'input_file_id' should not be None"
        )

    assert response["completion_window"] == expected_completion_window, (
        f"Expected completion_window '{expected_completion_window}', got '{response['completion_window']}'"
    )

    if expected_status is not None:
        assert response["status"] == expected_status, (
            f"Expected status '{expected_status}', got '{response['status']}'"
        )
    else:
        assert response["status"] is not None, (
            "Required field 'status' should not be None"
        )

    # created_at is required and should not be None
    assert response["created_at"] is not None, (
        "Required field 'created_at' should not be None"
    )
    assert isinstance(response["created_at"], int), (
        "Expected 'created_at' to be unix timestamp (int)"
    )

    # created_at is required and should not be None
    assert response["expires_at"] is not None, (
        "Required field 'expires_at' should not be None"
    )
    assert isinstance(response["expires_at"], int), (
        "Expected 'expires_at' to be unix timestamp (int)"
    )
    if expected_completion_window == "24h":
        assert response["expires_at"] == response["created_at"] + 86400, (
            "Expected 'expires_at' to be 'created_at' + 86400"
        )

    # Validate optional fields
    def check_optional_field(
        field_name: str, expected_value: Optional[T], expected_type: type
    ):
        if expected_value is None or expected_value is False:
            assert response[field_name] is None, f"Expected '{field_name}' to be None"
        elif expected_value is True and expected_type is not bool:
            assert response[field_name] is not None, (
                f"Required field '{field_name}' should not be None"
            )
            assert isinstance(response[field_name], expected_type), (
                f"Expected '{field_name}' to be type ({expected_type})"
            )
        elif field_name == "request_counts" and isinstance(expected_value, dict):
            actual_request_counts = response[field_name]
            assert actual_request_counts is not None, (
                "Required field 'request_counts' should not be None"
            )
            assert isinstance(actual_request_counts, dict), (
                "Expected 'request_counts' to be a dict"
            )
            for expected_key, expected_count in expected_value.items():
                if expected_key.endswith("_at_least"):
                    actual_key = expected_key.removesuffix("_at_least")
                    assert actual_key in actual_request_counts, (
                        f"Expected request_counts.{actual_key} to exist"
                    )
                    assert actual_request_counts[actual_key] >= expected_count, (
                        f"Expected request_counts.{actual_key} >= {expected_count}, "
                        f"got {actual_request_counts[actual_key]}"
                    )
                    continue
                assert actual_request_counts.get(expected_key) == expected_count, (
                    f"Expected request_counts.{expected_key} == {expected_count}, "
                    f"got {actual_request_counts.get(expected_key)}"
                )
        else:
            assert response[field_name] == expected_value, (
                f"Expected {field_name} '{expected_value}', got '{response[field_name]}'"
            )

    for field_name, expected_value, expected_type in optional_fields:
        check_optional_field(field_name, expected_value, expected_type)

    # Check non-timestamp optional fields
    if expected_errors not in (None, False):
        normalized_expected_errors: List[str]
        if isinstance(expected_errors, str):
            normalized_expected_errors = [expected_errors]
        else:
            assert isinstance(expected_errors, list)
            normalized_expected_errors = expected_errors
        assert isinstance(response["errors"], dict), "Expected 'errors' to be dict"
        assert "data" in response["errors"], "Expected 'errors.data' field"
        assert isinstance(response["errors"]["data"], list), (
            "Expected 'errors.data' to be list"
        )
        assert len(response["errors"]["data"]) > 0
        errors = {}
        for error in response["errors"]["data"]:
            assert "code" in error, f"No 'code' in error:{error}"
            assert "message" in error, f"No 'message' in error:{error}"
            errors[error["code"]] = error["message"]
        for err_code in normalized_expected_errors:
            assert err_code in errors


def capture_runtime_debug_state(
    e2e_test_app, test_backend: str
) -> Optional[Dict[str, int]]:
    handles = get_runtime_debug_handles(e2e_test_app, test_backend)
    if handles is None:
        return None

    return {
        "teardown_calls": len(handles["teardown_calls"]),
        "inference_client_builds": len(handles["inference_client_builds"]),
        "runtime_create_calls": len(handles["runtime_create_calls"]),
        "runtime_delete_calls": len(handles["runtime_delete_calls"]),
    }


def verify_runtime_teardown(
    e2e_test_app,
    test_backend: str,
    batch_id: str,
    debug_state: Optional[Dict[str, int]],
    *,
    expect_runtime_teardown: bool,
) -> None:
    handles = get_runtime_debug_handles(e2e_test_app, test_backend)
    if handles is None:
        return

    assert debug_state is not None
    teardown_calls = handles["teardown_calls"]
    inference_client_builds = handles["inference_client_builds"]
    runtime_create_calls = handles["runtime_create_calls"]
    runtime_delete_calls = handles["runtime_delete_calls"]

    expected_delta = 1 if expect_runtime_teardown else 0
    assert len(teardown_calls) == debug_state["teardown_calls"] + expected_delta
    assert len(inference_client_builds) == (
        debug_state["inference_client_builds"] + expected_delta
    )
    assert (
        len(runtime_create_calls)
        == debug_state["runtime_create_calls"] + expected_delta
    )
    assert (
        len(runtime_delete_calls)
        == debug_state["runtime_delete_calls"] + expected_delta
    )

    if expect_runtime_teardown:
        assert teardown_calls[-1]["job_id"] == batch_id


def validate_batch_response_with_runtime_check(
    response: Dict[str, Any],
    *,
    runtime_verifier,
    runtime_verifier_kwargs: Dict[str, Any],
    **kwargs,
) -> None:
    validate_batch_response(response, **kwargs)
    runtime_verifier(**runtime_verifier_kwargs)


def validate_batch_response_with_runtime_teardown(
    response: Dict[str, Any],
    *,
    e2e_test_app,
    test_backend: str,
    batch_id: str,
    debug_state: Optional[Dict[str, int]],
    expect_runtime_teardown: bool,
    **kwargs,
) -> None:
    validate_batch_response_with_runtime_check(
        response,
        runtime_verifier=verify_runtime_teardown,
        runtime_verifier_kwargs={
            "e2e_test_app": e2e_test_app,
            "test_backend": test_backend,
            "batch_id": batch_id,
            "debug_state": debug_state,
            "expect_runtime_teardown": expect_runtime_teardown,
        },
        **kwargs,
    )


def verify_runtime_not_torn_down(
    e2e_test_app,
    test_backend: str,
    debug_state: Optional[Dict[str, int]],
) -> None:
    handles = get_runtime_debug_handles(e2e_test_app, test_backend)
    if handles is None:
        return

    assert debug_state is not None
    teardown_calls = handles["teardown_calls"]
    runtime_delete_calls = handles["runtime_delete_calls"]

    assert len(teardown_calls) == debug_state["teardown_calls"]
    assert len(runtime_delete_calls) == debug_state["runtime_delete_calls"]


async def wait_for_runtime_teardown_delta(
    e2e_test_app,
    test_backend: str,
    debug_state: Optional[Dict[str, int]],
    expected_delta: int,
    timeout_seconds: float = 5.0,
    poll_interval: float = 0.1,
) -> None:
    if not backend_has_runtime_debug_state(test_backend):
        return

    assert debug_state is not None
    deadline = asyncio.get_running_loop().time() + timeout_seconds
    while True:
        current = capture_runtime_debug_state(e2e_test_app, test_backend)
        assert current is not None
        current_delta = current["teardown_calls"] - debug_state["teardown_calls"]
        if current_delta == expected_delta:
            return
        if asyncio.get_running_loop().time() >= deadline:
            raise AssertionError(
                f"Expected runtime teardown delta {expected_delta}, got {current_delta}"
            )
        await asyncio.sleep(poll_interval)


def verify_runtime_restart_completion_teardown(
    e2e_test_app,
    test_backend: str,
    batch_id: str,
    debug_state: Optional[Dict[str, int]],
    *,
    expect_runtime_created: bool,
    expect_runtime_used: bool,
    expect_runtime_teardown: bool,
) -> None:
    handles = get_runtime_debug_handles(e2e_test_app, test_backend)
    if handles is None:
        return

    assert debug_state is not None
    teardown_calls = handles["teardown_calls"]
    inference_client_builds = handles["inference_client_builds"]
    runtime_create_calls = handles["runtime_create_calls"]
    runtime_delete_calls = handles["runtime_delete_calls"]

    expected_create_delta = 1 if expect_runtime_created else 0
    expected_runtime_use_delta = 1 if expect_runtime_used else 0
    expected_teardown_delta = 1 if expect_runtime_teardown else 0
    assert (
        len(teardown_calls) == debug_state["teardown_calls"] + expected_teardown_delta
    )
    assert (
        len(inference_client_builds)
        == debug_state["inference_client_builds"] + expected_runtime_use_delta
    )
    assert (
        len(runtime_create_calls)
        == debug_state["runtime_create_calls"] + expected_create_delta
    )
    assert (
        len(runtime_delete_calls)
        == debug_state["runtime_delete_calls"] + expected_teardown_delta
    )
    if expect_runtime_teardown:
        assert teardown_calls[-1]["job_id"] == batch_id


def set_runtime_inference_delay(
    e2e_test_app, test_backend: str, delay_seconds: float
) -> Optional[float]:
    global _ORIGINAL_RUNTIME_DELAY_SEND, _PATCHED_RUNTIME_DELAY_SECONDS

    from aibrix.batch.client.engine import DispatchEngine

    values = e2e_test_app.state.batch_driver._context.values
    original_delay = values.get("endpoint_source_delay_seconds", 0.0)
    values["endpoint_source_delay_seconds"] = delay_seconds

    original_send = _ORIGINAL_RUNTIME_DELAY_SEND
    if original_send is None:
        _ORIGINAL_RUNTIME_DELAY_SEND = DispatchEngine._send_with_failover
        original_send = _ORIGINAL_RUNTIME_DELAY_SEND

    if delay_seconds > 0:
        _PATCHED_RUNTIME_DELAY_SECONDS = delay_seconds
        if DispatchEngine._send_with_failover is original_send:

            async def delayed_send(self, request):
                await asyncio.sleep(_PATCHED_RUNTIME_DELAY_SECONDS)
                return await original_send(self, request)

            cast(Any, DispatchEngine)._send_with_failover = delayed_send
    else:
        _PATCHED_RUNTIME_DELAY_SECONDS = 0.0
        cast(Any, DispatchEngine)._send_with_failover = original_send

    # Dry-run local/redis backends reuse a singleton NoopEndpointSource that
    # captures EchoChannel(delay=...) at app construction time. Update the live
    # source too so tests changing delay mid-run affect the active worker path.
    for owner in (
        e2e_test_app.state.batch_driver,
        getattr(e2e_test_app.state.batch_driver, "_batch_manager", None),
    ):
        if owner is None:
            continue
        endpoint_source = getattr(owner, "_endpoint_source", None)
        if endpoint_source is not None:
            channel = getattr(endpoint_source, "_channel", None)
            if channel is not None and hasattr(channel, "_delay"):
                channel._delay = delay_seconds
    return original_delay


async def run_follow_up_success_with_same_input(
    client: TestClient,
    e2e_test_app,
    test_backend: str,
    input_file_id: str,
    expected_total_requests: int,
) -> None:
    debug_state = capture_runtime_debug_state(e2e_test_app, test_backend)
    batch_id = create_batch_job(client, input_file_id, test_backend=test_backend)

    try:
        await wait_for_status(
            client, batch_id, "in_progress", max_polls=30, poll_interval=0.5
        )
        final_status = await wait_for_status(
            client, batch_id, "completed", max_polls=60, poll_interval=0.5
        )

        validate_batch_response_with_runtime_teardown(
            final_status,
            e2e_test_app=e2e_test_app,
            test_backend=test_backend,
            batch_id=batch_id,
            debug_state=debug_state,
            expect_runtime_teardown=backend_expect_runtime_teardown(test_backend),
            expected_status="completed",
            expected_endpoint="/v1/chat/completions",
            expected_input_file_id=input_file_id,
            expected_in_progress_at=True,
            expected_finalizing_at=True,
            expected_completed_at=True,
            expected_failed_at=False,
            expected_expired_at=False,
            expected_cancelling_at=False,
            expected_cancelled_at=False,
            expected_errors=False,
            expected_output_file_id=True,
            expected_error_file_id=False,
            expected_request_counts={
                "total": expected_total_requests,
                "completed": expected_total_requests,
                "failed": 0,
            },
        )
    finally:
        await e2e_test_app.state.batch_driver.clear_job(batch_id)


async def complete_job_after_restart(
    request,
    test_backend: str,
    tmp_path,
    monkeypatch,
    batch_id: str,
    expected_total_requests: int,
    expect_runtime_created_on_restart: bool,
    expect_runtime_used_on_restart: bool = True,
    expect_runtime_teardown_on_restart: bool = True,
    expect_runtime_teardown_before_restart: bool = False,
    original_app=None,
    original_debug_state: Optional[Dict[str, int]] = None,
    expected_request_counts=None,
):
    if expect_runtime_teardown_before_restart:
        verify_runtime_teardown(
            original_app,
            test_backend,
            batch_id,
            original_debug_state,
            expect_runtime_teardown=True,
        )
    else:
        verify_runtime_not_torn_down(original_app, test_backend, original_debug_state)
    restarted_app = build_e2e_test_app(
        request,
        test_backend,
        tmp_path,
        monkeypatch,
        preserve_redis_prefix=True,
    )
    restarted_debug_state = capture_runtime_debug_state(restarted_app, test_backend)
    try:
        with create_test_client(restarted_app) as restarted_client:
            final_status = await wait_for_status(
                restarted_client,
                batch_id,
                "completed",
                max_polls=120,
                poll_interval=0.5,
            )
            validate_batch_response_with_runtime_check(
                final_status,
                runtime_verifier=verify_runtime_restart_completion_teardown,
                runtime_verifier_kwargs={
                    "e2e_test_app": restarted_app,
                    "test_backend": test_backend,
                    "batch_id": batch_id,
                    "debug_state": restarted_debug_state,
                    "expect_runtime_created": expect_runtime_created_on_restart,
                    "expect_runtime_used": expect_runtime_used_on_restart,
                    "expect_runtime_teardown": expect_runtime_teardown_on_restart,
                },
                expected_status="completed",
                expected_endpoint="/v1/chat/completions",
                expected_in_progress_at=True,
                expected_finalizing_at=True,
                expected_completed_at=True,
                expected_failed_at=False,
                expected_expired_at=False,
                expected_cancelling_at=False,
                expected_cancelled_at=False,
                expected_errors=False,
                expected_output_file_id=True,
                expected_error_file_id=False,
                expected_request_counts=(
                    expected_request_counts
                    if expected_request_counts is not None
                    else {
                        "total": expected_total_requests,
                        "completed": expected_total_requests,
                        "failed": 0,
                    }
                ),
            )
            await restarted_app.state.batch_driver.clear_job(batch_id)
            return final_status
    except Exception:
        try:
            await restarted_app.state.batch_driver.clear_job(batch_id)
        except Exception:
            pass
        raise


class FailingBatchManager(BatchManager):
    """BatchManager that can be configured to fail at specific stages."""

    def __init__(
        self,
        fail_validation: bool = False,
        fail_during_processing: bool = False,
        fail_during_finalizing: bool = False,
        prevent_validation: bool = False,
        stall_validation: Optional[float] = None,
        stall_cancelling: Optional[float] = None,
        fail_after_n_requests: Optional[int] = None,
        pending_after_n_requests: Optional[int] = None,
        injected_opts: Optional[Dict[str, str]] = None,
        expiration: Optional[int] = None,
    ):
        super().__init__(InfrastructureContext())
        self.fail_validation = fail_validation
        self.fail_during_processing = fail_during_processing
        self.fail_during_finalizing = fail_during_finalizing
        self.prevent_validation = prevent_validation
        self.stall_validation = stall_validation
        self.stall_cancelling = stall_cancelling
        self.fail_after_n_requests = fail_after_n_requests
        self.pending_after_n_requests = pending_after_n_requests
        self.injected_opts = dict(injected_opts or {})
        self.expiration = expiration
        self._processed_requests = 0

    async def admit(self, job_id: str) -> Optional[JobDriver]:
        """Override to simulate validation failures during job execution start."""
        if self.prevent_validation:
            await asyncio.sleep(30.0)

        if self.stall_validation is not None:
            # Prolong validation duration to allow cancellation during validation
            await asyncio.sleep(self.stall_validation)

        if self.fail_validation:
            # Mark job as failed with authentication error
            raise BatchJobError(
                code=BatchJobErrorCode.AUTHENTICATION_ERROR,
                message="Simulated authentication failure",
                param="authentication",
            )

        return await super().admit(job_id)

    async def cancel_job(self, job_id: str) -> TerminateResult:
        if self.stall_cancelling is not None:
            await asyncio.sleep(self.stall_cancelling)

        return await super().cancel_job(job_id)

    async def create_job_with_spec(
        self,
        session_id: str,
        job_spec: BatchJobSpec,
        timeout: float = 30.0,
        initial_state: BatchJobState = BatchJobState.CREATED,
        request_count: int = 0,
    ) -> str:
        """Override create_job to inject test opts during job creation."""
        opts = dict(job_spec.opts or {})
        if self.fail_after_n_requests is not None:
            opts[constant.BATCH_OPTS_FAIL_AFTER_N_REQUESTS] = str(
                self.fail_after_n_requests
            )
        if self.pending_after_n_requests is not None:
            opts[TEST_OPTS_PENDING_AFTER_N_REQUESTS] = str(
                self.pending_after_n_requests
            )
        if self.injected_opts:
            opts.update({key: str(value) for key, value in self.injected_opts.items()})
        if opts:
            job_spec.opts = opts
        if self.expiration is not None:
            job_spec.completion_window = self.expiration

        return await super().create_job_with_spec(
            session_id, job_spec, timeout, initial_state, request_count
        )


@pytest.mark.asyncio
async def test_job_success_baseline(e2e_test_app, test_backend):
    """Baseline case: Create job, process successfully, verify completed payload."""
    print("Test 0: Job success baseline scenario")

    with create_test_client(e2e_test_app) as client:
        input_file_id = upload_batch_input_file(client, 2)
        debug_state = capture_runtime_debug_state(e2e_test_app, test_backend)
        batch_id = create_batch_job(client, input_file_id, test_backend=test_backend)

        try:
            await wait_for_status(client, batch_id, "in_progress")
            final_status = await wait_for_status(client, batch_id, "completed")

            validate_batch_response_with_runtime_teardown(
                final_status,
                e2e_test_app=e2e_test_app,
                test_backend=test_backend,
                batch_id=batch_id,
                debug_state=debug_state,
                expect_runtime_teardown=backend_expect_runtime_teardown(test_backend),
                expected_status="completed",
                expected_endpoint="/v1/chat/completions",
                expected_input_file_id=input_file_id,
                expected_in_progress_at=True,
                expected_finalizing_at=True,
                expected_completed_at=True,
                expected_failed_at=False,
                expected_expired_at=False,
                expected_cancelling_at=False,
                expected_cancelled_at=False,
                expected_errors=False,
                expected_output_file_id=True,
                expected_error_file_id=False,
                expected_request_counts={
                    "total": 2,
                    "completed": 2,
                    "failed": 0,
                },
            )

            print("✅ Success baseline test completed successfully")

            await run_follow_up_success_with_same_input(
                client, e2e_test_app, test_backend, input_file_id, 2
            )
            await e2e_test_app.state.batch_driver.clear_job(batch_id)
            batch_id = None
        finally:
            if batch_id is not None:
                await e2e_test_app.state.batch_driver.clear_job(batch_id)


@pytest.mark.asyncio
async def test_job_validation_failure(e2e_test_app, test_backend):
    """Test case 1: Create job, failure during validation."""
    print("Test 1: Job validation failure scenario")

    with create_test_client(e2e_test_app) as client:
        # Step 1: Skip uploading file

        # Step 2: Create batch job
        batch_request = build_batch_request(
            "invalid_input_file_id",
            "/v1/chat/completions",
            completion_window="24h",
            **dict(_backend(test_backend).request_kwargs),
        )

        batch_response = client.post("/v1/batches", json=batch_request)
        assert batch_response.status_code == 200
        batch_id = batch_response.json()["id"]
        debug_state = capture_runtime_debug_state(e2e_test_app, test_backend)

        # Step 3: Wait for validation to fail
        final_status = await wait_for_status(client, batch_id, "failed")

        # Step 4: Verify failed status using comprehensive validation
        validate_batch_response_with_runtime_teardown(
            final_status,
            e2e_test_app=e2e_test_app,
            test_backend=test_backend,
            batch_id=batch_id,
            debug_state=debug_state,
            expect_runtime_teardown=False,
            expected_status="failed",
            expected_failed_at=True,
            expected_errors=BatchJobErrorCode.INVALID_INPUT_FILE,
        )

        print("✅ Validation failure test completed successfully")

        follow_up_input_file_id = upload_batch_input_file(client, 2)

        await run_follow_up_success_with_same_input(
            client, e2e_test_app, test_backend, follow_up_input_file_id, 2
        )
        await e2e_test_app.state.batch_driver.clear_job(batch_id)
        batch_id = None


@pytest.mark.asyncio
async def test_job_processing_failure(e2e_test_app, test_backend):
    """Test case 2: Create job, failure during in progress using k8s job worker with fail_after metadata."""
    print("Test 2: Job processing failure scenario using worker fail_after metadata")

    with create_test_client(e2e_test_app) as client:
        input_file_id = upload_batch_input_file(client, 10)

        # Step 2: Inject FailingBatchManager to add fail_after metadata during job creation
        failing_manager = FailingBatchManager(
            fail_after_n_requests=1
        )  # Fail after processing 1 request
        original_manager = await swap_job_manager(e2e_test_app, failing_manager)

        try:
            # Step 3: Create batch job with failing manager that injects fail_after metadata
            debug_state = capture_runtime_debug_state(e2e_test_app, test_backend)
            batch_id = create_batch_job(
                client, input_file_id, test_backend=test_backend
            )

            # Step 4: Wait for job to start processing
            await wait_for_status(client, batch_id, "in_progress", max_polls=10)
            # Step 5: Wait for finalization to complete
            final_status = await wait_for_status(
                client, batch_id, "failed", max_polls=60, poll_interval=1.0
            )  # Wait longer for job retries

            # Step 7: Verify failed status using comprehensive validation
            validate_batch_response_with_runtime_teardown(
                final_status,
                e2e_test_app=e2e_test_app,
                test_backend=test_backend,
                batch_id=batch_id,
                debug_state=debug_state,
                expect_runtime_teardown=backend_expect_runtime_teardown(test_backend),
                expected_status="failed",
                expected_endpoint="/v1/chat/completions",
                expected_in_progress_at=True,  # Should have started processing
                expected_failed_at=True,  # Should have failure timestamp
                expected_finalizing_at=True,  # Should have reached finalizing stage
                expected_completed_at=False,
                expected_errors=(
                    BatchJobErrorCode.INTERNAL_ERROR
                    if backend_uses_fake_provisioning_runtime(test_backend)
                    else BatchJobErrorCode.INFERENCE_FAILED
                ),
                expected_output_file_id=True,
                expected_error_file_id=False,
                expected_request_counts=(
                    {
                        "total": 10,
                        "completed_at_least": 1,
                        "failed": 0,
                    }
                    if test_backend == "local_metastore_job_parallel"
                    else {
                        "total": 10,
                        "completed": 1,
                        "failed": 0,
                    }
                ),
            )

            print(
                "✅ Processing failure test with worker fail_after completed successfully"
            )
            failing_manager.fail_after_n_requests = None
            await run_follow_up_success_with_same_input(
                client, e2e_test_app, test_backend, input_file_id, 10
            )
        finally:
            if batch_id is not None:
                await e2e_test_app.state.batch_driver.clear_job(batch_id)
            restore_job_manager(e2e_test_app, original_manager)


@pytest.mark.asyncio
async def test_job_runtime_initialization_failure(e2e_test_app, test_backend):
    if not test_backend.support_runtime:
        pytest.skip("Runtime initialization failure only applies to runtime drivers")

    print("Test 2b: Job runtime initialization failure scenario")

    with create_test_client(e2e_test_app) as client:
        input_file_id = upload_batch_input_file(client, 2)
        failing_manager = FailingBatchManager(
            injected_opts={constant.BATCH_OPTS_FAIL_INIT_RUNTIME: "true"}
        )
        original_manager = await swap_job_manager(e2e_test_app, failing_manager)
        batch_id = None

        try:
            debug_state = capture_runtime_debug_state(e2e_test_app, test_backend)
            batch_id = create_batch_job(
                client, input_file_id, test_backend=test_backend
            )
            final_status = await wait_for_status(
                client, batch_id, "failed", max_polls=60, poll_interval=0.5
            )

            validate_batch_response_with_runtime_teardown(
                final_status,
                e2e_test_app=e2e_test_app,
                test_backend=test_backend,
                batch_id=batch_id,
                debug_state=debug_state,
                expect_runtime_teardown=False,
                expected_status="failed",
                expected_endpoint="/v1/chat/completions",
                expected_input_file_id=input_file_id,
                expected_in_progress_at=(
                    not (
                        backend_uses_deployment_provider(test_backend)
                        and not _backend(test_backend).fake_runtime
                    )
                ),
                expected_finalizing_at=False,
                expected_completed_at=False,
                expected_failed_at=True,
                expected_errors=BatchJobErrorCode.RESOURCE_CREATION_ERROR,
                expected_output_file_id=False,
                expected_error_file_id=False,
                expected_request_counts=True,
            )

            print("✅ Runtime initialization failure test completed successfully")
            failing_manager.injected_opts = {}
            await run_follow_up_success_with_same_input(
                client, e2e_test_app, test_backend, input_file_id, 2
            )
        finally:
            if batch_id is not None:
                await e2e_test_app.state.batch_driver.clear_job(batch_id)
            restore_job_manager(e2e_test_app, original_manager)


@pytest.mark.asyncio
async def test_job_preparation_failure(e2e_test_app, test_backend):
    print("Test 2c: Job preparation failure scenario")

    with create_test_client(e2e_test_app) as client:
        input_file_id = upload_batch_input_file(client, 2)
        failing_manager = FailingBatchManager(
            injected_opts={constant.BATCH_OPTS_FAIL_PREPARATION: "true"}
        )
        original_manager = await swap_job_manager(e2e_test_app, failing_manager)
        batch_id = None

        try:
            debug_state = capture_runtime_debug_state(e2e_test_app, test_backend)
            batch_id = create_batch_job(
                client, input_file_id, test_backend=test_backend
            )
            final_status = await wait_for_status(
                client, batch_id, "failed", max_polls=60, poll_interval=0.5
            )

            validate_batch_response_with_runtime_teardown(
                final_status,
                e2e_test_app=e2e_test_app,
                test_backend=test_backend,
                batch_id=batch_id,
                debug_state=debug_state,
                expect_runtime_teardown=backend_expect_runtime_teardown(test_backend),
                expected_status="failed",
                expected_endpoint="/v1/chat/completions",
                expected_input_file_id=input_file_id,
                expected_in_progress_at=True,
                expected_finalizing_at=False,
                expected_completed_at=False,
                expected_failed_at=True,
                expected_errors=BatchJobErrorCode.PREPARE_OUTPUT_ERROR,
                expected_output_file_id=False,
                expected_error_file_id=False,
                expected_request_counts={"total": 2, "completed": 0, "failed": 0},
            )

            print("✅ Preparation failure test completed successfully")
            failing_manager.injected_opts = {}
            await run_follow_up_success_with_same_input(
                client, e2e_test_app, test_backend, input_file_id, 2
            )
        finally:
            if batch_id is not None:
                await e2e_test_app.state.batch_driver.clear_job(batch_id)
            restore_job_manager(e2e_test_app, original_manager)


@pytest.mark.asyncio
async def test_job_finalizing_failure(e2e_test_app, test_backend):
    """Test case 3: Create job, failure during finalizing."""
    print("Test 3: Job finalizing failure scenario")

    with create_test_client(e2e_test_app) as client:
        input_file_id = upload_batch_input_file(client, 2)

        # Step 2: Inject the exception to the finalize_job_output_data to fail during finalizing
        finalizing_patcher = patch(
            "aibrix.batch.storage.adapter.BatchStorageAdapter.finalize_job_output_data"
        )
        mock_finalize = finalizing_patcher.start()
        mock_finalize.side_effect = Exception("Simulated finalization failure")
        patcher_stopped = False

        try:
            # Step 3: Create batch job
            debug_state = capture_runtime_debug_state(e2e_test_app, test_backend)
            batch_id = create_batch_job(
                client, input_file_id, test_backend=test_backend
            )

            await wait_for_status(client, batch_id, "in_progress")

            await asyncio.sleep(3)

            # Step 4: Wait for job to reach final status
            final_status = await wait_for_status(client, batch_id, "failed")

            # Step 6: Verify failed status due to finalization error using comprehensive validation
            validate_batch_response_with_runtime_teardown(
                final_status,
                e2e_test_app=e2e_test_app,
                test_backend=test_backend,
                batch_id=batch_id,
                debug_state=debug_state,
                expect_runtime_teardown=backend_expect_runtime_teardown(test_backend),
                expected_status="failed",
                expected_in_progress_at=True,  # Should have started processing
                expected_failed_at=True,  # Should have failure timestamp
                expected_finalizing_at=True,  # Should have reached finalizing stage
                expected_errors=BatchJobErrorCode.FINALIZING_ERROR,
                expected_output_file_id=True,  # Successful requests produced output
                expected_error_file_id=False,  # No requests failed -> no error file
                expected_request_counts={  # Should reflect what's done before finalizing
                    "total": 2,
                    "completed": 2,
                    "failed": 0,
                },
            )

            print("✅ Finalizing failure test completed successfully")
            finalizing_patcher.stop()
            patcher_stopped = True
            await run_follow_up_success_with_same_input(
                client, e2e_test_app, test_backend, input_file_id, 2
            )
        finally:
            await e2e_test_app.state.batch_driver.clear_job(batch_id)
            if not patcher_stopped:
                try:
                    finalizing_patcher.stop()
                except RuntimeError:
                    pass


@pytest.mark.asyncio
async def test_job_cancellation_in_validation(e2e_test_app, test_backend):
    """Test case 4: Create job, cancel during validation."""
    print("Test 4: Job cancellation during validation scenario")

    with create_test_client(e2e_test_app) as client:
        input_file_id = upload_batch_input_file(client, 1)

        # Step 2: Inject the FailingBatchManager to fail during validation
        failing_manager = FailingBatchManager(stall_validation=3.0)
        original_manager = await swap_job_manager(e2e_test_app, failing_manager)

        try:
            # Step 3: Create batch job
            debug_state = capture_runtime_debug_state(e2e_test_app, test_backend)
            batch_id = create_batch_job(
                client, input_file_id, test_backend=test_backend
            )

            # Step 4: Cancel job during processing
            cancel_response = client.post(f"/v1/batches/{batch_id}/cancel")
            assert cancel_response.status_code == 200

            # Step 5: Wait for validation to fail
            final_status = await wait_for_status(client, batch_id, "cancelled")

            # Step 6: Verify failed status using comprehensive validation
            validate_batch_response_with_runtime_teardown(
                final_status,
                e2e_test_app=e2e_test_app,
                test_backend=test_backend,
                batch_id=batch_id,
                debug_state=debug_state,
                expect_runtime_teardown=False,
                expected_status="cancelled",
                expected_cancelling_at=True,
                expected_cancelled_at=True,
            )

            print("✅ Validation failure test completed successfully")
            await run_follow_up_success_with_same_input(
                client, e2e_test_app, test_backend, input_file_id, 1
            )
        finally:
            await e2e_test_app.state.batch_driver.clear_job(batch_id)
            restore_job_manager(e2e_test_app, original_manager)


@pytest.mark.asyncio
async def test_job_cancellation_in_progress_before_preparation(
    e2e_test_app, test_backend
):
    """Test case 5: Create job, cancel during in progress, finalizing, validate finalized result."""
    print("Test 5: Job cancellation during processing scenario")

    with create_test_client(e2e_test_app) as client:
        # Step 1: Upload input file and set deterministic preparation barrier
        input_file_id = upload_batch_input_file(client, 10)
        entered_preparation = asyncio.Event()
        release_preparation = asyncio.Event()
        from aibrix.batch.job_driver.base import BaseJobDriver

        original_prepare = BaseJobDriver.prepare_job

        async def blocked_prepare_job(self, job):
            entered_preparation.set()
            await release_preparation.wait()
            return await original_prepare(self, job)

        with patch(
            "aibrix.batch.job_driver.base.BaseJobDriver.prepare_job",
            new=blocked_prepare_job,
        ):
            # Step 2: Create batch job
            debug_state = capture_runtime_debug_state(e2e_test_app, test_backend)
            batch_id = create_batch_job(
                client, input_file_id, test_backend=test_backend
            )

            try:
                # Step 3: Wait until the job is pinned inside prepare_job
                await asyncio.wait_for(entered_preparation.wait(), timeout=5)

                # Step 4: Verify the job is pending in progress before preparation completes
                status_during_prepare = client.get(f"/v1/batches/{batch_id}")
                assert status_during_prepare.status_code == 200
                assert status_during_prepare.json()["status"] == "in_progress"

                # Step 5: Cancel while preparation is still blocked
                cancel_response = client.post(f"/v1/batches/{batch_id}/cancel")
                assert cancel_response.status_code == 200

                # Step 6: Release preparation and wait for cancellation to settle
                release_preparation.set()

                final_status = await wait_for_status(
                    client, batch_id, "cancelled", max_polls=40, poll_interval=0.5
                )

                # Step 7: Verify cancelled status using comprehensive validation
                validate_batch_response_with_runtime_teardown(
                    final_status,
                    e2e_test_app=e2e_test_app,
                    test_backend=test_backend,
                    batch_id=batch_id,
                    debug_state=debug_state,
                    expect_runtime_teardown=backend_expect_runtime_teardown(
                        test_backend
                    ),
                    expected_status="cancelled",
                    expected_endpoint="/v1/chat/completions",
                    expected_in_progress_at=True,
                    expected_cancelled_at=True,
                    expected_cancelling_at=True,
                    expected_finalizing_at=True,
                    expected_completed_at=False,
                    expected_output_file_id=False,
                    expected_error_file_id=False,
                    expected_request_counts=True,
                )

                print("✅ Processing cancellation test completed successfully")
                # Step 8: Verify a follow-up job still succeeds
                await run_follow_up_success_with_same_input(
                    client, e2e_test_app, test_backend, input_file_id, 10
                )
                await e2e_test_app.state.batch_driver.clear_job(batch_id)
            finally:
                release_preparation.set()


@pytest.mark.asyncio
async def test_job_cancellation_in_progress_during_init_runtime(
    e2e_test_app,
    test_backend,
):
    skip_if_runtime_unavailable(test_backend)

    if not _backend(test_backend).support_runtime:
        pytest.skip("This test requires a supported runtime-backed driver")
    runtime_patch_target, original_runtime = backend_runtime_create_patch(test_backend)

    with create_test_client(e2e_test_app) as client:
        # Step 1: Upload input file and set deterministic runtime-init barrier
        input_file_id = upload_batch_input_file(client, 10)
        entered_init_runtime = asyncio.Event()
        release_init_runtime = asyncio.Event()

        async def blocked_create_runtime(self, job, job_id):
            entered_init_runtime.set()
            await release_init_runtime.wait()
            return await original_runtime(self, job, job_id)

        with patch(runtime_patch_target, new=blocked_create_runtime):
            # Step 2: Create deployment-backed batch job
            batch_id = create_batch_job(
                client,
                input_file_id,
                test_backend=test_backend,
            )

            try:
                # Step 3: Wait until the job is pinned inside runtime initialization
                await asyncio.wait_for(entered_init_runtime.wait(), timeout=5)

                # Step 4: Verify the job is pending in progress before runtime init completes
                status_during_init = client.get(f"/v1/batches/{batch_id}")
                assert status_during_init.status_code == 200
                assert status_during_init.json()["status"] == "in_progress"

                # Step 5: Cancel while runtime initialization is still blocked
                cancel_response = client.post(f"/v1/batches/{batch_id}/cancel")
                assert cancel_response.status_code == 200

                # Step 6: Release runtime initialization and wait for cancellation
                release_init_runtime.set()

                final_status = await wait_for_status(
                    client, batch_id, "cancelled", max_polls=40, poll_interval=0.5
                )
                # Step 7: Verify cancelled status using comprehensive validation
                validate_batch_response(
                    final_status,
                    expected_status="cancelled",
                    expected_endpoint="/v1/chat/completions",
                    expected_in_progress_at=True,
                    expected_cancelled_at=True,
                    expected_cancelling_at=True,
                    expected_finalizing_at=True,
                    expected_completed_at=False,
                    expected_output_file_id=False,
                    expected_error_file_id=False,
                    expected_request_counts=True,
                )
            finally:
                release_init_runtime.set()

            # Step 8: Verify a follow-up job still succeeds
            await run_follow_up_success_with_same_input(
                client,
                e2e_test_app,
                test_backend,
                input_file_id,
                10,
            )
            await e2e_test_app.state.batch_driver.clear_job(batch_id)
            batch_id = None


@pytest.mark.asyncio
async def test_job_cancellation_in_progress_during_service_discovery(
    e2e_test_app,
    test_backend,
    monkeypatch,
):
    if not backend_has_feature(test_backend, "service_discovery"):
        pytest.skip("This test requires service discovery support")

    with create_test_client(e2e_test_app) as client:
        input_file_id = upload_batch_input_file(client, 10)
        debug_state = capture_runtime_debug_state(e2e_test_app, test_backend)
        model_discovery = e2e_test_app.state.batch_driver._context.values[
            "service_model_discovery"
        ]
        entered_discovery = asyncio.Event()
        wait_slices: list[float] = []

        async def blocked_wait_for_model_endpoints(
            served_model_name: str,
            timeout_seconds: float,
            service_id: Optional[str] = None,
            filter_tags: Optional[dict[str, str]] = None,
            lookup_timeout_seconds: Optional[float] = None,
            poll_interval_seconds: float = 1.0,
        ):
            del (
                served_model_name,
                service_id,
                filter_tags,
                lookup_timeout_seconds,
                poll_interval_seconds,
            )
            entered_discovery.set()
            wait_slices.append(timeout_seconds)
            await asyncio.sleep(timeout_seconds)
            raise TimeoutError("simulated discovery slice timeout")

        monkeypatch.setattr(
            model_discovery,
            "wait_for_model_endpoints",
            blocked_wait_for_model_endpoints,
        )

        batch_id = create_batch_job(
            client,
            input_file_id,
            test_backend=test_backend,
        )
        try:
            await asyncio.wait_for(entered_discovery.wait(), timeout=5)

            status_during_discovery = client.get(f"/v1/batches/{batch_id}")
            assert status_during_discovery.status_code == 200
            assert status_during_discovery.json()["status"] == "in_progress"

            cancel_response = client.post(f"/v1/batches/{batch_id}/cancel")
            assert cancel_response.status_code == 200
            assert cancel_response.json()["status"] == "cancelling"

            final_status = await wait_for_status(
                client, batch_id, "cancelled", max_polls=40, poll_interval=0.1
            )
            await wait_for_runtime_teardown_delta(
                e2e_test_app,
                test_backend,
                debug_state,
                expected_delta=1,
            )
            validate_batch_response(
                final_status,
                expected_status="cancelled",
                expected_endpoint="/v1/chat/completions",
                expected_in_progress_at=True,
                expected_cancelling_at=True,
                expected_cancelled_at=True,
                expected_finalizing_at=True,
                expected_completed_at=False,
                expected_output_file_id=False,
                expected_error_file_id=False,
                expected_request_counts=True,
            )
            assert wait_slices, "caller should wait for discovery in short slices"
            assert max(wait_slices) <= 1.0
        finally:
            await e2e_test_app.state.batch_driver.clear_job(batch_id)


@pytest.mark.asyncio
async def test_job_cancellation_in_progress(e2e_test_app, test_backend, monkeypatch):
    """Test case 5: Create job, cancel during in progress, finalizing, validate finalized result."""
    print("Test 5: Job cancellation during processing scenario")
    pin_job_concurrency_to_serial(monkeypatch, test_backend, per_request_delay=0.3)
    entered_pending, release_pending = install_pending_after_n_requests_barrier(
        monkeypatch,
        e2e_test_app,
        test_backend,
        wait_for_release=True,
    )
    inject_job_creation_opts(
        monkeypatch,
        e2e_test_app,
        {TEST_OPTS_PENDING_AFTER_N_REQUESTS: "3"},
    )

    with create_test_client(e2e_test_app) as client:
        input_file_id = upload_batch_input_file(client, 10)
        # Step 2: Create batch job
        debug_state = capture_runtime_debug_state(e2e_test_app, test_backend)
        batch_id = create_batch_job(client, input_file_id, test_backend=test_backend)
        expect_exact_request_counts = backend_expect_exact_request_counts(test_backend)

        try:
            # Step 3: Wait until exactly 3 requests have been persisted and the job blocks
            entered = await asyncio.to_thread(entered_pending.wait, 10)
            if entered:
                status_during_pending = client.get(f"/v1/batches/{batch_id}")
                assert status_during_pending.status_code == 200
                payload = status_during_pending.json()
            else:
                pytest.fail("Job did not reach exactly 3 completed requests in time")

            assert payload["status"] == "in_progress"
            if expect_exact_request_counts:
                assert payload["request_counts"] == {
                    "total": 10,
                    "completed": 3,
                    "failed": 0,
                }
            else:
                assert payload["request_counts"] is not None
                assert payload["request_counts"]["total"] == 10
                assert payload["request_counts"]["failed"] == 0
                assert payload["request_counts"]["completed"] >= 3

            # Step 4: Cancel job during processing
            cancel_response = client.post(f"/v1/batches/{batch_id}/cancel")
            assert cancel_response.status_code == 200
            release_pending.set()

            # Step 5: Wait for cancellation and finalization
            final_status = await wait_for_status(
                client, batch_id, "cancelled", max_polls=40, poll_interval=0.5
            )

            # Step 6: Verify cancelled status using comprehensive validation
            validate_batch_response_with_runtime_teardown(
                final_status,
                e2e_test_app=e2e_test_app,
                test_backend=test_backend,
                batch_id=batch_id,
                debug_state=debug_state,
                expect_runtime_teardown=backend_expect_runtime_teardown(test_backend),
                expected_status="cancelled",
                expected_endpoint="/v1/chat/completions",
                expected_in_progress_at=True,  # Should have started processing
                expected_cancelled_at=True,
                expected_cancelling_at=True,  # Should have cancelling start timestamp
                expected_finalizing_at=True,  # Should have finalizing timestamp
                expected_completed_at=False,
                expected_output_file_id=True,
                expected_error_file_id=False,
                expected_request_counts=(
                    {"total": 10, "completed": 3, "failed": 0}
                    if expect_exact_request_counts
                    else True
                ),
            )
            if not expect_exact_request_counts:
                assert final_status["request_counts"]["total"] == 10
                assert final_status["request_counts"]["failed"] == 0
                assert final_status["request_counts"]["completed"] >= 3

            print("✅ Processing cancellation test completed successfully")
            await run_follow_up_success_with_same_input(
                client, e2e_test_app, test_backend, input_file_id, 10
            )
        finally:
            await e2e_test_app.state.batch_driver.clear_job(batch_id)


@pytest.mark.asyncio
async def test_job_cancellation_in_finalizing(e2e_test_app, test_backend):
    """Test case 6: Create job, cancel during finalizing, report completed."""
    print("Test 6: Job cancellation during finalizing (reports completed) scenario")

    with create_test_client(e2e_test_app) as client:
        input_file_id = upload_batch_input_file(client, 10)

        # Step 2: Inject the FailingBatchManager to stall cancellation execution
        # and hold finalization long enough to send the cancel while finalizing.
        from aibrix.batch import storage as batch_storage

        original_finalize = batch_storage.finalize_job_output_data

        async def delayed_finalize(job):
            await asyncio.sleep(2.0)
            return await original_finalize(job)

        failing_manager = FailingBatchManager(stall_cancelling=2.0)
        original_manager = await swap_job_manager(e2e_test_app, failing_manager)

        # Step 3: Create batch job
        debug_state = capture_runtime_debug_state(e2e_test_app, test_backend)
        batch_id = create_batch_job(client, input_file_id, test_backend=test_backend)

        try:
            with patch(
                "aibrix.batch.job_driver.base.storage.finalize_job_output_data",
                side_effect=delayed_finalize,
            ):
                # Step 4: Wait until the job is already finalizing
                status_during_finalizing = await wait_for_status(
                    client, batch_id, "finalizing", max_polls=40, poll_interval=0.5
                )
                assert status_during_finalizing["status"] == "finalizing"

                # Step 5: Cancellation should not interrupt finalization once it has started
                cancel_response = client.post(f"/v1/batches/{batch_id}/cancel")
                assert cancel_response.status_code == 200

                # Step 6: Wait for final status and verify the job still completes
                final_status = await wait_for_status(
                    client, batch_id, "completed", max_polls=20, poll_interval=0.5
                )

                # Step 7: Verify final status using comprehensive validation
                validate_batch_response_with_runtime_teardown(
                    final_status,
                    e2e_test_app=e2e_test_app,
                    test_backend=test_backend,
                    batch_id=batch_id,
                    debug_state=debug_state,
                    expect_runtime_teardown=backend_expect_runtime_teardown(
                        test_backend
                    ),
                    expected_status="completed",
                    expected_endpoint="/v1/chat/completions",
                    expected_in_progress_at=True,  # Should have started processing
                    expected_completed_at=True,  # Should have completion timestamp
                    expected_finalizing_at=True,  # Should have reached finalizing
                    expected_cancelling_at=False,
                    expected_cancelled_at=False,
                    expected_errors="cancel_rejected",
                    expected_output_file_id=True,  # Should have output file
                    expected_error_file_id=False,  # No failures -> no error file
                    expected_request_counts=True,  # Should have request counts
                )
                print("  Job completed without cancelling finalization")
                await run_follow_up_success_with_same_input(
                    client, e2e_test_app, test_backend, input_file_id, 10
                )
        finally:
            await e2e_test_app.state.batch_driver.clear_job(batch_id)
            restore_job_manager(e2e_test_app, original_manager)


@pytest.mark.asyncio
async def test_job_expiration_in_finalizing(e2e_test_app, test_backend):
    """Batch completion-window expiry after entering FINALIZING should not
    interrupt terminalization; the job still completes."""
    print("Test 6b: Job expiration during finalizing scenario")

    with create_test_client(e2e_test_app) as client:
        input_file_id = upload_batch_input_file(client, 2)

        from aibrix.batch import storage as batch_storage

        original_finalize = batch_storage.finalize_job_output_data

        async def delayed_finalize(job):
            await asyncio.sleep(3.0)
            return await original_finalize(job)

        failing_manager = FailingBatchManager(expiration=2)
        original_manager = await swap_job_manager(e2e_test_app, failing_manager)
        batch_id = None

        try:
            debug_state = capture_runtime_debug_state(e2e_test_app, test_backend)
            batch_id = create_batch_job(
                client, input_file_id, test_backend=test_backend
            )

            with patch(
                "aibrix.batch.job_driver.base.storage.finalize_job_output_data",
                side_effect=delayed_finalize,
            ):
                status_during_finalizing = await wait_for_status(
                    client, batch_id, "finalizing", max_polls=40, poll_interval=0.5
                )
                assert status_during_finalizing["status"] == "finalizing"

                final_status = await wait_for_status(
                    client, batch_id, "completed", max_polls=40, poll_interval=0.5
                )

                validate_batch_response_with_runtime_teardown(
                    final_status,
                    e2e_test_app=e2e_test_app,
                    test_backend=test_backend,
                    batch_id=batch_id,
                    debug_state=debug_state,
                    expect_runtime_teardown=backend_expect_runtime_teardown(
                        test_backend
                    ),
                    expected_status="completed",
                    expected_completion_window="0h",
                    expected_endpoint="/v1/chat/completions",
                    expected_in_progress_at=True,
                    expected_finalizing_at=True,
                    expected_completed_at=True,
                    expected_failed_at=False,
                    expected_expired_at=False,
                    expected_cancelled_at=False,
                    expected_cancelling_at=False,
                    expected_output_file_id=True,
                    expected_error_file_id=False,
                    expected_request_counts=True,
                )

                failing_manager.expiration = None
                await run_follow_up_success_with_same_input(
                    client, e2e_test_app, test_backend, input_file_id, 2
                )
        finally:
            if batch_id is not None:
                await e2e_test_app.state.batch_driver.clear_job(batch_id)
            restore_job_manager(e2e_test_app, original_manager)


@pytest.mark.asyncio
async def test_job_expiration_during_validation(e2e_test_app, test_backend):
    """Test case 7: Create job, set expire to 1min and prevent validation, expired and report."""
    print("Test 7: Job expiration during validation scenario")

    with create_test_client(e2e_test_app) as client:
        input_file_id = upload_batch_input_file(client, 2)

        # Step 2: Mock job manager to prevent validation
        failing_manager = FailingBatchManager(prevent_validation=True, expiration=1)
        original_manager = await swap_job_manager(e2e_test_app, failing_manager)
        batch_id = None

        try:
            # Step 3: Create batch job with very short completion window
            # Patch the completion window to be very short for testing
            with patch(
                "aibrix.batch.constant.EXPIRE_INTERVAL", 0.1
            ):  # Check every 100ms
                debug_state = capture_runtime_debug_state(e2e_test_app, test_backend)
                batch_id = create_batch_job(
                    client, input_file_id, test_backend=test_backend
                )

                # Step 4: Wait longer than completion window for expiration
                await asyncio.sleep(2)  # Wait for expiration to trigger

                # Step 5: Check that job expired
                final_status = await wait_for_status(
                    client, batch_id, "expired", max_polls=10
                )

                # Step 6: Verify expired status using comprehensive validation
                validate_batch_response_with_runtime_teardown(
                    final_status,
                    e2e_test_app=e2e_test_app,
                    test_backend=test_backend,
                    batch_id=batch_id,
                    debug_state=debug_state,
                    expect_runtime_teardown=False,
                    expected_status="expired",
                    expected_completion_window="0h",
                    expected_endpoint="/v1/chat/completions",
                    expected_in_progress_at=False,  # Should be None (expired before processing)
                    expected_expired_at=True,  # Should have expiration timestamp
                    expected_failed_at=False,  # Should be None (expired, not failed)
                    expected_completed_at=False,  # Should be None (expired, not completed)
                    expected_cancelled_at=False,  # Should be None (not cancelled)
                    expected_cancelling_at=False,  # Should be None (not cancelled)
                    expected_finalizing_at=False,  # Should be None (expired before finalizing)
                    expected_errors=False,  # Should be None (expired during validation)
                    expected_output_file_id=False,  # Should be None (expired before processing)
                    expected_error_file_id=False,  # Should be None (expired before processing)
                    expected_request_counts=False,  # Should be None (expired before processing)
                )

                print("✅ Validation expiration test completed successfully")
                failing_manager.prevent_validation = False
                failing_manager.expiration = None
                await run_follow_up_success_with_same_input(
                    client, e2e_test_app, test_backend, input_file_id, 2
                )

        finally:
            if batch_id is not None:
                await e2e_test_app.state.batch_driver.clear_job(batch_id)
            restore_job_manager(e2e_test_app, original_manager)


@pytest.mark.asyncio
async def test_job_expiration_during_processing(
    e2e_test_app, test_backend, monkeypatch
):
    """Test case 8: Create job, set expire to 1min, expired during in progress, finalizing, validate result."""
    print("Test 8: Job expiration during processing scenario")
    pin_job_concurrency_to_serial(monkeypatch, test_backend)

    with create_test_client(e2e_test_app) as client:
        input_file_id = upload_batch_input_file(client, 3)
        original_delay = set_runtime_inference_delay(
            e2e_test_app,
            test_backend,
            delay_seconds=(
                3.0 if test_backend == "local_metastore_job_parallel" else 1.0
            ),
        )

        # Step 2: Inject FailingBatchManager to add fail_after metadata during job creation
        failing_manager = FailingBatchManager(expiration=2)  # Expired after 2 seconds
        original_manager = await swap_job_manager(e2e_test_app, failing_manager)

        try:
            # Step 3: Create batch job with failing manager that injects fail_after metadata
            debug_state = capture_runtime_debug_state(e2e_test_app, test_backend)
            terminate_calls: list[str] = []
            original_terminate = BaseJobDriver.terminate

            async def recording_terminate(
                self, deleted_job: BatchJob
            ) -> TerminateResult:
                terminate_calls.append(deleted_job.job_id)
                return await original_terminate(self, deleted_job)

            with patch.object(
                BaseJobDriver,
                "terminate",
                new=recording_terminate,
            ):
                batch_id = create_batch_job(
                    client, input_file_id, test_backend=test_backend
                )

                # Step 4: Wait for job to start processing
                await wait_for_status(client, batch_id, "in_progress", max_polls=10)
                # Step 5: Wait for expiration to complete
                final_status = await wait_for_status(
                    client, batch_id, "expired", max_polls=60, poll_interval=1.0
                )

                # Step 7: Verify expired status using comprehensive validation
                validate_batch_response_with_runtime_teardown(
                    final_status,
                    e2e_test_app=e2e_test_app,
                    test_backend=test_backend,
                    batch_id=batch_id,
                    debug_state=debug_state,
                    expect_runtime_teardown=backend_expect_runtime_teardown(
                        test_backend
                    ),
                    expected_status="expired",
                    expected_endpoint="/v1/chat/completions",
                    expected_completion_window="0h",  # overrided to 2s
                    expected_in_progress_at=True,  # Should have started processing
                    expected_completed_at=False,
                    expected_expired_at=True,
                    expected_finalizing_at=True,  # Should have reached finalizing stage
                    expected_output_file_id=True,
                    expected_error_file_id=False,
                    expected_request_counts=True,
                )
                # terminate is idempotent: the expiry and deletion paths can each
                # invoke it for the same job, and the second call is rejected by the
                # guard in BaseJobDriver.terminate. Assert it ran for this job and
                # never for another, tolerating the benign retry.
                assert terminate_calls and set(terminate_calls) == {batch_id}

            print(
                "✅ Processing failure test with worker fail_after completed successfully"
            )
            failing_manager.expiration = None
            await run_follow_up_success_with_same_input(
                client, e2e_test_app, test_backend, input_file_id, 3
            )
        finally:
            if original_delay is not None:
                set_runtime_inference_delay(
                    e2e_test_app, test_backend, delay_seconds=original_delay
                )
            await e2e_test_app.state.batch_driver.clear_job(batch_id)
            restore_job_manager(e2e_test_app, original_manager)


@pytest.mark.asyncio
async def test_job_restart_during_validation(
    e2e_test_app, test_backend, request, tmp_path, monkeypatch
):
    if not backend_supports_restart_phase_recovery(test_backend):
        pytest.skip("Restart phase recovery is covered on local and redis backends")

    # Test setup
    force_batch_driver_crash_on_shutdown(monkeypatch, e2e_test_app)

    with create_test_client(e2e_test_app) as client:
        input_file_id = upload_batch_input_file(client, 2)
        await swap_job_manager(e2e_test_app, FailingBatchManager(stall_validation=2.0))
        batch_id = create_batch_job(client, input_file_id, test_backend=test_backend)
        await wait_for_status(
            client, batch_id, "validating", max_polls=20, poll_interval=0.2
        )
        original_debug_state = capture_runtime_debug_state(e2e_test_app, test_backend)

    expect_runtime_activity_on_restart = (
        backend_observes_restart_validation_runtime_delta(test_backend)
    )

    await complete_job_after_restart(
        request,
        test_backend,
        tmp_path,
        monkeypatch,
        batch_id,
        2,
        expect_runtime_created_on_restart=(
            backend_expect_runtime_teardown(test_backend)
            if expect_runtime_activity_on_restart
            else False
        ),
        expect_runtime_used_on_restart=(
            backend_expect_runtime_teardown(test_backend)
            if expect_runtime_activity_on_restart
            else False
        ),
        expect_runtime_teardown_on_restart=(
            backend_expect_runtime_teardown(test_backend)
            if expect_runtime_activity_on_restart
            else False
        ),
        original_app=e2e_test_app,
        original_debug_state=original_debug_state,
    )


@pytest.mark.asyncio
async def test_job_restart_during_in_progress(
    e2e_test_app, test_backend, request, tmp_path, monkeypatch
):
    if not backend_supports_restart_phase_recovery(test_backend):
        pytest.skip("Restart phase recovery is not covered for this backend")

    # Test setup
    pin_job_concurrency_to_serial(monkeypatch, test_backend)
    force_batch_driver_crash_on_shutdown(monkeypatch, e2e_test_app)
    entered_pending, _ = install_pending_after_n_requests_barrier(
        monkeypatch,
        e2e_test_app,
        test_backend,
    )
    inject_job_creation_opts(
        monkeypatch,
        e2e_test_app,
        {TEST_OPTS_PENDING_AFTER_N_REQUESTS: "1"},
    )

    with create_test_client(e2e_test_app) as client:
        input_file_id = upload_batch_input_file(client, 10)
        batch_id = create_batch_job(client, input_file_id, test_backend=test_backend)
        entered = await asyncio.to_thread(entered_pending.wait, 5)
        assert entered, "Job did not enter pending-after-n-requests barrier in time"
        status_during_pending = client.get(f"/v1/batches/{batch_id}")
        assert status_during_pending.status_code == 200
        payload = status_during_pending.json()
        assert payload["status"] == "in_progress"
        original_debug_state = capture_runtime_debug_state(e2e_test_app, test_backend)
    await complete_job_after_restart(
        request,
        test_backend,
        tmp_path,
        monkeypatch,
        batch_id,
        10,
        expect_runtime_created_on_restart=False,
        expect_runtime_used_on_restart=backend_expect_runtime_teardown(test_backend),
        expect_runtime_teardown_on_restart=backend_expect_runtime_teardown(
            test_backend
        ),
        original_app=e2e_test_app,
        original_debug_state=original_debug_state,
    )


@pytest.mark.asyncio
async def test_job_restart_during_finalizing_before_runtime_shutdown(
    e2e_test_app, test_backend, request, tmp_path, monkeypatch
):
    skip_if_runtime_unavailable(test_backend)

    # Test setup
    force_batch_driver_crash_on_shutdown(monkeypatch, e2e_test_app)
    install_runtime_finalize_shutdown_delay(monkeypatch, test_backend)
    inject_job_creation_opts(
        monkeypatch,
        e2e_test_app,
        {TEST_OPTS_DELAY_BEFORE_FINALIZE_SHUTDOWN_SECONDS: "3"},
    )

    with create_test_client(e2e_test_app) as client:
        input_file_id = upload_batch_input_file(client, 6)
        original_debug_state = capture_runtime_debug_state(e2e_test_app, test_backend)
        batch_id = create_batch_job(client, input_file_id, test_backend=test_backend)
        await wait_for_status(
            client, batch_id, "finalizing", max_polls=40, poll_interval=0.5
        )
        verify_runtime_not_torn_down(e2e_test_app, test_backend, original_debug_state)

    await complete_job_after_restart(
        request,
        test_backend,
        tmp_path,
        monkeypatch,
        batch_id,
        6,
        expect_runtime_created_on_restart=False,
        expect_runtime_used_on_restart=False,
        expect_runtime_teardown_on_restart=backend_expect_runtime_teardown(
            test_backend
        ),
        expect_runtime_teardown_before_restart=False,
        original_app=e2e_test_app,
        original_debug_state=original_debug_state,
    )


@pytest.mark.asyncio
async def test_job_restart_during_finalizing_after_runtime_shutdown(
    e2e_test_app, test_backend, request, tmp_path, monkeypatch
):
    # Test setup
    force_batch_driver_crash_on_shutdown(monkeypatch, e2e_test_app)
    install_runtime_finalize_shutdown_delay(monkeypatch, test_backend)
    inject_job_creation_opts(
        monkeypatch,
        e2e_test_app,
        {TEST_OPTS_DELAY_AFTER_FINALIZE_SHUTDOWN_SECONDS: "3"},
    )

    with create_test_client(e2e_test_app) as client:
        input_file_id = upload_batch_input_file(client, 6)
        original_debug_state = capture_runtime_debug_state(e2e_test_app, test_backend)
        batch_id = create_batch_job(client, input_file_id, test_backend=test_backend)
        await wait_for_status(
            client, batch_id, "finalizing", max_polls=40, poll_interval=0.5
        )
        await wait_for_runtime_teardown_delta(
            e2e_test_app, test_backend, original_debug_state, expected_delta=1
        )

    await complete_job_after_restart(
        request,
        test_backend,
        tmp_path,
        monkeypatch,
        batch_id,
        6,
        expect_runtime_created_on_restart=False,
        expect_runtime_used_on_restart=False,
        expect_runtime_teardown_on_restart=False,
        expect_runtime_teardown_before_restart=backend_expect_runtime_teardown(
            test_backend
        ),
        original_app=e2e_test_app,
        original_debug_state=original_debug_state,
    )


if __name__ == "__main__":
    # Allow running individual tests
    import sys

    if len(sys.argv) > 1:
        test_name = sys.argv[1]
        if hasattr(sys.modules[__name__], test_name):
            asyncio.run(getattr(sys.modules[__name__], test_name)())
    else:
        print("Available tests:")
        for name in dir(sys.modules[__name__]):
            if name.startswith("test_"):
                print(f"  {name}")

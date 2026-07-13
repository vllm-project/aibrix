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

import argparse
import asyncio
import copy
import json
import os
import re
import uuid
from concurrent.futures import ThreadPoolExecutor
from concurrent.futures import TimeoutError as FutureTimeoutError
from pathlib import Path
from typing import Any, Callable, Dict, Optional

import boto3
import pytest
import yaml
from fastapi.testclient import TestClient
from kubernetes import client, config

import aibrix.client.redis as redis_client
from aibrix import envs
from aibrix.batch.client.channel import EchoChannel
from aibrix.batch.client.engine import DispatchEngine
from aibrix.batch.client.errors import InferenceError, InferenceErrorCode
from aibrix.batch.client.sources import NoopEndpointSource
from aibrix.batch.job_driver.base import BaseJobDriver
from aibrix.logger import init_logger
from aibrix.metadata.app import build_app
from aibrix.metadata.setting import settings
from aibrix.storage import StorageType
from tests.batch.job_driver.runtime.deployment_backend import (
    configure_local_metastore_deployment_backend,
)
from tests.batch.job_driver.runtime.k8s_job_backend import (
    configure_local_metastore_k8s_job_backend,
)

logger = init_logger(__name__)


def pin_job_concurrency_to_serial(
    monkeypatch: pytest.MonkeyPatch,
    *,
    per_request_delay: float = 0.0,
) -> None:
    """Opt a test into the serial worker path; concurrent remains the default."""

    async def serial_execute_worker(self, job_id, next_pass_start=None):
        if self._engine is None:
            raise RuntimeError(
                "JobDriver was constructed without an engine; execute_worker "
                "requires one."
            )

        job, stopped = await self._is_job_stopped(job_id)
        if stopped:
            return await self._finish_stopped_job(job)
        return await self._execute_worker_serial(job, start_index=next_pass_start)

    monkeypatch.setattr(
        BaseJobDriver,
        "execute_worker",
        serial_execute_worker,
    )

    if per_request_delay <= 0:
        return

    original_send = DispatchEngine._send_with_failover

    async def delayed_send(self, request):
        await asyncio.sleep(per_request_delay)
        return await original_send(self, request)

    monkeypatch.setattr(DispatchEngine, "_send_with_failover", delayed_send)


@pytest.fixture
def serial_worker(
    request: pytest.FixtureRequest, monkeypatch: pytest.MonkeyPatch
) -> None:
    per_request_delay = float(getattr(request, "param", 0.0) or 0.0)
    pin_job_concurrency_to_serial(
        monkeypatch,
        per_request_delay=per_request_delay,
    )


def _keyword_enables_serial_worker(keyword: str) -> bool:
    return "serial_worker" in keyword


def pytest_collection_modifyitems(items: list[pytest.Item]) -> None:
    for item in items:
        item.extra_keyword_matches.add("serial_worker")


@pytest.fixture(autouse=True)
def enable_batch_driver_error_injection(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(envs, "BATCH_ERROR_INJECTION_ENABLED", True)


@pytest.fixture(autouse=True)
def keyword_serial_worker(
    request: pytest.FixtureRequest, monkeypatch: pytest.MonkeyPatch
) -> None:
    if "test_backend" not in request.fixturenames:
        return
    test_backend = get_e2e_backend(request.getfixturevalue("test_backend"))
    if not test_backend.serial_worker:
        return
    pin_job_concurrency_to_serial(monkeypatch)


@pytest.fixture(scope="session")
def k8s_config():
    """Initialize Kubernetes client and test connectivity."""
    try:
        # Try to load in-cluster config first, then fallback to local config
        try:
            config.load_incluster_config()
            logger.info("Loaded in-cluster Kubernetes configuration")
        except config.ConfigException:
            try:
                config.load_kube_config()
                logger.info("Loaded local Kubernetes configuration")
            except config.ConfigException as e:
                pytest.skip(f"Kubernetes configuration not available: {e}")

        # Test API server accessibility with client-side request timeout.
        try:
            v1 = client.CoreV1Api()
            api_host = v1.api_client.configuration.host
            if not api_host:
                pytest.skip(
                    "Kubernetes configuration is invalid: no API server host found"
                )

            logger.info(f"Testing Kubernetes API accessibility: {api_host}")
            v1.list_namespace(limit=1, _request_timeout=(1, 2))
            logger.info("Kubernetes API server accessibility verified")

        except Exception as e:
            pytest.skip(f"Failed to create Kubernetes API client: {e}")

    except Exception as e:
        pytest.skip(f"Failed to initialize Kubernetes client: {e}")


@pytest.fixture(scope="session")
def test_namespace():
    """Use default namespace for testing."""
    return "default"


@pytest.fixture(scope="session")
def s3_config_available():
    """Check if S3 configuration is available locally."""
    try:
        # Check for AWS credentials
        session = boto3.Session()
        credentials = session.get_credentials()

        if not credentials:
            pytest.skip("No AWS credentials found")

        # Check for required environment variables or default credentials
        access_key = credentials.access_key
        secret_key = credentials.secret_key

        if not access_key or not secret_key:
            pytest.skip("AWS credentials incomplete")

        # Test S3 access
        s3_client = session.client("s3")
        s3_client.list_buckets()

        return {
            "access_key": access_key,
            "secret_key": secret_key,
            "region": session.region_name or "us-west-2",
        }

    except Exception as e:
        pytest.skip(f"S3 configuration not available: {e}")


@pytest.fixture(scope="session")
def redis_available():
    """Test whether Redis connectivity is available for batch tests."""
    try:

        def verify_connection():
            async def ping():
                client = redis_client.get_redis_client(test=True)
                try:
                    return await client.ping()
                finally:
                    await client.aclose()

            return asyncio.run(ping())

        with ThreadPoolExecutor(max_workers=1) as executor:
            future = executor.submit(verify_connection)
            try:
                ping_result = future.result(timeout=10)
                if not ping_result:
                    raise RuntimeError(
                        f"Redis ping returned unexpected falsy result: {ping_result!r}"
                    )
            except FutureTimeoutError as ex:
                pytest.skip(
                    "Redis access not available: Redis connectivity check timed out after 10 seconds "
                    f"({type(ex).__name__})"
                )
            except Exception as ex:  # noqa: BLE001
                pytest.skip(f"Redis access not available: {ex}")
    except ImportError as ex:
        pytest.skip(f"Redis access not available: {ex}")
    except Exception as ex:  # noqa: BLE001
        pytest.skip(f"Redis access not available: {ex}")


async def _delete_prefixed_redis_keys(redis_prefix: str) -> int:
    client = redis_client.get_redis_client(test=True)
    try:
        timestamps_all_key = f"{redis_prefix}:timestamps:all"
        timestamp_members = await client.zrange(timestamps_all_key, 0, -1)
        object_keys = [
            member.decode("utf-8") if isinstance(member, bytes) else str(member)
            for member in timestamp_members
        ]

        cleanup_keys = {timestamps_all_key}
        for object_key in object_keys:
            cleanup_keys.add(object_key)
            relative_key = object_key.removeprefix(f"{redis_prefix}:")
            if "/" not in relative_key:
                continue
            parent_key, _ = relative_key.rsplit("/", 1)
            cleanup_keys.add(f"{redis_prefix}:{parent_key}:index")
            cleanup_keys.add(f"{redis_prefix}:timestamps:{parent_key}")

        if cleanup_keys:
            await client.delete(*sorted(cleanup_keys))
        return len(cleanup_keys)
    finally:
        await client.aclose()


def cleanup_test_redis_prefix(redis_prefix: str) -> None:
    deleted = asyncio.run(_delete_prefixed_redis_keys(redis_prefix))
    logger.info(  # type: ignore[call-arg]
        "Cleaned test Redis prefix",
        redis_prefix=redis_prefix,
        deleted_keys=deleted,
    )


@pytest.fixture(scope="session")
def test_s3_bucket(s3_config_available):
    """Get or create test S3 bucket."""
    bucket_name = os.getenv("AIBRIX_TEST_S3_BUCKET")

    session = boto3.Session()
    s3_client = session.client("s3")

    try:
        # Try to access the bucket
        s3_client.head_bucket(Bucket=bucket_name)
        logger.info(f"Using existing S3 bucket: {bucket_name}")
    except s3_client.exceptions.NoSuchBucket:
        pytest.skip(
            f"Test bucket {bucket_name} does not exist. Set TEST_S3_BUCKET env var or create bucket."
        )
    except Exception as e:
        pytest.skip(f"Cannot access S3 bucket {bucket_name}: {e}")

    return bucket_name


@pytest.fixture(scope="session")
def s3_credentials_secret(
    k8s_config, test_namespace, s3_config_available, test_s3_bucket
):
    """Create K8s secret with S3 credentials from YAML template."""
    import base64

    # Load secret template from YAML
    secret_template_path = Path(__file__).parent / "testdata" / "s3_secret.yaml"
    with open(secret_template_path, "r") as f:
        secret_template = yaml.safe_load(f)

    core_v1 = client.CoreV1Api()
    secret_name = secret_template["metadata"]["name"]

    # Populate secret data with actual values (K8s expects base64 encoded values)
    secret_template["data"] = {
        "access-key-id": base64.b64encode(
            s3_config_available["access_key"].encode()
        ).decode(),
        "secret-access-key": base64.b64encode(
            s3_config_available["secret_key"].encode()
        ).decode(),
        "region": base64.b64encode(s3_config_available["region"].encode()).decode(),
        "bucket-name": base64.b64encode(test_s3_bucket.encode()).decode(),
    }

    # Update namespace
    secret_template["metadata"]["namespace"] = test_namespace

    # Create K8s Secret object
    secret = client.V1Secret(
        metadata=client.V1ObjectMeta(name=secret_name, namespace=test_namespace),
        data=secret_template["data"],
        type=secret_template["type"],
    )

    try:
        # Delete existing secret if it exists
        try:
            core_v1.delete_namespaced_secret(name=secret_name, namespace=test_namespace)
        except client.ApiException as e:
            if e.status != 404:
                raise

        # Create the secret
        core_v1.create_namespaced_secret(namespace=test_namespace, body=secret)
        logger.info(f"Created K8s secret: {secret_name}")

        yield secret_name

    finally:
        # Cleanup: delete the secret
        try:
            core_v1.delete_namespaced_secret(name=secret_name, namespace=test_namespace)
            logger.info(f"Deleted K8s secret: {secret_name}")
        except client.ApiException as e:
            if e.status != 404:
                logger.warning(f"Failed to cleanup secret {secret_name}: {e}")


@pytest.fixture(scope="session")
def job_rbac(k8s_config, test_namespace):
    """
    Session-scoped fixture to set up RBAC resources for job testing.
    This ensures that tests using create_test_app with enable_k8s_support=True
    have the necessary service accounts and permissions.
    Returns the service account name for use in job creation.
    """
    from kubernetes import utils

    # Load RBAC resources from YAML
    rbac_yaml_path = Path(__file__).parent / "testdata" / "job_rbac.yaml"

    # Read YAML content and update namespace if needed
    with open(rbac_yaml_path, "r") as f:
        yaml_content = f.read()

    # Replace default namespace with test namespace if different
    if test_namespace != "default":
        yaml_content = yaml_content.replace(
            "namespace: default", f"namespace: {test_namespace}"
        )

    # Parse YAML to get resource info for cleanup
    rbac_docs = list(yaml.safe_load_all(yaml_content))
    created_resources = []
    service_account_name = None

    # Capture service account name for return
    for doc in rbac_docs:
        if doc and doc.get("kind") == "ServiceAccount":
            service_account_name = doc.get("metadata", {}).get("name")
            break

    try:
        # Apply YAML using Kubernetes utils
        logger.info(f"Applying RBAC resources from {rbac_yaml_path}")

        # Create API client
        k8s_client = client.ApiClient()

        # Apply the YAML content
        utils.create_from_yaml(
            k8s_client, yaml_objects=rbac_docs, namespace=test_namespace
        )

        # Track created resources for cleanup
        for doc in rbac_docs:
            if doc:
                kind = doc.get("kind")
                name = doc.get("metadata", {}).get("name")
                namespace = doc.get("metadata", {}).get("namespace", test_namespace)
                if namespace == "default":
                    namespace = test_namespace
                created_resources.append((kind, name, namespace))

        logger.info(
            f"Successfully applied RBAC resources. Service account: {service_account_name}"
        )
        yield service_account_name

    except Exception as e:
        logger.error(f"Failed to apply RBAC resources: {e}")
        raise

    finally:
        # Cleanup: delete created resources
        logger.info("Cleaning up RBAC resources...")
        core_v1 = client.CoreV1Api()
        rbac_v1 = client.RbacAuthorizationV1Api()

        for kind, name, namespace in reversed(created_resources):
            try:
                if kind == "ServiceAccount":
                    core_v1.delete_namespaced_service_account(
                        name=name, namespace=namespace
                    )
                elif kind == "Role":
                    rbac_v1.delete_namespaced_role(name=name, namespace=namespace)
                elif kind == "RoleBinding":
                    rbac_v1.delete_namespaced_role_binding(
                        name=name, namespace=namespace
                    )
                logger.info(f"Deleted {kind}: {name}")
            except client.ApiException as e:
                if e.status != 404:
                    logger.warning(f"Failed to cleanup {kind} {name}: {e}")


@pytest.fixture(scope="function")
def ensure_job_rbac(job_rbac):
    """
    Function-scoped fixture that ensures RBAC resources are available for tests.
    Use this fixture in tests that depend on create_test_app with enable_k8s_support=True.
    Returns the service account name for use in job creation.
    """
    return job_rbac


def create_test_app(
    enable_k8s_support: bool = False,
    storage_type: StorageType = StorageType.LOCAL,
    metastore_type: StorageType = StorageType.LOCAL,
    params: Optional[Dict[str, Any]] = None,
    dry_run: bool = False,
    batch_driver_stand_alone: bool = False,
):
    """Create a FastAPI app configured for e2e testing.

    The legacy k8s-job-patch parameter was removed when manifests
    became driven by ConfigMaps. Tests should ensure the
    template/profile ConfigMaps exist in the cluster (see
    ``template_configmaps`` fixture).
    """
    if params is None:
        params = {}

    # Save old settings
    oldStorage, oldMetaStore = settings.STORAGE_TYPE, settings.METASTORE_TYPE
    # Override settings
    settings.STORAGE_TYPE, settings.METASTORE_TYPE = storage_type, metastore_type
    # Create app
    app = build_app(
        argparse.Namespace(
            host=None,
            port=8090,
            enable_fastapi_docs=False,
            disable_batch_api=False,
            disable_file_api=False,
            enable_k8s_support=enable_k8s_support,
            dry_run=dry_run,
            batch_driver_stand_alone=batch_driver_stand_alone,
        ),
        params,
    )
    # RESTORE settings
    settings.STORAGE_TYPE, settings.METASTORE_TYPE = oldStorage, oldMetaStore
    return app


class MockMetadataStore:
    client = None

    async def ping(self) -> bool:
        return True

    async def close(self) -> None:
        return None


class FastNoopEndpointSource(NoopEndpointSource):
    def __init__(self, context, *, availability_check=None, channel_id: str = "echo"):
        self._context = context
        super().__init__(
            delay=context.values.get(
                "endpoint_source_delay_seconds",
                0.0,
            )
        )
        self._channel = InterruptibleEchoChannel(
            delay=context.values.get("endpoint_source_delay_seconds", 0.0),
            availability_check=availability_check,
            id=channel_id,
        )


class ParallelNoopEndpointSource(FastNoopEndpointSource):
    def __init__(self, context, capacity: int, *, availability_check=None):
        super().__init__(context, availability_check=availability_check)
        self._capacity = capacity


class InterruptibleEchoChannel(EchoChannel):
    """Echo requests while the fake runtime endpoint remains available."""

    def __init__(
        self,
        *,
        delay: float = 0.0,
        availability_check=None,
        id: str = "echo",
    ) -> None:
        super().__init__(delay=delay, id=id)
        self._availability_check = availability_check

    async def send(self, request):
        if self._delay:
            await asyncio.sleep(self._delay)
        if callable(self._availability_check) and not self._availability_check():
            raise InferenceError(
                InferenceErrorCode.NO_ENDPOINT,
                f"fake runtime endpoint {self.id} is unavailable",
                retryable=False,
            )
        return request.payload


ENDPOINT_SAMPLE_BODIES = {
    "/v1/chat/completions": {
        "model": "gpt-3.5-turbo-0125",
        "messages": [
            {"role": "system", "content": "You are a helpful assistant."},
            {"role": "user", "content": "Hello world!"},
        ],
        "max_tokens": 1000,
    },
    "/v1/completions": {
        "model": "gpt-3.5-turbo-0125",
        "prompt": "Once upon a time",
        "max_tokens": 100,
    },
    "/v1/embeddings": {
        "model": "text-embedding-ada-002",
        "input": "The food was delicious and the waiter was friendly.",
    },
    "/v1/rerank": {
        "model": "reranker-v1",
        "query": "What is deep learning?",
        "documents": [
            "Deep learning is a subset of machine learning.",
            "The weather is nice today.",
            "Neural networks are inspired by the brain.",
        ],
    },
}


class E2ETestBackend(str):
    """Named E2E backend with feature flags consumed by test helpers.

    `features` are symbolic capabilities used by `tests/batch` helpers to
    decide whether to skip a scenario or which runtime-debug delta to expect.
    See the feature reference above `E2E_BACKENDS` for the current readers and
    affected scope.
    """

    request_kwargs: dict[str, str]
    features: frozenset[str]
    runtime_debug_config: Optional[dict[str, str]]
    max_concurrency: int
    storage_type: StorageType
    metastore_type: StorageType
    serial_worker: bool
    configure_app: Optional[Callable[[Any, pytest.MonkeyPatch], None]]

    def __new__(
        cls,
        name: str,
        *,
        request_kwargs: Optional[dict[str, str]] = None,
        features: tuple[str, ...] = (),
        runtime_debug_config: Optional[dict[str, str]] = None,
        max_concurrency: int = 1,
        storage_type: StorageType = StorageType.LOCAL,
        metastore_type: StorageType = StorageType.LOCAL,
        serial_worker: bool = False,
        configure_app: Optional[Callable[[Any, pytest.MonkeyPatch], None]] = None,
    ):
        backend = str.__new__(cls, name)
        backend.request_kwargs = dict(request_kwargs or {})
        backend.features = frozenset(features)
        backend.runtime_debug_config = (
            dict(runtime_debug_config) if runtime_debug_config is not None else None
        )
        backend.max_concurrency = max_concurrency
        backend.storage_type = storage_type
        backend.metastore_type = metastore_type
        backend.serial_worker = serial_worker
        backend.configure_app = configure_app
        return backend

    def has_feature(self, feature: str) -> bool:
        return feature in self.features

    def with_metastore(self, metastore_type: StorageType) -> "E2ETestBackend":
        if self.metastore_type == metastore_type:
            return self
        return type(self)(
            str(self),
            request_kwargs=self.request_kwargs,
            features=tuple(self.features),
            runtime_debug_config=self.runtime_debug_config,
            max_concurrency=self.max_concurrency,
            storage_type=self.storage_type,
            metastore_type=metastore_type,
            serial_worker=self.serial_worker,
            configure_app=self.configure_app,
        )

    def with_serial_worker(self, serial_worker: bool = True) -> "E2ETestBackend":
        if self.serial_worker == serial_worker:
            return self
        return type(self)(
            str(self),
            request_kwargs=self.request_kwargs,
            features=tuple(self.features),
            runtime_debug_config=self.runtime_debug_config,
            max_concurrency=self.max_concurrency,
            storage_type=self.storage_type,
            metastore_type=self.metastore_type,
            serial_worker=serial_worker,
            configure_app=self.configure_app,
        )

    @property
    def support_runtime(self) -> bool:
        return self.has_feature("support_runtime")

    @property
    def fake_runtime(self) -> bool:
        return self.has_feature("fake_runtime")

    @property
    def uses_runtime(self) -> bool:
        return "_using_" in str(self)

    @property
    def runtime_name(self) -> Optional[str]:
        if not self.uses_runtime:
            return None
        return str(self).split("_using_", 1)[1]

    @property
    def dry_run(self) -> bool:
        return not self.uses_runtime

    @property
    def requires_fake_runtime(self) -> bool:
        return self.uses_runtime and (
            self.storage_type == StorageType.LOCAL
            or self.metastore_type == StorageType.LOCAL
        )

    @property
    def pytest_id(self) -> str:
        parts = [str(self)]
        if self.storage_type != StorageType.LOCAL:
            parts.append(f"{self.storage_type.value}_storage")
        if self.metastore_type != StorageType.LOCAL:
            parts.append(f"{self.metastore_type.value}_metastore")
        if self.serial_worker:
            parts.append("serial_worker")
        return "][".join(parts)


# E2E backend feature reference.
#
# `support_runtime`
#   Readers: `E2ETestBackend.support_runtime`, plus
#   `test_e2e_abnormal_job_behavior.py` runtime-gated scenarios such as
#   `test_job_runtime_failure_in_progress` and
#   `test_job_cancellation_in_progress_during_service_discovery`.
#   Scope: enables runtime-only tests instead of skipping them.
#
# `fake_runtime`
#   Readers: `E2ETestBackend.fake_runtime`,
#   `E2ETestBackend.requires_fake_runtime`, `build_e2e_test_app()`,
#   `backend_expect_runtime_teardown()`.
#   Scope: installs the local fake runtime backend and enables runtime debug
#   delta assertions that only make sense when create/delete hooks are visible.
#
# `fake_provisioning_runtime`
#   Readers: `backend_uses_fake_provisioning_runtime()` in
#   `test_e2e_abnormal_job_behavior.py`.
#   Scope: recovery/abnormal tests that require observable provisioned runtime
#   lifecycle rather than an already-external endpoint.
#
# `service_discovery`
#   Readers: `backend_has_feature(..., "service_discovery")`.
#   Scope: `test_job_cancellation_in_progress_during_service_discovery`.
#
# `restart_phase_recovery`
#   Readers: `backend_supports_restart_phase_recovery()`.
#   Scope: restart/reclaim scenarios in
#   `test_e2e_abnormal_job_behavior.py`, especially
#   `complete_job_after_restart()`-based flows.
#
# `restart_validation_runtime_delta_observable`
#   Readers: `backend_observes_restart_validation_runtime_delta()`.
#   Scope: `test_job_restore_after_mds_crash_during_validation`; controls
#   whether restart-time runtime create/use/teardown deltas should be asserted.
#
# `runtime_cleanup_interruption`
#   Readers: `backend_supports_runtime_cleanup_interruption()`.
#   Scope: teardown-interruption and rollover overlap tests such as
#   `test_job_runtime_failure_in_progress`,
#   `test_job_restore_after_mds_rollover_with_runtime_overlap`, and
#   `test_job_session_final_block_exception_surfaces_as_failure`.
#
# `runtime_cleanup_single_delete`
#   Readers: `backend_runtime_cleanup_delete_delta()`.
#   Scope: expected delete-call count for runtime cleanup assertions; deployment
#   tears down via a single runtime delete while Octagram observes two phases.
#
# `restart_in_progress_lock_retry`
#   Readers: `backend_supports_restart_in_progress_lock_retry()`.
#   Scope: restart-after-in-progress-crash tests that install shorter reclaim
#   timeouts and request-lock retry harnesses.
#
# `finalizing_cancel_api_race`
#   Current scope: declaration-only in `tests/batch`; kept as backend metadata
#   for finalizing/cancel API race coverage but has no direct reader today.
#
# `restart_in_progress_multi_teardown`
#   Current scope: declaration-only in `tests/batch`; kept to describe backend
#   behavior where restart recovery may observe more than one teardown phase.
E2E_BACKENDS: dict[str, E2ETestBackend] = {
    "local_metastore_job": E2ETestBackend(
        "local_metastore_job",
        features=("restart_phase_recovery",),
    ),
    "local_job_using_deployment": E2ETestBackend(
        "local_job_using_deployment",
        request_kwargs={
            "aibrix_template": "mock-template",
            "provider": "deployment",
        },
        features=(
            "support_runtime",
            "fake_runtime",
            "fake_provisioning_runtime",
            "restart_phase_recovery",
            "restart_validation_runtime_delta_observable",
            "runtime_cleanup_interruption",
            "runtime_cleanup_single_delete",
            "restart_in_progress_lock_retry",
        ),
        runtime_debug_config={
            "teardown_calls_key": "deployment_teardown_calls",
            "endpoint_source_builds_key": "deployment_endpoint_source_builds",
            "runtime_create_target_key": "deployment_apps_v1_api",
            "runtime_create_attr": "created",
            "runtime_delete_target_key": "deployment_apps_v1_api",
            "runtime_delete_attr": "deleted",
        },
        configure_app=configure_local_metastore_deployment_backend,
    ),
    # Self-hosting KubernetesJob backend on local storage/metastore, backed by
    # a fake BatchV1Api. The worker runs in-process in worker_mode, so this
    # exercises the coordinator-side finalize path the deployment backend does
    # not reach (the deployment fake dispatches from the control plane).
    "local_job_using_k8s_job": E2ETestBackend(
        "local_job_using_k8s_job",
        request_kwargs={
            "aibrix_template": "mock-template",
            "provider": "k8s_job",
        },
        features=("support_runtime", "fake_runtime", "fake_provisioning_runtime"),
        configure_app=configure_local_metastore_k8s_job_backend,
    ),
    # Example backend configuration for implementing e2e Kubernetes Job tests.
    # This documents the intended shape of a real k8s-job backend:
    # TOS-backed storage with a Redis-backed metastore.
    # NOTE: runtime_debug_config is intentionally omitted here. The deployment
    # debug keys above are not correct for K8s Job. A developer should add a
    # real fake/debug K8s Job backend first (for example, wiring BatchV1Api
    # create/delete handles plus job-specific teardown/endpoint-source state
    # into batch_driver._context.values), then set runtime_debug_config to
    # those K8s Job-specific keys.
    # "job_using_k8s_job": E2ETestBackend(
    #     "job_using_k8s_job",
    #     request_kwargs={
    #         "aibrix_template": "mock-template",
    #         "provider": "k8s_job",
    #     },
    #     features=("support_runtime"),
    #     storage_type=StorageType.TOS,
    #     metastore_type=StorageType.REDIS,
    # ),
}


def get_e2e_backend(test_backend: str | E2ETestBackend) -> E2ETestBackend:
    if isinstance(test_backend, E2ETestBackend):
        return test_backend
    return E2E_BACKENDS[test_backend]


def backend_has_feature(test_backend: str | E2ETestBackend, feature: str) -> bool:
    return get_e2e_backend(test_backend).has_feature(feature)


def apply_e2e_backend_keyword_overrides(
    test_backend: E2ETestBackend, keyword: str
) -> E2ETestBackend:
    # Keyword modifiers are applied after base backend selection so they
    # override whatever defaults the E2ETestBackend entry started with.
    if "redis_metastore" in keyword:
        test_backend = test_backend.with_metastore(StorageType.REDIS)
    if _keyword_enables_serial_worker(keyword):
        test_backend = test_backend.with_serial_worker()
    return test_backend


def select_e2e_backends(
    metafunc,
    keyword_backends: list[str],
    default_backend: str = "local_metastore_job",
):
    if "test_backend" not in metafunc.fixturenames:
        return

    keyword = metafunc.config.option.keyword or ""
    selected_backend = get_e2e_backend(default_backend)
    for backend in keyword_backends:
        if backend in keyword:
            selected_backend = get_e2e_backend(backend)
            break

    selected_backend = apply_e2e_backend_keyword_overrides(selected_backend, keyword)

    metafunc.parametrize(
        "test_backend",
        [pytest.param(selected_backend, id=selected_backend.pytest_id)],
    )


def generate_batch_input_data(
    num_requests: int = 3, endpoint: str = "/v1/chat/completions"
) -> str:
    sample_body = ENDPOINT_SAMPLE_BODIES.get(endpoint)
    if sample_body is None:
        raise ValueError(
            f"No sample body defined for endpoint '{endpoint}'. "
            f"Supported: {list(ENDPOINT_SAMPLE_BODIES.keys())}"
        )

    lines = []
    for i in range(num_requests):
        request = {
            "custom_id": f"request-{i + 1}",
            "method": "POST",
            "url": endpoint,
            "body": copy.deepcopy(sample_body),
        }
        lines.append(json.dumps(request))

    return "\n".join(lines)


def build_batch_request(
    input_file_id: str,
    endpoint: str = "/v1/chat/completions",
    *,
    completion_window: str = "24h",
    aibrix_template: str | None = None,
    aibrix_profile: str | None = None,
    provider: str | None = None,
) -> dict[str, Any]:
    request: dict[str, Any] = {
        "input_file_id": input_file_id,
        "endpoint": endpoint,
        "completion_window": completion_window,
    }
    aibrix: dict[str, Any] = {}
    if aibrix_template:
        aibrix["model_template"] = {"name": aibrix_template}
    if aibrix_profile:
        aibrix["profile"] = {"name": aibrix_profile}
    if provider:
        runtime_targets = {
            "deployment": "Kubernetes",
            "k8s_job": "KubernetesJob",
        }
        runtime_target = runtime_targets.get(provider)
        if runtime_target is not None:
            aibrix["runtime"] = {"target": runtime_target}
        aibrix["resource_allocation"] = {
            "provision_id": "reservation-1",
            "provision_resource_deadline": 3600,
            "resource_details": [
                {
                    "endpoint_cluster": "zone/HL/cluster-a/default",
                    "gpu_type": "H100",
                    "replica": 1,
                }
            ],
        }
    if aibrix:
        request["aibrix"] = aibrix
    return request


def e2e_batch_request_kwargs(test_backend: str | E2ETestBackend) -> dict[str, str]:
    return dict(get_e2e_backend(test_backend).request_kwargs)


def create_test_client(app) -> TestClient:
    return TestClient(app, raise_server_exceptions=False)


def upload_batch_input_file(
    client: TestClient,
    num_requests: int,
    endpoint: str = "/v1/chat/completions",
    filename: str = "test_input.jsonl",
) -> str:
    upload_response = client.post(
        "/v1/files",
        files={
            "file": (
                filename,
                generate_batch_input_data(num_requests, endpoint=endpoint),
                "application/jsonl",
            )
        },
        data={"purpose": "batch"},
    )
    assert upload_response.status_code == 200, upload_response.text
    return upload_response.json()["id"]


def create_batch_job(
    client: TestClient,
    input_file_id: str,
    endpoint: str = "/v1/chat/completions",
    *,
    test_backend: str | None = None,
    completion_window: str = "24h",
    aibrix_template: str | None = None,
    aibrix_profile: str | None = None,
    provider: str | None = None,
) -> str:
    if test_backend is not None:
        backend_kwargs = e2e_batch_request_kwargs(test_backend)
        aibrix_template = aibrix_template or backend_kwargs.get("aibrix_template")
        aibrix_profile = aibrix_profile or backend_kwargs.get("aibrix_profile")
        provider = provider or backend_kwargs.get("provider")
    batch_response = client.post(
        "/v1/batches",
        json=build_batch_request(
            input_file_id,
            endpoint,
            completion_window=completion_window,
            aibrix_template=aibrix_template,
            aibrix_profile=aibrix_profile,
            provider=provider,
        ),
    )
    assert batch_response.status_code == 200, batch_response.text
    return batch_response.json()["id"]


@pytest.fixture(scope="session")
def template_configmaps(
    k8s_config, test_namespace, s3_credentials_secret, test_s3_bucket
):
    """Apply the unittest ModelDeploymentTemplate / BatchProfile ConfigMaps
    to the test cluster.

    The metadata service registries query the 'aibrix-system' namespace
    by default. We ensure that namespace exists in the test cluster and
    apply the fixture there so build_app() loads the test data through
    the same code path as production.
    """
    from aibrix.batch.template.registry import DEFAULT_NAMESPACE

    fixture_path = (
        Path(__file__).parent / "testdata" / "template_configmaps_unittest.yaml"
    )
    core_v1 = client.CoreV1Api()

    # Ensure aibrix-system namespace exists in the test cluster.
    try:
        core_v1.read_namespace(name=DEFAULT_NAMESPACE)
    except client.ApiException as e:
        if e.status != 404:
            raise
        core_v1.create_namespace(
            body=client.V1Namespace(
                metadata=client.V1ObjectMeta(name=DEFAULT_NAMESPACE)
            )
        )
        logger.info(f"Created namespace: {DEFAULT_NAMESPACE}")

    applied: list[str] = []
    with open(fixture_path) as f:
        for doc in yaml.safe_load_all(f):
            if not isinstance(doc, dict) or doc.get("kind") != "ConfigMap":
                continue
            name = doc["metadata"]["name"]
            data = doc.get("data", {})
            if name == "aibrix-batch-profiles" and "profiles.yaml" in data:
                profiles = yaml.safe_load(data["profiles.yaml"])
                for item in profiles.get("items", []):
                    if item.get("name") == "unittest":
                        item.setdefault("spec", {})["storage"] = {
                            "backend": "s3",
                            "bucket": test_s3_bucket,
                            "credentials_secret_ref": s3_credentials_secret,
                        }
                data["profiles.yaml"] = yaml.safe_dump(profiles, sort_keys=False)
            cm = client.V1ConfigMap(
                metadata=client.V1ObjectMeta(name=name, namespace=DEFAULT_NAMESPACE),
                data=data,
            )
            try:
                core_v1.delete_namespaced_config_map(
                    name=name, namespace=DEFAULT_NAMESPACE
                )
            except client.ApiException as e:
                if e.status != 404:
                    raise
            core_v1.create_namespaced_config_map(namespace=DEFAULT_NAMESPACE, body=cm)
            applied.append(name)
            logger.info(f"Applied test ConfigMap: {name}")

    yield applied

    for name in applied:
        try:
            core_v1.delete_namespaced_config_map(name=name, namespace=DEFAULT_NAMESPACE)
        except client.ApiException as e:
            if e.status != 404:
                raise


def build_e2e_test_app(
    request,
    test_backend: str | E2ETestBackend,
    tmp_path,
    monkeypatch,
):
    test_backend = get_e2e_backend(test_backend)
    monkeypatch.chdir(tmp_path)
    if (
        test_backend.storage_type == StorageType.REDIS
        or test_backend.metastore_type == StorageType.REDIS
    ):
        request.getfixturevalue("redis_available")
        redis_prefix = getattr(request.node, "_aibrix_test_redis_prefix", None)
        if redis_prefix is None:
            node_id = re.sub(r"[^A-Za-z0-9_]+", "_", request.node.nodeid).strip("_")
            redis_prefix = f"pytest_{node_id}_{uuid.uuid4().hex[:8]}"
            setattr(request.node, "_aibrix_test_redis_prefix", redis_prefix)
        if not getattr(request.node, "_aibrix_test_redis_cleanup_registered", False):
            request.addfinalizer(
                lambda prefix=redis_prefix: cleanup_test_redis_prefix(prefix)
            )
            setattr(request.node, "_aibrix_test_redis_cleanup_registered", True)
        monkeypatch.setenv("DB_REDIS_PREFIX", redis_prefix)
        monkeypatch.setattr(envs, "DB_REDIS_PREFIX", redis_prefix)
    app = create_test_app(
        storage_type=test_backend.storage_type,
        metastore_type=test_backend.metastore_type,
        dry_run=test_backend.dry_run,
        batch_driver_stand_alone=bool(
            request.node.get_closest_marker("batch_driver_stand_alone")
        ),
    )
    app.state.metadata_store = MockMetadataStore()
    app.state.redis_client = None

    if not test_backend.uses_runtime:
        return app

    if test_backend.requires_fake_runtime:
        if not test_backend.fake_runtime:
            raise ValueError(
                f"Backend {test_backend} must declare fake_runtime for local storage/metastore tests"
            )

        if test_backend.configure_app is not None:
            test_backend.configure_app(app, monkeypatch)
            return app

        raise ValueError(
            f"Unsupported fake runtime backend: {test_backend.runtime_name}"
        )

    return app


@pytest.fixture(scope="function")
def e2e_test_app(request, test_backend, tmp_path, monkeypatch):
    return build_e2e_test_app(
        request,
        test_backend,
        tmp_path,
        monkeypatch,
    )

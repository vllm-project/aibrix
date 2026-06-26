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
import copy
import json
import os
from pathlib import Path
from types import SimpleNamespace
from typing import Any, Dict, Optional

import boto3
import pytest
import yaml
from fastapi.testclient import TestClient
from kubernetes import client, config

import aibrix.batch.job_driver.runtime.k8s_deployment as deployment_runtime_module
from aibrix.batch.client.sources import NoopEndpointSource
from aibrix.logger import init_logger
from aibrix.metadata.app import build_app
from aibrix.metadata.setting import settings
from aibrix.storage import StorageType

logger = init_logger(__name__)
ORIGINAL_DEPLOYMENT_TEARDOWN_RUNTIME = (
    deployment_runtime_module.DeploymentRuntime._teardown_runtime
)


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
def redis_config_available():
    """Check if S3 configuration is available locally."""
    # Check for AWS credentials
    if os.getenv("REDIS_HOST") is None:
        pytest.skip("Redis configuration not available")

    import redis

    try:
        client = redis.Redis(
            host=os.environ.get("REDIS_HOST", "localhost"),
            port=int(os.environ.get("REDIS_PORT", "6379")),
            db=int(os.environ.get("REDIS_DB", "0")),
            password=os.environ.get("REDIS_PASSWORD"),
            socket_connect_timeout=2,
            socket_timeout=2,
            decode_responses=True,
        )
        client.ping()
    except Exception as e:
        pytest.skip(f"Redis access not available: {e}")


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
    def __init__(self, context):
        self._context = context
        super().__init__(
            delay=context.values.get(
                "endpoint_source_delay_seconds",
                0.0,
            )
        )


class ParallelNoopEndpointSource(FastNoopEndpointSource):
    def __init__(self, context, capacity: int):
        super().__init__(context)
        self._capacity = capacity


class FakeDeploymentAppsV1Api:
    def __init__(self):
        self.created: list[tuple[str, dict[str, Any]]] = []
        self.deleted: list[tuple[str, str]] = []
        self._existing: set[tuple[str, str]] = set()

    def create_namespaced_deployment(self, namespace: str, body: dict[str, Any]):
        self.created.append((namespace, body))
        self._existing.add((namespace, body["metadata"]["name"]))

    def read_namespaced_deployment_status(self, name: str, namespace: str):
        if (namespace, name) not in self._existing:
            raise client.ApiException(status=404)
        return SimpleNamespace(status=SimpleNamespace(available_replicas=1))

    def delete_namespaced_deployment(self, name: str, namespace: str):
        if (namespace, name) not in self._existing:
            raise client.ApiException(status=404)
        self.deleted.append((namespace, name))
        self._existing.remove((namespace, name))


class FakeDeploymentCoreV1Api:
    def __init__(self):
        self.created: list[tuple[str, dict[str, Any]]] = []
        self.deleted: list[tuple[str, str]] = []
        self._existing: set[tuple[str, str]] = set()

    def create_namespaced_service(self, namespace: str, body: dict[str, Any]):
        self.created.append((namespace, body))
        self._existing.add((namespace, body["metadata"]["name"]))

    def read_namespaced_service(self, name: str, namespace: str):
        if (namespace, name) not in self._existing:
            raise client.ApiException(status=404)
        return SimpleNamespace(metadata=SimpleNamespace(name=name, namespace=namespace))

    def delete_namespaced_service(self, name: str, namespace: str):
        if (namespace, name) not in self._existing:
            raise client.ApiException(status=404)
        self.deleted.append((namespace, name))
        self._existing.remove((namespace, name))


class FakeDeploymentRenderer:
    def render(
        self,
        job_id: str,
        spec,
        prividerSpec,
    ):
        deployment_name = f"batch-{job_id[:8]}-engine"
        return {
            "deployment": {
                "apiVersion": "apps/v1",
                "kind": "Deployment",
                "metadata": {
                    "name": deployment_name,
                    "namespace": "default",
                    "labels": {"model.aibrix.ai/name": deployment_name},
                },
                "spec": {
                    "replicas": 1,
                    "selector": {
                        "matchLabels": {
                            "app": deployment_name,
                            "model.aibrix.ai/name": deployment_name,
                        }
                    },
                    "template": {
                        "metadata": {
                            "labels": {
                                "app": deployment_name,
                                "model.aibrix.ai/name": deployment_name,
                            }
                        },
                        "spec": {"containers": [{"name": "llm-engine"}]},
                    },
                },
            },
            "service": {
                "apiVersion": "v1",
                "kind": "Service",
                "metadata": {
                    "name": deployment_name,
                    "namespace": "default",
                    "labels": {"model.aibrix.ai/name": deployment_name},
                },
                "spec": {
                    "selector": {"model.aibrix.ai/name": deployment_name},
                    "ports": [{"port": 8000, "targetPort": 8000}],
                    "type": "ClusterIP",
                },
            },
        }


def configure_local_metastore_deployment_backend(app, monkeypatch) -> None:
    context = app.state.batch_driver._context
    shared_state = getattr(monkeypatch, "_aibrix_fake_deployment_backend_state", None)
    if shared_state is None:
        shared_state = {
            "apps_v1_api": FakeDeploymentAppsV1Api(),
            "core_v1_api": FakeDeploymentCoreV1Api(),
            "deployment_teardown_calls": [],
            "deployment_endpoint_source_builds": [],
        }
        setattr(monkeypatch, "_aibrix_fake_deployment_backend_state", shared_state)

    apps_v1_api = shared_state["apps_v1_api"]
    core_v1_api = shared_state["core_v1_api"]
    context.apps_v1_api = apps_v1_api
    context.core_v1_api = core_v1_api
    context.values["deployment_apps_v1_api"] = apps_v1_api
    context.values["deployment_core_v1_api"] = core_v1_api
    context.values["deployment_teardown_calls"] = shared_state[
        "deployment_teardown_calls"
    ]
    context.values["deployment_endpoint_source_builds"] = shared_state[
        "deployment_endpoint_source_builds"
    ]

    monkeypatch.setattr(
        deployment_runtime_module.DeploymentRuntime,
        "_build_renderer",
        staticmethod(lambda context: FakeDeploymentRenderer()),
    )

    def _build_test_endpoint_source(self, handle):
        self._context.values["deployment_endpoint_source_builds"].append(
            handle.deployment_name
        )
        return FastNoopEndpointSource(self._context)

    async def _recording_teardown(self, runtime):
        self._context.values["deployment_teardown_calls"].append(
            {
                "job_id": self._active_job_id,
                "deployment_name": runtime.deployment_name,
            }
        )
        return await ORIGINAL_DEPLOYMENT_TEARDOWN_RUNTIME(self, runtime)

    monkeypatch.setattr(
        deployment_runtime_module.DeploymentRuntime,
        "_build_endpoint_source",
        _build_test_endpoint_source,
    )
    monkeypatch.setattr(
        deployment_runtime_module.DeploymentRuntime,
        "_teardown_runtime",
        _recording_teardown,
    )


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
    request_kwargs: dict[str, str]
    features: frozenset[str]
    runtime_debug_config: Optional[dict[str, str]]
    max_concurrency: int

    def __new__(
        cls,
        name: str,
        *,
        request_kwargs: Optional[dict[str, str]] = None,
        features: tuple[str, ...] = (),
        runtime_debug_config: Optional[dict[str, str]] = None,
        max_concurrency: int = 1,
    ):
        backend = str.__new__(cls, name)
        backend.request_kwargs = dict(request_kwargs or {})
        backend.features = frozenset(features)
        backend.runtime_debug_config = (
            dict(runtime_debug_config) if runtime_debug_config is not None else None
        )
        backend.max_concurrency = max_concurrency
        return backend

    def has_feature(self, feature: str) -> bool:
        return feature in self.features

    @property
    def support_runtime(self) -> bool:
        return self.has_feature("support_runtime")

    @property
    def fake_runtime(self) -> bool:
        return self.has_feature("fake_runtime")


E2E_BACKENDS: dict[str, E2ETestBackend] = {
    "local_metastore_job": E2ETestBackend("local_metastore_job"),
    "local_metastore_job_parallel": E2ETestBackend(
        "local_metastore_job_parallel",
        max_concurrency=5,
    ),
    "local_job_using_deployment": E2ETestBackend(
        "local_job_using_deployment",
        request_kwargs={
            "aibrix_template": "mock-template",
            "provider": "deployment",
        },
        features=("support_runtime", "fake_runtime", "fake_provisioning_runtime"),
        runtime_debug_config={
            "teardown_calls_key": "deployment_teardown_calls",
            "endpoint_source_builds_key": "deployment_endpoint_source_builds",
            "runtime_create_target_key": "deployment_apps_v1_api",
            "runtime_create_attr": "created",
            "runtime_delete_target_key": "deployment_apps_v1_api",
            "runtime_delete_attr": "deleted",
        },
    ),
}


def get_e2e_backend(test_backend: str | E2ETestBackend) -> E2ETestBackend:
    if isinstance(test_backend, E2ETestBackend):
        return test_backend
    return E2E_BACKENDS[test_backend]


def backend_has_feature(test_backend: str | E2ETestBackend, feature: str) -> bool:
    return get_e2e_backend(test_backend).has_feature(feature)


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

    metafunc.parametrize(
        "test_backend",
        [pytest.param(selected_backend, id=selected_backend)],
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
        }
        runtime_target = runtime_targets.get(provider)
        if runtime_target is not None:
            aibrix["runtime"] = {"target": runtime_target}
        aibrix["resource_allocation"] = {
            "provision_id": "reservation-1",
            "provision_resource_deadline": 3600,
            "resource_details": [
                {
                    "endpoint_cluster": "cluster-a",
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
    *,
    preserve_redis_prefix: bool = False,
):
    test_backend = get_e2e_backend(test_backend)
    monkeypatch.chdir(tmp_path)
    del request, preserve_redis_prefix

    if test_backend in {"local_metastore_job", "local_metastore_job_parallel"}:
        app = create_test_app(
            storage_type=StorageType.LOCAL,
            metastore_type=StorageType.LOCAL,
            dry_run=True,
        )
        app.state.metadata_store = MockMetadataStore()
        app.state.redis_client = None
        if test_backend == "local_metastore_job_parallel":
            endpoint_source = ParallelNoopEndpointSource(
                app.state.batch_driver._context,
                capacity=test_backend.max_concurrency,
            )
            app.state.batch_driver._endpoint_source = endpoint_source
            app.state.batch_driver.job_manager.set_endpoint_source(endpoint_source)
        return app

    if test_backend == "local_job_using_deployment":
        app = create_test_app(
            storage_type=StorageType.LOCAL,
            metastore_type=StorageType.LOCAL,
            dry_run=False,
        )
        app.state.metadata_store = MockMetadataStore()
        app.state.redis_client = None
        configure_local_metastore_deployment_backend(app, monkeypatch)
        return app

    raise ValueError(f"Unsupported e2e backend: {test_backend}")


@pytest.fixture(scope="function")
def e2e_test_app(request, test_backend, tmp_path, monkeypatch):
    return build_e2e_test_app(
        request,
        test_backend,
        tmp_path,
        monkeypatch,
    )

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
import os
import threading
from concurrent.futures import ThreadPoolExecutor
from concurrent.futures import TimeoutError as FutureTimeoutError
from pathlib import Path
from typing import Any, Dict, Optional

import boto3
import kopf
import pytest
import yaml
from kubernetes import client, config

from aibrix.logger import init_logger
from aibrix.metadata.app import build_app
from aibrix.metadata.cache.job import JobCache
from aibrix.metadata.setting import settings
from aibrix.storage import StorageType

logger = init_logger(__name__)

# Use a threading.Event to signal when the operator is ready
OPERATOR_READY = threading.Event()


def run_operator_in_thread(stop_flag: threading.Event):
    """The target function for the operator thread."""
    # The 'ready_flag' is a special kopf argument that gets set
    # when the operator has started and is ready to handle events.
    kopf.run(
        standalone=True,
        ready_flag=OPERATOR_READY,
        namespace="default",  # Monitor default namespace for tests
        stop_flag=stop_flag,
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

        # Test API server accessibility with reliable timeout
        try:
            v1 = client.CoreV1Api()
            api_host = v1.api_client.configuration.host
            if not api_host:
                pytest.skip(
                    "Kubernetes configuration is invalid: no API server host found"
                )

            logger.info(f"Testing Kubernetes API accessibility: {api_host}")

            def test_api_call():
                """Make a simple API call to test connectivity."""
                # Use a simple API call that exists on CoreV1Api
                return v1.list_namespace(limit=1)

            # Use ThreadPoolExecutor with timeout to prevent hanging
            with ThreadPoolExecutor(max_workers=1) as executor:
                future = executor.submit(test_api_call)
                try:
                    # Wait maximum 10 seconds for the API call
                    future.result(timeout=10)
                    logger.info("Kubernetes API server accessibility verified")
                except FutureTimeoutError:
                    pytest.skip(
                        f"Kubernetes API server timeout after 10 seconds: {api_host}"
                    )
                except Exception as e:
                    pytest.skip(f"Kubernetes API server not accessible: {e}")

        except Exception as e:
            pytest.skip(f"Failed to create Kubernetes API client: {e}")

    except Exception as e:
        pytest.skip(f"Failed to initialize Kubernetes client: {e}")


@pytest.fixture
def kopf_operator(scope="function"):
    """
    A session-scoped fixture to run the kopf operator in a background thread.
    This ensures JobCache handlers are properly triggered during tests.
    """
    from aibrix.metadata.core import KopfOperatorWrapper

    operator = KopfOperatorWrapper(
        namespace="default",
        startup_timeout=30,
        shutdown_timeout=10,
    )
    try:
        # Start the kopf operator in a daemon thread
        print("--- Starting kopf operator in background thread ---")
        operator.start()
        print("--- Kopf operator is ready, yielding to tests ---")
        yield  # Tests run here

    finally:
        print("\n--- Kopf operator test session finished ---")
        operator.stop()


@pytest.fixture(scope="session")
def test_namespace():
    """Use default namespace for testing."""
    return "default"


@pytest.fixture(scope="function")
def job_cache(kopf_operator, ensure_job_rbac):
    """
    Function-scoped fixture that provides a JobCache instance.
    The kopf_operator fixture ensures the operator is running.

    Loads ModelDeploymentTemplate and BatchProfile registries from the
    multi-document fixture YAML so each test starts from a clean,
    deterministic configuration.
    """
    from pathlib import Path

    from aibrix.batch.template import (
        local_profile_registry,
        local_template_registry,
    )

    fixture_path = (
        Path(__file__).parent / "testdata" / "template_configmaps_unittest.yaml"
    )
    template_registry = local_template_registry(fixture_path)
    profile_registry = local_profile_registry(fixture_path)
    template_registry.reload()
    profile_registry.reload()
    return JobCache(
        template_registry=template_registry,
        profile_registry=profile_registry,
    )


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

    def test_connection():
        # Try to connect to Redis with a short timeout
        client = redis.Redis(
            host=os.environ.get("REDIS_HOST", "localhost"),
            port=int(os.environ.get("REDIS_PORT", "6379")),
            db=int(os.environ.get("REDIS_DB", "0")),
            password=os.environ.get("REDIS_PASSWORD"),
            socket_connect_timeout=2,
            socket_timeout=2,
            decode_responses=True,
        )
        # Test with a simple ping
        return client.ping()

    # Use ThreadPoolExecutor to enforce timeout
    with ThreadPoolExecutor(max_workers=1) as executor:
        future = executor.submit(test_connection)
        try:
            future.result(timeout=5)  # 5 second timeout
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
    This ensures that tests using create_test_app with enable_k8s_job=True
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
    Use this fixture in tests that depend on create_test_app with enable_k8s_job=True.
    Returns the service account name for use in job creation.
    """
    return job_rbac


def create_test_app(
    enable_k8s_job: bool = False,
    enable_redis_job: bool = False,
    enable_mongo_job: bool = False,
    storage_type: StorageType = StorageType.LOCAL,
    metastore_type: StorageType = StorageType.LOCAL,
    params: Optional[Dict[str, Any]] = None,
    dry_run: Optional[bool] = None,
):
    """Create a FastAPI app configured for e2e testing.

    The legacy k8s-job-patch parameter was removed when manifests
    became driven by ConfigMaps. Tests should ensure the
    template/profile ConfigMaps exist in the cluster (see
    ``template_configmaps`` fixture).
    """
    if params is None:
        params = {}
    if dry_run is None:
        dry_run = not (enable_k8s_job or enable_redis_job or enable_mongo_job)

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
            disable_k8s_support=False,
            disable_inference_endpoint=True,
            enable_k8s_job=enable_k8s_job,
            enable_mongo_job=enable_mongo_job,
            enable_redis_job=enable_redis_job,
            k8s_namespace="default",
            k8s_job_patch=None,  # accepted by parser but always None in tests
            registry_provider="configmap",
            kopf_startup_timeout=30.0,
            kopf_shutdown_timeout=10.0,
            dry_run=dry_run,
        ),
        params,
    )
    # RESTORE settings
    settings.STORAGE_TYPE, settings.METASTORE_TYPE = oldStorage, oldMetaStore
    return app


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


@pytest.fixture(scope="function")
def test_app(
    k8s_config,
    test_s3_bucket,
    s3_credentials_secret,
    redis_config_available,
    ensure_job_rbac,
    template_configmaps,
    monkeypatch,
):
    monkeypatch.setenv(
        "WORKER_REDIS_HOST", "aibrix-redis-master.aibrix-system.svc.cluster.local"
    )
    monkeypatch.setenv("WORKER_REDIS_PORT", os.environ.get("REDIS_PORT", "6379"))
    return create_test_app(
        enable_k8s_job=True,
        storage_type=StorageType.S3,
        metastore_type=StorageType.REDIS,
        params={"bucket_name": test_s3_bucket},
    )

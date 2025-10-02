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
                pytest.skip("Kubernetes configuration is invalid: no API server host found")
            
            logger.info(f"Testing Kubernetes API accessibility: {api_host}")
            
            def test_api_call():
                """Make a simple API call to test connectivity."""
                # Use a very simple API call that should be fast
                return v1.get_api_versions()
            
            # Use ThreadPoolExecutor with timeout to prevent hanging
            with ThreadPoolExecutor(max_workers=1) as executor:
                future = executor.submit(test_api_call)
                try:
                    # Wait maximum 10 seconds for the API call
                    future.result(timeout=10)
                    logger.info("Kubernetes API server accessibility verified")
                except FutureTimeoutError:
                    pytest.skip(f"Kubernetes API server timeout after 10 seconds: {api_host}")
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
    Uses the unittest job template with the correct service account.
    """
    from pathlib import Path

    template_patch_path = (
        Path(__file__).parent / "testdata" / "k8s_job_patch_unittest.yaml"
    )
    return JobCache(template_patch_path=template_patch_path)


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
    k8s_job_patch: Optional[Path] = None,
    storage_type: StorageType = StorageType.LOCAL,
    metastore_type: StorageType = StorageType.LOCAL,
    params: Optional[Dict[str, Any]] = None,
):
    """Create a FastAPI app configured for e2e testing."""
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
            port=8100,
            enable_fastapi_docs=False,
            disable_batch_api=False,
            enable_k8s_job=enable_k8s_job,
            k8s_job_patch=k8s_job_patch,
            e2e_test=True,
        ),
        params,
    )
    # RESTORE settings
    settings.STORAGE_TYPE, settings.METASTORE_TYPE = oldStorage, oldMetaStore
    return app


@pytest.fixture(scope="function")
def test_app(
    k8s_config,
    test_s3_bucket,
    s3_credentials_secret,
    redis_config_available,
    ensure_job_rbac,
):
    # Get the path to the unittest job template
    patch_path = Path(__file__).parent / "testdata" / "k8s_job_patch_unittest.yaml"
    return create_test_app(
        enable_k8s_job=True,
        k8s_job_patch=patch_path,
        storage_type=StorageType.S3,
        metastore_type=StorageType.REDIS,
        params={"bucket_name": test_s3_bucket},
    )

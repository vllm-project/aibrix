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
Test to verify that RBAC setup works correctly for tests that depend on create_test_app.
"""

import pytest
from kubernetes import client


@pytest.mark.asyncio
async def test_rbac_resources_exist(k8s_config, ensure_job_rbac, test_namespace):
    """
    Test that verifies the RBAC resources are properly created and accessible.
    This test ensures that:
    1. unittest-job-reader-sa service account exists
    2. Required roles and bindings are in place
    """
    core_v1 = client.CoreV1Api()
    rbac_v1 = client.RbacAuthorizationV1Api()

    # The fixture returns the service account name
    service_account_name = ensure_job_rbac
    expected_sa_name = "unittest-job-reader-sa"

    # Verify the fixture returns the correct service account name
    assert (
        service_account_name == expected_sa_name
    ), f"Expected {expected_sa_name}, got {service_account_name}"
    print(f"✓ Fixture returned correct service account name: {service_account_name}")

    # Check that unittest-job-reader-sa service account exists
    try:
        sa = core_v1.read_namespaced_service_account(
            name=service_account_name, namespace=test_namespace
        )
        assert sa.metadata.name == service_account_name
        print(
            f"✓ {service_account_name} service account exists in namespace {test_namespace}"
        )
    except client.ApiException as e:
        pytest.fail(f"{service_account_name} service account not found: {e}")

    # Check that unittest-job-reader role exists
    try:
        role = rbac_v1.read_namespaced_role(
            name="unittest-job-reader-role", namespace=test_namespace
        )
        assert role.metadata.name == "unittest-job-reader-role"
        print(f"✓ unittest-job-reader-role role exists in namespace {test_namespace}")
    except client.ApiException as e:
        pytest.fail(f"unittest-job-reader-role role not found: {e}")

    # Check that unittest-job-reader role binding exists
    try:
        role_binding = rbac_v1.read_namespaced_role_binding(
            name="unittest-job-reader-binding", namespace=test_namespace
        )
        assert role_binding.metadata.name == "unittest-job-reader-binding"
        print(
            f"✓ unittest-job-reader-binding role binding exists in namespace {test_namespace}"
        )
    except client.ApiException as e:
        pytest.fail(f"unittest-job-reader-binding role binding not found: {e}")

    print("✅ All RBAC resources are properly configured!")


@pytest.mark.asyncio
async def test_create_test_app_with_rbac(ensure_job_rbac):
    """
    Test that create_test_app can be called with enable_k8s_job=True
    when RBAC resources are available.
    """
    from aibrix.storage import StorageType
    from tests.batch.conftest import create_test_app

    # This should not raise any errors if RBAC is properly set up
    app = create_test_app(
        enable_k8s_job=True,
        storage_type=StorageType.LOCAL,
        metastore_type=StorageType.LOCAL,
    )

    assert app is not None
    print("✅ create_test_app with enable_k8s_job=True works correctly!")

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

import os
from pathlib import Path

# Set required environment variable before importing
os.environ.setdefault("SECRET_KEY", "test-secret-key-for-testing")

from aibrix.batch.job_entity import JobEntityManager
from aibrix.metadata.cache.job import JobCache


def test_job_cache_implements_job_entity_manager():
    """Test that JobCache properly implements JobEntityManager interface."""
    cache = JobCache()
    assert isinstance(cache, JobEntityManager)


def test_job_cache_with_custom_patch():
    """
    Test that JobCache can be initialized with a custom template path
    and loads the correct service account.
    """
    from aibrix.metadata.cache.job import JobCache

    # Get the path to the unittest job template
    patch_path = Path(__file__).parent / "testdata" / "k8s_job_patch_unittest.yaml"

    # Initialize JobCache with custom template
    job_cache = JobCache(template_patch_path=patch_path)

    # Verify that the template was loaded
    assert job_cache.job_template is not None
    assert job_cache.job_template["kind"] == "Job"
    assert job_cache.job_template["apiVersion"] == "batch/v1"

    # Verify that the unittest service account is used
    service_account_name = job_cache.job_template["spec"]["template"]["spec"][
        "serviceAccountName"
    ]
    assert service_account_name == "unittest-job-reader-sa"

    print(
        f"✓ JobCache loaded custom template with service account: {service_account_name}"
    )
    print("✅ Template path integration works correctly!")


def test_job_cache_with_default_template():
    """
    Test that JobCache still works with the default template when no path is provided.
    """
    from aibrix.metadata.cache.job import JobCache

    # Initialize JobCache without custom template (should use default)
    job_cache = JobCache()

    # Verify that the template was loaded
    assert job_cache.job_template is not None
    assert job_cache.job_template["kind"] == "Job"
    assert job_cache.job_template["apiVersion"] == "batch/v1"

    # Verify that the default service account is used
    service_account_name = job_cache.job_template["spec"]["template"]["spec"][
        "serviceAccountName"
    ]
    assert service_account_name == "job-reader-sa"  # Default service account

    print(
        f"✓ JobCache loaded default template with service account: {service_account_name}"
    )
    print("✅ Default template loading works correctly!")

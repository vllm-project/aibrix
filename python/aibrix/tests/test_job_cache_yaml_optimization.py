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

# Set required environment variable before importing
os.environ.setdefault("SECRET_KEY", "test-secret-key-for-testing")

from aibrix.batch.job_entity import BatchJobSpec
from aibrix.metadata.cache.job import JobCache


def test_job_cache_yaml_template_loaded_once():
    """Test that YAML template is loaded once during initialization."""
    cache = JobCache()

    # Verify template is loaded
    assert hasattr(cache, "job_template")
    assert cache.job_template is not None
    assert cache.job_template["metadata"]["name"] == "batch-job-template"
    assert (
        cache.job_template["spec"]["template"]["spec"]["containers"][0]["image"]
        == "batch-processor:latest"
    )


def test_batch_job_to_k8s_job_uses_preloaded_template():
    """Test that _batch_job_to_k8s_job uses the pre-loaded template efficiently."""
    cache = JobCache()

    # Create test BatchJobSpec
    job_spec = BatchJobSpec.from_strings(
        input_file_id="test-file-123",
        endpoint="/v1/chat/completions",
        completion_window="24h",
        metadata={"user": "test-user", "priority": "high"},
    )

    # Convert to K8s Job
    k8s_job = cache._batch_job_to_k8s_job(job_spec)

    # Verify job creation
    assert k8s_job is not None
    assert k8s_job.metadata.name.startswith("batch-")
    assert k8s_job.metadata.namespace == "default"  # From template

    # Verify annotations are properly merged
    annotations = k8s_job.metadata.annotations
    assert "batch.job.aibrix.ai/input-file-id" in annotations
    assert annotations["batch.job.aibrix.ai/input-file-id"] == "test-file-123"
    assert "batch.job.aibrix.ai/endpoint" in annotations
    assert annotations["batch.job.aibrix.ai/endpoint"] == "/v1/chat/completions"
    assert "batch.job.aibrix.ai/completion-window" in annotations
    assert annotations["batch.job.aibrix.ai/completion-window"] == "24h"

    # Verify metadata annotations
    assert "batch.job.aibrix.ai/metadata.user" in annotations
    assert annotations["batch.job.aibrix.ai/metadata.user"] == "test-user"
    assert "batch.job.aibrix.ai/metadata.priority" in annotations
    assert annotations["batch.job.aibrix.ai/metadata.priority"] == "high"


def test_namespace_from_k8s_job():
    """Test that namespace is correctly extracted from k8s_job."""
    cache = JobCache()

    job_spec = BatchJobSpec.from_strings(
        input_file_id="test-file-456",
        endpoint="/v1/embeddings",
        completion_window="24h",
    )

    k8s_job = cache._batch_job_to_k8s_job(job_spec)

    # Verify namespace extraction logic would work
    # (The template sets namespace to "default")
    namespace = k8s_job.metadata.namespace or "default"
    assert namespace == "default"


def test_unique_job_names_generated():
    """Test that unique job names are generated for each job."""
    cache = JobCache()

    job_spec = BatchJobSpec.from_strings(
        input_file_id="test-file-789",
        endpoint="/v1/completions",
        completion_window="24h",
    )

    # Create multiple jobs
    k8s_job1 = cache._batch_job_to_k8s_job(job_spec)
    k8s_job2 = cache._batch_job_to_k8s_job(job_spec)

    # Verify unique names
    assert k8s_job1.metadata.name != k8s_job2.metadata.name
    assert k8s_job1.metadata.name.startswith("batch-")
    assert k8s_job2.metadata.name.startswith("batch-")
    assert len(k8s_job1.metadata.name.split("-")[1]) == 8  # UUID hex[:8]
    assert len(k8s_job2.metadata.name.split("-")[1]) == 8


def test_environment_variables_populated():
    """Test that environment variables are properly populated from BatchJobSpec."""
    cache = JobCache()

    job_spec = BatchJobSpec.from_strings(
        input_file_id="env-test-file",
        endpoint="/v1/chat/completions",
        completion_window="24h",
    )

    k8s_job = cache._batch_job_to_k8s_job(job_spec)

    # Get container env vars
    container = k8s_job.spec.template.spec.containers[0]
    env_vars = {env.name: env.value for env in container.env}

    # Verify environment variables
    assert env_vars["INPUT_FILE_ID"] == "env-test-file"
    assert env_vars["ENDPOINT"] == "/v1/chat/completions"
    assert env_vars["COMPLETION_WINDOW"] == "24h"


def test_template_deep_copy_isolation():
    """Test that template modifications don't affect the original."""
    cache = JobCache()

    # Get original template name
    original_name = cache.job_template["metadata"]["name"]

    job_spec = BatchJobSpec.from_strings(
        input_file_id="isolation-test",
        endpoint="/v1/embeddings",
        completion_window="24h",
    )

    # Create job (this modifies the template copy)
    k8s_job = cache._batch_job_to_k8s_job(job_spec)

    # Verify original template is unchanged
    assert cache.job_template["metadata"]["name"] == original_name
    assert k8s_job.metadata.name != original_name
    assert k8s_job.metadata.name.startswith("batch-")


def test_annotation_merging_with_empty_template_annotations():
    """Test annotation merging when template has no annotations."""
    cache = JobCache()

    # Verify template has no annotations (or empty annotations)
    template_annotations = cache.job_template.get("metadata", {}).get("annotations")
    assert template_annotations is None or template_annotations == {}

    job_spec = BatchJobSpec.from_strings(
        input_file_id="merge-test",
        endpoint="/v1/completions",
        completion_window="24h",
        metadata={"test": "value"},
    )

    k8s_job = cache._batch_job_to_k8s_job(job_spec)

    # Verify annotations are properly set despite empty template annotations
    annotations = k8s_job.metadata.annotations
    assert len(annotations) >= 4  # 3 batch annotations + 1 metadata annotation
    assert "batch.job.aibrix.ai/input-file-id" in annotations
    assert "batch.job.aibrix.ai/metadata.test" in annotations

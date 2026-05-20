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

"""Pydantic schema validation tests for ModelDeploymentTemplate / BatchProfile."""

import pytest
from pydantic import ValidationError

from aibrix.batch.job_entity import BatchJobEndpoint
from aibrix.batch.template import (
    AcceleratorSpec,
    BatchProfile,
    BatchProfileList,
    EngineArgsSpec,
    EngineSpec,
    EngineType,
    MetastoreBackend,
    MetastoreSpec,
    ModelDeploymentTemplate,
    ModelDeploymentTemplateList,
    ModelSourceSpec,
    ModelSourceType,
    ParallelismSpec,
    ProfileOverridesSpec,
    StorageBackend,
    StorageSpec,
    TemplateOverridesSpec,
    TemplateStatus,
)


def _minimal_template_spec(**overrides):
    """Build a minimal valid ModelDeploymentTemplateSpec dict for tests."""
    base = {
        "engine": EngineSpec(type=EngineType.VLLM, version="0.6.3", image="x"),
        "model_source": ModelSourceSpec(type=ModelSourceType.S3, uri="s3://x"),
        "accelerator": AcceleratorSpec(type="H100", count=1),
        "supported_endpoints": [BatchJobEndpoint.CHAT_COMPLETIONS],
    }
    base.update(overrides)
    return base


class TestModelDeploymentTemplate:
    def test_minimal_valid(self):
        t = ModelDeploymentTemplate(
            name="m", version="v1", spec=_minimal_template_spec()
        )
        assert t.name == "m"
        assert t.status == TemplateStatus.ACTIVE  # default
        assert t.spec.parallelism.world_size == 1

    def test_parallelism_world_size_must_match_accelerator_count(self):
        with pytest.raises(ValidationError, match="world_size"):
            ModelDeploymentTemplate(
                name="m",
                version="v1",
                spec=_minimal_template_spec(
                    accelerator=AcceleratorSpec(type="H100", count=4),
                    parallelism=ParallelismSpec(tp=2),  # 2 != 4
                ),
            )

    def test_parallelism_product_matches(self):
        # tp=2 * pp=2 = 4 matches count=4
        t = ModelDeploymentTemplate(
            name="m",
            version="v1",
            spec=_minimal_template_spec(
                accelerator=AcceleratorSpec(type="H100", count=4),
                parallelism=ParallelismSpec(tp=2, pp=2),
            ),
        )
        assert t.spec.parallelism.world_size == 4

    def test_supported_endpoints_required(self):
        with pytest.raises(ValidationError):
            ModelDeploymentTemplate(
                name="m",
                version="v1",
                spec=_minimal_template_spec(supported_endpoints=[]),
            )

    def test_strict_mode_rejects_unknown_field(self):
        with pytest.raises(ValidationError):
            ModelDeploymentTemplate(
                name="m",
                version="v1",
                undocumented="x",
                spec=_minimal_template_spec(),
            )


class TestModelDeploymentTemplateList:
    def test_list_accepts_top_level_yaml_list(self):
        # No apiVersion/kind envelope; just a list
        data = [
            {
                "name": "a",
                "version": "v1",
                "status": "active",
                "spec": _minimal_template_spec(),
            },
        ]
        # Pydantic v2 RootModel accepts the list directly; we need to
        # build the underlying spec dicts since our helper returns model
        # instances. Build via model_validate which serializes models.
        cleaned = [
            {
                **item,
                "spec": ModelDeploymentTemplate(
                    name="x", version="v1", spec=item["spec"]
                ).spec.model_dump(),
            }
            for item in data
        ]
        lst = ModelDeploymentTemplateList.model_validate(cleaned)
        assert len(lst.items) == 1
        assert lst.items[0].name == "a"

    def test_duplicate_name_version_rejected(self):
        spec_dict = ModelDeploymentTemplate(
            name="x", version="v1", spec=_minimal_template_spec()
        ).spec.model_dump()
        data = [
            {"name": "a", "version": "v1", "status": "active", "spec": spec_dict},
            {"name": "a", "version": "v1", "status": "active", "spec": spec_dict},
        ]
        with pytest.raises(ValidationError, match="duplicate"):
            ModelDeploymentTemplateList.model_validate(data)


class TestEngineArgsSpec:
    def test_gpu_memory_utilization_must_be_in_range(self):
        with pytest.raises(ValidationError):
            EngineArgsSpec(gpu_memory_utilization=2.0)
        with pytest.raises(ValidationError):
            EngineArgsSpec(gpu_memory_utilization=0.0)
        # valid edge: just under 1
        EngineArgsSpec(gpu_memory_utilization=1.0)

    def test_lenient_extras_passed_through(self):
        ea = EngineArgsSpec(max_num_seqs=128, custom_vllm_flag=True)
        dumped = ea.model_dump()
        assert dumped["custom_vllm_flag"] is True

    def test_max_num_batched_tokens_must_be_positive(self):
        with pytest.raises(ValidationError):
            EngineArgsSpec(max_num_batched_tokens=0)


class TestBatchProfile:
    def test_minimal_valid(self):
        p = BatchProfile(
            name="p1",
            spec={"storage": StorageSpec(backend=StorageBackend.S3, bucket="b")},
        )
        assert p.name == "p1"
        # defaults applied
        assert p.spec.scheduling.completion_window.value == "24h"
        assert p.spec.quota.max_requests_per_batch == 50000

    def test_storage_bucket_required(self):
        with pytest.raises(ValidationError):
            StorageSpec(backend=StorageBackend.S3, bucket="")

    def test_metastore_config_is_accepted(self):
        p = BatchProfile(
            name="p1",
            spec={
                "storage": StorageSpec(backend=StorageBackend.S3, bucket="b"),
                "metastore": MetastoreSpec(
                    backend=MetastoreBackend.REDIS,
                    endpoint_url="redis.default.svc.cluster.local",
                ),
            },
        )
        assert p.spec.metastore is not None
        assert p.spec.metastore.backend == MetastoreBackend.REDIS


class TestBatchProfileList:
    def _profile_dict(self, name):
        return {
            "name": name,
            "spec": {"storage": {"backend": "s3", "bucket": "b"}},
        }

    def test_default_must_exist_in_items(self):
        with pytest.raises(ValidationError, match="default"):
            BatchProfileList.model_validate(
                {"default": "missing", "items": [self._profile_dict("p1")]}
            )

    def test_no_default_is_ok(self):
        lst = BatchProfileList.model_validate({"items": [self._profile_dict("p1")]})
        assert lst.default is None

    def test_duplicate_profile_name_rejected(self):
        with pytest.raises(ValidationError, match="duplicate"):
            BatchProfileList.model_validate(
                {"items": [self._profile_dict("p"), self._profile_dict("p")]}
            )


class TestTemplateOverridesSpec:
    def test_engine_args_override_validates(self):
        ovr = TemplateOverridesSpec(engine_args={"max_num_seqs": 1024})
        assert ovr.engine_args.max_num_seqs == 1024

    def test_strict_rejects_unknown_top_level_field(self):
        # Profile-side keys like 'scheduling' must not slip into the
        # template namespace — that is precisely the ambiguity the split
        # exists to remove.
        with pytest.raises(ValidationError):
            TemplateOverridesSpec(scheduling={"max_concurrency": 32})
        # Sensitive fields (accelerator, image, etc.) are also not in the
        # template-side allowlist.
        with pytest.raises(ValidationError):
            TemplateOverridesSpec(accelerator={"count": 8})

    def test_engine_args_invalid_value_rejected_at_request_time(self):
        # gpu_memory_utilization > 1.0 must fail before reaching renderer
        with pytest.raises(ValidationError):
            TemplateOverridesSpec(engine_args={"gpu_memory_utilization": 2.0})


class TestProfileOverridesSpec:
    def test_scheduling_override_accepted(self):
        ovr = ProfileOverridesSpec(scheduling={"max_concurrency": 32})
        assert ovr.scheduling is not None
        assert ovr.scheduling.max_concurrency == 32
        # Only the user-set field is preserved on the wire so partial
        # overrides don't silently overwrite profile defaults downstream.
        assert ovr.model_dump(exclude_unset=True) == {
            "scheduling": {"max_concurrency": 32}
        }

    def test_strict_rejects_template_side_keys(self):
        # engine_args belongs to the template namespace, not profile.
        with pytest.raises(ValidationError):
            ProfileOverridesSpec(engine_args={"max_num_seqs": 1024})
        # storage / quota are profile-managed but not user-overridable.
        with pytest.raises(ValidationError):
            ProfileOverridesSpec(storage={"bucket": "evil"})

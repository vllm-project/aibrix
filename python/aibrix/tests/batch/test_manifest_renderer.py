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

"""JobManifestRenderer end-to-end tests.

Each test builds inline registries via LocalFileSource so no
Kubernetes cluster is required.
"""

from pathlib import Path

import pytest
import yaml

from aibrix import envs
from aibrix.batch.job_entity import (
    BatchJobSpec,
    BatchJobTransformer,
)
from aibrix.batch.manifest import (
    EndpointNotSupported,
    ForbiddenOverride,
    JobManifestRenderer,
    ProfileNotFound,
    RenderError,
    TemplateNotFound,
    UnsupportedDeploymentMode,
)
from aibrix.batch.template import (
    local_profile_registry,
    local_template_registry,
)

# ─────────────────────────────────────────────────────────────────────────────
# Fixtures
# ─────────────────────────────────────────────────────────────────────────────

_TESTDATA_DIR = Path(__file__).parent / "testdata"


def _write_yaml(path: Path, obj) -> Path:
    path.write_text(yaml.safe_dump(obj))
    return path


def _load_testdata_yaml(name: str):
    return yaml.safe_load((_TESTDATA_DIR / name).read_text())


def _vllm_template(name="vllm-prod", count=4, **engine_args):
    return {
        "name": name,
        "version": "v1",
        "status": "active",
        "spec": {
            "engine": {
                "type": "vllm",
                "version": "0.6.3",
                "image": "vllm/vllm-openai:v0.6.3",
                "serve_args": ["--port=8000"],
            },
            "model_source": {"type": "huggingface", "uri": "Qwen/Qwen2.5-7B"},
            "accelerator": {"type": "H100", "count": count},
            "parallelism": {"tp": count},
            "engine_args": engine_args,
            "supported_endpoints": ["/v1/chat/completions", "/v1/embeddings"],
            "deployment_mode": "dedicated",
        },
    }


def _mock_template(name="mock"):
    return {
        "name": name,
        "version": "v1",
        "status": "active",
        "spec": {
            "engine": {
                "type": "mock",
                "version": "0.1",
                "image": "aibrix/vllm-mock:nightly",
            },
            "model_source": {"type": "local", "uri": "/x"},
            "accelerator": {"type": "cpu", "count": 1},
            "parallelism": {"tp": 1},
            "supported_endpoints": ["/v1/chat/completions"],
        },
    }


def _profile(name="default-profile", backend="s3", bucket="b", metastore=None):
    spec = {
        "storage": {
            "backend": backend,
            "bucket": bucket,
            "credentials_secret_ref": "creds",
        },
    }
    if metastore is not None:
        spec["metastore"] = metastore
    return {
        "name": name,
        "spec": spec,
    }


@pytest.fixture
def renderer_factory(tmp_path):
    """Build a renderer from inline templates + profiles."""

    def _build(templates, profiles, default_profile="default-profile"):
        t_path = _write_yaml(tmp_path / "t.yaml", templates)
        p_path = _write_yaml(
            tmp_path / "p.yaml",
            {"default": default_profile, "items": profiles},
        )
        treg = local_template_registry(t_path)
        preg = local_profile_registry(p_path)
        treg.reload()
        preg.reload()
        return JobManifestRenderer(treg, preg)

    return _build


def _spec(model_template="vllm-prod", endpoint="/v1/chat/completions", **kw):
    return BatchJobSpec.from_strings(
        input_file_id="file-1",
        endpoint=endpoint,
        completion_window="24h",
        model_template_name=model_template,
        **kw,
    )


def _engine_container(manifest):
    return next(
        c
        for c in manifest["spec"]["template"]["spec"]["containers"]
        if c["name"] == "llm-engine"
    )


def _worker_container(manifest):
    return next(
        c
        for c in manifest["spec"]["template"]["spec"]["containers"]
        if c["name"] == "batch-worker"
    )


class TestRenderJobExample:
    def test_k8s_job_example_matches_yaml(self, renderer_factory):
        template = _mock_template()
        template["spec"]["engine"]["health_endpoint"] = "/ready"
        r = renderer_factory(
            templates=[template],
            profiles=[
                _profile(
                    name="example-profile",
                    backend="local",
                    bucket="/tmp/aibrix-storage",
                )
            ],
            default_profile="example-profile",
        )
        spec = _spec(model_template="mock")
        spec.metadata = {"team": "infra", "project": "p"}

        rendered = r.render(
            session_id="example-session",
            spec=spec,
            job_name="batch-job-template",
        )

        assert rendered == _load_testdata_yaml("k8s_job_example.yaml")


# ─────────────────────────────────────────────────────────────────────────────
# Happy paths
# ─────────────────────────────────────────────────────────────────────────────


class TestRendererHappyPath:
    def test_vllm_h100x4(self, renderer_factory):
        r = renderer_factory(
            templates=[
                _vllm_template(
                    count=4, max_num_batched_tokens=32768, enable_prefix_caching=True
                )
            ],
            profiles=[_profile()],
        )
        m = r.render(session_id="s1", spec=_spec(), job_name="batch-1")
        assert m["metadata"]["name"] == "batch-1"
        # Templates are provider-agnostic; namespace is the system default.
        assert m["metadata"]["namespace"] == "default"
        engine = _engine_container(m)
        assert engine["image"] == "vllm/vllm-openai:v0.6.3"
        assert engine["resources"]["limits"]["nvidia.com/gpu"] == "4"
        # vLLM args: tp + engine args
        args = " ".join(engine["args"])
        assert "--tensor-parallel-size 4" in args
        assert "--max-num-batched-tokens 32768" in args
        assert "--enable-prefix-caching" in args

    def test_mock_template_no_gpu_resources(self, renderer_factory):
        r = renderer_factory(
            templates=[_mock_template()],
            profiles=[_profile()],
        )
        m = r.render(session_id="s1", spec=_spec(model_template="mock"))
        engine = _engine_container(m)
        assert engine["image"] == "aibrix/vllm-mock:nightly"
        # CPU accelerator: no GPU resources
        assert "resources" not in engine
        # Mock uses shell wrapper
        assert engine["command"] == ["/bin/sh", "-c"]

    def test_storage_env_injected(self, renderer_factory):
        r = renderer_factory(
            templates=[_vllm_template(count=1)],  # tp default 1, count=1
            profiles=[_profile(backend="s3")],
        )
        m = r.render(session_id="s1", spec=_spec())
        worker = _worker_container(m)
        env_names = {e["name"] for e in worker["env"]}
        assert "STORAGE_TYPE" in env_names
        assert "STORAGE_AWS_ACCESS_KEY_ID" in env_names
        assert "STORAGE_AWS_BUCKET" in env_names

    def test_local_storage_env_injected(self, renderer_factory):
        r = renderer_factory(
            templates=[_vllm_template(count=1)],
            profiles=[_profile(backend="local", bucket="/tmp/aibrix-storage")],
        )
        m = r.render(session_id="s1", spec=_spec())
        worker = _worker_container(m)
        env = {e["name"]: e.get("value") for e in worker["env"]}
        assert env["STORAGE_TYPE"] == "local"
        assert env["STORAGE_LOCAL_PATH"] == "/tmp/aibrix-storage"

    def test_default_profile_used_when_omitted(self, renderer_factory):
        r = renderer_factory(
            templates=[_vllm_template(count=1)],
            profiles=[_profile()],
            default_profile="default-profile",
        )
        m = r.render(session_id="s1", spec=_spec())
        # Default profile name persisted as annotation
        ann = m["spec"]["template"]["metadata"]["annotations"]
        assert ann["batch.job.aibrix.ai/profile-name"] == "default-profile"

    def test_explicit_profile_overrides_default(self, renderer_factory):
        r = renderer_factory(
            templates=[_vllm_template(count=1)],
            profiles=[_profile("default-profile"), _profile("alt", bucket="alt-b")],
        )
        spec = _spec(profile_name="alt")
        m = r.render(session_id="s1", spec=spec)
        ann = m["spec"]["template"]["metadata"]["annotations"]
        assert ann["batch.job.aibrix.ai/profile-name"] == "alt"

    def test_per_batch_metadata_persisted(self, renderer_factory):
        r = renderer_factory(
            templates=[_vllm_template(count=1)],
            profiles=[_profile()],
        )
        spec = _spec()
        spec.metadata = {"team": "infra", "project": "p"}
        m = r.render(session_id="s1", spec=spec)
        ann = m["spec"]["template"]["metadata"]["annotations"]
        assert ann["batch.job.aibrix.ai/metadata.team"] == "infra"
        assert ann["batch.job.aibrix.ai/metadata.project"] == "p"

    def test_completion_window_drives_deadline(self, renderer_factory):
        r = renderer_factory(
            templates=[_vllm_template(count=1)],
            profiles=[_profile()],
        )
        m = r.render(session_id="s1", spec=_spec())
        assert m["spec"]["activeDeadlineSeconds"] == 86400  # 24h

    def test_parallelism_override(self, renderer_factory):
        r = renderer_factory(
            templates=[_vllm_template(count=1)],
            profiles=[_profile()],
        )
        m = r.render(session_id="s1", spec=_spec(), parallelism=3)
        assert m["spec"]["parallelism"] == 3
        assert m["spec"]["completions"] == 3


# ─────────────────────────────────────────────────────────────────────────────
# Override paths
# ─────────────────────────────────────────────────────────────────────────────


class TestRendererOverrides:
    def test_engine_args_override(self, renderer_factory):
        r = renderer_factory(
            templates=[_vllm_template(count=1, max_num_seqs=256)],
            profiles=[_profile()],
        )
        spec = _spec(template_overrides={"engine_args": {"max_num_seqs": 1024}})
        m = r.render(session_id="s1", spec=spec)
        engine = _engine_container(m)
        idx = engine["args"].index("--max-num-seqs")
        assert engine["args"][idx + 1] == "1024"

    def test_forbidden_override_field_rejected(self, renderer_factory):
        r = renderer_factory(
            templates=[_vllm_template(count=1)],
            profiles=[_profile()],
        )
        spec = _spec(template_overrides={"accelerator": {"count": 8}})
        with pytest.raises(ForbiddenOverride):
            r.render(session_id="s1", spec=spec)

    def test_invalid_override_value_rejected(self, renderer_factory):
        from pydantic import ValidationError

        r = renderer_factory(
            templates=[_vllm_template(count=1)],
            profiles=[_profile()],
        )
        spec = _spec(
            template_overrides={"engine_args": {"gpu_memory_utilization": 2.0}}
        )
        # Renderer's merge step re-validates via EngineArgsSpec
        with pytest.raises(ValidationError):
            r.render(session_id="s1", spec=spec)


# ─────────────────────────────────────────────────────────────────────────────
# Validation / error paths
# ─────────────────────────────────────────────────────────────────────────────


class TestRendererValidation:
    def test_missing_template_name_raises(self, renderer_factory):
        r = renderer_factory(templates=[_vllm_template(count=1)], profiles=[_profile()])
        spec = _spec(model_template=None, job_id="job-123")
        assert spec.aibrix is not None
        assert spec.aibrix.job_id == "job-123"
        with pytest.raises(RenderError, match="model_template.name"):
            r.render(session_id="s1", spec=spec)

    def test_template_not_found(self, renderer_factory):
        r = renderer_factory(templates=[_vllm_template(count=1)], profiles=[_profile()])
        with pytest.raises(TemplateNotFound):
            r.render(session_id="s1", spec=_spec(model_template="missing"))

    def test_profile_not_found(self, renderer_factory):
        r = renderer_factory(
            templates=[_vllm_template(count=1)],
            profiles=[_profile()],
        )
        spec = _spec(profile_name="missing-profile")
        with pytest.raises(ProfileNotFound):
            r.render(session_id="s1", spec=spec)

    def test_no_default_and_no_profile_raises(self, tmp_path):
        # Profiles list has no 'default' field; spec doesn't pick one
        t_path = _write_yaml(tmp_path / "t.yaml", [_vllm_template(count=1)])
        p_path = _write_yaml(tmp_path / "p.yaml", {"items": [_profile("p")]})
        treg = local_template_registry(t_path)
        preg = local_profile_registry(p_path)
        treg.reload()
        preg.reload()
        r = JobManifestRenderer(treg, preg)
        with pytest.raises(RenderError, match="no profile"):
            r.render(session_id="s1", spec=_spec())

    def test_endpoint_not_in_supported(self, renderer_factory):
        r = renderer_factory(templates=[_vllm_template(count=1)], profiles=[_profile()])
        # _vllm_template supports chat/completions and embeddings
        with pytest.raises(EndpointNotSupported):
            r.render(session_id="s1", spec=_spec(endpoint="/v1/completions"))

    def test_rejects_shared_deployment_mode(self, renderer_factory):
        t = _vllm_template(count=1)
        t["spec"]["deployment_mode"] = "shared"
        r = renderer_factory(templates=[t], profiles=[_profile()])
        with pytest.raises(UnsupportedDeploymentMode):
            r.render(session_id="s1", spec=_spec())

    def test_rejects_external_deployment_mode(self, renderer_factory):
        t = _vllm_template(count=1)
        t["spec"]["deployment_mode"] = "external"
        r = renderer_factory(templates=[t], profiles=[_profile()])
        with pytest.raises(UnsupportedDeploymentMode):
            r.render(session_id="s1", spec=_spec())


# ─────────────────────────────────────────────────────────────────────────────
# Roundtrip: rendered manifest -> annotations -> BatchJobSpec
# ─────────────────────────────────────────────────────────────────────────────


class TestRendererRoundtrip:
    def test_annotations_extract_back_to_spec(self, renderer_factory):
        r = renderer_factory(
            templates=[_vllm_template(count=1)],
            profiles=[_profile(name="vllm-profile")],
        )
        spec = _spec(
            template_overrides={"engine_args": {"max_num_seqs": 512}},
            profile_name="vllm-profile",
            profile_overrides={"scheduling": {"max_concurrency": 16}},
        )
        spec.metadata = {"team": "x"}
        m = r.render(session_id="s1", spec=spec)
        extracted = BatchJobTransformer._extract_batch_job_spec(
            m["spec"]["template"]["metadata"]["annotations"], m["spec"]
        )
        assert extracted.input_file_id == spec.input_file_id
        assert extracted.endpoint == spec.endpoint
        assert extracted.model_template_name == "vllm-prod"
        # The renderer roundtrips the resolved version even when the spec
        # didn't pin one (the _vllm_template fixture defaults to "v1").
        assert extracted.model_template_version == "v1"
        assert extracted.profile_name == "vllm-profile"
        assert extracted.metadata == {"team": "x"}
        assert extracted.template_overrides == {"engine_args": {"max_num_seqs": 512}}
        assert extracted.profile_overrides == {"scheduling": {"max_concurrency": 16}}


class TestMetastoreEnv:
    """Regression tests for the Redis-metastore env var injection.

    The legacy controller applied two patches per batch (storage +
    metastore). The renderer must keep emitting REDIS_HOST/PORT/DB
    when the process metastore is Redis, otherwise the worker can't
    find the kv store and crashes silently.
    """

    def test_redis_metastore_env_injected(self, renderer_factory, monkeypatch):
        from aibrix.batch.storage import batch_metastore
        from aibrix.storage import StorageType

        monkeypatch.setattr(
            batch_metastore, "get_metastore_type", lambda: StorageType.REDIS
        )
        r = renderer_factory(
            templates=[_vllm_template(count=1)],
            profiles=[_profile()],
        )
        m = r.render(session_id="s1", spec=_spec())
        env_names = {e["name"] for e in _worker_container(m)["env"]}
        assert "REDIS_HOST" in env_names
        assert "REDIS_PORT" in env_names
        assert "REDIS_DB" in env_names

    def test_non_redis_metastore_emits_no_redis_env(
        self, renderer_factory, monkeypatch
    ):
        from aibrix.batch.storage import batch_metastore
        from aibrix.storage import StorageType

        monkeypatch.setattr(
            batch_metastore, "get_metastore_type", lambda: StorageType.LOCAL
        )
        r = renderer_factory(
            templates=[_vllm_template(count=1)],
            profiles=[_profile()],
        )
        m = r.render(session_id="s1", spec=_spec())
        env_names = {e["name"] for e in _worker_container(m)["env"]}
        assert "REDIS_HOST" not in env_names

    def test_worker_redis_host_overrides_metadata_redis_host(
        self, renderer_factory, monkeypatch
    ):
        """Metadata may use ``REDIS_HOST=localhost`` (port-forwarded
        for an off-cluster dev process), but workers running in-cluster
        need a Service DNS name. WORKER_REDIS_HOST is the override
        that lets the two views diverge."""
        from aibrix.batch.storage import batch_metastore
        from aibrix.storage import StorageType

        monkeypatch.setattr(
            batch_metastore, "get_metastore_type", lambda: StorageType.REDIS
        )
        monkeypatch.setenv("REDIS_HOST", "localhost")
        monkeypatch.setenv("WORKER_REDIS_HOST", "redis.default.svc.cluster.local")
        monkeypatch.setenv("WORKER_REDIS_PORT", "16379")

        r = renderer_factory(
            templates=[_vllm_template(count=1)],
            profiles=[_profile()],
        )
        m = r.render(session_id="s1", spec=_spec())
        env = {
            e["name"]: e["value"] for e in _worker_container(m)["env"] if "value" in e
        }
        assert env["REDIS_HOST"] == "redis.default.svc.cluster.local"
        assert env["REDIS_PORT"] == "16379"

    def test_worker_redis_host_falls_back_to_redis_host(
        self, renderer_factory, monkeypatch
    ):
        """When metadata and workers share the same Redis address (the
        common in-cluster production case), the operator only needs to
        set REDIS_HOST."""
        from aibrix.batch.storage import batch_metastore
        from aibrix.storage import StorageType

        monkeypatch.setattr(
            batch_metastore, "get_metastore_type", lambda: StorageType.REDIS
        )
        monkeypatch.delenv("WORKER_REDIS_HOST", raising=False)
        monkeypatch.delenv("WORKER_REDIS_PORT", raising=False)
        monkeypatch.setattr(envs, "STORAGE_REDIS_HOST", "redis-shared.aibrix.svc")
        monkeypatch.setattr(envs, "STORAGE_REDIS_PORT", "16379")

        r = renderer_factory(
            templates=[_vllm_template(count=1)],
            profiles=[_profile()],
        )
        m = r.render(session_id="s1", spec=_spec())
        env = {
            e["name"]: e["value"] for e in _worker_container(m)["env"] if "value" in e
        }
        assert env["REDIS_HOST"] == "redis-shared.aibrix.svc"
        assert env["REDIS_PORT"] == "16379"

    def test_profile_metastore_endpoint_url_overrides_process_host(
        self, renderer_factory, monkeypatch
    ):
        from aibrix.batch.storage import batch_metastore
        from aibrix.storage import StorageType

        monkeypatch.setattr(
            batch_metastore, "get_metastore_type", lambda: StorageType.REDIS
        )
        monkeypatch.setattr(envs, "STORAGE_REDIS_HOST", "redis-shared.aibrix.svc")
        monkeypatch.setattr(envs, "STORAGE_REDIS_PORT", "16379")

        r = renderer_factory(
            templates=[_vllm_template(count=1)],
            profiles=[
                _profile(
                    metastore={
                        "backend": "redis",
                        "endpoint_url": "redis-profile.aibrix.svc",
                    }
                )
            ],
        )
        m = r.render(session_id="s1", spec=_spec())
        env = {e["name"]: e for e in _worker_container(m)["env"]}
        assert env["REDIS_HOST"]["value"] == "redis-profile.aibrix.svc"
        assert env["REDIS_PORT"]["value"] == "16379"

    def test_profile_metastore_secret_endpoint_has_fallback_host(
        self, renderer_factory, monkeypatch
    ):
        from aibrix.batch.storage import batch_metastore
        from aibrix.storage import StorageType

        monkeypatch.setattr(
            batch_metastore, "get_metastore_type", lambda: StorageType.REDIS
        )
        monkeypatch.delenv("WORKER_REDIS_HOST", raising=False)
        monkeypatch.delenv("WORKER_REDIS_PORT", raising=False)

        r = renderer_factory(
            templates=[_vllm_template(count=1)],
            profiles=[
                _profile(
                    metastore={
                        "backend": "redis",
                        "credentials_secret_ref": "redis-creds",
                        "endpoint_url": "redis-profile.aibrix.svc",
                    }
                )
            ],
        )
        m = r.render(session_id="s1", spec=_spec())
        env = {e["name"]: e for e in _worker_container(m)["env"]}
        assert env["REDIS_HOST"]["valueFrom"]["secretKeyRef"] == {
            "name": "redis-creds",
            "key": "host",
        }
        assert env["REDIS_PORT"]["valueFrom"]["secretKeyRef"] == {
            "name": "redis-creds",
            "key": "port",
        }
        assert env["REDIS_DB"]["valueFrom"]["secretKeyRef"] == {
            "name": "redis-creds",
            "key": "db",
        }
        assert env["REDIS_PASSWORD"]["valueFrom"]["secretKeyRef"] == {
            "name": "redis-creds",
            "key": "password",
        }


class TestMockEngineOverrideNoop:
    def test_mock_template_ignores_engine_args_override(self, renderer_factory, caplog):
        r = renderer_factory(
            templates=[_mock_template()],
            profiles=[_profile()],
        )
        spec = _spec(
            model_template="mock",
            template_overrides={"engine_args": {"max_num_seqs": 256}},
        )
        m = r.render(session_id="s1", spec=spec)
        engine = _engine_container(m)
        # Mock engine args derived from serve_args / fallback only;
        # overridden engine_args do NOT appear.
        joined = " ".join(engine["args"])
        assert "--max-num-seqs" not in joined


class TestSuspendLogic:
    """Regression: K8s Job suspend must be True until file IDs are ready.

    suspend=True means "do not create Pods". A Job with missing file
    annotations should stay suspended until an external reconciler
    patches in the IDs and unsuspends.
    """

    def _make_prepared_job(self, **status_kwargs):
        """Build a minimal BatchJob with the given status fields populated."""
        from datetime import datetime, timezone

        from aibrix.batch.job_entity import (
            BatchJob,
            BatchJobSpec,
            BatchJobState,
            BatchJobStatus,
            ObjectMeta,
            RequestCountStats,
            TypeMeta,
        )

        # BatchJobStatus uses camelCase aliases on populate; build via the
        # validate API so populate-by-name works for our snake_case kwargs.
        status_data = {
            "jobID": "test-job-uid",
            "state": BatchJobState.CREATED,
            "requestCounts": RequestCountStats(),
            "createdAt": datetime.now(timezone.utc),
        }
        # Map snake_case test kwargs to camelCase aliases.
        alias_map = {
            "output_file_id": "outputFileID",
            "temp_output_file_id": "tempOutputFileID",
            "error_file_id": "errorFileID",
            "temp_error_file_id": "tempErrorFileID",
        }
        for k, v in status_kwargs.items():
            status_data[alias_map.get(k, k)] = v

        return BatchJob(
            sessionID="sess",
            typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
            metadata=ObjectMeta(name="x", namespace="default"),
            spec=BatchJobSpec(input_file_id="f", endpoint="/v1/chat/completions"),
            status=BatchJobStatus.model_validate(status_data),
        )

    def test_no_prepared_job_keeps_suspended(self, renderer_factory):
        r = renderer_factory(templates=[_vllm_template(count=1)], profiles=[_profile()])
        m = r.render(session_id="s1", spec=_spec(), prepared_job=None)
        assert m["spec"]["suspend"] is True

    def test_partial_file_ids_keeps_suspended(self, renderer_factory):
        r = renderer_factory(templates=[_vllm_template(count=1)], profiles=[_profile()])
        prepared = self._make_prepared_job(
            output_file_id="out-1",
            # temp_output / error / temp_error all missing
        )
        m = r.render(session_id="s1", spec=_spec(), prepared_job=prepared)
        assert m["spec"]["suspend"] is True

    def test_all_file_ids_unsuspends(self, renderer_factory):
        r = renderer_factory(templates=[_vllm_template(count=1)], profiles=[_profile()])
        prepared = self._make_prepared_job(
            output_file_id="out-1",
            temp_output_file_id="tmp-out-1",
            error_file_id="err-1",
            temp_error_file_id="tmp-err-1",
        )
        m = r.render(session_id="s1", spec=_spec(), prepared_job=prepared)
        assert m["spec"]["suspend"] is False
        ann = m["spec"]["template"]["metadata"]["annotations"]
        assert ann["batch.job.aibrix.ai/output-file-id"] == "out-1"
        assert ann["batch.job.aibrix.ai/error-file-id"] == "err-1"


class TestLlmReadyEndpoint:
    """Regression: LLM_READY_ENDPOINT must follow template's port + health_endpoint."""

    def test_default_port_and_health(self, renderer_factory):
        r = renderer_factory(templates=[_vllm_template(count=1)], profiles=[_profile()])
        m = r.render(session_id="s1", spec=_spec())
        worker = _worker_container(m)
        env = {e["name"]: e.get("value") for e in worker["env"] if "value" in e}
        # vllm template default health_endpoint=/health (from EngineSpec default)
        assert env["LLM_READY_ENDPOINT"] == "http://localhost:8000/health"

    def test_custom_port_in_serve_args(self, renderer_factory):
        t = _vllm_template(count=1)
        t["spec"]["engine"]["serve_args"] = ["--port=9001"]
        r = renderer_factory(templates=[t], profiles=[_profile()])
        m = r.render(session_id="s1", spec=_spec())
        worker = _worker_container(m)
        env = {e["name"]: e.get("value") for e in worker["env"] if "value" in e}
        assert env["LLM_READY_ENDPOINT"] == "http://localhost:9001/health"

    def test_custom_health_endpoint(self, renderer_factory):
        t = _vllm_template(count=1)
        t["spec"]["engine"]["health_endpoint"] = "/v1/healthz"
        r = renderer_factory(templates=[t], profiles=[_profile()])
        m = r.render(session_id="s1", spec=_spec())
        worker = _worker_container(m)
        env = {e["name"]: e.get("value") for e in worker["env"] if "value" in e}
        assert env["LLM_READY_ENDPOINT"] == "http://localhost:8000/v1/healthz"

    def test_engine_readiness_probe_matches(self, renderer_factory):
        """Engine container probe and worker LLM_READY_ENDPOINT must agree."""
        t = _vllm_template(count=1)
        t["spec"]["engine"]["serve_args"] = ["--port=7000"]
        t["spec"]["engine"]["health_endpoint"] = "/ping"
        r = renderer_factory(templates=[t], profiles=[_profile()])
        m = r.render(session_id="s1", spec=_spec())
        engine = _engine_container(m)
        worker = _worker_container(m)

        probe = engine["readinessProbe"]["httpGet"]
        assert probe["port"] == 7000
        assert probe["path"] == "/ping"

        env = {e["name"]: e.get("value") for e in worker["env"] if "value" in e}
        assert env["LLM_READY_ENDPOINT"] == "http://localhost:7000/ping"

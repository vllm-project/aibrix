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

"""JobManifestRenderer: 5-layer composition for K8s Job manifests.

Replaces the static k8s_job_template.yaml + storage patch + per-batch
merge with a typed pipeline driven by ConfigMap-loaded
ModelDeploymentTemplate and BatchProfile.

The 5 layers are applied in order; each layer can only add or extend
fields, not remove. The per-batch layer is final and not subject to
override merging.

  1. system_base()          -- worker container, pod policy
  2. apply_template()       -- engine container, GPU resources
  3. apply_profile()        -- storage env vars
  4. apply_overrides()      -- allowlisted user overrides
  5. apply_per_batch()      -- annotations, file IDs, deadline, name

Currently supports:
  - engine.type in {vllm, mock}
  - deployment_mode == dedicated

Templates are provider-agnostic: K8s defaults (namespace, serviceAccount)
are baked into _system_base; nodeSelector / tolerations / affinity are left
unset so cluster scheduling decides. Multi-provider arbitration belongs to
BatchProfile.scheduling, not Template.

Other values raise RenderError.
"""

from __future__ import annotations

import json
import uuid
from typing import Any, Dict, Iterable, List, Optional

from aibrix.batch.job_entity import (
    AibrixMetadata,
    BatchJob,
    BatchJobSpec,
    BatchProfileRef,
    JobAnnotationKey,
    ModelTemplateRef,
)
from aibrix.batch.manifest.engine_adapter import build_engine_args, needs_shell_wrapper
from aibrix.batch.manifest.storage_env import build_metastore_env, build_storage_env
from aibrix.batch.template import (
    BatchProfile,
    DeploymentMode,
    EngineArgsSpec,
    EngineType,
    ModelDeploymentTemplate,
    ModelSourceType,
    ProfileRegistry,
    SchedulingSpec,
    TemplateRegistry,
)
from aibrix.logger import init_logger

logger = init_logger(__name__)

# ─────────────────────────────────────────────────────────────────────────────
# Errors
# ─────────────────────────────────────────────────────────────────────────────


class RenderError(Exception):
    pass


class TemplateNotFound(RenderError):
    def __init__(self, name: str):
        super().__init__(f"ModelDeploymentTemplate '{name}' not found in registry")
        self.name = name


class ProfileNotFound(RenderError):
    def __init__(self, name: str):
        super().__init__(f"BatchProfile '{name}' not found in registry")
        self.name = name


class UnsupportedDeploymentMode(RenderError):
    def __init__(self, mode: DeploymentMode):
        super().__init__(
            f"deployment_mode '{mode.value}' is not currently supported; "
            f"only 'dedicated' is honored"
        )


class EndpointNotSupported(RenderError):
    def __init__(self, endpoint: str, supported: List[str]):
        super().__init__(
            f"endpoint '{endpoint}' is not in template's "
            f"supported_endpoints {supported}"
        )


class ForbiddenOverride(RenderError):
    def __init__(self, field: str, allowed: Iterable[str]):
        allowed_list = ", ".join(f"'{a}'" for a in sorted(allowed))
        super().__init__(
            f"override field '{field}' is not in the override allowlist; "
            f"only {allowed_list} may be overridden"
        )


# ─────────────────────────────────────────────────────────────────────────────
# Constants used by the renderer
# ─────────────────────────────────────────────────────────────────────────────


# These mirror the legacy k8s_job_template.yaml so the renderer produces
# byte-equivalent output for legacy mock-image setups. Keep in sync
# if the worker contract changes.
_DEFAULT_NAMESPACE = "default"
_DEFAULT_SERVICE_ACCOUNT = "job-reader-sa"

_ENGINE_PORT_DEFAULTS: Dict[str, int] = {
    EngineType.SGLANG.value: 30000,
    EngineType.LMDEPLOY.value: 23333,
}
_FALLBACK_ENGINE_PORT = 8000

# Per-engine health endpoint defaults. Used when the template leaves
# engine.health_endpoint empty so the console UI can hide this field —
# all supported engines expose /health by convention. Templates may still
# override (e.g. when fronting the engine with a reverse proxy that
# rewrites the path).
_ENGINE_HEALTH_DEFAULTS: Dict[str, str] = {
    EngineType.VLLM.value: "/health",
    EngineType.SGLANG.value: "/health",
    EngineType.TRTLLM.value: "/health",
}
_FALLBACK_HEALTH_ENDPOINT = "/health"


def _resolve_health_endpoint(engine_type: str, configured: str) -> str:
    """Return the engine health endpoint, falling back to per-engine default."""
    if configured:
        return configured
    return _ENGINE_HEALTH_DEFAULTS.get(engine_type, _FALLBACK_HEALTH_ENDPOINT)


def _build_source_auth_env(source_type: str, secret_name: str) -> List[Dict[str, Any]]:
    """Render auth_secret_ref into engine container env vars by source type.

    Phase 1 supports HuggingFace only: secret must contain a ``token`` key,
    mounted as ``HF_TOKEN``. S3/local are skipped with a warning so users see
    the field is recognized but not yet wired.
    """
    if not secret_name:
        return []
    if source_type == ModelSourceType.HUGGINGFACE.value:
        return [
            {
                "name": "HF_TOKEN",
                "valueFrom": {
                    "secretKeyRef": {"name": secret_name, "key": "token"},
                },
            }
        ]
    logger.warning(
        "auth_secret_ref set but source type has no auth wiring yet",
        source_type=source_type,
        secret_name=secret_name,
    )  # type: ignore[call-arg]
    return []


_DEFAULT_BACKOFF_LIMIT = 2
_DEFAULT_ACTIVE_DEADLINE = 86400  # 24h fallback when spec.completion_window absent
_DEFAULT_LABEL_APP = "aibrix-batch"
_MANAGED_BY_ANNOTATION = "batch.job.aibrix.ai/managed-by"
_WORKER_CONTAINER_NAME = "batch-worker"
_ENGINE_CONTAINER_NAME = "llm-engine"
_WORKER_IMAGE = "aibrix/runtime:nightly"
_WORKER_ENTRYPOINT = "aibrix_batch_worker"
# Override allowlists. Keys are top-level fields of TemplateOverridesSpec
# / ProfileOverridesSpec that users may provide under
# extra_body.aibrix.{model_template,profile}.overrides.
_TEMPLATE_OVERRIDE_ALLOWLIST = {"engine_args"}
_PROFILE_OVERRIDE_ALLOWLIST = {"scheduling"}


# ─────────────────────────────────────────────────────────────────────────────
# Renderer
# ─────────────────────────────────────────────────────────────────────────────


class _RendererSupport:
    def __init__(
        self,
        template_registry: Optional[TemplateRegistry] = None,
        profile_registry: Optional[ProfileRegistry] = None,
    ) -> None:
        self._templates = template_registry
        self._profiles = profile_registry

    def _validate_template(
        self,
        template: ModelDeploymentTemplate,
        endpoint: Optional[str] = None,
        skip_deployment_check: bool = True,
    ) -> None:
        # The endpoint setting overlap with per job spec.aibrix.planner_decision.resource_details[0].provider
        # Currently, endpoint can be None for platform independent template, and check will be waived.
        # TODO: Align the endpoint setting with per job spec.aibrix.planner_decision.resource_details[0].provider
        if endpoint is None:
            return
        supported = [e.value for e in template.spec.supported_endpoints]
        if endpoint not in supported:
            raise EndpointNotSupported(endpoint, supported)
        if (
            not skip_deployment_check
            and template.spec.deployment_mode != DeploymentMode.DEDICATED
        ):
            raise UnsupportedDeploymentMode(template.spec.deployment_mode)

    def _resolve(
        self, spec: BatchJobSpec, profile_required: bool = False
    ) -> tuple[ModelDeploymentTemplate, Optional[BatchProfile]]:
        if spec.aibrix is None:
            raise RenderError("aibrix is required to generate job specification")

        aibrix: AibrixMetadata = spec.aibrix

        if not aibrix.model_template:
            raise RenderError(
                "aibrix.model_template is required: "
                "the cluster has no built-in template fallback, so every "
                "batch must reference a registered ModelDeploymentTemplate"
            )

        template = self._resolve_template(aibrix.model_template)
        profile: Optional[BatchProfile] = None
        if profile_required:
            profile = self._resolve_profile(aibrix.profile)
        return template, profile

    def _resolve_template(
        self,
        ref: ModelTemplateRef,
    ) -> ModelDeploymentTemplate:
        if ref.spec is not None:
            payload = {"name": ref.name, "spec": ref.spec}
            if ref.version is not None:
                payload["version"] = ref.version
            template = ModelDeploymentTemplate.model_validate(payload)
        elif ref.version:
            if self._templates is None:
                raise RenderError("template registry is not configured")
            resolved_template = self._templates.get_by_version(ref.name, ref.version)
            if resolved_template is None:
                raise TemplateNotFound(f"{ref.name}@{ref.version}")
            template = resolved_template
        else:
            if self._templates is None:
                raise RenderError("template registry is not configured")
            resolved_template = self._templates.get(ref.name)
            if resolved_template is None:
                raise TemplateNotFound(ref.name)
            template = resolved_template

        # Apply template overrides
        # Template allowlist: 'engine_args' only.
        #
        # Unknown keys on either side raise ForbiddenOverride. Validation
        # of engine_args values is delegated to EngineArgsSpec; invalid
        # values bubble up as ValidationError.
        if ref.overrides:
            template = template.model_copy(deep=True)
            for key in ref.overrides:
                if key not in _TEMPLATE_OVERRIDE_ALLOWLIST:
                    raise ForbiddenOverride(
                        f"model_template.overrides.{key}",
                        _TEMPLATE_OVERRIDE_ALLOWLIST,
                    )

            ea_override = ref.overrides.get("engine_args")
            if ea_override:
                template = self._apply_template_engine_args_override(
                    template, ea_override
                )

        return template

    def _apply_template_engine_args_override(
        self,
        template: ModelDeploymentTemplate,
        ea_override: Dict[str, Any],
    ) -> ModelDeploymentTemplate:
        # Mock engines ignore engine_args entirely (they only look at
        # serve_args); rather than silently no-op the user's override
        # we warn and skip the rebuild to keep the existing shell command
        # untouched.
        if template.spec.engine.type == EngineType.MOCK:
            logger.warning(
                "engine_args override ignored for mock engine",
                template=template.name,
            )  # type: ignore[call-arg]
            return template

        # Merge override into template's engine_args + rebuild engine args.
        merged_dump = {
            **template.spec.engine_args.model_dump(exclude_none=True),
            **ea_override,
        }
        # Validate the merged result via EngineArgsSpec (raises ValidationError
        # on bad values like negative ints).
        template.spec.engine_args = EngineArgsSpec.model_validate(merged_dump)

        return template

    def _resolve_profile(self, ref: Optional[BatchProfileRef]) -> BatchProfile:
        if ref is not None and ref.spec is not None:
            profile = BatchProfile.model_validate({"name": ref.name, "spec": ref.spec})
        else:
            if self._profiles is None:
                raise RenderError("profile registry is not configured")

            resolved_profile_name = (
                ref.name
                if ref is not None and ref.name
                else self._profiles.default_name()
            )
            if not resolved_profile_name:
                raise RenderError(
                    "no profile specified and registry has no default profile"
                )

            resolved_profile = self._profiles.get(resolved_profile_name)
            if resolved_profile is None:
                raise ProfileNotFound(resolved_profile_name)
            profile = resolved_profile

        # Apply profile overrides
        # Profile allowlist:  'scheduling' only.

        # Profile.scheduling is currently accepted (and roundtripped via
        # annotations) but has no on-manifest effect — the deadline-aware
        # scheduler that consumes it has not landed yet. We still validate
        # the allowlist so unsupported keys cannot reach the worker.
        if ref and ref.overrides:
            profile = profile.model_copy(deep=True)
            for key in ref.overrides:
                if key not in _PROFILE_OVERRIDE_ALLOWLIST:
                    raise ForbiddenOverride(
                        f"profile.overrides.{key}",
                        _PROFILE_OVERRIDE_ALLOWLIST,
                    )

                s_override = ref.overrides.get("scheduling")
                if s_override:
                    profile = self._apply_profile_scheduling_override(
                        profile, s_override
                    )

        return profile

    def _apply_profile_scheduling_override(
        self,
        profile: BatchProfile,
        override: Dict[str, Any],
    ) -> BatchProfile:
        # Merge override into profile's scheduling + rebuild scheduling.
        merged_dump = {
            **profile.spec.scheduling.model_dump(exclude_none=True),
            **override,
        }
        # Validate the merged result via SchedulingSpec (raises ValidationError
        # on bad values like negative ints).
        profile.spec.scheduling = SchedulingSpec.model_validate(merged_dump)

        return profile

    @staticmethod
    def _resolve_engine_port(template: ModelDeploymentTemplate) -> int:
        port = _ENGINE_PORT_DEFAULTS.get(
            template.spec.engine.type, _FALLBACK_ENGINE_PORT
        )
        serve_args = template.spec.engine.serve_args
        for i, arg in enumerate(serve_args):
            raw_port: Optional[str] = None
            if arg in ("--port", "-p") and i + 1 < len(serve_args):
                raw_port = serve_args[i + 1]
            elif arg.startswith("--port="):
                raw_port = arg.split("=", 1)[1]
            if raw_port is None:
                continue
            try:
                port = int(raw_port)
            except ValueError:
                logger.warning(
                    "Ignoring non-integer port in serve_args; using default",
                    template_name=template.name,
                    raw_port=raw_port,
                    fallback_port=port,
                )  # type: ignore[call-arg]
        return port

    def _build_engine_container(
        self, template: ModelDeploymentTemplate, port: int
    ) -> Dict[str, Any]:
        spec = template.spec
        container: Dict[str, Any] = {
            "name": _ENGINE_CONTAINER_NAME,
            "image": spec.engine.image,
            "ports": [{"containerPort": port}],
            "readinessProbe": {
                "httpGet": {
                    "path": _resolve_health_endpoint(
                        spec.engine.type, spec.engine.health_endpoint
                    ),
                    "port": port,
                },
                "periodSeconds": 5,
                "successThreshold": 1,
                "timeoutSeconds": 1,
                "failureThreshold": 3,
            },
        }

        engine_args = build_engine_args(spec)
        if needs_shell_wrapper(spec.engine):
            container["command"] = ["/bin/sh", "-c"]
            container["args"] = engine_args
        else:
            container["args"] = engine_args

        resources = self._build_resources(template)
        if resources:
            container["resources"] = resources

        env = _build_source_auth_env(
            spec.model_source.type,
            spec.model_source.auth_secret_ref or "",
        )
        if env:
            container["env"] = env

        return container

    @staticmethod
    def _build_resources(template: ModelDeploymentTemplate) -> Optional[Dict[str, Any]]:
        acc = template.spec.accelerator
        if acc.type.lower() == "cpu":
            return None
        return {
            "limits": {"nvidia.com/gpu": str(acc.count)},
            "requests": {"nvidia.com/gpu": str(acc.count)},
        }

    @staticmethod
    def _find_container(manifest: Dict[str, Any], name: str) -> Dict[str, Any]:
        for container in manifest["spec"]["template"]["spec"]["containers"]:
            if container.get("name") == name:
                return container
        raise RenderError(f"container '{name}' not present in manifest")

    def _needs_model_download(self, template: ModelDeploymentTemplate) -> bool:
        return template.spec.model_source.type.value != "local"


class JobManifestRenderer(_RendererSupport):
    """Renders K8s Job manifests from BatchJobSpec + ConfigMap-loaded resources.

    Stateless once registries are bound. Multiple concurrent calls
    are safe; render() does not mutate registry state.
    """

    # ── Entry point ────────────────────────────────────────────────────────

    def render(
        self,
        session_id: str,
        spec: BatchJobSpec,
        prepared_job: Optional[BatchJob] = None,
        parallelism: Optional[int] = None,
        job_name: Optional[str] = None,
    ) -> Dict[str, Any]:
        """Render a complete K8s Job manifest.

        Args:
            session_id: Caller-supplied session id (annotation persistence).
            spec: BatchJobSpec from the create request.
            prepared_job: Optional BatchJob whose status carries file IDs;
                when present and complete, the Job is created un-suspended.
            parallelism: Optional override for spec.parallelism / completions
                (legacy callsite-provided value; templates do not encode it).
            job_name: Optional override for the K8s Job name; default
                'batch-{uuid8}'.

        Raises:
            TemplateNotFound, ProfileNotFound, UnsupportedDeploymentMode,
            EndpointNotSupported, ForbiddenOverride,
            UnsupportedEngineError (from engine_adapter).

        Returns:
            Dict ready for kubernetes.client.BatchV1Api.create_namespaced_job(body=...).
        """
        # overrides applied
        template, profile = self._resolve(spec, True)
        assert profile is not None

        # Validate the supportable value space up-front so downstream
        # layers can assume they're working with k8s + dedicated + supported endpoint.
        self._validate_template(template, spec.endpoint, False)

        # Layered composition.
        manifest = self._system_base()
        manifest = self._apply_template(manifest, template)
        manifest = self._apply_profile(manifest, profile)
        manifest = self._apply_per_batch(
            manifest,
            session_id=session_id,
            spec=spec,
            template=template,
            profile=profile,
            prepared_job=prepared_job,
            parallelism=parallelism,
            job_name=job_name,
        )
        return manifest

    # ── Layer 1: system base ────────────────────────────────────────────────

    def _system_base(self) -> Dict[str, Any]:
        """Return the immutable worker-and-pod-policy skeleton.

        Mirrors the structural fields of the legacy
        python/aibrix/aibrix/metadata/setting/k8s_job_template.yaml so
        existing controllers and worker entrypoint observe identical
        behavior.
        """
        return {
            "apiVersion": "batch/v1",
            "kind": "Job",
            "metadata": {
                "namespace": _DEFAULT_NAMESPACE,
                "labels": {"app": _DEFAULT_LABEL_APP},
            },
            "spec": {
                "suspend": True,
                "parallelism": 1,
                "completions": 1,
                "backoffLimit": _DEFAULT_BACKOFF_LIMIT,
                "activeDeadlineSeconds": _DEFAULT_ACTIVE_DEADLINE,
                "template": {
                    "metadata": {
                        "labels": {"app": _DEFAULT_LABEL_APP},
                    },
                    "spec": {
                        "serviceAccountName": _DEFAULT_SERVICE_ACCOUNT,
                        "automountServiceAccountToken": True,
                        "shareProcessNamespace": True,
                        "restartPolicy": "Never",
                        # WORKAROUND for an env-naming collision: legacy
                        # K8s service-link injection emits REDIS or S3 related
                        # metrics, we just disable it to avoid conflicts.
                        "enableServiceLinks": False,
                        "containers": [self._worker_container()],
                    },
                },
            },
        }

    def _worker_container(self) -> Dict[str, Any]:
        """The batch-worker container as it appears in legacy yaml."""
        return {
            "name": _WORKER_CONTAINER_NAME,
            "image": _WORKER_IMAGE,
            "command": [_WORKER_ENTRYPOINT],
            "env": list(_BASE_WORKER_ENV),
        }

    # ── Layer 2: template ──────────────────────────────────────────────────

    def _apply_template(
        self, manifest: Dict[str, Any], template: ModelDeploymentTemplate
    ) -> Dict[str, Any]:
        """Add the engine container based on the template spec."""
        port = self._resolve_engine_port(template)
        engine_container = self._build_engine_container(template, port)

        # Append engine container to the pod spec containers list.
        containers = manifest["spec"]["template"]["spec"]["containers"]
        containers.append(engine_container)

        # Worker readiness URL must follow the engine's actual port +
        # health endpoint; otherwise the worker probes the wrong target
        # when admins override --port via serve_args or set a non-default
        # health_endpoint on the template.
        health_path = _resolve_health_endpoint(
            template.spec.engine.type, template.spec.engine.health_endpoint
        )
        worker = self._find_container(manifest, _WORKER_CONTAINER_NAME)
        worker["env"].append(
            {
                "name": "LLM_READY_ENDPOINT",
                "value": f"http://localhost:{port}{health_path}",
            }
        )

        return manifest

    # ── Layer 3: profile ───────────────────────────────────────────────────

    def _apply_profile(
        self, manifest: Dict[str, Any], profile: BatchProfile
    ) -> Dict[str, Any]:
        """Inject storage and metastore env vars into batch-worker container.

        Storage env comes from the per-batch profile (where files live).
        Metastore env comes from per-profile metastore settings when
        configured, otherwise from the process-global metastore type.
        """
        worker = self._find_container(manifest, _WORKER_CONTAINER_NAME)
        existing_names = {entry["name"] for entry in worker["env"]}

        for entry in build_storage_env() + build_metastore_env():
            if entry["name"] not in existing_names:
                worker["env"].append(entry)
                existing_names.add(entry["name"])

        # Profile-driven scheduling fields that affect Job spec directly.
        # Only completion_window is honored, and only as 24h. The
        # actual deadline is set in apply_per_batch from
        # spec.completion_window so user-supplied window wins.

        return manifest

    # ── Layer 4: per-batch (final, immutable) ──────────────────────────────

    def _apply_per_batch(
        self,
        manifest: Dict[str, Any],
        session_id: str,
        spec: BatchJobSpec,
        template: ModelDeploymentTemplate,
        profile: BatchProfile,
        prepared_job: Optional[BatchJob],
        parallelism: Optional[int],
        job_name: Optional[str],
    ) -> Dict[str, Any]:
        """Set per-batch fields. Always last; not subject to override."""
        # Job name
        if job_name is None:
            job_name = f"batch-{uuid.uuid4().hex[:8]}"
        manifest["metadata"]["name"] = job_name
        manifest["metadata"].setdefault("annotations", {})[_MANAGED_BY_ANNOTATION] = (
            "aibrix"
        )

        # Pod annotations: spec fields + template/profile/overrides + file IDs.
        pod_annotations: Dict[str, str] = {
            JobAnnotationKey.SESSION_ID.value: session_id,
            JobAnnotationKey.INPUT_FILE_ID.value: spec.input_file_id,
            JobAnnotationKey.ENDPOINT.value: spec.endpoint,
        }

        # Template / profile / overrides persistence (annotation roundtrip
        # is exercised by k8s_transformer._extract_batch_job_spec).
        if spec.aibrix and spec.aibrix.model_template_name:
            pod_annotations[JobAnnotationKey.MODEL_TEMPLATE_NAME.value] = (
                spec.aibrix.model_template_name
            )
        # Persist the resolved version (the actual concrete one used) so a
        # reload sees the same template even if the registry's "latest
        # active" pointer moves later. Falls back to the spec's pinned value
        # when the resolver was bypassed.
        resolved_version = template.version or (
            spec.aibrix.model_template_version if spec.aibrix else None
        )
        if resolved_version:
            pod_annotations[JobAnnotationKey.MODEL_TEMPLATE_VERSION.value] = (
                resolved_version
            )
        if spec.aibrix and spec.aibrix.profile_name:
            pod_annotations[JobAnnotationKey.PROFILE_NAME.value] = (
                spec.aibrix.profile_name
            )
        elif self._profiles is not None:
            default_profile_name = self._profiles.default_name()
            if default_profile_name:
                # Persist the resolved profile so reload from K8s state is
                # deterministic even when the default later changes.
                pod_annotations[JobAnnotationKey.PROFILE_NAME.value] = (
                    default_profile_name
                )

        if spec.aibrix and spec.aibrix.template_overrides:
            pod_annotations[JobAnnotationKey.TEMPLATE_OVERRIDES.value] = json.dumps(
                spec.aibrix.template_overrides, sort_keys=True
            )
        if spec.aibrix and spec.aibrix.profile_overrides:
            pod_annotations[JobAnnotationKey.PROFILE_OVERRIDES.value] = json.dumps(
                spec.aibrix.profile_overrides, sort_keys=True
            )

        # Persist the full aibrix block as a single JSON annotation so the
        # non-template / profile fields (job_id, planner_decision) survive
        # the annotation roundtrip. Per-field annotations above remain for
        # backward compatibility with rows written by older builds.
        if spec.aibrix is not None:
            pod_annotations[JobAnnotationKey.AIBRIX.value] = (
                spec.aibrix.model_dump_json(exclude_none=True)
            )

        # User-supplied metadata / opts.
        if spec.metadata:
            for k, v in spec.metadata.items():
                pod_annotations[f"{JobAnnotationKey.METADATA_PREFIX.value}{k}"] = v
        if spec.opts:
            for k, v in spec.opts.items():
                pod_annotations[f"{JobAnnotationKey.OPTS_PREFIX.value}{k}"] = v

        # File IDs from prepared_job (output / temp_output / error / temp_error).
        # Suspend=True keeps the Job from creating Pods (per K8s semantics).
        # Only un-suspend when every required file ID is present; otherwise
        # keep the Job suspended so an external reconciler can patch in the
        # missing IDs (and flip suspend=False) once preparation completes.
        suspend = True
        if prepared_job is not None:
            status = prepared_job.status
            file_ids = (
                (JobAnnotationKey.OUTPUT_FILE_ID, status.output_file_id),
                (JobAnnotationKey.TEMP_OUTPUT_FILE_ID, status.temp_output_file_id),
                (JobAnnotationKey.ERROR_FILE_ID, status.error_file_id),
                (JobAnnotationKey.TEMP_ERROR_FILE_ID, status.temp_error_file_id),
            )
            for ann_key, value in file_ids:
                if value:
                    pod_annotations[ann_key.value] = value
            if all(v for _, v in file_ids):
                suspend = False

        manifest["spec"]["template"]["metadata"].setdefault("annotations", {}).update(
            pod_annotations
        )
        manifest["spec"]["suspend"] = suspend

        # The worker keys its metastore writes on its own job_id, taken from the
        # JOB_UID env. The base env wires JOB_UID from the k8s controller-uid —
        # a different UUID than the metadata service's batch job_id, which it
        # finalizes the output file under. Override JOB_UID with the batch
        # job_id so both sides share the same metastore key namespace; otherwise
        # the worker writes results under one id and finalize reads zero.
        if prepared_job is not None and prepared_job.job_id:
            worker = self._find_container(manifest, _WORKER_CONTAINER_NAME)
            for entry in worker["env"]:
                if entry["name"] == "JOB_UID":
                    entry.pop("valueFrom", None)
                    entry["value"] = prepared_job.job_id
                    break

        # Deadline: use the per-batch completion_window if supplied; else
        # fall back to system default. Phase 4 may further reduce this
        # based on profile.scheduling.completion_window tier.
        if spec.completion_window:
            manifest["spec"]["activeDeadlineSeconds"] = spec.completion_window

        # parallelism / completions: caller-provided override.
        if parallelism is not None:
            manifest["spec"]["parallelism"] = parallelism
            manifest["spec"]["completions"] = parallelism

        return manifest


def _ann_field_ref(annotation_key: str) -> Dict[str, Any]:
    return {"fieldRef": {"fieldPath": f"metadata.annotations['{annotation_key}']"}}


def _label_field_ref(label_key: str) -> Dict[str, Any]:
    return {"fieldRef": {"fieldPath": f"metadata.labels['{label_key}']"}}


_BASE_WORKER_ENV: List[Dict[str, Any]] = [
    {"name": "JOB_NAME", "valueFrom": _label_field_ref("job-name")},
    {
        "name": "JOB_NAMESPACE",
        "valueFrom": {"fieldRef": {"fieldPath": "metadata.namespace"}},
    },
    {
        "name": "JOB_UID",
        "valueFrom": _label_field_ref("batch.kubernetes.io/controller-uid"),
    },
    # LLM_READY_ENDPOINT is injected per-template by _apply_template
    # since both the port (from serve_args) and the health endpoint
    # (from template.engine.health_endpoint) are template-specific.
    {
        "name": "BATCH_INPUT_FILE_ID",
        "valueFrom": _ann_field_ref(JobAnnotationKey.INPUT_FILE_ID.value),
    },
    {
        "name": "BATCH_ENDPOINT",
        "valueFrom": _ann_field_ref(JobAnnotationKey.ENDPOINT.value),
    },
    {
        "name": "BATCH_OUTPUT_FILE_ID",
        "valueFrom": _ann_field_ref(JobAnnotationKey.OUTPUT_FILE_ID.value),
    },
    {
        "name": "BATCH_TEMP_OUTPUT_FILE_ID",
        "valueFrom": _ann_field_ref(JobAnnotationKey.TEMP_OUTPUT_FILE_ID.value),
    },
    {
        "name": "BATCH_ERROR_FILE_ID",
        "valueFrom": _ann_field_ref(JobAnnotationKey.ERROR_FILE_ID.value),
    },
    {
        "name": "BATCH_TEMP_ERROR_FILE_ID",
        "valueFrom": _ann_field_ref(JobAnnotationKey.TEMP_ERROR_FILE_ID.value),
    },
    {
        "name": "BATCH_OPTS_FAIL_AFTER_N_REQUESTS",
        "valueFrom": _ann_field_ref(
            f"{JobAnnotationKey.OPTS_PREFIX.value}fail_after_n_requests"
        ),
    },
]

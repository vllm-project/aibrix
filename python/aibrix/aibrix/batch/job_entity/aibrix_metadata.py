from enum import Enum
from typing import Any, Dict, List, Optional

from pydantic import Field, field_validator

from aibrix.batch.job_entity.base import _Lenient, _Strict

#: Absolute upper bound for the per-job smart-client in-flight cap. Protects
#: the gateway / inference endpoints from a single job requesting runaway
#: concurrency. Conservative for now; raise if real workloads need more.
MAX_CLIENT_CONCURRENCY = 256


class RuntimeTarget(str, Enum):
    """Where a batch job runs — the selector the user (or Console / planner)
    sets under ``extra_body.aibrix.runtime.target``.

    Each value maps to a registered ``Runtime`` (see ``register_runtime``).
    ``RuntimeSpec.target`` is typed as ``str`` rather than this enum so a
    downstream-registered backend stays wire-valid without editing this
    upstream enum; these are the known values.
    """

    #: Kubernetes Deployment + Service per job (the default k8s path).
    KUBERNETES = "Kubernetes"
    #: Kubernetes Job, fire-and-wait (the worker self-hosts and dispatches).
    KUBERNETES_JOB = "KubernetesJob"
    #: Lambda Cloud leased VM, SSH-launched engine.
    LAMBDA_CLOUD = "LambdaCloud"
    #: RunPod pod, SSH/API-launched engine.
    RUNPOD = "RunPod"
    #: No provisioning: the endpoint already exists (a pre-launched process or
    #: an OpenAI-style external API). The control plane just dispatches to it.
    EXTERNAL = "External"


class RuntimeSpec(_Lenient):
    """Selects the Runtime a batch job runs on.

    ``target`` maps to a registered Runtime (see ``register_runtime``).
    ``options`` is intentionally free-form and passed through for runtime-
    specific knobs such as Kubernetes namespace or cloud-region selectors.
    """

    target: str
    options: Dict[str, Any] = Field(default_factory=dict)


class ResourceDetail(_Lenient):
    endpoint_cluster: Optional[str] = None
    gpu_type: Optional[str] = None
    replica: Optional[int] = None


class ResourceAllocation(_Lenient):
    """Resource allocation metadata returned by a planner / resource manager."""

    provision_id: Optional[str] = None
    provision_resource_deadline: Optional[int] = None
    resource_details: Optional[List[ResourceDetail]] = None

    @field_validator("resource_details", mode="before")
    @classmethod
    def normalize_resource_details(cls, value: Any) -> Any:
        if value is None:
            return None
        if isinstance(value, list):
            return value
        return [value]


class ClientRetryPolicy(_Strict):
    """Per-job smart-client retry behavior."""

    max_retries: Optional[int] = Field(default=None, ge=0)
    base_delay_seconds: Optional[float] = Field(default=None, ge=0)
    max_delay_seconds: Optional[float] = Field(default=None, ge=0)
    no_endpoint_max_retries: Optional[int] = Field(default=None, ge=0)


class ClientConfig(_Strict):
    """Per-job smart-client execution controls."""

    max_concurrency: Optional[int] = Field(
        default=None, ge=1, le=MAX_CLIENT_CONCURRENCY
    )
    adaptive_concurrency: Optional[bool] = None
    adaptive_max_factor: Optional[float] = Field(default=None, ge=1)
    retry_policy: Optional[ClientRetryPolicy] = None


class ModelTemplateRef(_Strict):
    """Reference to a ModelDeploymentTemplate registered via ConfigMap."""

    name: str = Field(
        description="Name of ModelDeploymentTemplate registered via ConfigMap. Required at render time.",
    )
    version: Optional[str] = Field(
        default=None,
        description=(
            "Optional template version pin. Empty / null resolves to the "
            "latest active version of the named template."
        ),
    )
    spec: Optional[Dict[str, Any]] = Field(
        default=None,
        description=(
            "Inline template spec pushed by trusted callers (e.g. Console) "
            "so renderers can skip the local registry lookup. When set, "
            "consumers should prefer this over registry resolution."
        ),
    )
    overrides: Optional[Dict[str, Any]] = Field(
        default=None,
        description=(
            "Allowlisted overrides applied on top of the resolved template "
            "spec at render time."
        ),
    )


class BatchProfileRef(_Strict):
    """Reference to a BatchProfile registered via ConfigMap."""

    name: str = Field(
        description="Name of BatchProfile registered via ConfigMap; None means use system default.",
    )
    spec: Optional[Dict[str, Any]] = Field(
        default=None,
        description=(
            "Inline profile spec pushed by trusted callers so renderers can "
            "skip the local registry lookup. When set, consumers should "
            "prefer this over registry resolution."
        ),
    )
    overrides: Optional[Dict[str, Any]] = Field(
        default=None,
        description=(
            "Allowlisted overrides applied on top of the resolved profile "
            "spec at render time."
        ),
    )


class ResolvedModelTemplate(_Strict):
    name: str
    version: str
    status: str
    spec: Dict[str, Any] = Field(default_factory=dict)


class AibrixMetadata(_Strict):
    job_id: Optional[str] = None
    resource_allocation: Optional[ResourceAllocation] = None
    runtime: Optional[RuntimeSpec] = None
    model_template: Optional[ModelTemplateRef] = None
    profile: Optional[BatchProfileRef] = None
    model: Optional[str] = None
    client: Optional[ClientConfig] = None

    def to_metadata(self) -> "AibrixMetadata":
        return AibrixMetadata(**self.model_dump(exclude_none=True))

    @classmethod
    def from_extension_fields(
        cls,
        job_id: Optional[str] = None,
        model_template_name: Optional[str] = None,
        model_template_version: Optional[str] = None,
        profile_name: Optional[str] = None,
        template_overrides: Optional[Dict[str, Any]] = None,
        profile_overrides: Optional[Dict[str, Any]] = None,
        runtime_target: Optional[str] = None,
        runtime_options: Optional[Dict[str, Any]] = None,
        model: Optional[str] = None,
        client: Optional[ClientConfig] = None,
    ) -> Optional["AibrixMetadata"]:
        model_template = None
        if model_template_name:
            model_template = ModelTemplateRef(
                name=model_template_name,
                version=model_template_version,
                overrides=template_overrides,
            )

        profile = None
        if profile_name:
            profile = BatchProfileRef(
                name=profile_name,
                overrides=profile_overrides,
            )

        runtime = None
        if runtime_target:
            runtime = RuntimeSpec(target=runtime_target, options=runtime_options or {})

        if (
            job_id is None
            and model_template is None
            and profile is None
            and runtime is None
            and model is None
            and client is None
        ):
            return None

        return cls(
            job_id=job_id,
            runtime=runtime,
            model_template=model_template,
            profile=profile,
            model=model,
            client=client,
        )

    def to_extension_fields(self) -> Dict[str, Any]:
        return {
            "model": self.model,
            "client": self.client.model_dump(exclude_none=True)
            if self.client
            else None,
            "model_template_name": (
                self.model_template.name if self.model_template else None
            ),
            "model_template_version": (
                self.model_template.version if self.model_template else None
            ),
            "profile_name": self.profile.name if self.profile else None,
            "template_overrides": (
                self.model_template.overrides if self.model_template else None
            ),
            "profile_overrides": self.profile.overrides if self.profile else None,
            "runtime_target": self.runtime_target,
            "runtime_options": self.runtime.options if self.runtime else None,
        }

    @property
    def runtime_target(self) -> Optional[str]:
        """The Runtime selector; None routes to the default endpoint-source path."""
        return self.runtime.target if self.runtime else None

    @property
    def model_template_name(self) -> Optional[str]:
        """Name of ModelDeploymentTemplate to use. Required at render time."""
        return self.model_template.name if self.model_template else None

    @property
    def model_template_version(self) -> Optional[str]:
        """Optional version pin for the named template."""
        return self.model_template.version if self.model_template else None

    @property
    def profile_name(self) -> Optional[str]:
        """Name of BatchProfile to apply; None means use system default."""
        return self.profile.name if self.profile else None

    @property
    def template_overrides(self) -> Optional[Dict[str, Any]]:
        """User-supplied overrides applied on top of the resolved template."""
        return self.model_template.overrides if self.model_template else None

    @property
    def profile_overrides(self) -> Optional[Dict[str, Any]]:
        """User-supplied overrides applied on top of the resolved profile."""
        return self.profile.overrides if self.profile else None

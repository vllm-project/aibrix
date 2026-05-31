from enum import Enum
from typing import Any, Dict, List, Optional

from pydantic import Field

from aibrix.batch.job_entity.base import _Lenient, _Strict


class ComputeProvider(str, Enum):
    """Where a batch job's compute comes from — the selector the user (or
    Console / planner) sets under ``extra_body.aibrix.compute.provider``.

    Each value maps to a registered ``Runtime`` (see ``register_runtime``).
    ``ComputeSpec.provider`` is typed as ``str`` rather than this enum so a
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


class ComputeSpec(_Lenient):
    """Selects the Runtime a batch job runs on. Kept separate from
    ``model_template`` (engine startup) and from ``planner_decision``
    (resource reservation): this is purely *which provider*."""

    provider: str  # one of ComputeProvider; str keeps downstream providers valid


class ResourceDetail(_Lenient):
    endpoint_cluster: Optional[str] = None
    gpu_type: Optional[str] = None
    replica: Optional[int] = None


class PlannerDecision(_Lenient):
    provision_id: Optional[str] = None
    provision_resource_deadline: Optional[int] = None
    resource_details: Optional[List[ResourceDetail]] = None


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
    planner_decision: Optional[PlannerDecision] = None
    compute: Optional[ComputeSpec] = None
    model_template: Optional[ModelTemplateRef] = None
    profile: Optional[BatchProfileRef] = None

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
        compute_provider: Optional[str] = None,
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

        compute = ComputeSpec(provider=compute_provider) if compute_provider else None

        if (
            job_id is None
            and model_template is None
            and profile is None
            and compute is None
        ):
            return None

        return cls(
            job_id=job_id,
            compute=compute,
            model_template=model_template,
            profile=profile,
        )

    def to_extension_fields(self) -> Dict[str, Any]:
        return {
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
            "compute_provider": self.compute.provider if self.compute else None,
        }

    @property
    def compute_provider(self) -> Optional[str]:
        """The Runtime selector (one of ComputeProvider); None routes to the
        default/endpoint-source path."""
        return self.compute.provider if self.compute else None

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

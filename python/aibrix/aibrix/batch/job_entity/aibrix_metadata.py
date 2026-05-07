from typing import Any, Dict, List, Optional

from pydantic import Field

from .base import _Lenient, _Strict


class ResourceDetail(_Lenient):
    resource_type: str
    endpoint_cluster: Optional[str] = None
    gpu_type: Optional[str] = None
    worker_num: Optional[int] = None


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

        if job_id is None and model_template is None and profile is None:
            return None

        return cls(
            job_id=job_id,
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
        }

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

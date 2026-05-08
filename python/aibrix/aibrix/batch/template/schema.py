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

"""Typed schema for ModelDeploymentTemplate and BatchProfile.

These models define the contract for white-box batch configuration loaded
from ConfigMaps today; later releases may switch the source to CRDs or
a database without changing the model layout.

Field-level notes call out which values are actually honored at runtime
versus accepted-but-not-yet-active. Forward-compatible fields are
accepted by the schema so ConfigMaps don't need rewriting when more
behavior comes online; TemplateRegistry / ProfileRegistry emit warnings
when such fields carry non-default values.
"""

from enum import Enum
from typing import Dict, List, Optional

from pydantic import BaseModel, ConfigDict, Field, RootModel, model_validator

from aibrix.batch.job_entity import BatchJobEndpoint

# API version emitted in ConfigMap data so consumers can branch behavior
# if/when schema evolves.
SCHEMA_API_VERSION = "batch.aibrix.ai/v1alpha1"


class _Strict(BaseModel):
    """Base for strict schemas: forbids unknown fields, validates on assignment."""

    model_config = ConfigDict(extra="forbid", validate_assignment=True)


class _Lenient(BaseModel):
    """Base for lenient schemas: allows unknown fields (for engine_args extras)."""

    model_config = ConfigDict(extra="allow")


# ─────────────────────────────────────────────────────────────────────────────
# ModelDeploymentTemplate
# ─────────────────────────────────────────────────────────────────────────────


class EngineType(str, Enum):
    """Supported inference engines.

    Currently the renderer only ships a VLLM adapter; other types are
    accepted by the schema but rejected at render time until their
    adapters land.
    """

    VLLM = "vllm"
    SGLANG = "sglang"
    TRTLLM = "trtllm"
    LMDEPLOY = "lmdeploy"
    MOCK = "mock"


class EngineInvocation(str, Enum):
    """How the worker invokes the engine.

    Currently HTTP_SERVER is the only supported invocation mode.
    Embedded library invocation is explicitly not planned.
    """

    HTTP_SERVER = "http_server"


class EngineSpec(_Strict):
    """Configuration for the inference engine container.

    Today VLLM with HTTP_SERVER invocation is fully supported.
    Other engine types pass through the schema but require their own
    adapters to be implemented.
    """

    type: EngineType = Field(description="Inference engine implementation")
    version: str = Field(description="Engine version, e.g. '0.6.3'")
    image: str = Field(description="Container image with engine binary")
    invocation: EngineInvocation = Field(
        default=EngineInvocation.HTTP_SERVER,
        description="How the worker calls the engine. Only http_server is supported.",
    )
    serve_args: List[str] = Field(
        default_factory=list,
        description="Raw flags appended to engine startup command, e.g. ['--port=8000']",
    )
    health_endpoint: str = Field(
        default="",
        description=(
            "HTTP path checked for readiness. Empty (the default) lets the "
            "renderer pick the per-engine convention (vllm/sglang/trtllm/"
            "lmdeploy/mock all use /health). Override only when fronting the "
            "engine with a reverse proxy that rewrites the path."
        ),
    )
    ready_timeout_seconds: int = Field(
        default=600,
        ge=10,
        description="Max time to wait for engine /health to return 200",
    )
    metrics_endpoint: Optional[str] = Field(
        default=None,
        description="HTTP path for Prometheus metrics scraping (optional)",
    )


class ModelSourceType(str, Enum):
    """Where model weights come from."""

    HUGGINGFACE = "huggingface"
    S3 = "s3"
    TOS = "tos"
    LOCAL = "local"


class ModelSourceSpec(_Strict):
    """Where to fetch model weights and tokenizer."""

    type: ModelSourceType = Field(description="Source backend")
    uri: str = Field(description="URI or path to model artifacts")
    revision: Optional[str] = Field(
        default=None,
        description="Git revision / tag for HuggingFace; ignored for other sources",
    )
    tokenizer_path: Optional[str] = Field(
        default=None,
        description="Sub-path to tokenizer if separate from model weights",
    )
    chat_template_path: Optional[str] = Field(
        default=None,
        description="Sub-path or URI to chat template Jinja file",
    )
    auth_secret_ref: Optional[str] = Field(
        default=None,
        description="Name of K8s Secret holding download credentials (HF token, etc.)",
    )


class Interconnect(str, Enum):
    NVLINK = "nvlink"
    PCIE = "pcie"
    INFINIBAND = "ib"


class AcceleratorSpec(_Strict):
    """Hardware requirement for the engine."""

    type: str = Field(
        description=(
            "GPU SKU label, e.g. 'H100-SXM', 'A100-80G', 'MI300X'. "
            "Free-form. The exact literal 'cpu' (case-insensitive) is "
            "treated specially by the renderer to skip GPU resource "
            "limits — used for mock engines and CI runs without GPUs."
        ),
    )
    count: int = Field(ge=1, description="Number of GPUs per worker")
    interconnect: Optional[Interconnect] = Field(
        default=None, description="Inter-GPU interconnect type"
    )
    vram_gb: Optional[int] = Field(
        default=None, ge=1, description="Per-GPU memory in GB (informational)"
    )
    sku_hint: Optional[str] = Field(
        default=None,
        description="Provider-specific instance/SKU hint, e.g. 'aws/p5.48xlarge'",
    )


class ParallelismSpec(_Strict):
    """Engine parallelism strategy.

    The product tp * pp * dp * ep must equal accelerator.count.
    sp and cp are advisory hints used by some engines for additional
    sequence/context-level parallelism within the existing world size.
    """

    tp: int = Field(default=1, ge=1, description="Tensor parallel degree")
    pp: int = Field(default=1, ge=1, description="Pipeline parallel degree")
    dp: int = Field(default=1, ge=1, description="Data parallel degree")
    ep: int = Field(default=1, ge=1, description="Expert parallel degree (MoE)")
    sp: int = Field(default=1, ge=1, description="Sequence parallel degree")
    cp: int = Field(default=1, ge=1, description="Context parallel degree")

    @property
    def world_size(self) -> int:
        return self.tp * self.pp * self.dp * self.ep


class EngineArgsSpec(_Lenient):
    """Engine tuning flags.

    Common flags are typed for validation. Engine-specific flags pass
    through via the lenient model_config (extra='allow') and the renderer
    forwards them to the engine command line.
    """

    max_num_batched_tokens: Optional[int] = Field(default=None, ge=1)
    max_num_seqs: Optional[int] = Field(default=None, ge=1)
    max_model_len: Optional[int] = Field(default=None, ge=1)
    gpu_memory_utilization: Optional[float] = Field(default=None, gt=0.0, le=1.0)
    block_size: Optional[int] = Field(default=None, ge=1)
    swap_space: Optional[int] = Field(
        default=None, ge=0, description="CPU swap space in GB"
    )
    enable_prefix_caching: Optional[bool] = None
    enable_chunked_prefill: Optional[bool] = None
    speculative_model: Optional[str] = None
    num_speculative_tokens: Optional[int] = Field(default=None, ge=0)


class WeightQuantization(str, Enum):
    FP8 = "fp8"
    AWQ = "awq"
    GPTQ = "gptq"
    INT8 = "int8"
    BF16 = "bf16"
    FP16 = "fp16"


class KvCacheQuantization(str, Enum):
    AUTO = "auto"
    FP8 = "fp8"
    FP8_E4M3 = "fp8_e4m3"
    FP8_E5M2 = "fp8_e5m2"
    INT8 = "int8"


class QuantizationSpec(_Strict):
    """Quantization configuration."""

    weight: Optional[WeightQuantization] = Field(
        default=None, description="Weight quantization scheme"
    )
    kv_cache: Optional[KvCacheQuantization] = Field(
        default=None, description="KV cache quantization scheme"
    )
    weights_artifact_uri: Optional[str] = Field(
        default=None,
        description="Pre-quantized weights URI for AWQ/GPTQ",
    )


class DeploymentMode(str, Enum):
    """How the engine instance is materialized.

    Only DEDICATED is honored at runtime today. SHARED requires the
    client/server decoupling that is not yet implemented and is
    rejected by the renderer. EXTERNAL is functionally equivalent
    to SHARED but with the URL provided directly rather than resolved
    via the gateway.
    """

    DEDICATED = "dedicated"
    SHARED = "shared"
    EXTERNAL = "external"


class ModelDeploymentTemplateSpec(_Strict):
    """Full deployment template body.

    Required: engine, model_source, accelerator, parallelism,
    supported_endpoints.

    Cross-field invariant: parallelism.world_size == accelerator.count.
    """

    engine: EngineSpec
    model_source: ModelSourceSpec
    accelerator: AcceleratorSpec
    parallelism: ParallelismSpec = Field(default_factory=ParallelismSpec)
    engine_args: EngineArgsSpec = Field(default_factory=EngineArgsSpec)
    quantization: QuantizationSpec = Field(default_factory=QuantizationSpec)
    supported_endpoints: List[BatchJobEndpoint] = Field(
        min_length=1,
        description="OpenAI endpoints this deployment can serve",
    )
    deployment_mode: DeploymentMode = Field(
        default=DeploymentMode.DEDICATED,
        description="Only 'dedicated' is honored at runtime today.",
    )

    @model_validator(mode="after")
    def _validate_parallelism_matches_gpu_count(self) -> "ModelDeploymentTemplateSpec":
        ws = self.parallelism.world_size
        if ws != self.accelerator.count:
            raise ValueError(
                f"parallelism world_size (tp*pp*dp*ep={ws}) must equal "
                f"accelerator.count ({self.accelerator.count})"
            )
        return self


class TemplateStatus(str, Enum):
    ACTIVE = "active"
    DEPRECATED = "deprecated"
    DRAFT = "draft"


class ModelDeploymentTemplate(_Strict):
    """A single named, versioned deployment template.

    Identified by (name, version). Multiple versions of the same
    logical name may coexist; status controls which is selectable
    by users.
    """

    name: str = Field(min_length=1, max_length=128, description="Logical template name")
    version: str = Field(
        default="v1.0.0",
        description="SemVer-ish version. Multiple versions can coexist.",
    )
    status: TemplateStatus = Field(default=TemplateStatus.ACTIVE)
    spec: ModelDeploymentTemplateSpec


class ModelDeploymentTemplateList(RootModel[List[ModelDeploymentTemplate]]):
    """Templates ConfigMap data shape: a plain YAML top-level list.

    No wrapping apiVersion / kind envelope; the ConfigMap itself is the
    Kubernetes resource. Future migration to CRDs replaces this list
    with one CRD instance per item.
    """

    @model_validator(mode="after")
    def _validate_unique_names(self) -> "ModelDeploymentTemplateList":
        seen: Dict[str, str] = {}
        for it in self.root:
            key = f"{it.name}@{it.version}"
            if key in seen:
                raise ValueError(f"duplicate template '{key}'")
            seen[key] = it.name
        return self

    @property
    def items(self) -> List[ModelDeploymentTemplate]:
        """Backwards-compatible accessor; equivalent to .root."""
        return self.root


# ─────────────────────────────────────────────────────────────────────────────
# BatchProfile
# ─────────────────────────────────────────────────────────────────────────────


class StorageBackend(str, Enum):
    S3 = "s3"
    MINIO = "minio"
    GCS = "gcs"
    TOS = "tos"
    LOCAL = "local"


class StorageSpec(_Strict):
    """Where batch input/output/checkpoint files live."""

    backend: StorageBackend
    bucket: str = Field(min_length=1, description="Bucket / container name")
    region: Optional[str] = None
    credentials_secret_ref: Optional[str] = Field(
        default=None,
        description="Name of K8s Secret with backend credentials",
    )
    endpoint_url: Optional[str] = Field(
        default=None,
        description="Override service endpoint (MinIO / S3-compatible)",
    )


class MetastoreBackend(str, Enum):
    REDIS = "redis"
    LOCAL = "local"


class MetastoreSpec(_Strict):
    backend: MetastoreBackend = Field(default=MetastoreBackend.REDIS)
    credentials_secret_ref: Optional[str] = Field(
        default=None,
        description="Name of K8s Secret with metastore connection settings",
    )
    endpoint_url: Optional[str] = Field(
        default=None,
        description="Override metastore endpoint when secret does not provide one",
    )


class CompletionWindowOption(str, Enum):
    """SLO tier for batch completion.

    The schema accepts all values; only TWENTY_FOUR_HOURS is honored
    at runtime today. 1H/4H/BEST_EFFORT will take effect once the
    deadline-aware scheduler ships.
    """

    ONE_HOUR = "1h"
    FOUR_HOURS = "4h"
    TWENTY_FOUR_HOURS = "24h"
    BEST_EFFORT = "best_effort"


class Priority(str, Enum):
    HIGH = "high"
    NORMAL = "normal"
    LOW = "low"


class RetryPolicy(_Strict):
    """Per-request retry policy.

    Schema accepted; runtime retry is delegated to engine HTTP layer
    plus the smart client when that lands.
    """

    max_retries: int = Field(default=2, ge=0)
    backoff_seconds: List[int] = Field(default_factory=lambda: [5, 30])
    retryable_errors: List[int] = Field(
        default_factory=lambda: [429, 500, 502, 503, 504],
        description="HTTP status codes considered retryable",
    )


class SchedulingSpec(_Strict):
    """Scheduling policy for batches using this profile.

    Honored at runtime today: completion_window (only 24h),
    max_concurrency, request_timeout_seconds.

    Accepted by the schema but not yet honored (load-time warning):
    priority, provider_preference, allow_preempt, allow_spot,
    retry_policy.
    """

    completion_window: CompletionWindowOption = Field(
        default=CompletionWindowOption.TWENTY_FOUR_HOURS
    )
    priority: Priority = Field(default=Priority.NORMAL)
    provider_preference: List[str] = Field(
        default_factory=list,
        description="Ordered provider names; first available wins",
    )
    allow_preempt: bool = Field(default=False)
    allow_spot: bool = Field(default=False)
    max_concurrency: int = Field(default=256, ge=1)
    request_timeout_seconds: int = Field(default=600, ge=1)
    retry_policy: Optional[RetryPolicy] = None


class QuotaSpec(_Strict):
    """Per-batch hard limits enforced at validating phase."""

    max_requests_per_batch: int = Field(default=50000, ge=1)
    max_input_size_mb: int = Field(default=200, ge=1)
    max_output_size_gb: int = Field(default=10, ge=1)


class BatchProfileSpec(_Strict):
    """Full batch profile body.

    Note on naming: AIBrix BatchProfile is independent from OpenAI's
    ``service_tier``. ``service_tier`` is a field on OpenAI's synchronous
    endpoints (chat/completions, responses), not on the Batch object.
    Profiles are an AIBrix-native abstraction; admins may name profiles
    after tier-like terms (priority/flex/batch) for ergonomics, but
    there is no OpenAI compatibility being claimed at the field level.
    """

    storage: StorageSpec
    metastore: Optional[MetastoreSpec] = None
    scheduling: SchedulingSpec = Field(default_factory=SchedulingSpec)
    quota: QuotaSpec = Field(default_factory=QuotaSpec)


class BatchProfile(_Strict):
    """A single named batch profile."""

    name: str = Field(min_length=1, max_length=128)
    spec: BatchProfileSpec


class BatchProfileList(_Strict):
    """Profiles ConfigMap data shape.

    Unlike ModelDeploymentTemplateList, this needs a wrapper because of
    the sibling 'default' field. Shape:

        default: prod-24h           # optional
        items:                       # required
          - name: prod-24h
            spec: {...}

    Future CRD migration: items become CR instances; 'default' becomes
    a separate config (label on chosen profile or a small ConfigMap field).
    """

    default: Optional[str] = Field(
        default=None,
        description="Profile name applied when batch does not specify one",
    )
    items: List[BatchProfile] = Field(default_factory=list)

    @model_validator(mode="after")
    def _validate_unique_names_and_default(self) -> "BatchProfileList":
        names = set()
        for it in self.items:
            if it.name in names:
                raise ValueError(f"duplicate profile '{it.name}'")
            names.add(it.name)
        if self.default is not None and self.default not in names:
            raise ValueError(f"default profile '{self.default}' not present in items")
        return self


# ─────────────────────────────────────────────────────────────────────────────
# Per-batch overrides
#
# Each reference (model_template / profile) carries its OWN overrides
# namespace. This is the nested "B" contract documented in the console
# proto (apps/console/api/proto/console/v1/console.proto). A flat
# OverridesSpec with engine_args + scheduling siblings used to live here;
# it was ambiguous about which side each field overrode and is now split
# in two so the structure self-documents what gets patched.
# ─────────────────────────────────────────────────────────────────────────────


class TemplateOverridesSpec(_Strict):
    """User-supplied per-batch overrides applied on top of a ModelDeploymentTemplate.

    Allowlist today: ``engine_args`` only. Sensitive fields (image,
    accelerator type, parallelism, provider) are not user-overridable;
    administrators must change them via the template ConfigMap.

    Wire shape (under ``extra_body.aibrix.model_template.overrides``)::

        {"engine_args": {"max_num_seqs": 512}}

    Unknown keys are rejected at parse time (extra='forbid' via _Strict).
    """

    engine_args: Optional[EngineArgsSpec] = Field(
        default=None,
        description="Override engine tuning flags. Merged into template.engine_args.",
    )


class ProfileOverridesSpec(_Strict):
    """User-supplied per-batch overrides applied on top of a BatchProfile.

    Allowlist today: ``scheduling`` only. Storage, quota, and credentials
    references are profile-managed; users cannot relocate batches into
    different buckets or bypass quota at submission time.

    Wire shape (under ``extra_body.aibrix.profile.overrides``)::

        {"scheduling": {"max_concurrency": 32}}

    Today the renderer only honours the profile's completion_window and
    drops the rest with a warning at load time; the override field is
    accepted (and roundtripped) for forward compatibility with the
    deadline-aware scheduler.
    """

    scheduling: Optional[SchedulingSpec] = Field(
        default=None,
        description="Subset of SchedulingSpec fields to override on this batch",
    )


# ─────────────────────────────────────────────────────────────────────────────
# Resolved spec (template + profile + overrides combined)
# ─────────────────────────────────────────────────────────────────────────────


class ResolvedJobSpec(_Strict):
    """Final resolved configuration for a single batch.

    Produced by the resolver when a batch is created. Materializes
    the merge of (template + profile + overrides) so downstream
    consumers (renderer, scheduler, billing) operate on a single
    flat object instead of re-resolving every time.

    This is the contract between Metadata Service and the
    JobManifestRenderer. Stored in metadata.json._aibrix.resolved_spec
    for crash recovery and audit.
    """

    template_name: str
    template_version: str
    profile_name: str
    template_spec: ModelDeploymentTemplateSpec
    profile_spec: BatchProfileSpec
    applied_template_overrides: Optional[TemplateOverridesSpec] = None
    applied_profile_overrides: Optional[ProfileOverridesSpec] = None

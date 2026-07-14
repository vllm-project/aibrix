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

from datetime import datetime
from typing import Dict, List, Optional

from pydantic import BaseModel, ConfigDict, Field


class NoExtraBaseModel(BaseModel):
    # The class does not allow extra fields
    model_config = ConfigDict(extra="forbid")


class NoProtectedBaseModel(BaseModel):
    # The class does not allow extra fields
    model_config = ConfigDict(extra="forbid", protected_namespaces=())


class ErrorResponse(NoExtraBaseModel):
    object: str = "error"
    message: str
    type: str
    param: Optional[str] = None
    code: int


class LoadLoraAdapterRequest(NoExtraBaseModel):
    lora_name: str
    lora_path: str


class UnloadLoraAdapterRequest(NoExtraBaseModel):
    lora_name: str
    lora_int_id: Optional[int] = Field(default=None)


class DownloadModelRequest(NoProtectedBaseModel):
    model_uri: str
    local_dir: Optional[str] = None
    model_name: Optional[str] = None
    download_extra_config: Optional[Dict] = None


class ModelStatusCard(NoProtectedBaseModel):
    model_name: str
    model_root_path: str
    source: str
    model_status: str


class ListModelRequest(NoExtraBaseModel):
    local_dir: str


class ListModelResponse(NoExtraBaseModel):
    object: str = "list"
    data: List[ModelStatusCard] = Field(default_factory=list)


# Runtime API Protocol for Artifact Delegation


class LoadLoraAdapterRuntimeRequest(NoExtraBaseModel):
    """Request to load LoRA adapter with artifact delegation via runtime."""

    lora_name: str
    artifact_url: str  # Original URL (s3://, gs://, huggingface://, etc.)
    credentials_secret: Optional[str] = Field(
        default=None, description="Kubernetes secret name containing credentials"
    )
    credentials: Optional[Dict[str, str]] = Field(
        default=None, description="Direct credentials for artifact download"
    )
    additional_config: Optional[Dict[str, str]] = Field(
        default=None, description="Additional configuration for artifact download"
    )
    local_dir: Optional[str] = Field(
        default="/tmp/aibrix/adapters",
        description="Local directory for downloaded artifacts",
    )


class LoadLoraAdapterRuntimeResponse(NoExtraBaseModel):
    """Response from runtime after loading adapter."""

    status: str  # "success" or "error"
    message: str
    local_path: Optional[str] = Field(
        default=None, description="Local path where artifact was downloaded"
    )
    engine_response: Optional[Dict] = Field(
        default=None, description="Response from inference engine"
    )


class UnloadLoraAdapterRuntimeRequest(NoExtraBaseModel):
    """Request to unload LoRA adapter with optional cleanup via runtime."""

    lora_name: str
    cleanup_local: bool = Field(
        default=True, description="Whether to delete local artifact files"
    )


# --------------------------------------------------------------------------- #
# Runtime model lifecycle protocol (engine <-> control-plane co-design)
# --------------------------------------------------------------------------- #
class RuntimeEngineConfig(NoExtraBaseModel):
    """Engine-specific startup options."""

    args: Optional[Dict[str, str]] = Field(
        default=None,
        description="Engine CLI flags keyed by flag name, e.g. {'--max-model-len': '2048'}",
    )


class RuntimeClaimRef(NoExtraBaseModel):
    """Stable ModelClaim identity carried into a runtime instance."""

    namespace: str
    name: str
    uid: str


class ActivateRuntimeModelRequest(NoProtectedBaseModel):
    """Bring a model online as its own kvcached-enabled engine process."""

    model_name: str
    artifact_url: str
    engine: str = "vllm"
    port: int = Field(default=0, description="0 lets the agent pick a free port")
    ipc_name: Optional[str] = Field(
        default=None,
        description="kvcached KVCACHED_IPC_NAME; agent derives one if empty",
    )
    credentials: Optional[Dict[str, str]] = None
    engine_config: Optional[RuntimeEngineConfig] = None
    claim_ref: Optional[RuntimeClaimRef] = None
    # Controller-derived vLLM envelope, not a user-facing ModelClaim resource.
    # The sidecar reports it in snapshots so a controller restart can rebuild
    # the active per-pod capacity ledger.
    hbm_reservation_fraction: Optional[float] = Field(default=None, gt=0, le=1)
    # Legacy compatibility for older callers. New ModelClaim callers should use
    # engine_config.args for engine CLI flags.
    additional_config: Optional[Dict[str, str]] = None


class ActivateRuntimeModelResponse(NoProtectedBaseModel):
    status: str  # "success" | "error"
    model_name: str
    port: int = 0
    ipc_name: str = ""
    message: Optional[str] = None


class DeactivateRuntimeModelRequest(NoProtectedBaseModel):
    """Tear a model down by stopping its engine process."""

    model_name: str
    mode: str = "stop"


class SetRuntimeModelKVLimitRequest(NoProtectedBaseModel):
    """Controller-only request to set one model's kvcached limit."""

    model_name: str
    limit_bytes: int = Field(ge=0)
    operation_id: str = Field(min_length=1)


class SleepRuntimeModelRequest(NoProtectedBaseModel):
    """Controller-only request to put a vLLM engine to sleep."""

    model_name: str
    level: int = Field(ge=1, le=2)
    operation_id: str = Field(min_length=1)


class WakeRuntimeModelRequest(NoProtectedBaseModel):
    """Controller-only request to wake a vLLM engine."""

    model_name: str
    operation_id: str = Field(min_length=1)


class RuntimeOperationResponse(NoProtectedBaseModel):
    """Result of an idempotent runtime control operation."""

    status: str = "success"
    model_name: str
    operation_id: str
    applied: bool
    phase: str


class RuntimeModelInfo(NoProtectedBaseModel):
    model_name: str
    port: int
    ipc_name: str
    phase: str
    # Whether the engine can serve right now (a /health probe). The controller
    # gates routability on this: a model's warm-pod annotation stays at the
    # non-routable marker (port 0) until ready, so requests never hit a
    # still-booting engine.
    ready: bool = False
    # KV accounting comes from the model's kvcached /dev/shm MemInfoStruct and
    # is zero when the segment is absent (mock engine, or engine still starting).
    kv_used_bytes: int = 0
    kv_total_bytes: int = 0


class ListRuntimeModelsResponse(NoProtectedBaseModel):
    models: List[RuntimeModelInfo]


class RuntimeAcceleratorSnapshot(NoProtectedBaseModel):
    """Live memory observation for one GPU visible to the runtime pod."""

    id: str
    hbm_total_bytes: int
    hbm_free_bytes: int


class RuntimeSnapshotModel(NoProtectedBaseModel):
    """One runtime-managed engine in a point-in-time sidecar snapshot."""

    model_name: str
    artifact_url: str
    claim_ref: Optional[RuntimeClaimRef] = None
    port: int
    ipc_name: str
    phase: str
    ready: bool
    # Process liveness is independent from readiness: a booting engine is
    # alive but non-routable, while a restart/terminal failure is not alive.
    alive: bool = True
    restart_count: int = 0
    last_error: Optional[str] = None
    last_transition: Optional[datetime] = None
    kv_used_bytes: int
    kv_capacity_bytes: int
    # Largest amount of GPU memory attributable to this engine on any visible
    # accelerator. This folds vLLM's worker process tree into the engine it
    # belongs to, which makes it safe for fixed TP/PP topology admission.
    hbm_peak_bytes: int = 0
    # The controller-derived envelope carried at activation time. A zero value
    # means an older or non-vLLM caller did not provide a capacity reservation.
    hbm_reservation_fraction: float = 0
    # Per-engine request activity is read by the sidecar from localhost. False
    # means policy must not infer that an engine is idle from unavailable metrics.
    request_metrics_observed: bool = False
    requests_running: int = 0
    requests_waiting: int = 0
    request_success_total: Optional[int] = None


class RuntimeSnapshotResponse(NoProtectedBaseModel):
    """Runtime state used by the ModelClaim controller's placement cache."""

    observed_at: datetime
    accelerators: List[RuntimeAcceleratorSnapshot]
    models: List[RuntimeSnapshotModel]
    cached_artifacts: List[str]

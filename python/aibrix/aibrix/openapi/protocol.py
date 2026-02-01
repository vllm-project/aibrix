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

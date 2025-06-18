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

from typing import Dict, List, Literal, Optional, Union

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


class EmbeddingRequest(NoExtraBaseModel):
    input: Union[str, List[str], List[int], List[List[int]]]
    model: str
    encoding_format: Optional[Literal["float", "base64"]] = "float"
    dimensions: Optional[int] = None
    user: Optional[str] = None


class EmbeddingData(NoExtraBaseModel):
    object: Literal["embedding"] = "embedding"
    embedding: Union[List[float], str]  # float array or base64 string
    index: int


class EmbeddingUsage(NoExtraBaseModel):
    prompt_tokens: int
    total_tokens: int


class EmbeddingResponse(NoExtraBaseModel):
    object: Literal["list"] = "list"
    data: List[EmbeddingData]
    model: str
    usage: EmbeddingUsage

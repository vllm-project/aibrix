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

from typing import Dict, List, Optional, Union

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

class MultiModalityRequest(NoExtraBaseModel):
    json_body: str

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


class CompletionRequest(NoExtraBaseModel):
    # Ordered by official OpenAI API documentation
    # https://platform.openai.com/docs/api-reference/completions/create
    model: Optional[str] = None
    prompt: Optional[Union[list[int], list[list[int]], str, list[str]]] = None
    best_of: Optional[int] = None
    echo: Optional[bool] = False
    frequency_penalty: Optional[float] = 0.0
    logit_bias: Optional[dict[str, float]] = None
    logprobs: Optional[int] = None
    max_tokens: Optional[int] = 16
    n: int = 1
    presence_penalty: Optional[float] = 0.0
    seed: Optional[int] = None
    stop: Optional[Union[str, list[str]]] = []
    stream: Optional[bool] = False
    suffix: Optional[str] = None
    temperature: Optional[float] = None
    top_p: Optional[float] = None
    user: Optional[str] = None


class ChatCompletionMessage(NoExtraBaseModel):
    # https://platform.openai.com/docs/api-reference/chat/create
    role: str
    content: Optional[Union[str, List[str]]] = None
    name: Optional[str] = None


class ChatCompletionRequest(NoExtraBaseModel):
    # Ordered by official OpenAI API documentation
    # https://platform.openai.com/docs/api-reference/chat/create
    model: Optional[str] = None
    messages: Optional[List[ChatCompletionMessage]] = None
    temperature: Optional[float] = 1.0
    top_p: Optional[float] = 1.0
    n: Optional[int] = 1
    stream: Optional[bool] = False
    stop: Optional[Union[str, List[str]]] = None
    max_tokens: Optional[int] = None
    presence_penalty: Optional[float] = 0.0
    frequency_penalty: Optional[float] = 0.0
    logit_bias: Optional[Dict[str, float]] = None
    user: Optional[str] = None

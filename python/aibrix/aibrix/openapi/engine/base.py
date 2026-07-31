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

from abc import ABC
from dataclasses import dataclass, field
from http import HTTPStatus
from typing import Optional, Union

from packaging.version import Version

from aibrix.openapi.protocol import (
    ErrorResponse,
    LoadLoraAdapterRequest,
    UnloadLoraAdapterRequest,
)


@dataclass
class EngineEndpointConfig:
    """Configuration for engine API endpoints."""

    load_lora_path: str = "/v1/load_lora_adapter"
    unload_lora_path: str = "/v1/unload_lora_adapter"
    list_models_path: str = "/v1/models"


@dataclass
class InferenceEngine(ABC):
    """Base class for Inference Engine."""

    name: str
    version: str
    endpoint: str
    headers: Optional[dict] = None
    endpoint_config: EngineEndpointConfig = field(default_factory=EngineEndpointConfig)

    def _create_error_response(
        self,
        message: str,
        err_type: str = "BadRequestError",
        status_code: HTTPStatus = HTTPStatus.BAD_REQUEST,
    ) -> ErrorResponse:
        return ErrorResponse(message=message, type=err_type, code=status_code.value)

    async def load_lora_adapter(
        self, request: LoadLoraAdapterRequest
    ) -> Union[ErrorResponse, str]:
        return self._create_error_response(
            f"Inference engine {self.name} with version {self.version} "
            "not support load lora adapter",
            err_type="NotImplementedError",
            status_code=HTTPStatus.NOT_IMPLEMENTED,
        )

    async def unload_lora_adapter(
        self, request: UnloadLoraAdapterRequest
    ) -> Union[ErrorResponse, str]:
        return self._create_error_response(
            f"Inference engine {self.name} with version {self.version} "
            "not support unload lora adapter",
            err_type="NotImplementedError",
            status_code=HTTPStatus.NOT_IMPLEMENTED,
        )

    async def list_models(self) -> Union[ErrorResponse, str]:
        return self._create_error_response(
            f"Inference engine {self.name} with version {self.version} "
            "not support list models",
            err_type="NotImplementedError",
            status_code=HTTPStatus.NOT_IMPLEMENTED,
        )


def get_inference_engine(
    engine: str, version: str, endpoint: str, api_key: Optional[str] = None
) -> InferenceEngine:
    """
    Factory function to get the appropriate inference engine implementation.

    Args:
        engine: Engine name (vllm, sglang, etc.)
        version: Engine version
        endpoint: Engine endpoint URL
        api_key: Optional API key for authentication

    Returns:
        InferenceEngine instance for the specified engine type
    """
    engine_lower = engine.lower()

    if engine_lower == "vllm":
        # vLLM supports lora dynamic loading & unloading from v0.6.1
        if Version(version) < Version("0.6.1"):
            return InferenceEngine(engine, version, endpoint)
        else:
            from aibrix.openapi.engine.vllm import VLLMInferenceEngine

            return VLLMInferenceEngine(engine, version, endpoint, api_key=api_key)

    # TODO: support SGLang later

    else:
        raise ValueError(
            f"Engine {engine} with version {version} is not supported. "
            f"Supported engines: vllm"
        )

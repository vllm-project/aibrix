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

from http import HTTPStatus
from typing import Optional, Union
from urllib.parse import urljoin

import httpx

from aibrix.logger import init_logger
from aibrix.openapi.engine.base import InferenceEngine
from aibrix.openapi.protocol import (
    ErrorResponse,
    LoadLoraAdapterRequest,
    UnloadLoraAdapterRequest,
)

logger = init_logger(__name__)


class VLLMInferenceEngine(InferenceEngine):
    def __init__(
        self, name: str, version: str, endpoint: str, api_key: Optional[str] = None
    ):
        if api_key is not None:
            headers = {"Authorization": f"Bearer {api_key}"}
        else:
            headers = {}
        self.client = httpx.AsyncClient(headers=headers)
        super().__init__(name, version, endpoint)

    async def load_lora_adapter(
        self, request: LoadLoraAdapterRequest
    ) -> Union[ErrorResponse, str]:
        load_url = urljoin(self.endpoint, "/v1/load_lora_adapter")
        lora_name, lora_path = request.lora_name, request.lora_path

        try:
            response = await self.client.post(load_url, json=request.model_dump())
        except Exception as e:
            logger.error(
                f"Failed to load LoRA adapter '{lora_name}' from `{lora_path}` "
                f"for httpx request failed, {e}"
            )
            return self._create_error_response(
                f"Failed to load LoRA adapter '{lora_name}'",
                err_type="ServerError",
                status_code=HTTPStatus.INTERNAL_SERVER_ERROR,
            )

        if response.status_code != 200:
            logger.error(
                f"Failed to load LoRA adapter '{lora_name}' from `{lora_path}` "
                f"with error: {response.text}"
            )
            return self._create_error_response(
                f"Failed to load LoRA adapter '{lora_name}'",
                err_type="ServerError",
                status_code=HTTPStatus.INTERNAL_SERVER_ERROR,
            )
        return f"Success: LoRA adapter '{lora_name}' added successfully."

    async def unload_lora_adapter(
        self, request: UnloadLoraAdapterRequest
    ) -> Union[ErrorResponse, str]:
        unload_url = urljoin(self.endpoint, "/v1/unload_lora_adapter")
        lora_name, lora_int_id = request.lora_name, request.lora_int_id

        try:
            response = await self.client.post(unload_url, json=request.model_dump())
        except Exception as e:
            logger.error(
                f"Failed to remove LoRA adapter '{lora_name}' by id `{lora_int_id}`"
                f"for httpx request failed, {e}"
            )
            return self._create_error_response(
                f"Failed to unload LoRA adapter '{lora_name}'",
                err_type="ServerError",
                status_code=HTTPStatus.INTERNAL_SERVER_ERROR,
            )
        if response.status_code != 200:
            logger.error(
                f"Failed to remove LoRA adapter '{lora_name}' by id `{lora_int_id}`"
                f"with error: {response.text}"
            )
            return self._create_error_response(
                f"Failed to unload LoRA adapter '{lora_name}'",
                err_type="ServerError",
                status_code=HTTPStatus.INTERNAL_SERVER_ERROR,
            )

        return f"Success: LoRA adapter '{lora_name}' removed successfully."

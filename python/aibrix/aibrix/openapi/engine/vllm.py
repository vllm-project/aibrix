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
from tenacity import (
    retry,
    retry_if_exception_type,
    stop_after_attempt,
    wait_exponential,
)

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

        # Robust HTTP client configuration
        timeout = httpx.Timeout(
            connect=10.0,  # Connection timeout
            read=30.0,  # Read timeout for LoRA operations
            write=10.0,  # Write timeout
            pool=5.0,  # Pool timeout
        )

        limits = httpx.Limits(
            max_keepalive_connections=5, max_connections=10, keepalive_expiry=30.0
        )

        self.client = httpx.AsyncClient(
            headers=headers, timeout=timeout, limits=limits, follow_redirects=True
        )
        super().__init__(name, version, endpoint)

    def _validate_lora_request(
        self, lora_name: str, lora_path: Optional[str] = None
    ) -> Optional[str]:
        """Validate LoRA request parameters."""
        if not lora_name or not lora_name.strip():
            return "LoRA name cannot be empty"
        if len(lora_name) > 100:
            return "LoRA name too long (max 100 characters)"
        if not lora_name.replace("-", "").replace("_", "").isalnum():
            return "LoRA name must contain only alphanumeric characters, hyphens, and underscores"

        if lora_path is not None and (not lora_path or not lora_path.strip()):
            return "LoRA path cannot be empty"

        return None

    @retry(
        stop=stop_after_attempt(3),
        wait=wait_exponential(multiplier=1, min=1, max=10),
        retry=retry_if_exception_type(
            (
                httpx.RequestError,
                httpx.TimeoutException,
                httpx.ConnectError,
                httpx.HTTPStatusError,
            )
        ),
    )
    async def _make_request(self, method: str, url: str, **kwargs) -> httpx.Response:
        """Make HTTP request with retry logic."""
        if method.lower() == "post":
            response = await self.client.post(url, **kwargs)
        elif method.lower() == "get":
            response = await self.client.get(url, **kwargs)
        else:
            raise ValueError(f"Unsupported HTTP method: {method}")

        # Raise for HTTP error status codes to trigger retry
        if response.status_code >= 500:
            response.raise_for_status()

        return response

    async def load_lora_adapter(
        self, request: LoadLoraAdapterRequest
    ) -> Union[ErrorResponse, str]:
        load_url = urljoin(self.endpoint, "/v1/load_lora_adapter")
        lora_name, lora_path = request.lora_name, request.lora_path

        # Validate request parameters
        validation_error = self._validate_lora_request(lora_name, lora_path)
        if validation_error:
            logger.error(f"LoRA load validation failed: {validation_error}")
            return self._create_error_response(
                validation_error,
                err_type="ValidationError",
                status_code=HTTPStatus.BAD_REQUEST,
            )

        try:
            response = await self._make_request(
                "post", load_url, json=request.model_dump()
            )
        except httpx.TimeoutException as e:
            logger.error(
                f"Timeout loading LoRA adapter '{lora_name}' from '{lora_path}': {e}"
            )
            return self._create_error_response(
                f"Timeout loading LoRA adapter '{lora_name}' - operation took too long",
                err_type="TimeoutError",
                status_code=HTTPStatus.REQUEST_TIMEOUT,
            )
        except httpx.ConnectError as e:
            logger.error(f"Connection error loading LoRA adapter '{lora_name}': {e}")
            return self._create_error_response(
                f"Cannot connect to inference engine for LoRA adapter '{lora_name}'",
                err_type="ConnectionError",
                status_code=HTTPStatus.BAD_GATEWAY,
            )
        except httpx.RequestError as e:
            logger.error(
                f"Request error loading LoRA adapter '{lora_name}' from '{lora_path}': {e}"
            )
            return self._create_error_response(
                f"Request failed for LoRA adapter '{lora_name}'",
                err_type="RequestError",
                status_code=HTTPStatus.INTERNAL_SERVER_ERROR,
            )
        except Exception as e:
            logger.error(
                f"Unexpected error loading LoRA adapter '{lora_name}' from '{lora_path}': {e}"
            )
            return self._create_error_response(
                f"Unexpected error loading LoRA adapter '{lora_name}'",
                err_type="ServerError",
                status_code=HTTPStatus.INTERNAL_SERVER_ERROR,
            )

        if response.status_code != HTTPStatus.OK:
            error_detail = ""
            try:
                error_detail = response.text
            except Exception:
                error_detail = "Unable to read error details"

            logger.error(
                f"Failed to load LoRA adapter '{lora_name}' from '{lora_path}' - "
                f"Status: {response.status_code}, Error: {error_detail}"
            )

            if response.status_code == 409:
                return self._create_error_response(
                    f"LoRA adapter '{lora_name}' already exists",
                    err_type="ConflictError",
                    status_code=HTTPStatus.CONFLICT,
                )
            elif response.status_code == 404:
                return self._create_error_response(
                    f"LoRA path '{lora_path}' not found",
                    err_type="NotFoundError",
                    status_code=HTTPStatus.NOT_FOUND,
                )
            else:
                return self._create_error_response(
                    f"Failed to load LoRA adapter '{lora_name}': {error_detail}",
                    err_type="ServerError",
                    status_code=HTTPStatus(value=response.status_code),
                )

        logger.info(
            f"Successfully loaded LoRA adapter '{lora_name}' from '{lora_path}'"
        )
        return f"Success: LoRA adapter '{lora_name}' loaded successfully from '{lora_path}'."

    async def unload_lora_adapter(
        self, request: UnloadLoraAdapterRequest
    ) -> Union[ErrorResponse, str]:
        unload_url = urljoin(self.endpoint, "/v1/unload_lora_adapter")
        lora_name = request.lora_name

        # Validate request parameters
        validation_error = self._validate_lora_request(lora_name)
        if validation_error:
            logger.error(f"LoRA unload validation failed: {validation_error}")
            return self._create_error_response(
                validation_error,
                err_type="ValidationError",
                status_code=HTTPStatus.BAD_REQUEST,
            )

        try:
            response = await self._make_request(
                "post", unload_url, json=request.model_dump()
            )
        except httpx.TimeoutException as e:
            logger.error(f"Timeout unloading LoRA adapter '{lora_name}': {e}")
            return self._create_error_response(
                f"Timeout unloading LoRA adapter '{lora_name}' - operation took too long",
                err_type="TimeoutError",
                status_code=HTTPStatus.REQUEST_TIMEOUT,
            )
        except httpx.ConnectError as e:
            logger.error(f"Connection error unloading LoRA adapter '{lora_name}': {e}")
            return self._create_error_response(
                f"Cannot connect to inference engine for LoRA adapter '{lora_name}'",
                err_type="ConnectionError",
                status_code=HTTPStatus.BAD_GATEWAY,
            )
        except httpx.RequestError as e:
            logger.error(f"Request error unloading LoRA adapter '{lora_name}': {e}")
            return self._create_error_response(
                f"Request failed for LoRA adapter '{lora_name}'",
                err_type="RequestError",
                status_code=HTTPStatus.INTERNAL_SERVER_ERROR,
            )
        except Exception as e:
            logger.error(f"Unexpected error unloading LoRA adapter '{lora_name}': {e}")
            return self._create_error_response(
                f"Unexpected error unloading LoRA adapter '{lora_name}'",
                err_type="ServerError",
                status_code=HTTPStatus.INTERNAL_SERVER_ERROR,
            )

        if response.status_code != HTTPStatus.OK:
            error_detail = ""
            try:
                error_detail = response.text
            except Exception:
                error_detail = "Unable to read error details"

            logger.error(
                f"Failed to unload LoRA adapter '{lora_name}' - "
                f"Status: {response.status_code}, Error: {error_detail}"
            )

            if response.status_code == 404:
                return self._create_error_response(
                    f"LoRA adapter '{lora_name}' not found",
                    err_type="NotFoundError",
                    status_code=HTTPStatus.NOT_FOUND,
                )
            else:
                return self._create_error_response(
                    f"Failed to unload LoRA adapter '{lora_name}': {error_detail}",
                    err_type="ServerError",
                    status_code=HTTPStatus(value=response.status_code),
                )

        logger.info(f"Successfully unloaded LoRA adapter '{lora_name}'")
        return f"Success: LoRA adapter '{lora_name}' unloaded successfully."

    async def list_models(self) -> Union[ErrorResponse, str]:
        model_list_url = urljoin(self.endpoint, "/v1/models")

        try:
            response = await self._make_request("get", model_list_url)
        except httpx.TimeoutException as e:
            logger.error(f"Timeout listing models: {e}")
            return self._create_error_response(
                "Timeout listing models - operation took too long",
                err_type="TimeoutError",
                status_code=HTTPStatus.REQUEST_TIMEOUT,
            )
        except httpx.ConnectError as e:
            logger.error(f"Connection error listing models: {e}")
            return self._create_error_response(
                "Cannot connect to inference engine",
                err_type="ConnectionError",
                status_code=HTTPStatus.BAD_GATEWAY,
            )
        except httpx.RequestError as e:
            logger.error(f"Request error listing models: {e}")
            return self._create_error_response(
                "Request failed for listing models",
                err_type="RequestError",
                status_code=HTTPStatus.INTERNAL_SERVER_ERROR,
            )
        except Exception as e:
            logger.error(f"Unexpected error listing models: {e}")
            return self._create_error_response(
                "Unexpected error listing models",
                err_type="ServerError",
                status_code=HTTPStatus.INTERNAL_SERVER_ERROR,
            )

        if response.status_code != HTTPStatus.OK:
            error_detail = ""
            try:
                error_detail = response.text
            except Exception:
                error_detail = "Unable to read error details"

            logger.error(
                f"Failed to list models - Status: {response.status_code}, Error: {error_detail}"
            )
            return self._create_error_response(
                f"Failed to list models: {error_detail}",
                err_type="ServerError",
                status_code=HTTPStatus(value=response.status_code),
            )

        try:
            return response.json()
        except Exception as e:
            logger.error(f"Failed to parse models response as JSON: {e}")
            return self._create_error_response(
                "Failed to parse models response",
                err_type="ServerError",
                status_code=HTTPStatus.INTERNAL_SERVER_ERROR,
            )

    async def __aenter__(self):
        return self

    async def __aexit__(self, exc_type, exc_val, exc_tb):
        await self.client.aclose()

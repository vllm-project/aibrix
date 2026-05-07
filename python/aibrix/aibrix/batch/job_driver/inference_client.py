import asyncio
from urllib.parse import urljoin

import httpx

import aibrix.batch.constant as constant
from aibrix.logger import init_logger

logger = init_logger(__name__)


class InferenceEngineClient:
    """Abstract base for inference clients used by ``JobDriver``."""

    async def inference_request(self, endpoint: str, request_data):
        raise NotImplementedError(
            "InferenceEngineClient is abstract; instantiate "
            "ProxyInferenceEngineClient (production) or "
            "EchoInferenceEngineClient (--dry-run) instead."
        )


class EchoInferenceEngineClient(InferenceEngineClient):
    """Returns the request body verbatim. Only valid under --dry-run."""

    async def inference_request(self, endpoint: str, request_data):
        await asyncio.sleep(constant.EXPIRE_INTERVAL)
        return request_data


class ProxyInferenceEngineClient(InferenceEngineClient):
    def __init__(self, base_url: str):
        """
        Initiate client to inference engine.
        """
        self.base_url = base_url

    async def inference_request(self, endpoint: str, request_data):
        """Real inference request to LLM engine."""
        url = urljoin(self.base_url, endpoint)

        logger.debug("requesting inference", url=url, body=request_data)  # type: ignore[call-arg]

        async with httpx.AsyncClient() as client:
            response = await client.post(url, json=request_data, timeout=30.0)
            response.raise_for_status()
            return response.json()

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

    async def get_worker_capacity(self) -> tuple[int, int]:
        """Get worker capacity and version number of LLM engine worker."""
        return 1, 0

    async def wait_for_worker_capacity_change(
        self,
        previous_max_workers: int,
        previous_version: int | None = None,
    ) -> tuple[int, int]:
        """Wait for worker capacity change. If previous_version is not None, wait for version to change."""
        await asyncio.Future()
        raise AssertionError("unreachable")


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

import asyncio
from typing import Any
from urllib.parse import urljoin

import httpx

import aibrix.batch.constant as constant
from aibrix.batch.job_entity import BatchJobError, BatchJobErrorCode
from aibrix.context import ModelDiscovery, ModelEndpoint
from aibrix.logger import init_logger

logger = init_logger(__name__)


class InferenceEngineClient:
    """Abstract base for inference clients used by ``JobDriver``."""

    async def inference_request(self, endpoint: str, request_data) -> Any:
        """Send inference request to LLM engine."""
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
        return await _post_inference_request(self.base_url, endpoint, request_data)


async def _post_inference_request(base_url: str, endpoint: str, request_data):
    url = urljoin(base_url, endpoint)

    logger.debug("requesting inference", url=url, body=request_data)  # type: ignore[call-arg]

    async with httpx.AsyncClient() as client:
        response = await client.post(url, json=request_data, timeout=30.0)
        response.raise_for_status()
        return response.json()


# --- Discovery-based federated routing across endpoints ---
class FederalInferenceEngineClient(InferenceEngineClient):
    """Routes requests across discovered endpoints while avoiding busy candidates."""

    def __init__(
        self,
        model_discovery: ModelDiscovery,
        served_model_name: str,
        service_id: str | None = None,
    ) -> None:
        self._model_discovery = model_discovery
        self.served_model_name = served_model_name
        self.service_id = service_id
        self._cached_endpoints: list[ModelEndpoint] = []
        self._cached_snapshot_version: int | None = None
        self._next_endpoint_index = 0
        self._busy_base_urls: set[str] = set()
        self._state_lock = asyncio.Lock()
        self._state_condition = asyncio.Condition(self._state_lock)

    async def inference_request(self, endpoint: str, request_data):
        """Send one request to one idle endpoint and fail over only after errors."""
        errors: list[str] = []
        attempted_base_urls: set[str] = set()
        while True:
            candidate = await self._claim_candidate(
                attempted_base_urls=attempted_base_urls,
            )
            if candidate is None:
                break
            attempted_base_urls.add(candidate.base_url)
            try:
                return await _post_inference_request(
                    candidate.base_url,
                    endpoint,
                    request_data,
                )
            except httpx.HTTPError as ex:
                errors.append(f"{candidate.base_url}: {ex}")
            finally:
                await self._release_candidate(candidate.base_url)
        if errors:
            raise BatchJobError(
                code=BatchJobErrorCode.INFERENCE_FAILED,
                message="All federal inference endpoints failed: " + "; ".join(errors),
            )
        raise BatchJobError(
            code=BatchJobErrorCode.INFERENCE_FAILED,
            message=f"No discovery endpoint found for model '{self.served_model_name}'",
        )

    async def _discover_endpoints(self) -> list[ModelEndpoint]:
        """Refresh the local cache only when discovery reports a newer snapshot."""
        snapshot = await self._model_discovery.discover_model_endpoints(
            served_model_name=self.served_model_name,
            service_id=self.service_id,
        )
        async with self._state_condition:
            if snapshot.version != self._cached_snapshot_version:
                self._cached_endpoints = snapshot.endpoints
                self._cached_snapshot_version = snapshot.version
                self._state_condition.notify_all()
            return list(self._cached_endpoints)

    async def get_worker_capacity(self) -> tuple[int, int]:
        await self._discover_endpoints()
        async with self._state_condition:
            return len(self._cached_endpoints), self._cached_snapshot_version or 0

    async def wait_for_worker_capacity_change(
        self,
        previous_max_workers: int,
        previous_version: int | None = None,
    ) -> tuple[int, int]:
        async with self._state_condition:
            await self._state_condition.wait_for(
                lambda: (
                    (self._cached_snapshot_version or 0) != previous_version
                    if previous_version is not None
                    else len(self._cached_endpoints) != previous_max_workers
                )
            )
            return len(self._cached_endpoints), self._cached_snapshot_version or 0

    async def _claim_candidate(
        self,
        attempted_base_urls: set[str],
    ) -> ModelEndpoint | None:
        """Claim the next idle endpoint or wait until one is released."""
        while True:
            candidates = await self._discover_endpoints()
            async with self._state_condition:
                candidate = self._next_claimable_candidate_locked(
                    candidates,
                    attempted_base_urls=attempted_base_urls,
                )
                if candidate is not None:
                    self._busy_base_urls.add(candidate.base_url)
                    return candidate
                if self._cached_endpoints and self._all_candidates_attempted_locked(
                    attempted_base_urls=attempted_base_urls,
                ):
                    return None
                if not self._busy_base_urls:
                    return None
                await self._state_condition.wait()

    async def _release_candidate(self, base_url: str) -> None:
        """Release a previously claimed endpoint and wake blocked waiters."""
        async with self._state_condition:
            self._busy_base_urls.discard(base_url)
            self._state_condition.notify_all()

    def _next_claimable_candidate_locked(
        self,
        endpoints: list[ModelEndpoint],
        attempted_base_urls: set[str],
    ) -> ModelEndpoint | None:
        """Return the next round-robin endpoint that is neither busy nor attempted."""
        for candidate in self._ordered_endpoints(endpoints):
            if candidate.base_url in attempted_base_urls:
                continue
            if candidate.base_url in self._busy_base_urls:
                continue
            return candidate
        return None

    def _all_candidates_attempted_locked(
        self,
        attempted_base_urls: set[str],
    ) -> bool:
        """Report whether the current cached endpoint set was fully exhausted."""
        if not self._cached_endpoints:
            return False
        return all(
            endpoint.base_url in attempted_base_urls
            for endpoint in self._cached_endpoints
        )

    def _ordered_endpoints(self, endpoints: list[ModelEndpoint]) -> list[ModelEndpoint]:
        """Return endpoints in round-robin order.

        This helper expects the caller to hold ``_state_condition`` so the
        shared round-robin cursor stays consistent under concurrency.
        """
        if not endpoints:
            return []
        start = self._next_endpoint_index % len(endpoints)
        self._next_endpoint_index = (start + 1) % len(endpoints)
        return endpoints[start:] + endpoints[:start]

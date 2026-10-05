from dataclasses import dataclass, field
from typing import Any, Dict, Optional, Protocol

from aibrix.batch.template import ProfileRegistry, TemplateRegistry


class ModelEndpoint(Protocol):
    @property
    def base_url(self) -> str: ...


class ModelLookupSnapshot(Protocol):
    version: int
    endpoints: list[ModelEndpoint]


class ModelDiscovery(Protocol):
    async def lookup(
        self,
        service_id: Optional[str] = None,
        filter_tags: Optional[Dict[str, str]] = None,
        lookup_timeout_seconds: Optional[float] = None,
    ) -> ModelLookupSnapshot: ...

    async def discover_model_endpoints(
        self,
        served_model_name: str,
        service_id: Optional[str] = None,
        filter_tags: Optional[Dict[str, str]] = None,
        lookup_timeout_seconds: Optional[float] = None,
    ) -> ModelLookupSnapshot: ...

    async def wait_for_model_endpoints(
        self,
        served_model_name: str,
        timeout_seconds: float,
        service_id: Optional[str] = None,
        filter_tags: Optional[Dict[str, str]] = None,
        lookup_timeout_seconds: Optional[float] = None,
        poll_interval_seconds: float = 1.0,
    ) -> ModelLookupSnapshot: ...


@dataclass(slots=True)
class InfrastructureContext:
    template_registry: Optional[TemplateRegistry] = None
    profile_registry: Optional[ProfileRegistry] = None
    apps_v1_api: Any = None
    core_v1_api: Any = None
    httpx_client_wrapper: Any = None
    model_discovery: Optional[ModelDiscovery] = None
    values: Dict[str, Any] = field(default_factory=dict)

    def get(self, name: str, default: Any = None) -> Any:
        return self.values.get(name, default)

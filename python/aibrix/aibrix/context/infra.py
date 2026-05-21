from dataclasses import dataclass, field
from typing import Any, Dict, Optional

from aibrix.batch.template import ProfileRegistry, TemplateRegistry


@dataclass(slots=True)
class InfrastructureContext:
    template_registry: Optional[TemplateRegistry] = None
    profile_registry: Optional[ProfileRegistry] = None
    apps_v1_api: Any = None
    core_v1_api: Any = None
    values: Dict[str, Any] = field(default_factory=dict)

    def get(self, name: str, default: Any = None) -> Any:
        return self.values.get(name, default)

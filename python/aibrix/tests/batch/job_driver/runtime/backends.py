from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Callable, Dict


@dataclass(frozen=True)
class RuntimePatchTarget:
    path: str
    original: Callable[..., Any]


@dataclass(frozen=True)
class RuntimePatchBackend:
    runtime_class: type[Any]
    create: RuntimePatchTarget
    teardown: RuntimePatchTarget
    delete_wait: RuntimePatchTarget
    should_teardown: RuntimePatchTarget


_RUNTIME_PATCH_BACKENDS: Dict[str, RuntimePatchBackend] = {}


def register_runtime_patch_backend(provider: str, backend: RuntimePatchBackend) -> None:
    _RUNTIME_PATCH_BACKENDS[provider] = backend


def get_runtime_patch_backend(provider: str) -> RuntimePatchBackend:
    try:
        return _RUNTIME_PATCH_BACKENDS[provider]
    except KeyError as exc:
        raise ValueError(
            f"Provider {provider!r} does not have a registered runtime patch backend"
        ) from exc

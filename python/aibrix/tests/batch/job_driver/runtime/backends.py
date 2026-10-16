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

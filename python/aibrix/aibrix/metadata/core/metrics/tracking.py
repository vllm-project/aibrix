# Copyright 2026 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

from __future__ import annotations

import contextvars
from typing import Optional

_BACKEND_OPERATION_COUNT: contextvars.ContextVar[int | None] = contextvars.ContextVar(
    "backend_operation_count", default=None
)


def begin_backend_operation_count() -> contextvars.Token[Optional[int]]:
    return _BACKEND_OPERATION_COUNT.set(0)


def reset_backend_operation_count(token: contextvars.Token[Optional[int]]) -> None:
    _BACKEND_OPERATION_COUNT.reset(token)


def get_backend_operation_count() -> int | None:
    return _BACKEND_OPERATION_COUNT.get()


def record_backend_operation(count: int = 1) -> None:
    current = _BACKEND_OPERATION_COUNT.get()
    if current is not None:
        _BACKEND_OPERATION_COUNT.set(current + count)

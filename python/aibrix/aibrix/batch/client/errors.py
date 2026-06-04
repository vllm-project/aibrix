# Copyright 2026 The Aibrix Team.
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
"""Errors raised by the inference client/engine layer.

This layer is an atomic capability and must NOT depend on batch (or any
caller) error types. It raises only ``InferenceError``; callers translate it
into their own domain error at the boundary (e.g. batch maps it to a
per-request output error or a job-level ``BatchJobError``).
"""

from __future__ import annotations

from enum import Enum
from typing import Iterable, Optional


class InferenceErrorCode(str, Enum):
    TRANSPORT_ERROR = "transport_error"
    ALL_ENDPOINTS_FAILED = "all_endpoints_failed"
    NO_ENDPOINT = "no_endpoint"
    CONNECTION_SETUP = "connection_setup"


class InferenceError(Exception):
    """The only error type this layer raises."""

    def __init__(
        self,
        code: InferenceErrorCode,
        message: str,
        *,
        causes: Optional[Iterable[str]] = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.causes = list(causes) if causes else []

    def __str__(self) -> str:
        base = f"[{self.code.value}] {self.message}"
        if self.causes:
            base += " (" + "; ".join(self.causes) + ")"
        return base

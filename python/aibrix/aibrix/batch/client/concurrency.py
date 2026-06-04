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
"""Concurrency control for the inference client.

The dispatch loop owns admission. This module owns the policy for choosing the
current admission limit from request outcomes. It deliberately has no storage
or job-progress dependency.
"""

from __future__ import annotations

from dataclasses import dataclass
from math import floor
from time import monotonic
from typing import Any, Optional, Protocol, runtime_checkable

from aibrix.batch.client.errors import InferenceError

_OVERLOAD_STATUSES = {408, 429, 500, 502, 503, 504}
_CLIENT_ERROR_STATUSES = {400, 401, 403, 404, 422}


@dataclass(frozen=True, slots=True)
class ConcurrencyOutcome:
    """One completed request as seen by concurrency control."""

    success: bool
    latency_seconds: Optional[float] = None
    status_code: Optional[int] = None
    error_code: Optional[str] = None
    retryable: Optional[bool] = None
    prompt_tokens: Optional[int] = None
    output_tokens: Optional[int] = None
    ttft_seconds: Optional[float] = None
    tpot_seconds: Optional[float] = None
    e2e_tpot_seconds: Optional[float] = None


@runtime_checkable
class ConcurrencyController(Protocol):
    """Dynamic in-flight admission policy."""

    def limit(self) -> int: ...

    def on_complete(self, outcome: ConcurrencyOutcome) -> None: ...


class FixedConcurrencyController:
    """Fixed admission limit, preserving the previous engine behavior."""

    def __init__(self, limit: int) -> None:
        self._limit = max(int(limit), 1)

    def limit(self) -> int:
        return self._limit

    def admission_delay_seconds(self) -> float:
        return 0.0

    def on_complete(self, outcome: ConcurrencyOutcome) -> None:
        return None


@dataclass(frozen=True, slots=True)
class LLMAdaptiveConcurrencySettings:
    """Small AIMD-style policy tuned for LLM batch inference.

    Absolute targets are optional because many backends do not expose TTFT/TPOT
    yet. When absent, the controller still reacts to overload errors and can use
    EWMA-relative TTFT/TPOT slowdowns once those metrics are present.
    """

    min_limit: int = 1
    healthy_window: int = 8
    additive_increase: int = 1
    overload_decrease_factor: float = 0.5
    slow_decrease_factor: float = 0.8
    target_ttft_seconds: Optional[float] = None
    target_tpot_seconds: Optional[float] = None
    target_e2e_tpot_seconds: Optional[float] = None
    slow_ttft_factor: float = 1.5
    slow_tpot_factor: float = 1.3
    relative_slowdown_factor: float = 1.75
    ewma_alpha: float = 0.2
    min_output_tokens_for_e2e_tpot: int = 4
    failure_backoff_after: int = 2
    failure_backoff_base_seconds: float = 0.5
    failure_backoff_max_seconds: float = 10.0

    def __post_init__(self) -> None:
        if self.min_limit < 1:
            raise ValueError("min_limit must be >= 1")
        if self.healthy_window < 1:
            raise ValueError("healthy_window must be >= 1")
        if self.additive_increase < 1:
            raise ValueError("additive_increase must be >= 1")
        if not 0 < self.overload_decrease_factor <= 1:
            raise ValueError("overload_decrease_factor must be in (0, 1]")
        if not 0 < self.slow_decrease_factor <= 1:
            raise ValueError("slow_decrease_factor must be in (0, 1]")
        if not 0 < self.ewma_alpha <= 1:
            raise ValueError("ewma_alpha must be in (0, 1]")
        if self.failure_backoff_after < 1:
            raise ValueError("failure_backoff_after must be >= 1")
        if self.failure_backoff_base_seconds < 0:
            raise ValueError("failure_backoff_base_seconds must be >= 0")
        if self.failure_backoff_max_seconds < 0:
            raise ValueError("failure_backoff_max_seconds must be >= 0")


class LLMAdaptiveConcurrencyController:
    """Conservative AIMD controller for LLM serving backends."""

    def __init__(
        self,
        *,
        initial_limit: int,
        max_limit: int,
        settings: Optional[LLMAdaptiveConcurrencySettings] = None,
    ) -> None:
        self._settings = settings or LLMAdaptiveConcurrencySettings()
        self._max_limit = max(int(max_limit), self._settings.min_limit)
        self._limit = min(
            max(int(initial_limit), self._settings.min_limit), self._max_limit
        )
        self._healthy = 0
        self._ttft_ewma: Optional[float] = None
        self._tpot_ewma: Optional[float] = None
        self._e2e_tpot_ewma: Optional[float] = None
        self._consecutive_capacity_errors = 0
        self._backoff_until = 0.0

    def limit(self) -> int:
        return self._limit

    def admission_delay_seconds(self) -> float:
        if self._consecutive_capacity_errors < self._settings.failure_backoff_after:
            return 0.0
        return max(self._backoff_until - monotonic(), 0.0)

    def on_complete(self, outcome: ConcurrencyOutcome) -> None:
        if self._is_capacity_error(outcome):
            self._consecutive_capacity_errors += 1
            self._refresh_backoff_until()
            self._decrease(self._settings.overload_decrease_factor)
            return

        if not outcome.success:
            self._consecutive_capacity_errors = 0
            self._backoff_until = 0.0
            return

        self._consecutive_capacity_errors = 0
        self._backoff_until = 0.0
        slow = self._is_slow(outcome)
        self._update_ewmas(outcome)
        if slow:
            self._decrease(self._settings.slow_decrease_factor)
            return

        self._healthy += 1
        if self._healthy >= self._settings.healthy_window:
            self._healthy = 0
            self._limit = min(
                self._max_limit, self._limit + self._settings.additive_increase
            )

    def _is_capacity_error(self, outcome: ConcurrencyOutcome) -> bool:
        if outcome.success:
            return False
        if outcome.status_code in _CLIENT_ERROR_STATUSES and outcome.retryable is False:
            return False
        if outcome.status_code in _OVERLOAD_STATUSES:
            return True
        return outcome.retryable is not False

    def _is_slow(self, outcome: ConcurrencyOutcome) -> bool:
        return (
            self._metric_slow(
                outcome.ttft_seconds,
                target=self._settings.target_ttft_seconds,
                target_factor=self._settings.slow_ttft_factor,
                ewma=self._ttft_ewma,
            )
            or self._metric_slow(
                outcome.tpot_seconds,
                target=self._settings.target_tpot_seconds,
                target_factor=self._settings.slow_tpot_factor,
                ewma=self._tpot_ewma,
            )
            or self._metric_slow(
                (
                    outcome.e2e_tpot_seconds
                    if (
                        outcome.output_tokens is not None
                        and outcome.output_tokens
                        >= self._settings.min_output_tokens_for_e2e_tpot
                    )
                    else None
                ),
                target=self._settings.target_e2e_tpot_seconds,
                target_factor=self._settings.slow_tpot_factor,
                ewma=self._e2e_tpot_ewma,
            )
        )

    def _metric_slow(
        self,
        value: Optional[float],
        *,
        target: Optional[float],
        target_factor: float,
        ewma: Optional[float],
    ) -> bool:
        if value is None:
            return False
        if target is not None and value > target * target_factor:
            return True
        return (
            ewma is not None and value > ewma * self._settings.relative_slowdown_factor
        )

    def _update_ewmas(self, outcome: ConcurrencyOutcome) -> None:
        self._ttft_ewma = self._ewma(self._ttft_ewma, outcome.ttft_seconds)
        self._tpot_ewma = self._ewma(self._tpot_ewma, outcome.tpot_seconds)
        self._e2e_tpot_ewma = self._ewma(self._e2e_tpot_ewma, outcome.e2e_tpot_seconds)

    def _ewma(
        self, current: Optional[float], value: Optional[float]
    ) -> Optional[float]:
        if value is None:
            return current
        if current is None:
            return value
        alpha = self._settings.ewma_alpha
        return alpha * value + (1 - alpha) * current

    def _decrease(self, factor: float) -> None:
        self._healthy = 0
        next_limit = max(self._settings.min_limit, floor(self._limit * factor))
        if next_limit >= self._limit and self._limit > self._settings.min_limit:
            next_limit = self._limit - 1
        self._limit = next_limit

    def _refresh_backoff_until(self) -> None:
        if self._consecutive_capacity_errors < self._settings.failure_backoff_after:
            return
        exponent = (
            self._consecutive_capacity_errors - self._settings.failure_backoff_after
        )
        delay = min(
            self._settings.failure_backoff_base_seconds * (2**exponent),
            self._settings.failure_backoff_max_seconds,
        )
        self._backoff_until = max(self._backoff_until, monotonic() + delay)


def concurrency_outcome_from_result(
    response: Optional[dict[str, Any]],
    error: Optional[InferenceError],
    *,
    latency_seconds: float,
) -> ConcurrencyOutcome:
    if error is not None:
        return ConcurrencyOutcome(
            success=False,
            latency_seconds=latency_seconds,
            status_code=error.status_code,
            error_code=error.code.value,
            retryable=error.retryable,
        )

    usage = response.get("usage") if isinstance(response, dict) else None
    prompt_tokens = _int_from_mapping(usage, "prompt_tokens", "input_tokens")
    output_tokens = _int_from_mapping(usage, "completion_tokens", "output_tokens")
    ttft = _float_from_nested(
        response,
        "ttft_seconds",
        "ttft",
        "time_to_first_token_seconds",
        "time_to_first_token",
    )
    tpot = _float_from_nested(
        response,
        "tpot_seconds",
        "tpot",
        "time_per_output_token_seconds",
        "time_per_output_token",
    )
    e2e_tpot = None
    if output_tokens is not None and output_tokens > 0:
        e2e_tpot = latency_seconds / output_tokens

    return ConcurrencyOutcome(
        success=True,
        latency_seconds=latency_seconds,
        prompt_tokens=prompt_tokens,
        output_tokens=output_tokens,
        ttft_seconds=ttft,
        tpot_seconds=tpot,
        e2e_tpot_seconds=e2e_tpot,
    )


def _int_from_mapping(mapping: Any, *keys: str) -> Optional[int]:
    if not isinstance(mapping, dict):
        return None
    for key in keys:
        value = mapping.get(key)
        if value is not None:
            try:
                return int(value)
            except (TypeError, ValueError):
                continue
    return None


def _float_from_nested(mapping: Any, *keys: str) -> Optional[float]:
    if not isinstance(mapping, dict):
        return None
    candidates = [mapping]
    for nested_key in ("metrics", "timings", "usage"):
        nested = mapping.get(nested_key)
        if isinstance(nested, dict):
            candidates.append(nested)
    for candidate in candidates:
        for key in keys:
            value = candidate.get(key)
            if value is not None:
                try:
                    return float(value)
                except (TypeError, ValueError):
                    continue
            ms_value = candidate.get(f"{key}_ms")
            if ms_value is not None:
                try:
                    return float(ms_value) / 1000.0
                except (TypeError, ValueError):
                    continue
    return None

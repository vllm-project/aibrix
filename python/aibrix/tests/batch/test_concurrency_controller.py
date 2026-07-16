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

from aibrix.batch.client import (
    ConcurrencyOutcome,
    InferenceError,
    InferenceErrorCode,
    LLMAdaptiveConcurrencyController,
    LLMAdaptiveConcurrencySettings,
    concurrency_outcome_from_result,
)


def test_llm_controller_ignores_isolated_overload_error():
    controller = LLMAdaptiveConcurrencyController(
        initial_limit=128,
        max_limit=128,
    )
    overload = ConcurrencyOutcome(
        success=False,
        status_code=429,
        error_code=InferenceErrorCode.HTTP_ERROR.value,
        retryable=True,
    )

    controller.on_complete(overload)
    for _ in range(127):
        controller.on_complete(ConcurrencyOutcome(success=True))

    assert controller.limit() == 128


def test_llm_controller_reduces_once_per_overloaded_sample_window():
    controller = LLMAdaptiveConcurrencyController(
        initial_limit=128,
        max_limit=128,
    )
    overload = ConcurrencyOutcome(success=False, status_code=503, retryable=True)

    for _ in range(128):
        controller.on_complete(overload)
    assert controller.limit() == 115

    for _ in range(115):
        controller.on_complete(overload)
    assert controller.limit() == 103


def test_llm_controller_recovers_quickly_after_small_decrease():
    controller = LLMAdaptiveConcurrencyController(
        initial_limit=128,
        max_limit=128,
    )
    overload = ConcurrencyOutcome(success=False, status_code=503, retryable=True)
    healthy = ConcurrencyOutcome(success=True)

    for outcome in [*([overload] * 16), *([healthy] * 112)]:
        controller.on_complete(outcome)
    assert controller.limit() == 115

    for _ in range(104):
        controller.on_complete(healthy)
    assert controller.limit() == 128


def test_llm_controller_honors_configured_min_limit():
    settings = LLMAdaptiveConcurrencySettings(
        min_limit=4,
        healthy_window=4,
        overload_decrease_factor=0.5,
    )
    controller = LLMAdaptiveConcurrencyController(
        initial_limit=32,
        max_limit=32,
        settings=settings,
    )
    overload = ConcurrencyOutcome(success=False, status_code=503, retryable=True)

    for _ in range(96):
        controller.on_complete(overload)

    assert controller.limit() == 4


def test_llm_controller_ignores_non_retryable_client_error():
    controller = LLMAdaptiveConcurrencyController(initial_limit=8, max_limit=8)

    controller.on_complete(
        ConcurrencyOutcome(
            success=False,
            status_code=400,
            error_code=InferenceErrorCode.HTTP_ERROR.value,
            retryable=False,
        )
    )

    assert controller.limit() == 8


def test_llm_controller_reduces_on_slow_e2e_tpot():
    settings = LLMAdaptiveConcurrencySettings(target_e2e_tpot_seconds=0.1)
    controller = LLMAdaptiveConcurrencyController(
        initial_limit=8, max_limit=8, settings=settings
    )
    outcome = concurrency_outcome_from_result(
        {
            "usage": {
                "prompt_tokens": 10,
                "completion_tokens": 10,
            }
        },
        None,
        latency_seconds=2.0,
    )

    controller.on_complete(outcome)

    assert outcome.e2e_tpot_seconds == 0.2
    assert controller.limit() == 6


def test_llm_controller_recovers_after_healthy_window():
    settings = LLMAdaptiveConcurrencySettings(
        healthy_window=2,
        target_e2e_tpot_seconds=0.1,
    )
    controller = LLMAdaptiveConcurrencyController(
        initial_limit=4, max_limit=4, settings=settings
    )
    overload = ConcurrencyOutcome(success=False, status_code=503, retryable=True)
    controller.on_complete(overload)
    controller.on_complete(overload)
    controller.on_complete(ConcurrencyOutcome(success=True))
    controller.on_complete(ConcurrencyOutcome(success=True))
    assert controller.limit() == 3

    healthy = ConcurrencyOutcome(success=True, e2e_tpot_seconds=0.05)
    controller.on_complete(healthy)
    assert controller.limit() == 3
    controller.on_complete(healthy)
    assert controller.limit() == 4


def test_llm_controller_waits_for_warmup_before_relative_slowdown():
    settings = LLMAdaptiveConcurrencySettings(
        relative_slowdown_warmup=2,
        relative_slowdown_factor=1.5,
    )
    controller = LLMAdaptiveConcurrencyController(
        initial_limit=8, max_limit=8, settings=settings
    )

    controller.on_complete(ConcurrencyOutcome(success=True, tpot_seconds=0.01))
    controller.on_complete(ConcurrencyOutcome(success=True, tpot_seconds=0.10))
    assert controller.limit() == 8

    controller.on_complete(ConcurrencyOutcome(success=True, tpot_seconds=0.10))
    assert controller.limit() == 6


def test_llm_controller_adds_backoff_after_overloaded_sample_window():
    settings = LLMAdaptiveConcurrencySettings(
        healthy_window=2,
        failure_backoff_after=2,
        failure_backoff_base_seconds=0.5,
        failure_backoff_max_seconds=2.0,
    )
    controller = LLMAdaptiveConcurrencyController(
        initial_limit=2, max_limit=4, settings=settings
    )

    controller.on_complete(
        ConcurrencyOutcome(success=False, status_code=503, retryable=True)
    )
    assert controller.admission_delay_seconds() == 0.0
    controller.on_complete(
        ConcurrencyOutcome(success=False, status_code=503, retryable=True)
    )
    delay = controller.admission_delay_seconds()
    assert 0.0 < delay <= 0.5

    controller.on_complete(ConcurrencyOutcome(success=True))
    assert controller.admission_delay_seconds() == 0.0


def test_concurrency_outcome_preserves_error_metadata():
    err = InferenceError(
        InferenceErrorCode.HTTP_ERROR,
        "unavailable",
        status_code=503,
        retryable=True,
    )

    outcome = concurrency_outcome_from_result(None, err, latency_seconds=0.25)

    assert outcome.success is False
    assert outcome.status_code == 503
    assert outcome.retryable is True
    assert outcome.error_code == InferenceErrorCode.HTTP_ERROR.value


def test_concurrency_outcome_extracts_nested_llm_latency_metrics():
    outcome = concurrency_outcome_from_result(
        {
            "usage": {
                "input_tokens": 7,
                "output_tokens": 5,
            },
            "metrics": {
                "time_to_first_token_ms": 120,
            },
            "timings": {
                "time_per_output_token_ms": 25,
            },
        },
        None,
        latency_seconds=0.5,
    )

    assert outcome.success is True
    assert outcome.prompt_tokens == 7
    assert outcome.output_tokens == 5
    assert outcome.ttft_seconds == 0.12
    assert outcome.tpot_seconds == 0.025
    assert outcome.e2e_tpot_seconds == 0.1

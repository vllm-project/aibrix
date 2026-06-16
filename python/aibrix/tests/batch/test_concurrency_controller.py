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


def test_llm_controller_reduces_on_retryable_overload_error():
    controller = LLMAdaptiveConcurrencyController(initial_limit=8, max_limit=8)

    controller.on_complete(
        ConcurrencyOutcome(
            success=False,
            status_code=429,
            error_code=InferenceErrorCode.HTTP_ERROR.value,
            retryable=True,
        )
    )

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
    controller.on_complete(
        ConcurrencyOutcome(success=False, status_code=503, retryable=True)
    )
    assert controller.limit() == 2

    healthy = ConcurrencyOutcome(success=True, e2e_tpot_seconds=0.05)
    controller.on_complete(healthy)
    assert controller.limit() == 2
    controller.on_complete(healthy)
    assert controller.limit() == 3


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


def test_llm_controller_adds_backoff_after_consecutive_capacity_errors():
    settings = LLMAdaptiveConcurrencySettings(
        failure_backoff_after=2,
        failure_backoff_base_seconds=0.5,
        failure_backoff_max_seconds=2.0,
    )
    controller = LLMAdaptiveConcurrencyController(
        initial_limit=4, max_limit=4, settings=settings
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
    controller.on_complete(
        ConcurrencyOutcome(success=False, status_code=503, retryable=True)
    )
    delay = controller.admission_delay_seconds()
    assert 0.5 < delay <= 1.5

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

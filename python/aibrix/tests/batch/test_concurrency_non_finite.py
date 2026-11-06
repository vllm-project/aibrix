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

import pytest

from aibrix.batch.client import (
    ConcurrencyOutcome,
    LLMAdaptiveConcurrencyController,
    concurrency_outcome_from_result,
)

NON_FINITE = [float("nan"), float("inf"), float("-inf"), "nan", "inf", "-Infinity"]


def _outcome(metrics):
    return concurrency_outcome_from_result(
        {"usage": {"input_tokens": 7, "output_tokens": 5}, **metrics},
        None,
        latency_seconds=0.5,
    )


@pytest.mark.parametrize("bad", NON_FINITE, ids=repr)
def test_non_finite_plain_latency_falls_back_to_the_ms_form(bad):
    outcome = _outcome(
        {
            "metrics": {
                "time_to_first_token": bad,
                "time_to_first_token_ms": 120,
            },
        }
    )

    assert outcome.ttft_seconds == 0.12


@pytest.mark.parametrize("bad", NON_FINITE, ids=repr)
def test_non_finite_latency_is_missing_not_a_measurement(bad):
    outcome = _outcome(
        {
            "metrics": {"time_to_first_token": bad},
            "timings": {"time_per_output_token_ms": bad},
        }
    )

    assert outcome.ttft_seconds is None
    assert outcome.tpot_seconds is None


def test_finite_latencies_are_unchanged():
    outcome = _outcome(
        {
            "metrics": {"time_to_first_token": 0.12},
            "timings": {"time_per_output_token_ms": 25},
        }
    )

    assert outcome.ttft_seconds == 0.12
    assert outcome.tpot_seconds == 0.025


@pytest.mark.parametrize("bad", ["NaN", "Infinity"])
def test_controller_still_backs_off_after_a_non_finite_latency(bad):
    """One NaN used to poison the running average, so slowdowns were never seen again."""

    def final_limit(body):
        controller = LLMAdaptiveConcurrencyController(initial_limit=64, max_limit=64)
        for _ in range(10):
            controller.on_complete(ConcurrencyOutcome(success=True, ttft_seconds=0.1))
        if body is not None:
            controller.on_complete(
                concurrency_outcome_from_result(body, None, latency_seconds=0.5)
            )
        for _ in range(30):  # the engine becomes 50 times slower
            controller.on_complete(ConcurrencyOutcome(success=True, ttft_seconds=5.0))
        return controller.limit()

    baseline = final_limit(None)
    assert baseline < 64

    value = float(bad)
    limit = final_limit({"metrics": {"ttft": value}, "usage": {"output_tokens": 1}})

    # a bad latency is simply ignored, so the controller behaves as if it was never sent
    assert limit == baseline

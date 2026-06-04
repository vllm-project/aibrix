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

"""Tests for the OpenAI Batch ``usage`` field pipeline.

Covers four layers:
  1. Schema (BatchUsage / Input/OutputTokensDetails validation)
  2. Worker accumulation in JobDriver (idempotency, naming map, isolation)
  3. K8s annotation persistence roundtrip
  4. API response surfacing (BatchResponse.usage + state flatten + model)
"""

from datetime import datetime, timezone
from typing import Optional
from unittest.mock import MagicMock

import pytest
from pydantic import ValidationError

from aibrix.batch.client import NoopEndpointSource
from aibrix.batch.job_driver import BaseJobDriver, ExternalRuntime, JobDriver
from aibrix.batch.job_entity import (
    AibrixMetadata,
    BatchJob,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    BatchJobStatusCopy,
    BatchUsage,
    Condition,
    ConditionStatus,
    ConditionType,
    InputTokensDetails,
    ModelTemplateRef,
    ObjectMeta,
    OutputTokensDetails,
    RequestCountStats,
    TypeMeta,
    aggregate_batch_job_status,
    merge_batch_job_status_copies,
)
from aibrix.metadata.api.v1.batch import _batch_job_to_openai_response

# ─────────────────────────────────────────────────────────────────────────────
# Schema
# ─────────────────────────────────────────────────────────────────────────────


class TestBatchUsageSchema:
    def test_defaults_zero(self):
        u = BatchUsage()
        assert u.input_tokens == 0
        assert u.output_tokens == 0
        assert u.total_tokens == 0
        assert u.input_tokens_details.cached_tokens == 0
        assert u.output_tokens_details.reasoning_tokens == 0

    def test_negative_tokens_rejected(self):
        with pytest.raises(ValidationError):
            BatchUsage(input_tokens=-1)
        with pytest.raises(ValidationError):
            BatchUsage(output_tokens=-1)

    def test_negative_details_rejected(self):
        with pytest.raises(ValidationError):
            InputTokensDetails(cached_tokens=-1)
        with pytest.raises(ValidationError):
            OutputTokensDetails(reasoning_tokens=-1)

    def test_populated_from_dict(self):
        u = BatchUsage.model_validate(
            {
                "input_tokens": 1000,
                "output_tokens": 500,
                "total_tokens": 1500,
                "input_tokens_details": {"cached_tokens": 100},
                "output_tokens_details": {"reasoning_tokens": 50},
            }
        )
        assert u.input_tokens_details.cached_tokens == 100
        assert u.output_tokens_details.reasoning_tokens == 50

    def test_strict_mode_rejects_unknown_field(self):
        with pytest.raises(ValidationError):
            BatchUsage(unknown_field=123)


# ─────────────────────────────────────────────────────────────────────────────
# Worker accumulation
# ─────────────────────────────────────────────────────────────────────────────


@pytest.fixture
def driver() -> JobDriver:
    return BaseJobDriver(
        progress_manager=MagicMock(),
        runtime=ExternalRuntime(NoopEndpointSource()),
    )


def _make_status_copy(
    *,
    total: int,
    launched: int,
    completed: int,
    failed: int,
    input_tokens: int = 0,
    output_tokens: int = 0,
    cached_tokens: int = 0,
    reasoning_tokens: int = 0,
) -> BatchJobStatusCopy:
    return BatchJobStatusCopy(
        state=BatchJobState.IN_PROGRESS,
        requestCounts=RequestCountStats(
            total=total,
            launched=launched,
            completed=completed,
            failed=failed,
        ),
        usage=BatchUsage(
            input_tokens=input_tokens,
            output_tokens=output_tokens,
            total_tokens=input_tokens + output_tokens,
            input_tokens_details=InputTokensDetails(cached_tokens=cached_tokens),
            output_tokens_details=OutputTokensDetails(
                reasoning_tokens=reasoning_tokens
            ),
        ),
    )


def _make_status(
    *, status_copies: Optional[dict[str, BatchJobStatusCopy]]
) -> BatchJobStatus:
    return BatchJobStatus(
        jobID="job-1",
        state=BatchJobState.IN_PROGRESS,
        createdAt=datetime.now(timezone.utc),
        requestCounts=RequestCountStats(),
        statusCopies=status_copies,
    )


class TestWorkerAccumulation:
    def test_single_response_naming_map(self, driver):
        """prompt_tokens / completion_tokens (Completions API) → input/output."""
        driver._accumulate_usage(
            "j1",
            "r1",
            {
                "prompt_tokens": 100,
                "completion_tokens": 50,
                "total_tokens": 150,
                "prompt_tokens_details": {"cached_tokens": 20},
                "completion_tokens_details": {"reasoning_tokens": 10},
            },
        )
        u = driver.get_accumulated_usage("j1")
        assert u.input_tokens == 100
        assert u.output_tokens == 50
        assert u.total_tokens == 150
        assert u.input_tokens_details.cached_tokens == 20
        assert u.output_tokens_details.reasoning_tokens == 10

    def test_accumulation_across_requests(self, driver):
        driver._accumulate_usage(
            "j1", "r1", {"prompt_tokens": 100, "completion_tokens": 50}
        )
        driver._accumulate_usage(
            "j1", "r2", {"prompt_tokens": 200, "completion_tokens": 100}
        )
        u = driver.get_accumulated_usage("j1")
        assert u.input_tokens == 300
        assert u.output_tokens == 150
        assert u.total_tokens == 450

    def test_idempotent_on_retry_same_custom_id(self, driver):
        driver._accumulate_usage(
            "j1", "r1", {"prompt_tokens": 100, "completion_tokens": 50}
        )
        # Same custom_id again (e.g. retry succeeded after we counted the failed attempt's usage)
        driver._accumulate_usage(
            "j1", "r1", {"prompt_tokens": 999, "completion_tokens": 999}
        )
        u = driver.get_accumulated_usage("j1")
        assert u.input_tokens == 100
        assert u.output_tokens == 50

    def test_per_job_isolation(self, driver):
        driver._accumulate_usage(
            "a", "r", {"prompt_tokens": 100, "completion_tokens": 0}
        )
        driver._accumulate_usage("b", "r", {"prompt_tokens": 5, "completion_tokens": 0})
        assert driver.get_accumulated_usage("a").input_tokens == 100
        assert driver.get_accumulated_usage("b").input_tokens == 5

    def test_no_op_when_usage_absent(self, driver):
        driver._accumulate_usage("j", "r", None)
        driver._accumulate_usage("j", "r", {})
        assert driver.get_accumulated_usage("j") is None

    def test_embeddings_only_prompt_tokens(self, driver):
        """Embeddings endpoint responses carry prompt_tokens but no completion_tokens."""
        driver._accumulate_usage("emb", "e1", {"prompt_tokens": 50})
        u = driver.get_accumulated_usage("emb")
        assert u.input_tokens == 50
        assert u.output_tokens == 0
        assert u.total_tokens == 50

    def test_get_accumulated_usage_returns_deep_copy(self, driver):
        driver._accumulate_usage(
            "j", "r", {"prompt_tokens": 10, "completion_tokens": 5}
        )
        copy = driver.get_accumulated_usage("j")
        copy.input_tokens = 99999  # mutate the copy
        fresh = driver.get_accumulated_usage("j")
        assert fresh.input_tokens == 10  # internal state untouched

    def test_drop_state_releases_memory(self, driver):
        driver._accumulate_usage(
            "j", "r", {"prompt_tokens": 10, "completion_tokens": 5}
        )
        assert driver.get_accumulated_usage("j") is not None
        driver._drop_usage_state("j")
        assert driver.get_accumulated_usage("j") is None

    def test_returns_none_for_unknown_job(self, driver):
        assert driver.get_accumulated_usage("never-seen") is None

    def test_non_numeric_token_count_treated_as_zero(self, driver):
        """Defensive: engine returns weird shape, should not crash."""
        driver._accumulate_usage(
            "j", "r", {"prompt_tokens": None, "completion_tokens": None}
        )
        # No exception; usage stays at zero (no entry created)
        assert driver.get_accumulated_usage("j") is not None
        u = driver.get_accumulated_usage("j")
        assert u.input_tokens == 0
        assert u.output_tokens == 0

    def test_aggregated_usage_includes_old_and_new_local_driver_status_copies(self):
        status = BatchJobStatus(
            jobID="job-1",
            state=BatchJobState.IN_PROGRESS,
            createdAt=datetime.now(timezone.utc),
            statusCopies={
                "worker-old": BatchJobStatusCopy(
                    state=BatchJobState.IN_PROGRESS,
                    usage=BatchUsage(
                        input_tokens=100,
                        output_tokens=40,
                        total_tokens=140,
                        input_tokens_details=InputTokensDetails(cached_tokens=10),
                        output_tokens_details=OutputTokensDetails(reasoning_tokens=4),
                    ),
                ),
                "worker-new": BatchJobStatusCopy(
                    state=BatchJobState.IN_PROGRESS,
                    usage=BatchUsage(
                        input_tokens=30,
                        output_tokens=12,
                        total_tokens=42,
                        input_tokens_details=InputTokensDetails(cached_tokens=3),
                        output_tokens_details=OutputTokensDetails(reasoning_tokens=2),
                    ),
                ),
            },
        )

        aggregated = aggregate_batch_job_status(status, False)

        assert aggregated.usage is not None
        assert aggregated.usage.input_tokens == 130
        assert aggregated.usage.output_tokens == 52
        assert aggregated.usage.total_tokens == 182
        assert aggregated.usage.input_tokens_details.cached_tokens == 13
        assert aggregated.usage.output_tokens_details.reasoning_tokens == 6


class TestStatusCopyMerging:
    def test_merge_preserves_existing_copies_when_new_status_has_none(self):
        existing = _make_status(
            status_copies={
                "worker-old": _make_status_copy(
                    total=4,
                    launched=4,
                    completed=3,
                    failed=1,
                    input_tokens=100,
                    output_tokens=40,
                    cached_tokens=10,
                    reasoning_tokens=4,
                )
            }
        )
        new = _make_status(status_copies=None)

        merged = merge_batch_job_status_copies(existing, new)

        assert merged.status_copies is not None
        assert set(merged.status_copies) == {"worker-old"}
        assert merged.request_counts.total == 4
        assert merged.request_counts.launched == 4
        assert merged.request_counts.completed == 3
        assert merged.request_counts.failed == 1
        assert merged.usage is not None
        assert merged.usage.input_tokens == 100
        assert merged.usage.output_tokens == 40

    def test_merge_combines_existing_and_new_status_copies(self):
        existing = _make_status(
            status_copies={
                "worker-old": _make_status_copy(
                    total=5,
                    launched=5,
                    completed=2,
                    failed=1,
                    input_tokens=70,
                    output_tokens=20,
                    cached_tokens=7,
                    reasoning_tokens=2,
                )
            }
        )
        new = _make_status(
            status_copies={
                "worker-new": _make_status_copy(
                    total=5,
                    launched=4,
                    completed=3,
                    failed=1,
                    input_tokens=30,
                    output_tokens=10,
                    cached_tokens=3,
                    reasoning_tokens=1,
                )
            }
        )

        merged = merge_batch_job_status_copies(existing, new)

        assert merged.status_copies is not None
        assert set(merged.status_copies) == {"worker-old", "worker-new"}
        assert merged.request_counts.total == 5
        assert merged.request_counts.launched == 5
        assert merged.request_counts.completed == 5
        assert merged.request_counts.failed == 0
        assert merged.usage is not None
        assert merged.usage.input_tokens == 100
        assert merged.usage.output_tokens == 30


# ─────────────────────────────────────────────────────────────────────────────
# API response (state flatten + model + usage)
# ─────────────────────────────────────────────────────────────────────────────


def _make_batch_job(
    state, condition_type=None, model_name="llama3-70b-prod", usage=None
) -> BatchJob:
    aibrixMetadata: Optional[AibrixMetadata] = None
    if model_name is not None:
        aibrixMetadata = AibrixMetadata(
            model_template=ModelTemplateRef(name=model_name)
        )
    spec = BatchJobSpec(
        input_file_id="f1",
        endpoint="/v1/chat/completions",
        completion_window=86400,
        aibrix=aibrixMetadata,
    )
    status_data = {
        "jobID": "job-1",
        "state": state,
        "createdAt": datetime.now(timezone.utc),
    }
    if condition_type:
        status_data["conditions"] = [
            Condition(
                type=condition_type,
                status=ConditionStatus.TRUE,
                lastTransitionTime=datetime.now(timezone.utc),
            )
        ]
        status_data["finalizedAt"] = datetime.now(timezone.utc)

    status = BatchJobStatus.model_validate(status_data)
    if usage is not None:
        status.usage = usage
    return BatchJob(
        sessionID="s1",
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
        metadata=ObjectMeta.model_validate({"name": "x", "namespace": "default"}),
        spec=spec,
        status=status,
    )


_OPENAI_8_STATES = {
    "validating",
    "failed",
    "in_progress",
    "finalizing",
    "completed",
    "expired",
    "cancelling",
    "cancelled",
}


class TestApiResponseStateFlatten:
    """Internal CREATED / FINALIZED states must not leak to the API."""

    @pytest.mark.parametrize(
        "state,expected",
        [
            (BatchJobState.CREATED, "validating"),
            (BatchJobState.VALIDATING, "validating"),
            (BatchJobState.IN_PROGRESS, "in_progress"),
            (BatchJobState.FINALIZING, "finalizing"),
            (BatchJobState.CANCELLING, "cancelling"),
        ],
    )
    def test_non_terminal_states(self, state, expected):
        r = _batch_job_to_openai_response(_make_batch_job(state))
        assert r.status == expected
        assert r.status in _OPENAI_8_STATES

    @pytest.mark.parametrize(
        "condition,expected",
        [
            (ConditionType.COMPLETED, "completed"),
            (ConditionType.FAILED, "failed"),
            (ConditionType.EXPIRED, "expired"),
            (ConditionType.CANCELLED, "cancelled"),
        ],
    )
    def test_finalized_via_condition(self, condition, expected):
        r = _batch_job_to_openai_response(
            _make_batch_job(BatchJobState.FINALIZED, condition)
        )
        assert r.status == expected
        assert r.status in _OPENAI_8_STATES


class TestApiResponseModelField:
    def test_populated_from_template_name(self):
        r = _batch_job_to_openai_response(
            _make_batch_job(BatchJobState.IN_PROGRESS, model_name="llama3-70b-prod")
        )
        assert r.model == "llama3-70b-prod"

    def test_omitted_for_legacy_batch_without_template(self):
        r = _batch_job_to_openai_response(
            _make_batch_job(BatchJobState.IN_PROGRESS, model_name=None)
        )
        assert r.model is None


class TestApiResponseUsage:
    def test_usage_passed_through_when_present(self):
        usage = BatchUsage(
            input_tokens=1000,
            output_tokens=500,
            total_tokens=1500,
            input_tokens_details=InputTokensDetails(cached_tokens=100),
            output_tokens_details=OutputTokensDetails(reasoning_tokens=50),
        )
        r = _batch_job_to_openai_response(
            _make_batch_job(BatchJobState.IN_PROGRESS, usage=usage)
        )
        assert r.usage is not None
        assert r.usage.input_tokens == 1000
        assert r.usage.output_tokens == 500
        assert r.usage.total_tokens == 1500
        assert r.usage.input_tokens_details.cached_tokens == 100
        assert r.usage.output_tokens_details.reasoning_tokens == 50

    def test_usage_omitted_when_none(self):
        r = _batch_job_to_openai_response(
            _make_batch_job(BatchJobState.IN_PROGRESS, usage=None)
        )
        assert r.usage is None

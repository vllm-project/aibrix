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

from aibrix.batch.job_driver import EchoInferenceEngineClient, JobDriver, LocalJobDriver
from aibrix.batch.job_entity import (
    AibrixMetadata,
    BatchJob,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    BatchJobTransformer,
    BatchUsage,
    Condition,
    ConditionStatus,
    ConditionType,
    InputTokensDetails,
    ModelTemplateRef,
    ObjectMeta,
    OutputTokensDetails,
    TypeMeta,
)
from aibrix.batch.job_entity.k8s_transformer import JobAnnotationKey
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
    return LocalJobDriver(
        progress_manager=MagicMock(),
        inference_client=EchoInferenceEngineClient(),
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


# ─────────────────────────────────────────────────────────────────────────────
# K8s annotation persistence roundtrip
# ─────────────────────────────────────────────────────────────────────────────


def _make_status(usage=None, **kwargs) -> BatchJobStatus:
    data = {
        "jobID": "j1",
        "state": kwargs.get("state", BatchJobState.IN_PROGRESS),
        "createdAt": datetime.now(timezone.utc),
        "requestCounts": {
            "total": kwargs.get("total", 100),
            "launched": 100,
            "completed": kwargs.get("completed", 90),
            "failed": kwargs.get("failed", 0),
        },
    }
    s = BatchJobStatus.model_validate(data)
    if usage is not None:
        s.usage = usage
    return s


class TestUsageAnnotationRoundtrip:
    def test_full_usage_persisted_and_restored(self):
        original = _make_status(
            usage=BatchUsage(
                input_tokens=10_000,
                output_tokens=5_000,
                total_tokens=15_000,
                input_tokens_details=InputTokensDetails(cached_tokens=2_000),
                output_tokens_details=OutputTokensDetails(reasoning_tokens=300),
            )
        )
        annotations = BatchJobTransformer.create_status_annotations(original)
        assert JobAnnotationKey.USAGE.value in annotations

        # Hydrate a fresh status from annotations
        fresh = _make_status()  # usage is None
        restored = BatchJobTransformer.update_status_from_annotations(
            fresh, annotations
        )
        assert restored.usage is not None
        assert restored.usage.input_tokens == 10_000
        assert restored.usage.output_tokens == 5_000
        assert restored.usage.input_tokens_details.cached_tokens == 2_000
        assert restored.usage.output_tokens_details.reasoning_tokens == 300

    def test_zero_usage_not_persisted(self):
        """Empty BatchUsage shouldn't bloat annotations on uninitialized batches."""
        original = _make_status(usage=BatchUsage())  # all zero
        annotations = BatchJobTransformer.create_status_annotations(original)
        assert JobAnnotationKey.USAGE.value not in annotations

    def test_none_usage_not_persisted(self):
        original = _make_status()  # usage stays None
        annotations = BatchJobTransformer.create_status_annotations(original)
        assert JobAnnotationKey.USAGE.value not in annotations

    def test_malformed_usage_annotation_yields_none(self):
        fresh = _make_status()
        restored = BatchJobTransformer.update_status_from_annotations(
            fresh, {JobAnnotationKey.USAGE.value: "not-json{{"}
        )
        assert restored.usage is None  # graceful, no crash


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

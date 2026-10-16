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

from typing import Optional

from aibrix.batch.job_entity import BatchJob, ConditionType
from aibrix.metadata.core.metrics import T, Tag


def _finalize_type(job: BatchJob) -> str:
    condition = job.status.condition
    if condition is None:
        return ConditionType.COMPLETED.value
    return condition.value


def _terminal_error_code(job: BatchJob) -> str:
    if job.status.errors:
        code = job.status.errors[0].code
        return str(getattr(code, "value", code))
    return "none"


def tags_from_job(job: Optional[BatchJob]) -> tuple[Tag, ...]:
    if job is None:
        return (
            T("endpoint", "none"),
            T("completion_window", "none"),
            T("job_id", "none"),
            T("console_job_id", "none"),
        )
    console_job_id = (
        job.spec.aibrix.job_id
        if job.spec.aibrix is not None and job.spec.aibrix.job_id is not None
        else "none"
    )
    return (
        T("endpoint", job.spec.endpoint),
        T("completion_window", str(job.spec.completion_window)),
        T("job_id", job.job_id),
        T("console_job_id", console_job_id),
    )


def tags_from_finalized_job(job: BatchJob) -> tuple[Tag, ...]:
    return (
        *tags_from_job(job),
        T("finalize_type", _finalize_type(job)),
        T("error_code", _terminal_error_code(job)),
    )

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
"""Conformance: our hand-written batch wire DTO must stay a structural superset
of the OpenAI Batch object.

We deliberately do NOT import ``openai`` at runtime (it is a dev-only
dependency) — the internal model and the ``BatchResponse`` DTO are owned types.
This test uses ``openai.types`` purely as a dev-time *oracle* so the DTO can
never silently drift from the OpenAI contract: if OpenAI adds/changes a field,
this fails and tells us to mirror it. See
``docs/source/designs/batch-openai-type-reuse-proposal.md`` (Option C).
"""

import typing
from typing import Union, get_args, get_origin

import pydantic
import pytest

from aibrix.metadata.api.v1.batch import BatchRequestCounts, BatchResponse

openai_types = pytest.importorskip(
    "openai.types", reason="openai is a dev dependency; oracle for this test"
)


def _strip_optional(ann):
    """Drop the ``None`` arm of ``Optional[X]`` / ``Union[X, None]``."""
    if get_origin(ann) is Union:
        args = [a for a in get_args(ann) if a is not type(None)]
        return args[0] if len(args) == 1 else Union[tuple(args)]
    return ann


def _shape(ann) -> str:
    """Normalize an annotation to a comparable 'shape', so we compare structure
    rather than class identity (OpenAI's ``request_counts``/``errors`` are
    different classes with the same shape; ``status`` is a ``Literal`` while
    ours is a plain ``str``)."""
    ann = _strip_optional(ann)
    origin = get_origin(ann)
    if origin is typing.Literal:
        vals = get_args(ann)
        return type(vals[0]).__name__ if vals else "str"
    if origin in (dict,) or ann is dict:
        return "mapping"
    if origin in (list,) or ann is list:
        return "sequence"
    try:
        if isinstance(ann, type) and issubclass(ann, pydantic.BaseModel):
            return "model"
    except TypeError:
        pass
    if ann is object:
        return "any"  # OpenAI's metadata is typed `object` — accepts anything
    return ann.__name__ if isinstance(ann, type) else str(ann)


def _compatible(openai_ann, ours_ann) -> bool:
    o, m = _shape(openai_ann), _shape(ours_ann)
    return o == "any" or o == m


def test_batchresponse_covers_every_openai_batch_field():
    """Every field on openai.types.Batch must exist on our BatchResponse.

    This is the core drift guard: a new OpenAI Batch field fails here until we
    mirror it. Our extension fields (model, usage, aibrix) are a superset and
    are allowed.
    """
    openai_fields = set(openai_types.Batch.model_fields)
    ours = set(BatchResponse.model_fields)
    # Guard the oracle itself: if openai.types.Batch ever resolves to an empty
    # / stubbed model, the superset check would pass trivially. The real type
    # has ~20 fields; a floor well below that catches a degraded oracle.
    assert len(openai_fields) >= 15, (
        f"openai.types.Batch looks degraded ({len(openai_fields)} fields); "
        "the conformance oracle is not trustworthy."
    )
    missing = openai_fields - ours
    assert not missing, (
        "BatchResponse is missing OpenAI Batch field(s): "
        f"{sorted(missing)}. Mirror them in aibrix/metadata/api/v1/batch.py "
        "(and map them in _batch_job_to_openai_response)."
    )


def test_batchresponse_field_types_are_compatible():
    """For every shared field, our type must be structurally compatible with
    OpenAI's (same shape, or OpenAI typed it as `object`/widened)."""
    ours = BatchResponse.model_fields
    incompatible = []
    for name, ofield in openai_types.Batch.model_fields.items():
        if name not in ours:
            continue  # presence is enforced by the test above
        if not _compatible(ofield.annotation, ours[name].annotation):
            incompatible.append(
                f"{name}: openai={ofield.annotation!r} -> shape "
                f"'{_shape(ofield.annotation)}', "
                f"ours={ours[name].annotation!r} -> shape "
                f"'{_shape(ours[name].annotation)}'"
            )
    assert not incompatible, "Type drift vs OpenAI Batch:\n" + "\n".join(incompatible)


def test_batchrequestcounts_covers_openai():
    """The nested request_counts DTO must also cover OpenAI's."""
    openai_fields = set(openai_types.BatchRequestCounts.model_fields)
    ours = set(BatchRequestCounts.model_fields)
    missing = openai_fields - ours
    assert not missing, f"BatchRequestCounts missing OpenAI field(s): {sorted(missing)}"

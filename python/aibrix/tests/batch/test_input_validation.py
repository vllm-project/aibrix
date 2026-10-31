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

import asyncio
import json
from unittest.mock import Mock

import pytest

from aibrix.batch.storage import input_validation
from aibrix.storage.base import BaseStorage

ENDPOINT = "/v1/completions"


def _request(custom_id, prompt="hello"):
    return json.dumps(
        {
            "custom_id": custom_id,
            "method": "POST",
            "url": ENDPOINT,
            "body": {"model": "test-model", "prompt": prompt},
        }
    )


def _validate(lines):
    async def iter_lines():
        for line in lines:
            yield line

    storage = Mock(spec=BaseStorage)
    storage.readline_iter.return_value = iter_lines()
    return asyncio.run(
        input_validation.validate_batch_input_file(storage, "file-1", ENDPOINT)
    )


@pytest.mark.parametrize(
    "lines, line_num, custom_id",
    [
        ([_request("same", "first"), _request("same", "second")], 2, "same"),
        (
            [_request("same"), "", "  ", _request("other"), _request("same")],
            5,
            "same",
        ),
        (
            [_request("请求"), _request("请求").replace("\\u8bf7\\u6c42", "请求")],
            2,
            "请求",
        ),
    ],
)
def test_reject_duplicate_custom_ids(lines, line_num, custom_id):
    assert _validate(lines) == (
        0,
        f"Line {line_num}: Duplicate custom_id '{custom_id}'",
    )


def test_distinct_custom_ids_remain_case_and_whitespace_sensitive():
    assert _validate([_request("id"), "", _request("ID"), _request("id ")]) == (
        3,
        None,
    )


def test_custom_ids_are_scoped_to_one_validation():
    for _ in range(2):
        assert _validate([_request("same")]) == (1, None)


def test_non_string_custom_id_keeps_validation_error():
    assert _validate([_request(["id"])]) == (
        0,
        "Line 1: 'custom_id' must be a string",
    )


def test_invalid_json_keeps_line_number():
    count, error = _validate([_request("id"), "", "{"])
    assert count == 0
    assert error.startswith("Line 3: Invalid JSON - ")


def test_request_limit_is_preserved(monkeypatch):
    monkeypatch.setattr(input_validation, "MAX_BATCH_REQUESTS", 2)
    assert _validate([_request(str(i)) for i in range(3)]) == (
        3,
        "Batch input contains more than 2 requests",
    )

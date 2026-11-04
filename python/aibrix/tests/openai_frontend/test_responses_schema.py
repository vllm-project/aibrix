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

"""Minimal envelope schemas preserve nested JSON and schema-derived media."""

from pydantic import BaseModel

from aibrix.openai_frontend.schemas import responses
from aibrix.openai_frontend.schemas.media import media_url_fields
from aibrix.openai_frontend.schemas.responses import CreateResponseRequest


def test_responses_wire_roundtrip_and_media_metadata():
    for value in vars(responses).values():
        if (
            isinstance(value, type)
            and issubclass(value, BaseModel)
            and value is not BaseModel
        ):
            value.model_json_schema()
    payload = {
        "input": [
            {
                "role": "user",
                "content": [
                    {
                        "type": "input_image",
                        "image_url": "s3://b/image",
                        "detail": "auto",
                    },
                    {"type": "input_file", "file_id": "file_1"},
                    {"type": "new_content", "custom": {"keep": True}},
                ],
            }
        ],
        "tools": [{"type": "new_tool", "nested": {"keep": [1, 2]}}],
        "vendor_request": {"keep": True},
    }
    assert (
        CreateResponseRequest.model_validate(payload).model_dump(
            mode="json", by_alias=True, exclude_unset=True
        )
        == payload
    )
    fields = media_url_fields(CreateResponseRequest)
    assert fields["input_image"] == ("image_url", "")
    assert fields["input_file"] == ("file_url", "")
    body = {
        "id": "r",
        "object": "response",
        "created_at": 1,
        "model": "m",
        "output": [{"type": "new_output", "data": {"keep": True}}],
        "usage": {"engine_details": {"keep": True}},
    }
    assert (
        responses.Response.model_validate(body).model_dump(exclude_unset=True) == body
    )
    event = {"type": "response.new_event", "data": {"keep": True}}
    assert responses.ResponseStreamEvent.model_validate(event).model_dump() == event

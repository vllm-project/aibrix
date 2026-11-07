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

"""Small, opt-in processor examples shared by demos and contract tests.

Register ``DEMO_PROCESSORS`` explicitly; they are never added to the default
catalog. These examples do not decode or transcode media. File selection uses filename MIME hints for demo
purposes only, not content validation or a security boundary.
"""

from __future__ import annotations

import asyncio
import json
import mimetypes
from copy import deepcopy
from typing import Tuple

from aibrix.openai_frontend.proxy.processors import (
    ProcessorContext,
    ProcessorDefinition,
    ProcessorMedia,
    ProcessorOutput,
    ProcessorPayload,
)
from aibrix.openai_frontend.utils.utils import ClientError

_COPY_CHUNK_BYTES = 64 * 1024
_NO_ARGUMENTS = {"type": "object", "additionalProperties": False}


async def noop(ctx: ProcessorContext) -> ProcessorOutput:
    """Preserve the scoped target, demonstrating validation/no-op success."""
    return ProcessorOutput()


async def copy_media(ctx: ProcessorContext) -> ProcessorOutput:
    """Copy selected files to the invocation directory in their original order.

    An index-based filename prevents collisions between equally named sources.
    Unselected files stay unchanged; cancellation closes the handles and removes
    this invocation's partial copies before propagating to the executor.
    """
    if not isinstance(ctx.target, ProcessorMedia):
        raise ClientError("copy_media requires local media")
    paths = []
    created = []
    try:
        for index, source in enumerate(ctx.target.paths):
            mime = mimetypes.guess_type(source.name)[0]
            kind = mime.split("/", 1)[0] if mime else None
            if ctx.media_types and kind not in ctx.media_types:
                paths.append(source)
                continue
            output = ctx.workdir / f"{index:04d}{source.suffix}"
            with ctx.target.open(source) as reader, output.open("xb") as writer:
                created.append(output)
                while chunk := reader.read(_COPY_CHUNK_BYTES):
                    writer.write(chunk)
                    await asyncio.sleep(0)
            paths.append(output)
        if not created:
            raise ClientError("copy_media found no files matching media_types")
        return ProcessorOutput(ProcessorMedia(tuple(paths)))
    except BaseException:
        for output in created:
            output.unlink(missing_ok=True)
        raise


async def replace_payload(ctx: ProcessorContext) -> ProcessorOutput:
    """Replace payload bytes with UTF-8 text, including an explicit empty value."""
    if not isinstance(ctx.target, ProcessorPayload):
        raise ClientError("replace_payload requires a payload")
    return ProcessorOutput(ProcessorPayload(ctx.arguments["content"].encode("utf-8")))


async def merge_json(ctx: ProcessorContext) -> ProcessorOutput:
    """Merge arguments.fields into the JSON object; there are no other options."""
    if not isinstance(ctx.target, ProcessorPayload):
        raise ClientError("merge_json requires a payload")
    try:
        payload = json.loads(ctx.target.content)
    except (ValueError, UnicodeError) as exc:
        raise ClientError("merge_json requires valid JSON") from exc
    if not isinstance(payload, dict):
        raise ClientError("merge_json requires a JSON object")
    payload.update(deepcopy(ctx.arguments["fields"]))
    content = json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode(
        "utf-8"
    )
    return ProcessorOutput(ProcessorPayload(content))


# These are argument schemas, not execution options. No-op/copy accept no
# arguments; replace uses only content; merge uses only fields.
DEMO_PROCESSORS: Tuple[ProcessorDefinition, ...] = (
    ProcessorDefinition(
        "demo.pre.noop",
        "pre",
        "input_media",
        _NO_ARGUMENTS,
        noop,
    ),
    ProcessorDefinition(
        "demo.post.noop",
        "post",
        "output_payload",
        _NO_ARGUMENTS,
        noop,
    ),
    ProcessorDefinition(
        "demo.pre.copy",
        "pre",
        "input_media",
        _NO_ARGUMENTS,
        copy_media,
    ),
    ProcessorDefinition(
        "demo.post.copy",
        "post",
        "output_media",
        _NO_ARGUMENTS,
        copy_media,
    ),
    ProcessorDefinition(
        "demo.payload.replace",
        "post",
        "output_payload",
        {
            "type": "object",
            "properties": {"content": {"type": "string"}},
            "required": ["content"],
            "additionalProperties": False,
        },
        replace_payload,
    ),
    ProcessorDefinition(
        "demo.json.merge",
        "post",
        "output_payload",
        {
            "type": "object",
            "properties": {"fields": {"type": "object"}},
            "required": ["fields"],
            "additionalProperties": False,
        },
        merge_json,
    ),
)

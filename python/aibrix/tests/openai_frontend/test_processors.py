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

from __future__ import annotations

import asyncio
import json
import tempfile
import time
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock
from urllib.parse import urlparse

import httpx
import pytest
from pydantic import ValidationError

from aibrix.openai_frontend.common.artifact_fetcher import FetchResult
from aibrix.openai_frontend.engine.proxy_engine import ProxyEngine
from aibrix.openai_frontend.proxy.output.path_ref import PathRefs
from aibrix.openai_frontend.proxy.processors import (
    ProcessorContext,
    ProcessorDefinition,
    ProcessorExecutionError,
    ProcessorExecutor,
    ProcessorMedia,
    ProcessorOutput,
    ProcessorPayload,
    ProcessorRegistry,
    ProcessorState,
)
from aibrix.openai_frontend.proxy.processors.demo_processors import DEMO_PROCESSORS
from aibrix.openai_frontend.schemas.generation import VideoResponse
from aibrix.openai_frontend.schemas.openai import CreateChatCompletionRequest
from aibrix.openai_frontend.schemas.output import OutputRequest, OutputResponse
from aibrix.openai_frontend.schemas.processors import (
    MAX_PROCESSORS_PER_STAGE,
    ProcessorPipeline,
)
from aibrix.openai_frontend.utils.utils import ClientError


def _call(name: str, **processor) -> dict:
    return {"type": "processor", "processor": {"name": name, **processor}}


class TestProcessorSchema:
    @pytest.mark.parametrize(
        "raw",
        [
            {"pre": [_call("demo.pre.noop")], "extra": 1},
            {
                "pre": [
                    {
                        "type": "processor",
                        "processor": {"name": "demo.pre.noop", "arg": {}},
                    }
                ]
            },
            {"pre": [], "post": []},
            {"pre": [_call("demo.pre.noop")] * (MAX_PROCESSORS_PER_STAGE + 1)},
            {"pre": [_call("demo.pre.noop", media_types=["video", "video"])]},
        ],
    )
    def test_invalid_pipeline_is_rejected(self, raw):
        with pytest.raises(ValidationError):
            ProcessorPipeline.model_validate(raw)

    @pytest.mark.parametrize(
        "timeout", [0, -1, True, "1", float("nan"), float("inf"), -float("inf")]
    )
    def test_invalid_timeout_is_rejected(self, timeout):
        with pytest.raises(ValidationError):
            ProcessorPipeline.model_validate(
                {"pre": [_call("demo.pre.noop", timeout_seconds=timeout)]}
            )

    def test_request_model_carries_the_extension_to_the_proxy(self):
        request = CreateChatCompletionRequest.model_validate(
            {
                "model": "m",
                "messages": [{"role": "user", "content": "hi"}],
                "processors": {
                    "pre": [_call("demo.pre.copy", arguments={}, timeout_seconds=2)]
                },
            }
        )
        dumped = request.model_dump(exclude_unset=True, mode="json")
        assert dumped["processors"]["pre"][0]["processor"]["name"] == "demo.pre.copy"
        assert dumped["processors"]["pre"][0]["processor"]["timeout_seconds"] == 2
        assert request.processors.post == []


class TestProcessorRegistry:
    def test_unknown_name_lists_the_catalogue(self):
        pipeline = ProcessorPipeline.model_validate({"pre": [_call("unknown")]})
        with pytest.raises(ValueError, match="available: demo.pre.noop"):
            ProcessorRegistry(DEMO_PROCESSORS).validate(pipeline)
        with pytest.raises(ValueError, match="available: none"):
            ProcessorRegistry(()).validate(pipeline)

    @pytest.mark.parametrize(
        "stage,name,arguments,message",
        [
            ("pre", "demo.json.merge", {}, "belongs to stage 'post'"),
            (
                "post",
                "demo.json.merge",
                {"fields": {}, "field": {}},
                "Additional properties",
            ),
            ("post", "demo.json.merge", {}, "required property"),
            (
                "pre",
                "demo.pre.copy",
                {"media_types": ["video"]},
                "Additional properties",
            ),
            ("post", "demo.payload.replace", {"content": None}, "is not of type"),
        ],
    )
    def test_invalid_catalog_calls_are_rejected(self, stage, name, arguments, message):
        with pytest.raises(ValueError, match=message):
            ProcessorRegistry(DEMO_PROCESSORS).validate(
                ProcessorPipeline.model_validate(
                    {stage: [_call(name, arguments=arguments)]}
                )
            )

    @pytest.mark.parametrize("value", [["banana"], ["ab"], ["toolong"], [1], ["ok"]])
    def test_nested_argument_constraints_are_enforced(self, value):
        registry = ProcessorRegistry(
            [
                ProcessorDefinition(
                    "custom",
                    "pre",
                    "input_media",
                    {
                        "type": "object",
                        "additionalProperties": False,
                        "properties": {
                            "values": {
                                "type": "array",
                                "items": {
                                    "type": "string",
                                    "enum": ["ok", "ab", "toolong"],
                                    "minLength": 2,
                                    "maxLength": 3,
                                    "pattern": "^ok$",
                                },
                            }
                        },
                    },
                )
            ]
        )
        pipeline = ProcessorPipeline.model_validate(
            {"pre": [_call("custom", arguments={"values": value})]}
        )
        if value == ["ok"]:
            registry.validate(pipeline)
        else:
            with pytest.raises(ValueError, match=r"arguments.values\[0\]"):
                registry.validate(pipeline)

    def test_allowlist_narrows_available_names(self):
        registry = ProcessorRegistry(DEMO_PROCESSORS, allowlist=["demo.json.merge"])
        with pytest.raises(ValueError, match="not available"):
            registry.validate(
                ProcessorPipeline.model_validate(
                    {"pre": [_call("demo.pre.copy", arguments={})]}
                )
            )
        registry.validate(
            ProcessorPipeline.model_validate(
                {"post": [_call("demo.json.merge", arguments={"fields": {}})]}
            )
        )


class TestDemoProcessors:
    def test_post_copy_preserves_payload_and_handles_duplicate_basenames(
        self, tmp_path
    ):
        sources = []
        for folder in ("a", "b"):
            directory = tmp_path / folder
            directory.mkdir()
            source = directory / "source.png"
            source.write_bytes(folder.encode())
            sources.append(source)
        registry = ProcessorRegistry(DEMO_PROCESSORS)
        pipeline = ProcessorPipeline.model_validate({"post": [_call("demo.post.copy")]})
        registry.validate(pipeline)
        result = asyncio.run(
            ProcessorExecutor(registry).execute(
                "req",
                tmp_path / "work",
                pipeline,
                "post",
                ProcessorState(paths=tuple(sources), payload=b"result"),
            )
        )
        assert result.payload == b"result"
        assert [p.read_bytes() for p in result.paths] == [b"a", b"b"]
        assert len(set(result.paths)) == 2

    @pytest.mark.asyncio
    async def test_copy_cancellation_removes_partial_files(self, tmp_path):
        source = tmp_path / "source.mp4"
        source.write_bytes(b"raw" * 100000)
        work = tmp_path / "work"
        registry = ProcessorRegistry(DEMO_PROCESSORS)
        pipeline = ProcessorPipeline.model_validate({"pre": [_call("demo.pre.copy")]})
        registry.validate(pipeline)
        task = asyncio.create_task(
            ProcessorExecutor(registry).execute(
                "req",
                work,
                pipeline,
                "pre",
                ProcessorState(paths=(source,)),
            )
        )
        # copy_media yields after the first bounded chunk, before completion.
        await asyncio.sleep(0)
        await asyncio.sleep(0)
        outdir = work / "pre" / "00-demo.pre.copy"
        assert list(outdir.iterdir()) and not task.done()
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        assert not list(outdir.iterdir())
        assert source.read_bytes() == b"raw" * 100000

    @pytest.mark.parametrize("payload", [b"not JSON", b"[]"])
    def test_json_merge_rejects_non_objects(self, tmp_path, payload):
        registry = ProcessorRegistry(DEMO_PROCESSORS)
        pipeline = ProcessorPipeline.model_validate(
            {
                "post": [
                    _call("demo.json.merge", arguments={"fields": {"checked": True}}),
                ]
            }
        )
        registry.validate(pipeline)
        with pytest.raises(ProcessorExecutionError, match="rejected output"):
            asyncio.run(
                ProcessorExecutor(registry).execute(
                    "req",
                    tmp_path / "work",
                    pipeline,
                    "post",
                    ProcessorState(payload=payload),
                )
            )


class TestProcessorExecutor:
    def test_scope_and_registration_contracts(self):
        with pytest.raises(ValueError, match="stage/scope"):
            ProcessorDefinition("bad", "pre", "output_payload", {})
        definition = ProcessorDefinition("check", "post", "output_payload", {})
        with pytest.raises(ValueError, match="already registered"):
            ProcessorRegistry([definition, definition])
        registry = ProcessorRegistry([definition])
        with pytest.raises(ValueError, match="only valid for media"):
            registry.validate(
                ProcessorPipeline.model_validate(
                    {"post": [_call("check", media_types=["video"])]}
                )
            )

    def test_scoped_replacements_chain_and_preserve_other_results(self, tmp_path):
        source = tmp_path / "source.mp4"
        source.write_bytes(b"raw")

        async def clear_media(ctx: ProcessorContext) -> ProcessorOutput:
            # Deliberate edge case: a post processor may explicitly clear media.
            assert isinstance(ctx.target, ProcessorMedia)
            return ProcessorOutput(ProcessorMedia(()))

        registry = ProcessorRegistry(DEMO_PROCESSORS)
        registry.define(
            ProcessorDefinition("clear_media", "post", "output_media", {}, clear_media)
        )
        pipeline = ProcessorPipeline.model_validate(
            {
                "post": [
                    _call("demo.post.noop"),
                    _call("clear_media"),
                    _call("demo.payload.replace", arguments={"content": ""}),
                    _call("demo.post.noop"),
                ]
            }
        )
        registry.validate(pipeline)
        result = asyncio.run(
            ProcessorExecutor(registry).execute(
                "req",
                tmp_path / "work",
                pipeline,
                "post",
                ProcessorState(paths=(source,), payload=b"result"),
            )
        )
        assert result.paths == () and result.payload == b""

    def test_pre_noop_and_multi_step_mapping(self, tmp_path):
        sources = (tmp_path / "a.mp4", tmp_path / "b.mp4")
        for path in sources:
            path.write_bytes(b"raw")
        registry = ProcessorRegistry(DEMO_PROCESSORS)
        pipeline = ProcessorPipeline.model_validate(
            {
                "pre": [
                    _call("demo.pre.noop"),
                    _call("demo.pre.copy"),
                    _call("demo.pre.copy"),
                ]
            }
        )
        registry.validate(pipeline)
        result = asyncio.run(
            ProcessorExecutor(registry).execute(
                "req",
                tmp_path / "work",
                pipeline,
                "pre",
                ProcessorState(paths=sources),
            )
        )
        assert len(result.paths) == len(sources)
        assert tuple(result.replacements[p] for p in sources) == result.paths
        assert all(p.read_bytes() == b"raw" for p in result.paths)
        assert all(p.parent.name == "02-demo.pre.copy" for p in result.paths)
        assert all(p.read_bytes() == b"raw" for p in sources)

    @pytest.mark.parametrize(
        "case", ["wrong_kind", "missing", "escape", "clear_pre", "split", "merge"]
    )
    def test_invalid_replacements_are_rejected(self, tmp_path, case):
        source = tmp_path / "source.mp4"
        source.write_bytes(b"raw")

        async def run(ctx: ProcessorContext) -> ProcessorOutput:
            if case == "wrong_kind":
                return ProcessorOutput(ProcessorPayload(b"wrong"))
            if case == "missing":
                return ProcessorOutput(ProcessorMedia((ctx.workdir / "missing.mp4",)))
            if case == "escape":
                outside = tmp_path / "outside.mp4"
                outside.write_bytes(b"outside")
                return ProcessorOutput(ProcessorMedia((outside,)))
            if case in ("split", "merge"):
                output = ctx.workdir / "new.mp4"
                output.write_bytes(b"processed")
                count = 2 if case == "split" else 1
                return ProcessorOutput(ProcessorMedia((output,) * count))
            return ProcessorOutput(ProcessorMedia(()))

        registry = ProcessorRegistry(
            [ProcessorDefinition("test", "pre", "input_media", {}, run)]
        )
        pipeline = ProcessorPipeline.model_validate({"pre": [_call("test")]})
        with pytest.raises(ProcessorExecutionError) as error:
            asyncio.run(
                ProcessorExecutor(registry).execute(
                    "req",
                    tmp_path / "work",
                    pipeline,
                    "pre",
                    ProcessorState(paths=(source,) * (2 if case == "merge" else 1)),
                )
            )
        assert error.value.status_code == 502

    @pytest.mark.parametrize(
        "stage,crash,status",
        [("pre", False, 400), ("post", False, 502), ("pre", True, 502)],
    )
    def test_processor_failures_have_explicit_http_semantics(
        self, tmp_path, stage, crash, status
    ):
        async def run(ctx):
            if crash:
                raise RuntimeError("crash")
            raise ClientError("rejected")

        scope = "input_media" if stage == "pre" else "output_payload"
        registry = ProcessorRegistry(
            [ProcessorDefinition("test", stage, scope, {}, run)]
        )
        with pytest.raises(
            ClientError if status == 400 else ProcessorExecutionError
        ) as error:
            asyncio.run(
                ProcessorExecutor(registry).execute(
                    "req",
                    tmp_path / "work",
                    ProcessorPipeline.model_validate({stage: [_call("test")]}),
                    stage,
                    ProcessorState(paths=(tmp_path / "source.mp4",), payload=b"{}"),
                )
            )
        assert error.value.status_code == status

    def test_media_selection_preserves_unselected_inputs(self, tmp_path):
        image, video = tmp_path / "a.png", tmp_path / "b.mp4"
        image.write_bytes(b"image")
        video.write_bytes(b"video")
        registry = ProcessorRegistry(DEMO_PROCESSORS)
        pipeline = ProcessorPipeline.model_validate(
            {
                "pre": [
                    _call("demo.pre.copy", media_types=["video"]),
                ]
            }
        )
        registry.validate(pipeline)
        result = asyncio.run(
            ProcessorExecutor(registry).execute(
                "req",
                tmp_path / "work",
                pipeline,
                "pre",
                ProcessorState(paths=(image, video)),
            )
        )
        assert result.paths[0] == image
        assert result.replacements[image] == image
        assert result.paths[1] != video
        assert result.paths[1].read_bytes() == b"video"

    def test_each_call_receives_its_own_timeout(self, tmp_path):
        budgets = []

        async def run(ctx):
            budgets.append(ctx.deadline - time.monotonic())
            assert "timeout_seconds" not in ctx.arguments
            return ProcessorOutput()

        registry = ProcessorRegistry(
            [ProcessorDefinition("test", "post", "output_payload", {}, run)]
        )
        pipeline = ProcessorPipeline.model_validate(
            {
                "post": [
                    _call("test"),
                    _call("test", timeout_seconds=None),
                    _call("test", timeout_seconds=2),
                    _call("test", timeout_seconds=0.5),
                ]
            }
        )
        registry.validate(pipeline)
        asyncio.run(
            ProcessorExecutor(registry, 1).execute(
                "req",
                tmp_path / "work",
                pipeline,
                "post",
                ProcessorState(payload=b"{}"),
            )
        )
        assert budgets == pytest.approx([1, 1, 2, 0.5], abs=0.1)

    @pytest.mark.parametrize(
        "stage,timeout",
        [("pre", 0.01), ("post", None), ("pre", 1e-30), ("post", 1e-30)],
    )
    def test_arguments_are_isolated_and_timeout_cancels_runner(
        self, tmp_path, stage, timeout
    ):
        invoked = []
        cancelled = []

        def run(ctx: ProcessorContext):
            invoked.append(True)

            async def wait() -> ProcessorOutput:
                ctx.arguments["schema"]["changed"] = True
                try:
                    await asyncio.Event().wait()
                finally:
                    cancelled.append(True)
                return ProcessorOutput()

            return wait()

        scope = "input_media" if stage == "pre" else "output_payload"
        registry = ProcessorRegistry(
            [ProcessorDefinition("test", stage, scope, {}, run)]
        )
        pipeline = ProcessorPipeline.model_validate(
            {stage: [_call("test", arguments={"schema": {}}, timeout_seconds=timeout)]}
        )
        with pytest.raises(ProcessorExecutionError, match="timed out") as error:
            asyncio.run(
                ProcessorExecutor(registry, 0.01 if timeout is None else 1).execute(
                    "req",
                    tmp_path / "work",
                    pipeline,
                    stage,
                    ProcessorState(paths=(tmp_path / "source.png",), payload=b"{}"),
                )
            )
        assert error.value.status_code == 502
        expected = [] if timeout == 1e-30 else [True]
        assert invoked == expected and cancelled == expected
        assert pipeline.calls(stage)[0].processor.arguments == {"schema": {}}


class TestEngineProcessorWiring:
    @pytest.mark.parametrize(
        "enabled,stage,stream,background,output,response_model,message",
        [
            (False, "pre", False, False, False, None, "not enabled"),
            (True, "pre", True, False, True, None, "stream"),
            (True, "pre", False, True, True, None, "background"),
            (True, "post", False, False, False, None, "requires 'output'"),
            (True, "pre", False, False, False, None, "engine-native"),
            (True, "post", False, False, False, VideoResponse, "requires 'output'"),
        ],
    )
    def test_request_conflicts_are_rejected(
        self, enabled, stage, stream, background, output, response_model, message
    ):
        engine = ProxyEngine(["unused"], enable_processors=enabled)
        name = "demo.pre.copy" if stage == "pre" else "demo.json.merge"
        try:
            with pytest.raises(ClientError, match=message):
                engine._parse_processors(
                    {stage: [_call(name)]},
                    stream=stream,
                    has_output=output,
                    background=background,
                    response_model=response_model,
                )
        finally:
            asyncio.run(engine.aclose())

    def test_pre_requires_local_media_inputs(self):
        engine = ProxyEngine(
            engine_cmd=["unused"],
            enable_processors=True,
            processor_registry=ProcessorRegistry(DEMO_PROCESSORS),
        )
        pipeline = ProcessorPipeline.model_validate(
            {"pre": [_call("demo.pre.copy", arguments={})]}
        )

        async def _run() -> None:
            with tempfile.TemporaryDirectory() as work:
                context = SimpleNamespace(
                    req_id="req-1",
                    inputs=[],
                    processors=pipeline,
                    processor_dir=SimpleNamespace(name=work),
                )
                with pytest.raises(ClientError, match="local media input"):
                    await engine._apply_pre_processors(context, {})

        try:
            asyncio.run(_run())
        finally:
            asyncio.run(engine.aclose())

    @pytest.mark.parametrize("spelling", ["encoded", "decoded"])
    def test_pre_rewrites_payload_paths_to_processor_output(self, spelling):
        engine = ProxyEngine(
            engine_cmd=["unused"],
            enable_processors=True,
            processor_registry=ProcessorRegistry(DEMO_PROCESSORS),
        )
        pipeline = ProcessorPipeline.model_validate({"pre": [_call("demo.pre.copy")]})
        engine._processor_registry.validate(pipeline)

        async def _run() -> None:
            with tempfile.TemporaryDirectory() as media:
                source = Path(media) / "input space.mp4"
                source.write_bytes(b"raw")
                uri = source.as_uri()
                if spelling == "decoded":
                    uri = uri.replace("%20", " ")
                with tempfile.TemporaryDirectory() as work:
                    context = type(
                        "Ctx",
                        (),
                        {
                            "req_id": "req-1",
                            "inputs": [
                                FetchResult(uri, uri, None, source.stat().st_size)
                            ],
                            "processors": pipeline,
                            "processor_dir": type("Dir", (), {"name": work})(),
                            "sources": PathRefs(),
                        },
                    )()
                    payload = {"messages": [{"video_url": {"url": uri}}]}
                    rewritten = await engine._apply_pre_processors(context, payload)
                    rewritten_uri = rewritten["messages"][0]["video_url"]["url"]
                    assert rewritten_uri != uri
                    assert Path(urlparse(rewritten_uri).path).read_bytes() == (b"raw")

        try:
            asyncio.run(_run())
        finally:
            asyncio.run(engine.aclose())

    def test_post_pipeline_chains_payload_before_write(self, tmp_path):
        fields = {"label": "示例", "nested": {"items": [1]}}
        registry = ProcessorRegistry(DEMO_PROCESSORS)
        engine = ProxyEngine(
            ["unused"], enable_processors=True, processor_registry=registry
        )
        pipeline = ProcessorPipeline.model_validate(
            {
                "post": [
                    _call(
                        "demo.payload.replace", arguments={"content": '{"first":true}'}
                    ),
                    _call("demo.json.merge", arguments={"fields": fields}),
                    _call("demo.json.merge", arguments={"fields": {"label": "final"}}),
                    _call("demo.post.noop"),
                ]
            }
        )
        registry.validate(pipeline)
        engine._write_output = AsyncMock(return_value=OutputResponse(url=str(tmp_path)))

        async def run():
            try:
                with tempfile.TemporaryDirectory() as work:
                    await engine._persist_output(
                        "req",
                        OutputRequest(url=str(tmp_path)),
                        httpx.Response(
                            200,
                            json={
                                "id": "response",
                                "choices": [],
                                "usage": {"total_tokens": 1},
                            },
                        ),
                        None,
                        processors=pipeline,
                        processor_dir=SimpleNamespace(name=work),
                    )
                engine._write_output.assert_awaited_once()
                assert json.loads(engine._write_output.call_args.args[3]) == {
                    "first": True,
                    "label": "final",
                    "nested": {"items": [1]},
                }
                assert pipeline.post[1].processor.arguments["fields"] == fields
            finally:
                await engine.aclose()

        asyncio.run(run())

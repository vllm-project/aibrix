# Copyright 2026 The Aibrix Team.
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy at http://www.apache.org/licenses/LICENSE-2.0

"""Real files and proxy persistence tests for the executable built-ins."""

import asyncio
import base64
import dataclasses
import errno
import json
import mimetypes
import os
import tarfile
import tempfile
import threading
import zipfile
from io import BytesIO
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock
from urllib.parse import unquote, urlparse

import httpx
import pytest
from PIL import Image, ImageCms

from aibrix.openai_frontend.common.artifact_fetcher import FetchResult
from aibrix.openai_frontend.engine.proxy_engine import EngineState, ProxyEngine
from aibrix.openai_frontend.frontend.fastapi_frontend import FastApiFrontend
from aibrix.openai_frontend.proxy.output.output_writer import (
    OutputWriteError,
    OutputWriter,
)
from aibrix.openai_frontend.proxy.output.path_ref import PathRef, PathRefs
from aibrix.openai_frontend.proxy.processors import (
    ProcessorDefinition,
    ProcessorExecutionError,
    ProcessorExecutor,
    ProcessorMedia,
    ProcessorOutput,
    ProcessorRegistry,
    ProcessorState,
    builtin_processors,
)
from aibrix.openai_frontend.schemas.generation import ImageGenerationResponse
from aibrix.openai_frontend.schemas.output import OutputRequest, OutputResponse
from aibrix.openai_frontend.schemas.processors import ProcessorPipeline
from aibrix.openai_frontend.utils.utils import ClientError


def _call(name, **arguments):
    return {"type": "processor", "processor": {"name": name, "arguments": arguments}}


def _pipeline(stage, calls):
    pipeline = ProcessorPipeline.model_validate({stage: calls})
    ProcessorRegistry().validate(pipeline)
    return pipeline


def _image(path, size=(200, 100)):
    Image.new("RGBA" if path.suffix == ".png" else "RGB", size, "red").save(path)
    return path


def _http_engine(upstream, **options):
    engine = ProxyEngine(["unused"], enable_processors=True, **options)
    engine._set_state(EngineState.READY)
    engine._subprocess._client = httpx.AsyncClient(
        base_url="http://engine", transport=httpx.MockTransport(upstream)
    )
    engine._subprocess._client_loop = asyncio.get_running_loop()
    return engine


async def _post(engine, endpoint, body):
    async with httpx.AsyncClient(
        base_url="http://frontend",
        transport=httpx.ASGITransport(app=FastApiFrontend(engine=engine).app),
    ) as client:
        return await client.post(endpoint, json=body)


@pytest.mark.parametrize("format_name", ["zip", "tar", "tar.gz"])
def test_compress_roundtrip_and_proxy_persistence(tmp_path, format_name):
    first = _image(tmp_path / "结果 1.png")
    second = tmp_path / "other" / "结果 1.png"
    second.parent.mkdir()
    _image(second, (100, 200))
    images = (
        first,
        second,
        _image(tmp_path / "my photo.png"),
        _image(tmp_path / "a\\b.png"),
    )
    originals = [p.read_bytes() for p in images]
    members = [
        "0000-结果 1.png",
        "0001-结果 1.png",
        "0002-my photo.png",
        "0003-a_b.png",
    ]
    video = tmp_path / "video.mp4"
    video.write_bytes(b"unchanged video")
    envelope = {
        "id": "images",
        "created": 1,
        "data": [
            {"file_path": str(first)},
            *({"file_path": str(image)} for image in images),
            {"file_path": str(video)},
        ],
        "usage": {"tokens": 2},
    }
    parsed = ImageGenerationResponse.model_validate(envelope)
    engine = ProxyEngine(["unused"], enable_processors=True)
    sources = {}
    engine._output_writer.remember_sources([str(p) for p in (*images, video)], sources)
    call = _call("files.compress", format=format_name)
    call["processor"]["media_types"] = ["image"]

    async def run():
        try:
            with tempfile.TemporaryDirectory() as work:
                response = await engine._persist_output(
                    "req",
                    OutputRequest(url=str(tmp_path / "out")),
                    httpx.Response(200, json=envelope),
                    parsed,
                    sources,
                    processors=_pipeline("post", [call]),
                    processor_dir=SimpleNamespace(name=work),
                )
            receipt = json.loads(response.body)
            assert receipt["usage"] == {"tokens": 2} and receipt["data"] == []
            names = receipt["output"]["artifacts"]
            assert names == ["images.json", f"images-0.{format_name}", "images-1.mp4"]
            assert (tmp_path / "out" / names[2]).read_bytes() == b"unchanged video"
            assert json.loads((tmp_path / "out" / names[0]).read_bytes()) == {
                "data": envelope["data"]
            }
            path = tmp_path / "out" / names[1]
            if format_name == "zip":
                with zipfile.ZipFile(path) as archive:
                    assert archive.namelist() == members
                    assert [archive.read(n) for n in archive.namelist()] == originals
            else:
                with tarfile.open(path) as archive:
                    assert archive.getnames() == members
                    assert all(member.isfile() for member in archive.getmembers())
                    assert [
                        archive.extractfile(n).read() for n in archive.getnames()
                    ] == originals
            assert not any(p.exists() for p in (*images, video))
        finally:
            await engine.aclose()

    asyncio.run(run())


def test_default_catalog_and_invalid_arguments():
    from aibrix.openai_frontend.proxy.processors.demo_processors import DEMO_PROCESSORS

    assert all(not name.startswith("demo.") for name in ProcessorRegistry().names())
    assert all(d.implemented for d in DEMO_PROCESSORS)
    registry = ProcessorRegistry(allowlist=["image.resize"])
    assert registry.names() == ("image.resize",)
    assert (
        registry.resolve("image.resize", "pre").run
        is registry.resolve("image.resize", "post").run
    )
    invalid = [
        _call("files.compress", format="rar"),
        _call("files.compress", paths=["/etc/passwd"]),
        _call("json.validate", schema={"type": "unknown"}),
        _call(
            "json.validate",
            schema={"$schema": "http://json-schema.org/draft-07/schema#"},
        ),
        _call("image.resize", max_edge=0),
        _call("image.resize", max_edge=True),
        _call("image.resize", max_edge=100, width=50),
    ]
    for call in invalid:
        with pytest.raises(ValueError):
            _pipeline("post", [call])
    # A declared capability must reject media types it cannot select before
    # execution turns the mismatch into a 502, while a mixed selection still
    # keeps the unselected files untouched.
    with pytest.raises(ValueError):
        _pipeline(
            "post",
            [
                {
                    "type": "processor",
                    "processor": {
                        "name": "image.resize",
                        "arguments": {"max_edge": 50},
                        "media_types": ["video"],
                    },
                }
            ],
        )
    _pipeline(
        "post",
        [
            {
                "type": "processor",
                "processor": {
                    "name": "image.resize",
                    "arguments": {"max_edge": 50},
                    "media_types": ["image", "video"],
                },
            }
        ],
    )
    # Boolean schemas are valid Draft 2020-12 documents.
    _pipeline("post", [_call("json.validate", schema=True)])
    payload_call = _call("json.validate", schema=True)
    payload_call["processor"]["media_types"] = []
    with pytest.raises(ValueError, match="only valid for media"):
        _pipeline("post", [payload_call])


@pytest.mark.parametrize("media_types", [None, []])
@pytest.mark.parametrize("stage", ["pre", "post"])
def test_resize_real_images_and_pre_references(tmp_path, stage, media_types):
    large = _image(tmp_path / "large.png")
    small = _image(tmp_path / "small.jpg", (20, 10))
    video = tmp_path / "video.mp4"
    video.write_bytes(b"unchanged non-image")
    original = large.read_bytes()
    engine = ProxyEngine(["unused"], enable_processors=True)
    call = _call("image.resize", max_edge=50)
    if media_types is not None:
        call["processor"]["media_types"] = media_types
    pipeline = _pipeline(stage, [call])

    async def run():
        try:
            with tempfile.TemporaryDirectory() as work:
                if stage == "pre":
                    uris = [p.as_uri() for p in (large, small, video)]
                    context = SimpleNamespace(
                        req_id="req",
                        inputs=[FetchResult(u, u, None, 1) for u in uris],
                        processors=pipeline,
                        processor_dir=SimpleNamespace(name=work),
                        sources=PathRefs(),
                    )
                    payload = {"images": uris[:2], "video": uris[2]}
                    rewritten = await engine._apply_pre_processors(context, payload)
                    path = Path(unquote(urlparse(rewritten["images"][0]).path))
                    assert (
                        rewritten["images"][1] == uris[1]
                        and rewritten["video"] == uris[2]
                    )
                else:
                    state = await engine._processor_executor.execute(
                        "req",
                        Path(work),
                        pipeline,
                        stage,
                        ProcessorState(
                            paths=(large, small, video), payload=b"original"
                        ),
                    )
                    path = state.paths[0]
                    assert (
                        state.paths[1:] == (small, video)
                        and state.payload == b"original"
                    )
                with Image.open(path) as resized:
                    assert resized.size == (50, 25) and resized.mode == "RGBA"
                assert large.read_bytes() == original
        finally:
            await engine.aclose()

    asyncio.run(run())


@pytest.mark.parametrize(
    "source_name,format_name,source_mime,suffix",
    [
        ("photo.jpg", "JPEG", "image/jpeg", ".jpg"),
        ("photo.jfif", "JPEG", "image/jpeg", ".jfif"),
        ("photo.jfif", "JPEG", None, ".jpg"),
        ("photo", "JPEG", None, ".jpg"),
        ("still.apng", "PNG", "image/apng", ".apng"),
        ("still.apng", "PNG", None, ".png"),
    ],
)
@pytest.mark.parametrize("size", [(20, 10), (200, 100)])
def test_resize_output_name_is_writer_accepted(
    tmp_path, monkeypatch, source_name, format_name, source_mime, suffix, size
):
    source = tmp_path / source_name
    # System MIME databases differ; exercise both accepted and unknown aliases.
    mimetypes.init()
    if source.suffix:
        if source_mime is None:
            monkeypatch.delitem(mimetypes.types_map, source.suffix, raising=False)
        else:
            monkeypatch.setitem(mimetypes.types_map, source.suffix, source_mime)
    icc = ImageCms.ImageCmsProfile(ImageCms.createProfile("sRGB")).tobytes()
    Image.new("RGB", size, "red").save(
        source, format=format_name, quality=95, icc_profile=icc
    )
    original = source.read_bytes()
    with Image.open(source) as image:
        quantization = image.quantization if format_name == "JPEG" else None
    pipeline = _pipeline("post", [_call("image.resize", max_edge=40)])
    writer = OutputWriter()
    sources = {}
    writer.remember_sources([str(source)], sources)

    async def run():
        with tempfile.TemporaryDirectory() as work:
            state = await ProcessorExecutor(ProcessorRegistry()).execute(
                "req",
                Path(work),
                pipeline,
                "post",
                ProcessorState(paths=(source,)),
                source_identities=sources,
            )
            output = state.paths[0]
            assert output.suffix == suffix
            if max(size) <= 40:
                assert output.read_bytes() == original
                assert (output == source) == (source.suffix == suffix)
            receipt, _ = await writer.write_bundle(
                OutputRequest(url=str(tmp_path / "out")),
                None,
                "req.json",
                [str(output)],
                dict(state.source_identities),
                processor_workdir=Path(work),
            )
            assert receipt.artifacts == [f"req-0{suffix}"]
            persisted = tmp_path / "out" / receipt.artifacts[0]
            with Image.open(persisted) as image:
                image.load()
                assert image.size == (size if max(size) <= 40 else (40, 20))
                assert image.format == format_name
                assert image.info["icc_profile"] == icc
                if format_name == "JPEG":
                    assert image.quantization == quantization
            if max(size) <= 40:
                assert persisted.read_bytes() == original

    asyncio.run(run())


@pytest.mark.parametrize(
    "content,remote",
    [
        (b'{"items":[1,2]}', False),
        (b'{"items":["bad"]}', False),
        (b"not json", False),
        (b'{"items":[NaN]}', False),
        (b"{}", True),
    ],
)
def test_json_validation_preserves_payload_or_never_writes(tmp_path, content, remote):
    schema = {
        "type": "object",
        "properties": {"items": {"type": "array", "items": {"$ref": "#/$defs/int"}}},
        "$defs": {"int": {"type": "integer"}},
        "required": ["items"],
    }
    if remote:
        schema = {"$ref": "http://127.0.0.1/private"}
    pipeline = _pipeline("post", [_call("json.validate", schema=schema)])
    engine = ProxyEngine(["unused"], enable_processors=True)
    engine._output_payload = lambda *args: ({}, {"id": "req"}, content)
    engine._write_output = AsyncMock(return_value=OutputResponse(url=str(tmp_path)))

    async def run():
        try:
            with tempfile.TemporaryDirectory() as work:
                if content == b'{"items":[1,2]}' and not remote:
                    await engine._persist_output(
                        "req",
                        OutputRequest(url=str(tmp_path)),
                        httpx.Response(200, json={}),
                        None,
                        processors=pipeline,
                        processor_dir=SimpleNamespace(name=work),
                    )
                    assert engine._write_output.call_args.args[3] == content
                else:
                    with pytest.raises(ProcessorExecutionError) as exc:
                        await engine._persist_output(
                            "req",
                            OutputRequest(url=str(tmp_path)),
                            httpx.Response(200, json={}),
                            None,
                            processors=pipeline,
                            processor_dir=SimpleNamespace(name=work),
                        )
                    assert exc.value.status_code == 502
                    engine._write_output.assert_not_awaited()
        finally:
            await engine.aclose()

    asyncio.run(run())


@pytest.mark.parametrize(
    "kind,stage",
    [
        ("corrupt", "pre"),
        ("corrupt", "post"),
        ("truncated", "pre"),
        ("truncated", "post"),
        ("animated", "pre"),
        ("animated", "post"),
        ("missing", "post"),
        ("write_failure", "post"),
    ],
)
def test_resize_rejection_cleans_partial_files(tmp_path, monkeypatch, kind, stage):
    first = _image(tmp_path / "first.png")
    second = tmp_path / "second.png"
    saved_outputs = []
    if kind == "corrupt":
        second.write_bytes(b"invalid PNG")
    elif kind == "truncated":
        _image(second)
        second.write_bytes(second.read_bytes()[:65])
    elif kind == "write_failure":
        _image(second)

        real_save = Image.Image.save

        def fail_second_save(image, writer, *args, **kwargs):
            if saved_outputs:
                with Image.open(saved_outputs[0]) as output:
                    assert output.size == (50, 25)
                raise OSError("destination write failed")
            result = real_save(image, writer, *args, **kwargs)
            saved_outputs.append(Path(writer.name))
            return result

        monkeypatch.setattr(Image.Image, "save", fail_second_save)
    elif kind == "animated":
        frames = [Image.new("RGB", (100, 100), color) for color in ("red", "blue")]
        frames[0].save(second, save_all=True, append_images=frames[1:])
    pipeline = _pipeline(stage, [_call("image.resize", max_edge=50)])

    async def run():
        expected = (
            ClientError
            if stage == "pre" and kind not in ("missing", "write_failure")
            else ProcessorExecutionError
        )
        with pytest.raises(
            expected,
            match="destination write failed" if kind == "write_failure" else None,
        ) as error:
            await ProcessorExecutor(ProcessorRegistry()).execute(
                "req",
                tmp_path / "work",
                pipeline,
                stage,
                ProcessorState(paths=(first, second)),
            )
        assert error.value.status_code == (400 if expected is ClientError else 502)
        if kind == "write_failure":
            assert len(saved_outputs) == 1
            assert not saved_outputs[0].exists()
        assert not list((tmp_path / "work").rglob("*.png"))
        assert first.exists()

    asyncio.run(run())


def test_archive_symlink_and_writer_authorization(tmp_path):
    source = tmp_path / "source.png"
    source.write_bytes(b"secret")
    symlink = tmp_path / "link.png"
    symlink.symlink_to(source)
    pipeline = _pipeline("post", [_call("files.compress")])

    async def run():
        with pytest.raises(ProcessorExecutionError):
            await ProcessorExecutor(ProcessorRegistry()).execute(
                "req",
                tmp_path / "work",
                pipeline,
                "post",
                ProcessorState(paths=(symlink,)),
            )
        assert source.read_bytes() == b"secret"
        assert not list((tmp_path / "work").rglob("*.zip"))
        outside = tmp_path / "outside.zip"
        outside.write_bytes(b"archive")
        with pytest.raises(OutputWriteError):
            await OutputWriter().write_bundle(
                OutputRequest(url=str(tmp_path / "out")),
                None,
                "req.json",
                [str(outside)],
                processor_workdir=tmp_path / "work",
            )
        assert not (tmp_path / "out").exists()

    asyncio.run(run())


@pytest.mark.parametrize(
    "schema,expected", [({"type": "object"}, 200), ({"type": "array"}, 502)]
)
def test_http_post_pipeline_strips_controls_and_preserves_receipt(
    tmp_path, schema, expected
):
    source = _image(tmp_path / "image.png")
    seen = {}
    envelope = {
        "id": "result",
        "created": 1,
        "data": [{"file_path": str(source)}],
        "usage": {"total_tokens": 3},
    }

    def upstream(request):
        seen["body"] = json.loads(request.content)
        return httpx.Response(200, json=envelope)

    async def run():
        engine = _http_engine(upstream, fetch_media=False)
        try:
            response = await _post(
                engine,
                "/v1/images/generations",
                {
                    "prompt": "image",
                    "processors": {
                        "post": [
                            _call("json.validate", schema=schema),
                            _call("image.resize", max_edge=50),
                            _call("files.compress", format="zip"),
                        ]
                    },
                    "output": {"url": str(tmp_path / "out")},
                },
            )
            assert response.status_code == expected, response.text
            assert seen["body"] == {"prompt": "image"}
            if expected == 200:
                assert response.json()["usage"] == {"total_tokens": 3}
                assert response.json()["output"]["artifacts"] == [
                    "result.json",
                    "result-0.zip",
                ]
                with zipfile.ZipFile(tmp_path / "out" / "result-0.zip") as archive:
                    from io import BytesIO

                    with Image.open(
                        BytesIO(archive.read(archive.namelist()[0]))
                    ) as image:
                        assert image.size == (50, 25)
            else:
                assert not (tmp_path / "out").exists()
            assert not source.exists()
        finally:
            await engine.aclose()

    asyncio.run(run())


@pytest.mark.parametrize("scheme", ["s3", "https"])
def test_archive_destinations_preserve_bytes_and_mime(tmp_path, monkeypatch, scheme):
    from io import BytesIO

    from aibrix.storage.types import StorageType

    image = _image(tmp_path / "image.png")
    seen = {}

    class Store:
        bucket_name = "bucket"

        def get_type(self):
            return StorageType.S3

        async def put_object(self, key, data, content_type):
            seen[key] = (data, content_type)
            return True

    def upload(request):
        seen["upload"] = (request.content, request.headers["content-type"])
        return httpx.Response(200)

    real_client = httpx.AsyncClient

    class UploadClient(real_client):
        def __init__(self, *args, **kwargs):
            kwargs.setdefault("transport", httpx.MockTransport(upload))
            super().__init__(*args, **kwargs)

    monkeypatch.setattr(httpx, "AsyncClient", UploadClient)

    async def run():
        state = await ProcessorExecutor(ProcessorRegistry()).execute(
            "req",
            tmp_path / "work",
            _pipeline("post", [_call("files.compress", format="tar.gz")]),
            "post",
            ProcessorState(paths=(image,), payload=b"{}"),
        )
        archive_bytes = state.paths[0].read_bytes()
        writer = OutputWriter(storage_factory=Store)
        spec = OutputRequest(
            url="s3://bucket/out" if scheme == "s3" else "https://upload/result"
        )
        await writer.write_bundle(
            spec,
            state.payload,
            "result.json",
            [str(state.paths[0])],
            processor_workdir=tmp_path / "work",
        )
        if scheme == "s3":
            assert seen["out/result-0.tar.gz"] == (archive_bytes, "application/gzip")
            assert seen["out/result.json"] == (b"{}", "application/json")
        else:
            assert seen["upload"][1] == "application/zip"
            with zipfile.ZipFile(BytesIO(seen["upload"][0])) as archive:
                assert archive.read("result.json") == b"{}"
                assert archive.read("result-0.tar.gz") == archive_bytes
        assert not state.paths[0].exists()

    asyncio.run(run())


@pytest.mark.parametrize(
    "name,arguments",
    [
        ("files.compress", {"format": "zip"}),
        ("files.compress", {"format": "tar"}),
        ("image.resize", {"max_edge": 50}),
        ("demo.post.copy", {}),
    ],
)
def test_post_processor_checks_identity_on_open(tmp_path, monkeypatch, name, arguments):
    from aibrix.openai_frontend.proxy.processors.demo_processors import DEMO_PROCESSORS

    source = _image(tmp_path / "source.png")
    replacement = _image(tmp_path / "replacement.png", (100, 200))
    replacement_bytes = replacement.read_bytes()
    sources = {}
    engine = ProxyEngine(["unused"], enable_processors=True)
    if name.startswith("demo."):
        engine._processor_registry.define(
            next(d for d in DEMO_PROCESSORS if d.name == name)
        )
    engine._output_writer.remember_sources([str(source)], sources)
    engine._write_output = AsyncMock()
    real_open = os.open
    checked = []

    def replace_at_open(path, flags, *args, **kwargs):
        if Path(path) == source and not checked:
            checked.append(True)
            replacement.replace(source)
        return real_open(path, flags, *args, **kwargs)

    monkeypatch.setattr(os, "open", replace_at_open)
    envelope = {"id": "images", "created": 1, "data": [{"file_path": str(source)}]}
    pipeline = ProcessorPipeline.model_validate({"post": [_call(name, **arguments)]})
    engine._processor_registry.validate(pipeline)

    async def run():
        try:
            with tempfile.TemporaryDirectory() as work:
                with pytest.raises(
                    ProcessorExecutionError, match="input was replaced"
                ) as error:
                    await engine._persist_output(
                        "req",
                        OutputRequest(url=str(tmp_path / "out")),
                        httpx.Response(200, json=envelope),
                        ImageGenerationResponse.model_validate(envelope),
                        sources,
                        processors=pipeline,
                        processor_dir=SimpleNamespace(name=work),
                    )
                assert error.value.status_code == 502
                assert not list(Path(work).rglob("*.png")) and not list(
                    Path(work).rglob("*.zip")
                )
            assert checked
            engine._write_output.assert_not_awaited()
            assert source.read_bytes() == replacement_bytes
            assert not (tmp_path / "out").exists()
        finally:
            await engine.aclose()

    asyncio.run(run())


@pytest.mark.parametrize(
    "kind", ["invalid_schema", "invalid_pattern_properties", "truncated_image"]
)
def test_http_invalid_processor_input_fails_before_inference(tmp_path, kind):
    body = {"model": "m", "messages": [{"role": "user", "content": "hello"}]}
    if kind.startswith("invalid"):
        schema = (
            {"type": "string", "pattern": "["}
            if kind == "invalid_schema"
            else {"type": "object", "patternProperties": {"[": {"type": "string"}}}
        )
        body.update(
            processors={"post": [_call("json.validate", schema=schema)]},
            output={"url": str(tmp_path / "out")},
        )
    else:
        image = BytesIO()
        Image.new("RGB", (200, 100), "red").save(image, format="PNG")
        uri = (
            "data:image/png;base64," + base64.b64encode(image.getvalue()[:65]).decode()
        )
        body["messages"] = [
            {
                "role": "user",
                "content": [{"type": "image_url", "image_url": {"url": uri}}],
            }
        ]
        body["processors"] = {"pre": [_call("image.resize", max_edge=50)]}
    forwarded = []

    def upstream(request):
        forwarded.append(request)
        return httpx.Response(
            200, json={"id": "result", "created": 1, "model": "m", "choices": []}
        )

    async def run():
        engine = _http_engine(
            upstream, materialize_inline_media=True, media_dir=str(tmp_path / "media")
        )
        engine._write_output = AsyncMock()
        try:
            response = await _post(engine, "/v1/chat/completions", body)
            assert response.status_code == 400, response.text
            assert not forwarded
            engine._write_output.assert_not_awaited()
            assert not (tmp_path / "out").exists()
            if kind.startswith("invalid"):
                assert "arguments.schema" in response.text
        finally:
            await engine.aclose()

    asyncio.run(run())


@pytest.mark.parametrize(
    "stage,phase",
    [
        ("post", "during_read"),
        ("post", "between_processors"),
        ("post", "writer_handoff"),
        ("post", "image_handoff"),
        ("post", "during_persistence"),
        ("pre", "during_read"),
        ("pre", "between_processors"),
        ("pre", "identity_registration"),
        ("pre", "parent_registration"),
        ("post", "identity_registration"),
        ("pre", "engine_post"),
        ("pre", "engine_writer"),
    ],
)
def test_pipeline_rejects_source_changes(tmp_path, monkeypatch, stage, phase):
    source = _image(tmp_path / "source.png")
    changed = []
    forwarded = []

    async def run():
        def upstream(request):
            forwarded.append(request)
            if phase.startswith("engine_"):
                body = json.loads(request.content)
                final = Path(
                    unquote(urlparse(body["reference"]["image_url"]["url"]).path)
                )
                replacement = _image(tmp_path / "replacement.png")
                assert replacement.stat().st_ino != final.stat().st_ino
                replacement.replace(final)
                changed.append(final)
                return httpx.Response(
                    200,
                    json={
                        "id": "result",
                        "status": "completed",
                        "file_path": str(final),
                    },
                )
            return httpx.Response(
                200,
                json={
                    "id": "result",
                    "created": 1,
                    "data": [{"file_path": str(source)}],
                },
            )

        engine = _http_engine(upstream, fetch_media=False, media_dir=str(tmp_path))
        if phase == "during_read":
            real_save = Image.Image.save

            def mutate_source(image, *args, **kwargs):
                result = real_save(image, *args, **kwargs)
                if not changed:
                    source.write_bytes(source.read_bytes() + b"changed")
                    changed.append(source)
                return result

            monkeypatch.setattr(Image.Image, "save", mutate_source)
        elif phase == "between_processors":
            original_run = engine._processor_executor._run

            async def replace_intermediate(definition, ctx):
                if not changed and (
                    definition.name == "files.compress"
                    or (stage == "pre" and ctx.target.paths[0] != source)
                ):
                    intermediate = ctx.target.paths[0]
                    replacement = _image(tmp_path / "replacement.png")
                    assert replacement.stat().st_ino != intermediate.stat().st_ino
                    replacement.replace(intermediate)
                    changed.append(intermediate)
                return await original_run(definition, ctx)

            monkeypatch.setattr(
                engine._processor_executor, "_run", replace_intermediate
            )
        elif phase in ("identity_registration", "parent_registration"):
            original_replace = engine._processor_executor._replace

            def remove_before_registration(*args):
                state = original_replace(*args)
                if not changed:
                    final = state.paths[0]
                    if phase == "parent_registration":
                        for path in final.parent.iterdir():
                            path.unlink()
                        final.parent.rmdir()
                    else:
                        final.unlink()
                    changed.append(final)
                return state

            monkeypatch.setattr(
                engine._processor_executor, "_replace", remove_before_registration
            )
        elif not phase.startswith("engine_"):
            original_write = engine._write_output

            async def replace_final(*args, **kwargs):
                final = Path(args[4][0])
                # Identity must be pinned before the Writer's first path inspection.
                assert final in args[5]
                if phase == "during_persistence":
                    from aibrix.openai_frontend.proxy.output.local_output import (
                        PinnedLocalOutput,
                    )

                    original_replace = PinnedLocalOutput._replace_at

                    def mutate(directory_fd, name, data, check_destination):
                        if name.endswith(".zip") and not changed:
                            final.write_bytes(final.read_bytes() + b"changed")
                            changed.append(final)
                        return original_replace(
                            directory_fd, name, data, check_destination
                        )

                    monkeypatch.setattr(
                        PinnedLocalOutput, "_replace_at", staticmethod(mutate)
                    )
                else:
                    replacement = tmp_path / "replacement"
                    replacement.write_bytes(b"replacement")
                    replacement.replace(final)
                    changed.append(final)
                return await original_write(*args, **kwargs)

            monkeypatch.setattr(engine, "_write_output", replace_final)
        try:
            calls = [_call("image.resize", max_edge=50)]
            if phase != "image_handoff":
                calls.append(
                    _call("image.resize", max_edge=25)
                    if stage == "pre"
                    else _call("files.compress", format="zip")
                )
            body = {"processors": {stage: calls}}
            if stage == "pre":
                body.update(
                    {
                        "prompt": "video",
                        "reference": {
                            "type": "image_url",
                            "image_url": {"url": source.as_uri()},
                        },
                    }
                )
            else:
                body.update(
                    {"prompt": "image", "output": {"url": str(tmp_path / "out")}}
                )
            if phase.startswith("engine_"):
                body["output"] = {"url": str(tmp_path / "out")}
                if phase == "engine_post":
                    body["processors"]["post"] = [_call("image.resize", max_edge=12)]
            response = await _post(
                engine,
                "/v1/videos" if stage == "pre" else "/v1/images/generations",
                body,
            )
            assert response.status_code == (
                500
                if phase.endswith("handoff")
                or phase in ("during_persistence", "engine_writer")
                else 502
            ), response.text
            assert changed
            if stage == "pre":
                assert len(forwarded) == int(phase.startswith("engine_"))
                assert not engine._request_done
                assert source.exists()
                if changed[0] != source:
                    assert not changed[0].exists()
            if phase == "during_persistence":
                assert "changed during persistence" in response.text
                assert "output" not in response.json()
            else:
                assert not (tmp_path / "out").exists()
            assert not engine._in_flight
        finally:
            await engine.aclose()

    asyncio.run(run())


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "initial_status,valid_output",
    [
        ("queued", True),
        ("completed", True),
        ("queued", False),
        ("completed", False),
        ("failed", True),
        ("cancelled", True),
    ],
)
async def test_video_processors_run_before_upload_and_release_directory(
    tmp_path, monkeypatch, initial_status, valid_output
):
    source = tmp_path / "generated.mp4"
    source.write_bytes(b"generated video")
    image = BytesIO()
    Image.new("RGB", (200, 100), "red").save(image, format="PNG")
    uri = "data:image/png;base64," + base64.b64encode(image.getvalue()).decode()
    poll_entered, complete = asyncio.Event(), asyncio.Event()
    seen = {}
    directories = []
    envelope = {
        "id": "video-job",
        "status": "completed",
        "file_path": str(source),
        "data": [{"value": 1}],
        "seconds": 5,
    }

    async def upstream(request):
        if request.method == "POST":
            seen["body"] = json.loads(request.content)
            reference = seen["body"]["reference"]["image_url"]["url"]
            seen["reference"] = Path(unquote(urlparse(reference).path))
            with Image.open(seen["reference"]) as resized:
                assert resized.size == (50, 25)
            assert "processors" not in seen["body"] and "output" not in seen["body"]
            return httpx.Response(
                200,
                json={**envelope, "status": initial_status}
                if initial_status != "queued"
                else {"id": "video-job", "status": "queued"},
            )
        poll_entered.set()
        await complete.wait()
        assert seen["reference"].exists()
        return httpx.Response(200, json=envelope)

    engine = _http_engine(upstream, fetch_media=False, materialize_inline_media=False)
    original_new = engine._new_processor_dir

    def track_directory(*args):
        directory = original_new(*args)
        directories.append(Path(directory.name))
        return directory

    post_calls = []
    original_run = engine._processor_executor._run

    async def track_post(definition, ctx):
        if definition.stage == "post":
            post_calls.append(definition.name)
        return await original_run(definition, ctx)

    monkeypatch.setattr(engine._processor_executor, "_run", track_post)
    monkeypatch.setattr(engine, "_new_processor_dir", track_directory)
    task = asyncio.create_task(
        _post(
            engine,
            "/v1/videos",
            {
                "prompt": "video",
                "reference": {"type": "image_url", "image_url": {"url": uri}},
                "output": {"url": str(tmp_path / "out")},
                "processors": {
                    "pre": [_call("image.resize", max_edge=50)],
                    "post": [
                        _call("json.validate", schema=valid_output),
                        _call("files.compress", format="zip"),
                    ],
                },
            },
        )
    )
    try:
        if initial_status == "queued":
            await asyncio.wait_for(poll_entered.wait(), 2)
            assert directories[0].exists() and seen["reference"].exists()
            assert not task.done() and not (tmp_path / "out").exists()
            complete.set()
        response = await asyncio.wait_for(task, 2)
        terminal_failure = initial_status in ("failed", "cancelled")
        assert response.status_code == (200 if valid_output else 502), response.text
        if valid_output and not terminal_failure:
            assert post_calls == ["json.validate", "files.compress"]
            receipt = response.json()
            assert receipt["seconds"] == 5 and receipt["data"] == []
            assert receipt["output"]["artifacts"] == [
                "video-job.json",
                "video-job-0.zip",
            ]
            with zipfile.ZipFile(tmp_path / "out" / "video-job-0.zip") as archive:
                assert archive.read("0000-generated.mp4") == b"generated video"
            assert receipt["url"].endswith("video-job-0.zip")
        else:
            assert post_calls == ([] if terminal_failure else ["json.validate"])
            assert not (tmp_path / "out").exists()
        cached = engine._uploaded_videos["video-job"]
        assert cached.status == (
            "completed" if valid_output and not terminal_failure else "failed"
        )
        assert not directories[0].exists() and not source.exists()
        assert not engine._in_flight and not engine._video_tasks
    finally:
        complete.set()
        await asyncio.gather(task, return_exceptions=True)
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize("with_pre", [False, True])
async def test_pre_implicitly_fetches_remote_media_without_changing_other_requests(
    tmp_path, monkeypatch, with_pre
):
    from aibrix.openai_frontend.proxy.runtime.engine_spec import VLLM_SPEC
    from tests.openai_frontend.test_media_foundation import _StubDownloader

    image = BytesIO()
    Image.new("RGB", (200, 100), "red").save(image, format="PNG")
    downloader = _StubDownloader(image.getvalue())
    monkeypatch.setattr(
        "aibrix.openai_frontend.common.artifact_fetcher.get_downloader",
        lambda url: downloader,
    )
    references = []

    def upstream(request):
        body = json.loads(request.content)
        reference = body["messages"][0]["content"][0]["image_url"]["url"]
        references.append(reference)
        if with_pre:
            path = Path(unquote(urlparse(reference).path))
            assert path.is_relative_to(Path(engine._media_dir))
            with Image.open(path) as resized:
                assert resized.size == (50, 25)
        else:
            assert reference == "https://media/image.png"
        assert "processors" not in body
        return httpx.Response(
            200, json={"id": "chat", "created": 1, "model": "vision", "choices": []}
        )

    engine = _http_engine(upstream, spec=VLLM_SPEC, fetch_media=False)
    body = {
        "model": "vision",
        "messages": [
            {
                "role": "user",
                "content": [
                    {
                        "type": "image_url",
                        "image_url": {"url": "https://media/image.png"},
                    }
                ],
            }
        ],
    }
    if with_pre:
        body["processors"] = {"pre": [_call("image.resize", max_edge=50)]}
    try:
        assert engine._subprocess._cmd[-2:] == [
            "--allowed-local-media-path",
            engine._media_dir,
        ]
        response = await _post(engine, "/v1/chat/completions", body)
        assert response.status_code == 200, response.text
        assert bool(downloader.calls) is with_pre and not engine._fetch_media
        assert not engine._media_inspector._materialize_inline
        if with_pre:
            assert not Path(unquote(urlparse(references[0]).path)).exists()
        assert not engine._fetcher._active_tmpdirs and not engine._request_done
    finally:
        await engine.aclose()


@pytest.mark.asyncio
async def test_processor_directory_failure_untracks_request(monkeypatch):
    def upstream(request):
        pytest.fail("directory allocation failure must not reach inference")

    engine = _http_engine(upstream, fetch_media=False)
    done = []

    def fail_directory(req_id, pipeline):
        done.append(engine._request_done[req_id])
        raise OSError(errno.ENOSPC, "processor directory full")

    monkeypatch.setattr(engine, "_new_processor_dir", fail_directory)
    try:
        with pytest.raises(OSError, match="processor directory full"):
            await _post(
                engine,
                "/v1/videos",
                {
                    "prompt": "video",
                    "processors": {"pre": [_call("image.resize", max_edge=50)]},
                },
            )
        assert done and done[0].done()
        assert not engine._request_done and not engine._in_flight_tasks
        await asyncio.wait_for(engine.aclose(), 2)
    finally:
        # Keep a failing regression from hanging the suite during shutdown.
        for req_id, started in list(engine._in_flight.items()):
            engine._requests_end(req_id, started)
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize("endpoint", ["/v1/videos", "/v1/images/generations"])
async def test_pre_rejects_requested_stream_before_forwarding(endpoint):
    def upstream(request):
        pytest.fail("stream conflict must be rejected before forwarding")

    engine = _http_engine(upstream, fetch_media=False)
    try:
        response = await _post(
            engine,
            endpoint,
            {
                "prompt": "media",
                "stream": True,
                "processors": {"pre": [_call("image.resize", max_edge=50)]},
            },
        )
        assert response.status_code == 400 and "stream" in response.text
        assert not engine._request_done
    finally:
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize("mixed", [False, True])
@pytest.mark.parametrize("size", [(200, 100), (40, 20)])
async def test_pre_processes_external_local_media_with_inline_inputs(
    tmp_path, mixed, size
):
    media = tmp_path / "media"
    media.mkdir()
    source = _image(media / "input space.png", size)
    original = source.read_bytes()
    # Multiple spellings and repeated references resolve to the same external file.
    urls = [source.as_uri(), str(source), source.as_uri()]
    if mixed:
        urls.append("data:image/png;base64," + base64.b64encode(original).decode())
    seen = []

    def upstream(request):
        body = json.loads(request.content)
        for part in body["references"]:
            uri = part["image_url"]["url"]
            assert uri.startswith("file://")
            path = Path(unquote(urlparse(uri).path))
            seen.append(path)
            assert path.is_relative_to(media)
            if size == (200, 100):
                assert path != source
            with Image.open(path) as resized:
                assert resized.size == ((50, 25) if size == (200, 100) else size)
        assert body["prompt"] == source.as_uri()
        return httpx.Response(200, json={"id": "job", "status": "completed"})

    engine = _http_engine(
        upstream,
        fetch_media=False,
        materialize_inline_media=False,
        media_dir=str(media),
    )
    try:
        response = await _post(
            engine,
            "/v1/videos",
            {
                "prompt": source.as_uri(),
                "references": [
                    {"type": "image_url", "image_url": {"url": url}} for url in urls
                ],
                "processors": {"pre": [_call("image.resize", max_edge=50)]},
            },
        )
        assert response.status_code == 200, response.text
        assert len(seen) == len(urls)
        assert all(not path.exists() for path in seen if path != source)
        assert seen[0] == seen[1] == seen[2]
        assert source.read_bytes() == original
        assert not engine._request_done and not engine._fetcher._active_tmpdirs
    finally:
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "with_pre,available,subclass,fetch_media,stat_denied",
    [
        (pre, True, subclass, True, False)
        for pre in (False, True)
        for subclass in (False, True)
    ]
    + [(False, False, subclass, True, False) for subclass in (False, True)]
    + [(False, False, False, False, False), (False, True, True, True, True)],
)
async def test_custom_inspector_preserves_local_fetch_policy(
    tmp_path, monkeypatch, with_pre, available, subclass, fetch_media, stat_denied
):
    from aibrix.openai_frontend.proxy.media.request_inspector import (
        MediaURLInspector,
        RequestInspector,
        URLLocation,
    )

    source = _image(tmp_path / "input space.png")
    original = source.read_bytes()
    if not available:
        source.unlink()
    staged = tmp_path / "staged.png"
    forwarded, inspected = [], []

    class LocalInspector(MediaURLInspector if subclass else RequestInspector):
        def extract_urls(self, payload):
            uri = payload["messages"][0]["content"][0]["image_url"]["url"]
            inspected.append(uri)
            return (
                [URLLocation(uri, ["messages", 0, "content", 0, "image_url", "url"])]
                if len(inspected) == 1
                else []
            )

    async def fetch_many(urls, credentials=None):
        assert urls == [source.as_uri()]
        staged.write_bytes(original)
        return [FetchResult(source.as_uri(), staged.as_uri(), None, len(original))]

    fetcher = SimpleNamespace(
        fetch_many=AsyncMock(side_effect=fetch_many), cleanup=AsyncMock()
    )

    def upstream(request):
        body = json.loads(request.content)
        uri = body["messages"][0]["content"][0]["image_url"]["url"]
        forwarded.append(uri)
        expected = source if with_pre or not fetch_media else staged
        assert uri == expected.as_uri()
        if available or fetch_media:
            assert expected.read_bytes() == original
        return httpx.Response(
            200, json={"id": "chat", "created": 1, "model": "m", "choices": []}
        )

    engine = _http_engine(
        upstream,
        fetch_media=fetch_media,
        chat_inspector=LocalInspector(),
        artifact_fetcher=fetcher,
        media_dir=str(tmp_path),
    )

    async def noop(ctx):
        return ProcessorOutput()

    if with_pre:
        engine._processor_registry.define(
            ProcessorDefinition("test.noop", "pre", "input_media", {}, noop)
        )
    real_stat = Path.stat

    def checked_stat(path, *args, **kwargs):
        if stat_denied and path == source:
            raise PermissionError("input stat unavailable")
        return real_stat(path, *args, **kwargs)

    if stat_denied:
        monkeypatch.setattr(Path, "stat", checked_stat)
    try:
        response = await _post(
            engine,
            "/v1/chat/completions",
            {
                "model": "m",
                "messages": [
                    {
                        "role": "user",
                        "content": [
                            {
                                "type": "image_url",
                                "image_url": {
                                    "url": str(source) if with_pre else source.as_uri()
                                },
                            }
                        ],
                    }
                ],
                **({"processors": {"pre": [_call("test.noop")]}} if with_pre else {}),
                "output": {"url": str(tmp_path / "out")},
            },
        )
        assert response.status_code == 200, response.text
        assert inspected == [str(source) if with_pre else source.as_uri()]
        assert forwarded == [
            (source if with_pre or not fetch_media else staged).as_uri()
        ]
        if with_pre or not fetch_media:
            fetcher.fetch_many.assert_not_awaited()
            fetcher.cleanup.assert_not_awaited()
        else:
            fetcher.fetch_many.assert_awaited_once()
            fetcher.cleanup.assert_awaited_once()
        if stat_denied:
            monkeypatch.setattr(Path, "stat", real_stat)
        assert source.exists() is available and not engine._request_done
        if available:
            assert source.read_bytes() == original
    finally:
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "size,reference_kind",
    [
        ((200, 100), "outside"),
        ((40, 20), "outside"),
        ((200, 100), "raw_nul"),
        ((200, 100), "encoded_nul"),
        ((200, 100), "invalid_authority"),
        ((200, 100), "missing"),
    ],
)
async def test_pre_rejects_invalid_external_media_before_read(
    tmp_path, monkeypatch, size, reference_kind
):
    media = tmp_path / "media"
    source = _image(tmp_path / "outside.png", size)
    reference = {
        "outside": source.as_uri(),
        "raw_nul": str(media / "input") + "\x00.png",
        "encoded_nul": (media / "input").as_uri() + "%00.png",
        "invalid_authority": "file://[invalid/input.png",
        "missing": (media / "missing.png").as_uri(),
    }[reference_kind]
    original = source.read_bytes()
    opened = []
    real_open = os.open

    def track_open(path, *args, **kwargs):
        if Path(path) == source:
            opened.append(path)
        return real_open(path, *args, **kwargs)

    monkeypatch.setattr(os, "open", track_open)

    def upstream(request):
        pytest.fail("unauthorized local media must not reach inference")

    engine = _http_engine(upstream, fetch_media=False, media_dir=str(media))
    try:
        response = await _post(
            engine,
            "/v1/videos",
            {
                "prompt": "video",
                "reference": {
                    "type": "image_url",
                    "image_url": {"url": reference},
                },
                "processors": {"pre": [_call("image.resize", max_edge=50)]},
            },
        )
        assert response.status_code == 400, response.text
        assert (
            "allowed media directory"
            if reference_kind == "outside"
            else "Local media input is unavailable"
        ) in response.text
        assert not opened and source.read_bytes() == original
        assert not engine._request_done and not list(media.iterdir())
    finally:
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize("link_kind", ["file", "parent_after_check"])
async def test_external_pre_cannot_follow_symlink_outside_root(
    tmp_path, monkeypatch, link_kind
):
    media = tmp_path / "media"
    nested = media / "nested"
    nested.mkdir(parents=True)
    outside = tmp_path / "outside"
    outside.mkdir()
    private = _image(outside / "input.png")
    source = nested / "input.png"
    swapped = []
    if link_kind == "file":
        source.symlink_to(private)
    else:
        _image(source)
    real_open = os.open
    opened_private = []

    def swap_parent(path, flags, *args, **kwargs):
        if path == "nested" and kwargs.get("dir_fd") is not None and not swapped:
            nested.rename(media / "original")
            nested.symlink_to(outside, target_is_directory=True)
            swapped.append(True)
        fd = real_open(path, flags, *args, **kwargs)
        if os.fstat(fd).st_ino == private.stat().st_ino:
            opened_private.append(fd)
        return fd

    if link_kind != "file":
        monkeypatch.setattr(os, "open", swap_parent)

    def upstream(request):
        pytest.fail("symlink escape must not reach inference")

    engine = _http_engine(upstream, fetch_media=False, media_dir=str(media))
    try:
        response = await _post(
            engine,
            "/v1/videos",
            {
                "prompt": "video",
                "reference": {
                    "type": "image_url",
                    "image_url": {"url": source.as_uri()},
                },
                "processors": {"pre": [_call("image.resize", max_edge=50)]},
            },
        )
        assert response.status_code == (400 if link_kind == "file" else 502), (
            response.text
        )
        assert not opened_private and bool(swapped) is (link_kind != "file")
        assert private.exists() and not engine._request_done
    finally:
        await engine.aclose()


@pytest.mark.asyncio
async def test_pre_preserves_first_custom_inspector_locations(tmp_path):
    from aibrix.openai_frontend.proxy.media.request_inspector import (
        MediaURLInspector,
        URLLocation,
    )

    inspected = []

    class InlineInspector(MediaURLInspector):
        def extract_urls(self, payload):
            uri = payload["messages"][0]["content"][0]["image_url"]["url"]
            inspected.append(uri)
            return (
                [URLLocation(uri, ["messages", 0, "content", 0, "image_url", "url"])]
                if uri.startswith("data:")
                else []
            )

    image = BytesIO()
    Image.new("RGB", (200, 100), "red").save(image, format="PNG")
    uri = "data:image/png;base64," + base64.b64encode(image.getvalue()).decode()
    seen = []

    def upstream(request):
        body = json.loads(request.content)
        path = Path(
            unquote(
                urlparse(body["messages"][0]["content"][0]["image_url"]["url"]).path
            )
        )
        with Image.open(path) as resized:
            assert resized.size == (50, 25)
        seen.append(path)
        return httpx.Response(
            200, json={"id": "chat", "model": "m", "created": 1, "choices": []}
        )

    engine = _http_engine(upstream, fetch_media=False, chat_inspector=InlineInspector())
    try:
        response = await _post(
            engine,
            "/v1/chat/completions",
            {
                "model": "m",
                "messages": [
                    {
                        "role": "user",
                        "content": [{"type": "image_url", "image_url": {"url": uri}}],
                    }
                ],
                "processors": {"pre": [_call("image.resize", max_edge=50)]},
            },
        )
        assert response.status_code == 200, response.text
        assert inspected == [uri] and len(seen) == 1 and not seen[0].exists()
        assert not engine._request_done and not engine._fetcher._active_tmpdirs
    finally:
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "outcome,with_pre",
    [
        (outcome, pre)
        for outcome in ("success", "poll", "clear", "unknown", "image_error")
        for pre in (False, True)
    ]
    + [("late_alias", False)],
)
async def test_external_input_is_never_claimed_by_output_cleanup(
    tmp_path, outcome, with_pre
):
    media = tmp_path / "media"
    media.mkdir()
    source = _image((media if with_pre else tmp_path) / "input.png", (40, 20))
    original = source.read_bytes()
    returned = source
    if outcome in ("poll", "late_alias"):
        returned = media / "alias.png"
        if outcome == "late_alias":
            source.unlink()
        else:
            os.link(source, returned)
    generated = _image(media / "generated.png")
    envelope = {
        "id": "job",
        "status": "completed",
        "file_paths": [str(returned), str(generated)],
    }
    if outcome == "unknown":
        envelope.pop("id")
        envelope["status"] = "in_progress"
    if outcome == "image_error":
        envelope = {
            "data": [{"file_path": str(returned)}, {"file_path": str(generated)}]
        }

    def upstream(request):
        if outcome == "late_alias":
            source.write_bytes(original)
            os.link(source, returned)
        if outcome == "poll" and request.method == "POST":
            return httpx.Response(200, json={"id": "job", "status": "queued"})
        return httpx.Response(
            500 if outcome == "unknown" else 400 if outcome == "image_error" else 200,
            json=envelope,
        )

    engine = _http_engine(upstream, fetch_media=False, media_dir=str(media))

    async def clear(ctx):
        return ProcessorOutput(ProcessorMedia(()))

    engine._processor_registry.define(
        ProcessorDefinition("test.clear", "post", "output_media", {}, clear)
    )
    processors = {"pre": [_call("image.resize", max_edge=50)]} if with_pre else {}
    if outcome == "clear":
        processors["post"] = [_call("test.clear")]
    try:
        response = await _post(
            engine,
            "/v1/images/generations" if outcome == "image_error" else "/v1/videos",
            {
                "prompt": "video",
                "reference": {
                    "type": "image_url",
                    "image_url": {"url": source.as_uri()},
                },
                "output": {"url": str(tmp_path / "out")},
                **({"processors": processors} if processors else {}),
            },
        )
        assert response.status_code == (
            500 if outcome == "unknown" else 400 if outcome == "image_error" else 200
        ), response.text
        assert source.read_bytes() == original and returned.exists()
        if outcome in ("success", "poll"):
            artifacts = response.json()["output"]["artifacts"]
            assert (tmp_path / "out" / artifacts[0]).read_bytes() == original
        if outcome == "unknown":
            assert generated.exists() and engine._retained.pending()
        else:
            assert not generated.exists()
        await engine.aclose()
        assert (
            source.read_bytes() == original
            and returned.exists()
            and not generated.exists()
        )
        assert not engine._request_done and not engine._retained.pending()
    finally:
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize("with_pre", [False, True])
@pytest.mark.parametrize("custom_inspector", [False, True])
@pytest.mark.parametrize("collision", ["media", "encoded", "metadata", "payload"])
async def test_local_output_cannot_overwrite_external_input(
    tmp_path, collision, with_pre, custom_inspector
):
    media = tmp_path / "media"
    media.mkdir()
    response_id = "视频" if collision == "encoded" else "job"
    filename = {
        "media": "job-0.png",
        "encoded": "%E8%A7%86%E9%A2%91-0.png",
        "metadata": "job.json.metadata",
        "payload": "job.json.metadata",
    }[collision]
    source = _image(media / "input.png", (40, 20))
    source = source.rename(media / filename)
    original = source.read_bytes()
    generated = _image(media / "generated.png")
    envelope = {"id": response_id, "status": "completed", "file_path": str(generated)}
    if collision in ("metadata", "payload"):
        envelope["data"] = [{"value": 1}]
    if collision == "payload":
        envelope.pop("file_path")

    engine = _http_engine(
        lambda request: httpx.Response(200, json=envelope),
        fetch_media=False,
        media_dir=str(media),
    )
    references = [{"type": "image_url", "image_url": {"url": source.as_uri()}}]
    if custom_inspector:
        from aibrix.openai_frontend.proxy.media.request_inspector import (
            MediaURLInspector,
        )

        selected = _image(media / "selected.png", (40, 20))
        engine._media_inspector = MediaURLInspector(media_types=["video_url"])
        references.append(
            {"type": "video_url", "video_url": {"url": selected.as_uri()}}
        )
    try:
        response = await _post(
            engine,
            "/v1/videos",
            {
                "prompt": "video",
                "references": references,
                **(
                    {"processors": {"pre": [_call("image.resize", max_edge=50)]}}
                    if with_pre
                    else {}
                ),
                "output": {"url": media.as_uri()},
            },
        )
        assert response.status_code == 500 and "external input" in response.text
        assert source.read_bytes() == original
        assert generated.exists() == (collision == "payload")
        assert not (media / "job.json").exists()
        assert not engine._request_done
    finally:
        await engine.aclose()
    assert source.read_bytes() == original


@pytest.mark.asyncio
async def test_local_output_rejects_link_created_between_check_and_replace(
    tmp_path, monkeypatch
):
    from aibrix.openai_frontend.proxy.output.local_output import PinnedLocalOutput

    media = tmp_path / "media"
    media.mkdir()
    source = _image(media / "input.png", (40, 20))
    original = source.read_bytes()
    generated = _image(media / "generated.png")
    out = tmp_path / "out"
    out.mkdir()
    actual_replace_at = PinnedLocalOutput._replace_at
    linked = False

    def replace_at(directory_fd, name, data, check_destination):
        nonlocal linked
        if not linked and name == "job-0.png.metadata":
            # The alias appears after both batch pre-checks and before replace.
            linked = True
            os.link(source, out / name)
        return actual_replace_at(directory_fd, name, data, check_destination)

    monkeypatch.setattr(PinnedLocalOutput, "_replace_at", staticmethod(replace_at))
    engine = _http_engine(
        lambda request: httpx.Response(
            200, json={"id": "job", "status": "completed", "file_path": str(generated)}
        ),
        fetch_media=False,
        media_dir=str(media),
    )
    try:
        response = await _post(
            engine,
            "/v1/videos",
            {
                "prompt": "video",
                "reference": {
                    "type": "image_url",
                    "image_url": {"url": source.as_uri()},
                },
                "processors": {"pre": [_call("image.resize", max_edge=50)]},
                "output": {"url": str(out)},
            },
        )
        assert linked and response.status_code == 500, response.text
        assert "external input" in response.text
        assert "output" not in response.json()
    finally:
        await engine.aclose()
    assert source.read_bytes() == original
    assert (out / "job-0.png.metadata").stat().st_ino == source.stat().st_ino
    assert not list(out.glob("*.tmp"))


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "stage,swap,bundle,external",
    [
        (stage, swap, bundle, True)
        for stage in ("data", "metadata")
        for swap in ("link", "directory")
        for bundle in (False, True)
    ]
    # Pinning cannot depend on a external input being present.
    + [("data", "link", False, False)],
)
async def test_local_output_pins_directory_before_actual_write(
    tmp_path, monkeypatch, stage, swap, bundle, external
):
    from aibrix.openai_frontend.proxy.output.local_output import PinnedLocalOutput

    media = tmp_path / "media"
    media.mkdir()
    generated = _image(media / "generated.png")
    wanted = generated.read_bytes()
    safe = tmp_path / "out"
    safe.mkdir()
    destination = tmp_path / "destination"
    destination.symlink_to(safe, target_is_directory=True)
    parked = tmp_path / "parked"
    source = None
    if external:
        filename = "job-0.png" if stage == "data" else "job-0.png.metadata"
        source = _image(media / "input.png", (40, 20)).rename(media / filename)
        original = source.read_bytes()
    actual_open = os.open
    actual_replace_at = PinnedLocalOutput._replace_at
    active_name = None
    switched = False
    switch_name = ("job.json" if bundle else "job-0.png") + (
        ".metadata" if stage == "metadata" else ""
    )

    def replace_at(directory_fd, name, data, check_destination):
        nonlocal active_name
        active_name = name
        return actual_replace_at(directory_fd, name, data, check_destination)

    def open_after_swap(path, flags, *args, **kwargs):
        nonlocal switched
        if (
            not switched
            and active_name == switch_name
            and flags & os.O_CREAT
            and str(path).endswith(".tmp")
        ):
            switched = True
            if external:
                assert source.read_bytes() == original
            if swap == "link":
                destination.unlink()
                destination.symlink_to(media, target_is_directory=True)
            else:
                safe.rename(parked)
                safe.symlink_to(media, target_is_directory=True)
        return actual_open(path, flags, *args, **kwargs)

    monkeypatch.setattr(PinnedLocalOutput, "_replace_at", staticmethod(replace_at))
    monkeypatch.setattr(os, "open", open_after_swap)
    envelope = {"id": "job", "status": "completed", "file_path": str(generated)}
    if bundle:
        envelope["data"] = [{"value": 1}]
    engine = _http_engine(
        lambda request: httpx.Response(200, json=envelope),
        fetch_media=False,
        media_dir=str(media),
    )
    body = {"prompt": "video", "output": {"url": str(destination)}}
    if external:
        body.update(
            reference={"type": "image_url", "image_url": {"url": source.as_uri()}},
            processors={"pre": [_call("image.resize", max_edge=50)]},
        )
    try:
        response = await _post(engine, "/v1/videos", body)
        assert switched and response.status_code == 500, response.text
        assert "directory changed" in response.text
        assert "output" not in response.json()
        output = safe if swap == "link" else parked
        assert (output / "job-0.png").read_bytes() == wanted
        if bundle:
            assert json.loads((output / "job.json").read_bytes()) == {
                "data": [{"value": 1}]
            }
            assert (output / "job.json.metadata").exists()
            assert not (media / "job.json").exists()
        sidecar = json.loads((output / "job-0.png.metadata").read_bytes())
        assert sidecar["content_length"] == len(wanted)
        assert sidecar["content_type"] == "image/png" and sidecar["etag"]
        assert not generated.exists() and not engine._request_done
        assert not list(output.glob("*.tmp")) and not list(media.glob("*.tmp"))
        if stage == "metadata":
            assert not (media / "job-0.png").exists()
        if external:
            assert source.read_bytes() == original
    finally:
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "collision,retained,phase",
    [
        (collision, retained, "storage")
        for collision in ("media", "sidecar")
        for retained in (False, True)
    ]
    + [(collision, True, "pipeline") for collision in ("media", "sidecar")],
)
async def test_local_output_rechecks_entire_batch_after_storage_creation(
    tmp_path, monkeypatch, collision, retained, phase
):
    import aibrix.storage

    media = tmp_path / "media"
    media.mkdir()
    name = ("job.json" if retained else "job-0.png") + (
        ".metadata" if collision == "sidecar" else ""
    )
    source = _image(media / "input.png", (40, 20)).rename(media / name)
    original = source.read_bytes()
    generated = _image(media / "generated.png")
    safe = tmp_path / "out"
    safe.mkdir()
    destination = tmp_path / "destination"
    destination.symlink_to(safe, target_is_directory=True)
    actual_create = aibrix.storage.create_storage
    switched = False

    def create(*args, **kwargs):
        nonlocal switched
        storage = actual_create(*args, **kwargs)
        if phase == "pipeline":
            return storage
        switched = True
        destination.unlink()
        if retained:
            media.rename(tmp_path / "moved")
            (tmp_path / "moved" / name).unlink()
            destination.symlink_to(tmp_path / "moved", target_is_directory=True)
        else:
            destination.symlink_to(media, target_is_directory=True)
        return storage

    monkeypatch.setattr(aibrix.storage, "create_storage", create)
    if retained:
        # Use the real pre handoff, then observe the reference through its moved parent.
        created = []

        async def copy(ctx):
            nonlocal media, switched
            path = ctx.workdir / name
            path.write_bytes(original)
            if phase == "pipeline" and created:
                media = created[0].parent
                media.rename(tmp_path / "moved")
                (tmp_path / "moved" / name).unlink()
                media.mkdir()
                destination.unlink()
                destination.symlink_to(tmp_path / "moved", target_is_directory=True)
                switched = True
            created.append(path)
            return ProcessorOutput(ProcessorMedia((path,)))

        engine = _http_engine(
            lambda request: httpx.Response(
                200,
                json={
                    "id": "job",
                    "object": "response",
                    "created_at": 1.0,
                    "model": "test",
                    "status": "completed",
                    "output": [],
                },
            ),
            processor_registry=ProcessorRegistry(
                [ProcessorDefinition("test.copy", "pre", "input_media", {}, copy)]
            ),
            fetch_media=False,
            media_dir=str(tmp_path),
        )
        original_pre = engine._apply_pre_processors

        async def retain_input(context, *args, **kwargs):
            nonlocal media
            result = await original_pre(context, *args, **kwargs)
            path = created[0]
            media = path.parent
            ref = context.sources.ref(path)
            context.sources[path] = ref.identity
            context.sources.add(PathRef(path, ref.identity))
            return result

        monkeypatch.setattr(engine, "_apply_pre_processors", retain_input)
        try:
            response = await _post(
                engine,
                "/v1/responses",
                {
                    "store": True,
                    "input": [
                        {
                            "role": "user",
                            "content": [
                                {"type": "input_image", "image_url": source.as_uri()}
                            ],
                        }
                    ],
                    "processors": {
                        "pre": [_call("test.copy")] * (2 if phase == "pipeline" else 1)
                    },
                    "output": {"url": str(destination)},
                },
            )
            assert switched and response.status_code == 500, response.text
            assert "retained input" in response.text
            assert not (tmp_path / "moved" / name).exists()
            assert not engine._request_done
        finally:
            if (tmp_path / "moved").exists():
                if media.exists():
                    media.rmdir()
                (tmp_path / "moved").rename(media)
            await engine.aclose()
        assert source.read_bytes() == original and not media.exists()
        return
    engine = _http_engine(
        lambda request: httpx.Response(
            200,
            json={
                "id": "job",
                "status": "completed",
                "file_path": str(generated),
                "data": [{"value": 1}],
            },
        ),
        fetch_media=False,
        media_dir=str(media),
    )
    try:
        response = await _post(
            engine,
            "/v1/videos",
            {
                "prompt": "video",
                "reference": {
                    "type": "image_url",
                    "image_url": {"url": source.as_uri()},
                },
                "processors": {"pre": [_call("image.resize", max_edge=50)]},
                "output": {"url": str(destination)},
            },
        )
        assert (
            switched
            and response.status_code == 500
            and "external input" in response.text
        )
        assert source.read_bytes() == original and not generated.exists()
        assert (
            not (media / "job.json").exists()
            and not (media / "job.json.metadata").exists()
        )
        assert not list(safe.iterdir()) and not engine._request_done
    finally:
        await engine.aclose()


@pytest.mark.asyncio
async def test_local_output_long_response_id(tmp_path):
    media = tmp_path / "media"
    media.mkdir()
    response_id = "j" * 220
    output = tmp_path / "out"
    engine = _http_engine(
        lambda request: httpx.Response(
            200,
            json={
                "id": response_id,
                "status": "completed",
                "data": [{"value": 1}],
            },
        ),
        fetch_media=False,
        media_dir=str(media),
    )
    try:
        response = await _post(
            engine,
            "/v1/videos",
            {"prompt": "video", "output": {"url": str(output)}},
        )
        assert response.status_code == 200, response.text
        filename = response_id + ".json"
        assert response.json()["output"]["artifacts"] == [filename]
        content = (output / filename).read_bytes()
        assert json.loads(content) == {"data": [{"value": 1}]}
        assert json.loads((output / (filename + ".metadata")).read_bytes())[
            "content_length"
        ] == len(content)
        assert not list(output.glob("*.tmp")) and not engine._request_done
    finally:
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize("cancel", [False, True])
async def test_pinned_local_write_settles_worker_and_closes_fds(
    tmp_path, monkeypatch, cancel
):
    source = _image(tmp_path / "input.png", (40, 20))
    original = source.read_bytes()
    sources = PathRefs()
    ref = PathRef.external(source, source.stat())
    sources.add(ref)
    sources.add(PathRef(source, ref.identity))
    assert sources.ref(source) == ref  # An output declaration cannot take ownership.
    OutputWriter.remember_sources([str(source)], sources)
    assert sources.ref(source).ownership is ref.ownership
    writer = OutputWriter()
    entered, release = threading.Event(), threading.Event()
    descriptors = []
    actual_replace = os.replace
    actual_dup = os.dup

    def remember_dup(fd):
        result = actual_dup(fd)
        descriptors.append(result)
        return result

    def blocked_replace(src, dst, **kwargs):
        descriptors.extend([kwargs["src_dir_fd"], kwargs["dst_dir_fd"]])
        entered.set()
        assert release.wait(3)
        if not cancel:
            raise OSError("injected replace failure")
        return actual_replace(src, dst, **kwargs)

    monkeypatch.setattr(os, "dup", remember_dup)
    monkeypatch.setattr(os, "replace", blocked_replace)
    caller = asyncio.create_task(
        writer.write(
            OutputRequest(url=str(tmp_path / "out")),
            b"{}",
            "nested/job.json",
            sources=sources,
        )
    )
    try:
        for _ in range(200):
            if entered.is_set():
                break
            await asyncio.sleep(0.01)
        assert entered.is_set()
        if cancel:
            caller.cancel()
            await asyncio.sleep(0.02)
            caller.cancel()
            await asyncio.sleep(0.02)
            assert not caller.done()
            for fd in descriptors:
                os.fstat(fd)
        release.set()
        with pytest.raises(asyncio.CancelledError if cancel else OutputWriteError):
            await caller
        for fd in descriptors:
            with pytest.raises(OSError):
                os.fstat(fd)
        assert source.read_bytes() == original
        output = tmp_path / "out" / "nested"
        assert not list(output.glob("*.tmp"))
        assert (output / "job.json").exists() == cancel
        assert (output / "job.json.metadata").exists() == cancel
        if cancel:
            from aibrix.storage.local import LocalStorage

            storage = LocalStorage(str(tmp_path / "out"))
            metadata = await storage._load_metadata("nested/job.json")
            assert metadata[0] == "application/json" and metadata[2] == 2
    finally:
        release.set()
        await asyncio.gather(caller, return_exceptions=True)


@pytest.mark.asyncio
@pytest.mark.parametrize("phase", ["initial_post", "poll"])
@pytest.mark.parametrize("timeout", [False, True])
async def test_video_transport_cancellation_drains_before_shutdown(
    tmp_path, monkeypatch, phase, timeout
):
    from aibrix.openai_frontend.proxy.runtime import video_job
    from aibrix.runtime.downloaders import await_thread_completion

    media = tmp_path / "media"
    media.mkdir()
    source = _image(media / "input.png")
    entered, closing, release = asyncio.Event(), asyncio.Event(), asyncio.Event()
    stopped = asyncio.Event()
    transports, references = [], []

    async def upstream(request):
        if request.method == "POST":
            uri = json.loads(request.content)["reference"]["image_url"]["url"]
            references.append(Path(unquote(urlparse(uri).path)))
            assert references[0] != source and references[0].exists()
            if phase == "poll":
                return httpx.Response(200, json={"id": "job", "status": "queued"})
        else:
            assert phase == "poll" and request.method == "GET"
        transports.append(asyncio.current_task())
        entered.set()
        try:
            await asyncio.Event().wait()
        except asyncio.CancelledError:
            closing.set()
            await await_thread_completion(release.wait())
            assert references[0].exists()
            stopped.set()
            raise

    engine = _http_engine(
        upstream,
        fetch_media=False,
        media_dir=str(media),
        video_output_timeout=0.03 if timeout and phase == "initial_post" else 30,
    )
    if timeout and phase == "poll":
        monkeypatch.setattr(video_job, "VIDEO_POLL_REQUEST_TIMEOUT_SECONDS", 0.01)
    caller = asyncio.create_task(
        _post(
            engine,
            "/v1/videos",
            {
                "prompt": "video",
                "reference": {
                    "type": "image_url",
                    "image_url": {"url": source.as_uri()},
                },
                "processors": {"pre": [_call("image.resize", max_edge=50)]},
                "output": {"url": str(tmp_path / "out")},
            },
        )
    )
    closer = None
    try:
        await asyncio.wait_for(entered.wait(), 2)
        if not timeout:
            caller.cancel()
        await asyncio.wait_for(closing.wait(), 2)
        closer = asyncio.create_task(engine.aclose())
        done, _ = await asyncio.wait((closer,), timeout=0.05)
        assert not done and not caller.done() and engine._request_done
        assert references[0].exists() and not transports[0].done()
        assert not stopped.is_set()
        release.set()
        await asyncio.wait_for(
            asyncio.gather(caller, closer, return_exceptions=True), 2
        )
        assert stopped.is_set() and transports[0].done()
        assert not transports[0]._log_traceback
        assert not references[0].exists() and not engine._request_done
        assert not engine._retained.pending() and not engine._video_tasks
    finally:
        release.set()
        await asyncio.gather(
            caller, *([closer] if closer else []), *transports, return_exceptions=True
        )
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize("with_payload", [False, True])
async def test_video_post_clear_media_removes_paths_from_receipt_and_cache(
    tmp_path, with_payload
):
    source = tmp_path / "generated.mp4"
    source.write_bytes(b"video")
    envelope = {
        "id": "job",
        "status": "completed",
        "seconds": 5,
        "file_path": str(source),
        "file_paths": [str(source)],
        "url": source.as_uri(),
        "urls": [source.as_uri()],
    }
    if with_payload:
        envelope["data"] = [{"value": 1}]
    calls = []

    async def clear_media(ctx):
        calls.append(ctx.workdir)
        assert ctx.target.paths == (source,)
        return ProcessorOutput(ProcessorMedia(()))

    def upstream(request):
        assert request.method == "POST", "cached retrieval must not poll"
        return httpx.Response(200, json=envelope)

    engine = _http_engine(upstream, fetch_media=False)
    engine._processor_registry.define(
        ProcessorDefinition("clear_media", "post", "output_media", {}, clear_media)
    )
    try:
        response = await _post(
            engine,
            "/v1/videos",
            {
                "prompt": "video",
                "output": {"url": str(tmp_path / "out")},
                "processors": {"post": [_call("clear_media")]},
            },
        )
        assert response.status_code == 200, response.text
        receipt = response.json()
        assert receipt["status"] == "completed" and receipt["seconds"] == 5
        assert receipt["file_path"] is None and receipt["file_paths"] == []
        assert receipt["url"] is None and receipt["urls"] == []
        assert receipt["output"]["artifacts"] == (["job.json"] if with_payload else [])
        async with httpx.AsyncClient(
            base_url="http://frontend",
            transport=httpx.ASGITransport(app=FastApiFrontend(engine=engine).app),
        ) as client:
            cached = await client.get("/v1/videos/job")
        assert cached.status_code == 200 and cached.json() == receipt
        assert len(calls) == 1 and not calls[0].exists() and not source.exists()
        assert not engine._request_done
    finally:
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize("stage", ["pre", "post"])
@pytest.mark.parametrize("timeout", [False, True])
async def test_video_processor_cancellation_waits_for_worker_before_cleanup(
    tmp_path, monkeypatch, stage, timeout
):
    entered, release, stopped = threading.Event(), threading.Event(), threading.Event()
    source = tmp_path / "video.mp4"
    source.write_bytes(b"video")
    seen = {}

    def upstream(request):
        assert stage == "post", "pre cancellation must not reach inference"
        assert request.method == "POST", "completed generation must not poll again"
        return httpx.Response(
            200, json={"id": "job", "status": "completed", "file_path": str(source)}
        )

    engine = _http_engine(upstream, fetch_media=False, media_dir=str(tmp_path))
    original_run = engine._processor_executor._run

    async def blocked_processor(definition, ctx):
        seen["work"] = ctx.workdir

        def work(cancelled):
            seen["cancelled"] = cancelled
            with ctx.target.open(source) as reader:
                entered.set()
                assert release.wait(5)
                assert source.exists() and ctx.workdir.exists()
                assert reader.read() == b"video" and cancelled.is_set()
                stopped.set()
            return ProcessorOutput()

        return await original_run(
            dataclasses.replace(
                definition, run=lambda ctx: builtin_processors._blocking(work)
            ),
            ctx,
        )

    monkeypatch.setattr(engine._processor_executor, "_run", blocked_processor)
    call = (
        _call("image.resize", max_edge=50)
        if stage == "pre"
        else _call("files.compress")
    )
    if timeout:
        call["processor"]["timeout_seconds"] = 0.01
    body = {"prompt": "video", "processors": {stage: [call]}}
    if stage == "pre":
        body["reference"] = {"type": "image_url", "image_url": {"url": source.as_uri()}}
    else:
        body["output"] = {"url": str(tmp_path / "out")}
    task = asyncio.create_task(_post(engine, "/v1/videos", body))
    closer = None
    try:
        assert await asyncio.to_thread(entered.wait, 2)
        if not timeout:
            task.cancel()
        assert await asyncio.to_thread(seen["cancelled"].wait, 2)
        if not timeout:
            task.cancel()
            closer = asyncio.create_task(engine.aclose())
            await asyncio.sleep(0.02)
        assert not task.done() and engine._request_done
        assert source.exists() and seen["work"].exists()
        if closer is not None:
            assert not closer.done()
        release.set()
        result = (
            await asyncio.wait_for(asyncio.gather(task, return_exceptions=True), 2)
        )[0]
        if timeout:
            assert result.status_code == 502 and "timed out" in result.text
        else:
            assert isinstance(result, asyncio.CancelledError)
            await asyncio.wait_for(closer, 2)
        assert stopped.is_set() and source.exists() is (stage == "pre")
        assert not seen["work"].exists()
        assert not (tmp_path / "out").exists() and not engine._request_done
        if stage == "post":
            assert engine._uploaded_videos["job"].status == "failed"
        else:
            assert not engine._uploaded_videos
    finally:
        release.set()
        await asyncio.gather(
            task, *([closer] if closer else []), return_exceptions=True
        )
        await engine.aclose()

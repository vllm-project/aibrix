# Copyright 2026 The Aibrix Team.
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy at http://www.apache.org/licenses/LICENSE-2.0

"""Real files and proxy persistence tests for the executable built-ins."""

import asyncio
import base64
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
from aibrix.openai_frontend.proxy import builtin_processors
from aibrix.openai_frontend.proxy.output_writer import OutputWriteError, OutputWriter
from aibrix.openai_frontend.proxy.processors import (
    ProcessorExecutionError,
    ProcessorExecutor,
    ProcessorRegistry,
    ProcessorState,
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
    from aibrix.openai_frontend.proxy.demo_processors import DEMO_PROCESSORS

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


def test_blocking_cancellation_waits_for_worker():
    entered = threading.Event()
    release = threading.Event()
    stopped = threading.Event()

    def work(cancelled):
        entered.set()
        release.wait(5)
        assert cancelled.is_set()
        stopped.set()

    async def run():
        task = asyncio.create_task(builtin_processors._blocking(work))
        assert await asyncio.to_thread(entered.wait, 5)
        task.cancel()
        await asyncio.sleep(0)
        task.cancel()
        await asyncio.sleep(0)
        assert not task.done()
        release.set()
        with pytest.raises(asyncio.CancelledError):
            await task
        assert stopped.is_set()

    try:
        asyncio.run(run())
    finally:
        release.set()


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
    from aibrix.openai_frontend.proxy.demo_processors import DEMO_PROCESSORS

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
    "phase", ["during_read", "between_processors", "writer_handoff", "image_handoff"]
)
def test_post_pipeline_rejects_source_changes(tmp_path, monkeypatch, phase):
    source = _image(tmp_path / "source.png")
    changed = []

    async def run():
        def upstream(request):
            return httpx.Response(
                200,
                json={
                    "id": "result",
                    "created": 1,
                    "data": [{"file_path": str(source)}],
                },
            )

        engine = _http_engine(upstream, fetch_media=False)
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
                if definition.name == "files.compress":
                    intermediate = ctx.target.paths[0]
                    replacement = _image(tmp_path / "replacement.png")
                    replacement.replace(intermediate)
                    changed.append(intermediate)
                return await original_run(definition, ctx)

            monkeypatch.setattr(
                engine._processor_executor, "_run", replace_intermediate
            )
        else:
            original_write = engine._write_output

            async def replace_final(*args, **kwargs):
                final = Path(args[4][0])
                # Identity must be pinned before the Writer's first path inspection.
                assert final in args[5]
                replacement = tmp_path / "replacement"
                replacement.write_bytes(b"replacement")
                replacement.replace(final)
                changed.append(final)
                return await original_write(*args, **kwargs)

            monkeypatch.setattr(engine, "_write_output", replace_final)
        try:
            calls = [_call("image.resize", max_edge=50)]
            if phase != "image_handoff":
                calls.append(_call("files.compress", format="zip"))
            response = await _post(
                engine,
                "/v1/images/generations",
                {
                    "prompt": "image",
                    "processors": {"post": calls},
                    "output": {"url": str(tmp_path / "out")},
                },
            )
            assert response.status_code == (
                500 if phase.endswith("handoff") else 502
            ), response.text
            assert changed and not (tmp_path / "out").exists()
            assert not engine._in_flight
        finally:
            await engine.aclose()

    asyncio.run(run())

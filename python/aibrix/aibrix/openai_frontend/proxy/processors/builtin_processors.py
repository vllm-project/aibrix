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
import mimetypes
import os
import tarfile
import threading
import time
import zipfile
from pathlib import Path
from typing import Any, BinaryIO, Callable, Dict, Mapping, TypeVar, cast

from jsonschema import Draft202012Validator
from jsonschema.exceptions import ValidationError
from referencing import Registry
from referencing.exceptions import NoSuchResource

from aibrix.openai_frontend.proxy.processors import (
    ProcessorContext,
    ProcessorDefinition,
    ProcessorMedia,
    ProcessorOutput,
    ProcessorPayload,
)
from aibrix.openai_frontend.schemas.processors import PROCESSOR_ARCHIVE_FORMATS
from aibrix.openai_frontend.utils.utils import ClientError
from aibrix.runtime.downloaders import await_thread_completion

_CHUNK_BYTES = 1024 * 1024
_T = TypeVar("_T")


def _check(ctx: ProcessorContext, stopped: threading.Event) -> None:
    if stopped.is_set() or time.monotonic() >= ctx.deadline:
        raise TimeoutError("processor work cancelled or deadline exceeded")


async def _blocking(operation: Callable[[threading.Event], _T]) -> _T:
    """Stop cooperatively, but never release files while a worker still uses them."""
    stopped = threading.Event()
    task = asyncio.create_task(asyncio.to_thread(operation, stopped))
    try:
        return await asyncio.shield(task)
    except BaseException:
        stopped.set()
        try:
            await await_thread_completion(task)
        except BaseException:
            pass  # Preserve the caller's original error/cancellation.
        raise


def _selected(path: Path, media_types: tuple[str, ...]) -> bool:
    if not media_types:
        return True
    mime = mimetypes.guess_type(path.name)[0] or ""
    return mime.split("/", 1)[0] in media_types


# Canonical suffix per image format. The output writer only accepts paths whose
# MIME type is image/video, so registry-only aliases (.jfif, .j2k, .apng, ...)
# must never become replacement names even when Pillow lists them first.
_FORMAT_SUFFIXES: Mapping[str, str] = {
    "BMP": ".bmp",
    "GIF": ".gif",
    "ICO": ".ico",
    "JPEG": ".jpg",
    "PNG": ".png",
    "PPM": ".ppm",
    "TGA": ".tga",
    "TIFF": ".tif",
    "WEBP": ".webp",
}


def _output_suffix(
    format_name: str, registered: Mapping[str, str], source: Path
) -> str:
    """Choose an output suffix the proxy accepts; never a registry-only alias."""
    accepted = tuple(
        ext
        for ext, fmt in registered.items()
        if fmt == format_name
        and (mimetypes.guess_type("x" + ext)[0] or "").startswith(("image/", "video/"))
    )
    if source.suffix.lower() in accepted:
        return source.suffix.lower()
    canonical = _FORMAT_SUFFIXES.get(format_name)
    if canonical is not None:
        return canonical
    if accepted:
        return accepted[0]
    raise ClientError("unsupported image output format")


def _archive_member(index: int, source: Path) -> str:
    """Relative member name; the proxy owns the file, so keep its readable name."""
    name = "".join(char for char in source.name if char.isprintable())
    name = name.replace("\\", "_")
    if name in ("", ".", ".."):
        name = "file"
    return f"{index:04d}-{name}"


def _save_options(image: Any, format_name: str) -> Dict[str, Any]:
    """Keep metadata and JPEG quality instead of re-encoding at the defaults."""
    options: Dict[str, Any] = {
        key: image.info[key] for key in ("exif", "icc_profile") if key in image.info
    }
    if format_name == "JPEG":
        options["quality"] = "keep"
    return options


async def compress_files(ctx: ProcessorContext) -> ProcessorOutput:
    """Replace selected local output files with one archive; preserve other files."""
    if not isinstance(ctx.target, ProcessorMedia):
        raise ClientError("files.compress requires local output files")
    media = ctx.target
    selected = tuple(
        p for p in dict.fromkeys(media.paths) if _selected(p, ctx.media_types)
    )
    if not selected:
        raise ClientError("files.compress has no matching local files")
    unselected = tuple(p for p in media.paths if p not in selected)
    format_name = ctx.arguments.get("format", "zip")
    if format_name not in PROCESSOR_ARCHIVE_FORMATS:
        raise ClientError("unsupported archive format")
    output = ctx.workdir / f"files.{format_name}"

    def pack(stopped: threading.Event) -> ProcessorOutput:
        created = False
        try:
            _check(ctx, stopped)
            with output.open("xb") as destination:
                created = True
                if format_name == "zip":
                    with zipfile.ZipFile(
                        destination, "w", compression=zipfile.ZIP_DEFLATED
                    ) as archive:
                        for index, source in enumerate(selected):
                            _check(ctx, stopped)
                            # Generated relative names: no source directories or
                            # links, but the readable name is preserved.
                            name = _archive_member(index, source)
                            with (
                                media.open(source) as reader,
                                archive.open(name, "w", force_zip64=True) as writer,
                            ):
                                while chunk := reader.read(_CHUNK_BYTES):
                                    _check(ctx, stopped)
                                    writer.write(chunk)
                else:
                    mode = "w|gz" if format_name == "tar.gz" else "w|"
                    with tarfile.open(fileobj=destination, mode=mode) as tar_archive:
                        for index, source in enumerate(selected):
                            _check(ctx, stopped)
                            with media.open(source) as reader:
                                info = tarfile.TarInfo(_archive_member(index, source))
                                info.size = os.fstat(reader.fileno()).st_size
                                info.mode = 0o644

                                class CheckedReader:
                                    def read(self, size: int = -1) -> bytes:
                                        _check(ctx, stopped)
                                        return reader.read(size)

                                tar_archive.addfile(
                                    info, cast(BinaryIO, CheckedReader())
                                )
            _check(ctx, stopped)
            return ProcessorOutput(ProcessorMedia((output, *unselected)))
        except BaseException:
            if created:
                output.unlink(missing_ok=True)
            raise

    return await _blocking(pack)


def _no_remote_schema(uri: str):
    raise NoSuchResource(uri)


def _offline_registry() -> Any:
    # referencing's attrs-generated keyword constructor is not visible to mypy.
    factory: Any = Registry
    return factory(retrieve=_no_remote_schema)


def _reject_constant(value: str):
    raise ValueError(f"invalid JSON number: {value}")


async def validate_json(ctx: ProcessorContext) -> ProcessorOutput:
    """Validate JSON with Draft 2020-12, without modifying bytes or fetching refs."""
    if not isinstance(ctx.target, ProcessorPayload):
        raise ClientError("json.validate requires a payload")
    content = ctx.target.content

    def validate(stopped: threading.Event) -> ProcessorOutput:
        _check(ctx, stopped)
        try:
            value = json.loads(content, parse_constant=_reject_constant)
            validator = Draft202012Validator(
                ctx.arguments["schema"], registry=_offline_registry()
            )
            validator.validate(value)
        except ValidationError as exc:
            path = "$" + "".join(
                f"[{part}]" if isinstance(part, int) else f".{part}"
                for part in exc.absolute_path
            )
            # Do not echo result values or full schemas into error responses.
            raise ClientError(
                f"JSON result at {path} violates '{exc.validator}'"
            ) from exc
        except (ValueError, UnicodeError) as exc:
            raise ClientError("JSON result is not valid JSON") from exc
        _check(ctx, stopped)
        return ProcessorOutput()

    return await _blocking(validate)


async def resize_images(ctx: ProcessorContext) -> ProcessorOutput:
    """Downsize still images, preserve aspect ratio, and leave small/non-image files alone."""
    from PIL import Image, UnidentifiedImageError

    if not isinstance(ctx.target, ProcessorMedia):
        raise ClientError("image.resize requires local media")
    media = ctx.target
    inputs = media.paths
    if ctx.media_types and "image" not in ctx.media_types:
        raise ClientError("image.resize requires image media selection")
    max_edge = ctx.arguments["max_edge"]

    def resize(stopped: threading.Event) -> ProcessorOutput:
        paths = []
        created = []
        images = 0
        try:
            for index, source in enumerate(inputs):
                _check(ctx, stopped)
                with media.open(source) as reader:
                    try:
                        image = Image.open(reader)
                    except UnidentifiedImageError:
                        mime = mimetypes.guess_type(source.name)[0] or ""
                        if mime.startswith("image/"):
                            raise ClientError(
                                "image.resize received an invalid image"
                            ) from None
                        paths.append(source)
                        continue
                    except (
                        OSError,
                        SyntaxError,
                        ValueError,
                        Image.DecompressionBombError,
                    ) as exc:
                        raise ClientError(
                            "image.resize received an invalid image"
                        ) from exc
                    with image:
                        images += 1
                        try:
                            if getattr(image, "n_frames", 1) > 1:
                                raise ClientError(
                                    "image.resize does not support animated or multi-page images"
                                )
                            image.load()  # Reject truncated input even when no resizing is needed.
                        except (
                            OSError,
                            SyntaxError,
                            ValueError,
                            Image.DecompressionBombError,
                        ) as exc:
                            raise ClientError(
                                "image.resize received an invalid image"
                            ) from exc
                        format_name = image.format or ""
                        extension = _output_suffix(
                            format_name, Image.registered_extensions(), source
                        )
                        needs_resize = max(image.size) > max_edge
                        if not needs_resize and source.suffix.lower() == extension:
                            paths.append(source)
                            continue
                        if needs_resize:
                            image.thumbnail(
                                (max_edge, max_edge), Image.Resampling.LANCZOS
                            )
                        _check(ctx, stopped)
                        output = ctx.workdir / f"{index:04d}{extension}"
                        with output.open("xb") as writer:
                            created.append(output)
                            if needs_resize:
                                image.save(
                                    writer,
                                    format=format_name,
                                    **_save_options(image, format_name),
                                )
                            else:
                                # Normalize the name without re-encoding small images.
                                # Keep the scoped reader's identity checks and deadline.
                                reader.seek(0)
                                while chunk := reader.read(_CHUNK_BYTES):
                                    _check(ctx, stopped)
                                    writer.write(chunk)
                        paths.append(output)
            if not images:
                raise ClientError("image.resize found no images")
            _check(ctx, stopped)
            return ProcessorOutput(ProcessorMedia(tuple(paths)))
        except BaseException:
            for output in created:
                output.unlink(missing_ok=True)
            raise

    return await _blocking(resize)


_RESIZE_PARAMETERS = {
    "type": "object",
    "properties": {"max_edge": {"type": "integer", "minimum": 1}},
    "required": ["max_edge"],
    "additionalProperties": False,
}
BUILTIN_PROCESSORS = (
    ProcessorDefinition(
        "files.compress",
        "post",
        "output_media",
        {
            "type": "object",
            "properties": {"format": {"enum": list(PROCESSOR_ARCHIVE_FORMATS)}},
            "additionalProperties": False,
        },
        compress_files,
    ),
    ProcessorDefinition(
        "json.validate",
        "post",
        "output_payload",
        {
            "type": "object",
            "properties": {
                "schema": {
                    # Boolean schemas are valid Draft 2020-12 documents.
                    "type": ["object", "boolean"],
                    "allOf": [{"$ref": "https://json-schema.org/draft/2020-12/schema"}],
                    "properties": {
                        "$schema": {
                            "const": "https://json-schema.org/draft/2020-12/schema"
                        }
                    },
                }
            },
            "required": ["schema"],
            "additionalProperties": False,
        },
        validate_json,
    ),
    ProcessorDefinition(
        "image.resize",
        "pre",
        "input_media",
        _RESIZE_PARAMETERS,
        resize_images,
        media=("image",),
    ),
    ProcessorDefinition(
        "image.resize",
        "post",
        "output_media",
        _RESIZE_PARAMETERS,
        resize_images,
        media=("image",),
    ),
)

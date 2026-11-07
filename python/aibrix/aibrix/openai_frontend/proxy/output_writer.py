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

"""Persist a response to the destination named by an ``OutputRequest``.

Object-store and local destinations both go through a storage built with
``create_storage_from_env``, so credentials, endpoint and CRC settings are
configured once via ``STORAGE_*`` env vars:

* ``tos://bucket/prefix`` / ``s3://bucket/prefix`` -- the env storage must be
  of the matching type and bucket; anything else is rejected rather than
  silently written to a different bucket.
* local path -- a ``LocalStorage`` rooted at the requested directory.  The
  ``.metadata`` sidecars LocalStorage keeps next to each object are part of
  its format and are left in place.
* ``http(s)://`` -- a single ``PUT`` of the body (e.g. a pre-signed URL); not
  a storage backend, so it uses httpx directly.
"""

from __future__ import annotations

import asyncio
import logging
import mimetypes
import os
import stat
import tempfile
import zipfile
from contextlib import ExitStack
from pathlib import Path
from typing import Any, BinaryIO, Callable, Optional, Sequence, Union
from urllib.parse import quote, unquote, urlparse

import httpx

from aibrix.openai_frontend.schemas.output import (
    HTTP_SCHEMES,
    LOCAL_SCHEMES,
    OutputRequest,
    OutputResponse,
    local_output_path,
    object_store_schemes,
)
from aibrix.openai_frontend.schemas.processors import PROCESSOR_ARCHIVE_FORMATS
from aibrix.runtime.downloaders import await_thread_completion

logger = logging.getLogger(__name__)

OUTPUT_CHUNK_SIZE_BYTES = 1024 * 1024

StorageFactory = Callable[[], object]
SourceIdentities = dict[Path, Optional[os.stat_result]]


class OutputWriteError(RuntimeError):
    """The response could not be written to the requested destination."""


def storage_type_for_scheme(scheme: str):
    """The storage backend that owns ``<scheme>://`` URLs.

    Identity is taken from :class:`aibrix.storage.types.StorageType`, whose
    values double as URL schemes for object backends. Imported lazily so this
    module never eagerly loads storage backend SDKs.
    """
    from aibrix.storage.types import StorageType

    try:
        return StorageType(scheme)
    except ValueError:
        raise OutputWriteError(
            f"no object-storage backend matches URL scheme '{scheme}://'"
        ) from None


def _default_storage_factory():
    from aibrix.storage import create_storage_from_env

    return create_storage_from_env()


def _strip_prefix_once(value: str, prefix: str) -> str:
    return value[len(prefix) :] if prefix and value.startswith(prefix) else value


def relative_artifact(base_url: str, artifact_url: str) -> str:
    """Return ``artifact_url`` relative to the requested destination prefix.

    ``OutputResponse.url`` keeps the user's exact destination; ``artifacts`` are
    the keys/paths written *under* it, without repeating that prefix::

        url=tos://b/run  -> artifact tos://b/run/out.json  -> "out.json"
        url=/data/out    -> artifact /data/out/sub/x.json  -> "sub/x.json"

    Single-object HTTP PUT destinations are not prefixes, so the full URL is
    returned unchanged.
    """
    parsed_base = urlparse(base_url)
    parsed_art = urlparse(artifact_url)
    if parsed_art.scheme in HTTP_SCHEMES:
        return artifact_url
    if parsed_base.scheme in object_store_schemes():
        # Bucket + key prefix: tos://bkt/pref -> strip tos://bkt/pref
        prefix = f"{parsed_base.scheme.lower()}://{parsed_base.netloc}{parsed_base.path}".rstrip(
            "/"
        )
        rest = _strip_prefix_once(
            f"{parsed_art.scheme.lower()}://{parsed_art.netloc}{parsed_art.path}",
            prefix,
        ).lstrip("/")
        return rest or ""
    # Local destination: an absolute directory or a file:// directory.
    base_dir = Path(local_output_path(base_url)).resolve()
    art_path = Path(local_output_path(artifact_url)).resolve()
    relative = str(art_path.relative_to(base_dir))
    # file:// receipts are URIs; encode literal filename '%' characters as well.
    return quote(relative, safe="/") if parsed_base.scheme == "file" else relative


class OutputWriter:
    def __init__(
        self,
        storage_factory: Optional[StorageFactory] = None,
        http_timeout: float = 60.0,
    ):
        self._storage_factory = storage_factory or _default_storage_factory
        self._storage: Any = None
        self._storage_task: Optional[asyncio.Task[Any]] = None
        self._http_timeout = http_timeout

    async def _env_storage(self):
        # Construction can make blocking SDK calls (e.g. S3 head_bucket).
        # Concurrent callers share initialization; cancelling a waiter does not
        # abandon the client or start a second factory invocation.
        if self._storage is not None:
            return self._storage
        if self._storage_task is None:
            self._storage_task = asyncio.create_task(self._create_env_storage())
            self._storage_task.add_done_callback(
                lambda task: task.exception() if not task.cancelled() else None
            )
        task = self._storage_task
        try:
            return await asyncio.shield(task)
        except Exception:
            if self._storage_task is task:
                self._storage_task = None  # A later request may retry the factory.
            raise

    async def _create_env_storage(self):
        self._storage = await asyncio.to_thread(self._storage_factory)
        return self._storage

    async def write_bundle(
        self,
        spec: OutputRequest,
        data: Optional[bytes],
        name: str,
        media_paths: Sequence[str],
        sources: Optional[SourceIdentities] = None,
        *,
        processor_workdir: Optional[Path] = None,
    ) -> tuple[OutputResponse, int]:
        """Copy explicitly declared local media; HTTP multi-file outputs use ZIP.

        Sources are cleaned on success, failure and cancellation, after readers
        close and SDK workers finish. Cleanup cannot remove a replaced file or
        destination. A failed bundle may leave earlier destination artifacts.
        """
        entries: list[tuple[str, Union[bytes, BinaryIO], str]] = []
        sources = sources if sources is not None else {}
        total = 0
        result = OutputResponse(url=spec.url)
        error: Optional[BaseException] = None
        try:
            self.remember_sources(
                [*media_paths, *(str(path) for path in sources)], sources
            )
            with ExitStack() as stack:
                if data is not None:
                    entries.append((name, data, "application/json"))
                    total += len(data)
                for index, path in enumerate(dict.fromkeys(media_paths)):
                    source = Path(path)
                    media_type = mimetypes.guess_type(source.name)[0] or ""
                    # Archives are allowed only when produced inside this request's
                    # processor workspace; never broaden engine output discovery.
                    archive_suffix = next(
                        (
                            "." + fmt
                            for fmt in PROCESSOR_ARCHIVE_FORMATS
                            if source.name.endswith("." + fmt)
                        ),
                        None,
                    )
                    processor_archive = (
                        archive_suffix is not None
                        and processor_workdir is not None
                        and source.resolve().is_relative_to(processor_workdir.resolve())
                    )
                    if (
                        not source.is_absolute()
                        or ".." in source.parts
                        or (
                            not media_type.startswith(("image/", "video/"))
                            and not processor_archive
                        )
                    ):
                        raise OutputWriteError(
                            "Engine output must name an absolute image/video file"
                        )
                    try:
                        fd = os.open(
                            source, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK
                        )
                    except FileNotFoundError:
                        # A terminal response declared this output; a missing
                        # file fails the write instead of dropping the payload.
                        raise OutputWriteError(
                            f"Engine output is missing: {source}"
                        ) from None
                    reader = stack.enter_context(os.fdopen(fd, "rb"))
                    info = os.fstat(reader.fileno())
                    if not stat.S_ISREG(info.st_mode):
                        raise OutputWriteError(
                            "Engine output is not a regular media file"
                        )
                    saved = sources.get(source)
                    if saved is None or (saved.st_dev, saved.st_ino) != (
                        info.st_dev,
                        info.st_ino,
                    ):
                        raise OutputWriteError(f"Engine output was replaced: {source}")
                    sources[source] = info
                    if processor_archive and archive_suffix == ".tar.gz":
                        # guess_type returns application/x-tar plus gzip encoding;
                        # these are compressed archive bytes, not a plain TAR body.
                        media_type = "application/gzip"
                    suffix = (
                        archive_suffix if processor_archive else source.suffix.lower()
                    )
                    artifact_name = f"{Path(name).stem}-{index}{suffix}"
                    entries.append((artifact_name, reader, media_type))
                    total += info.st_size
                if not entries:
                    raise OutputWriteError(
                        "Engine response has no payload or local media"
                    )
                if (
                    urlparse(spec.url).scheme.lower() in HTTP_SCHEMES
                    and len(entries) > 1
                ):
                    archive = stack.enter_context(tempfile.TemporaryFile())

                    def pack() -> None:
                        with zipfile.ZipFile(
                            archive, "w", compression=zipfile.ZIP_STORED
                        ) as zf:
                            for entry_name, content, _ in entries:
                                if isinstance(content, bytes):
                                    zf.writestr(entry_name, content)
                                else:
                                    with zf.open(
                                        entry_name, "w", force_zip64=True
                                    ) as target:
                                        while chunk := content.read(
                                            OUTPUT_CHUNK_SIZE_BYTES
                                        ):
                                            target.write(chunk)
                        archive.seek(0)

                    await await_thread_completion(asyncio.to_thread(pack))
                    result = await self.write(
                        spec, archive, f"{Path(name).stem}.zip", "application/zip"
                    )
                else:
                    artifacts: list[str] = []
                    for entry_name, content, media_type in entries:
                        result = await self.write(spec, content, entry_name, media_type)
                        artifacts.extend(result.artifacts)
                    result = OutputResponse(url=spec.url, artifacts=artifacts)
            return result, total
        except BaseException as exc:
            error = exc
            if isinstance(exc, Exception) and not isinstance(exc, OutputWriteError):
                raise OutputWriteError(
                    f"Failed to persist engine media: {exc}"
                ) from exc
            raise
        finally:
            await self._finalize(spec, result, sources, error)

    async def _finalize(
        self,
        spec: OutputRequest,
        result: OutputResponse,
        sources: SourceIdentities,
        error: Optional[BaseException],
    ) -> None:
        # Readers are closed and SDK threads have settled before cleanup starts.
        # Preserve the primary failure, including repeated caller cancellation.
        try:
            await await_thread_completion(
                asyncio.to_thread(self._remove_sources, spec, result, sources)
            )
        except BaseException as exc:
            if isinstance(exc, Exception):
                logger.exception("Source cleanup failed after output write")
            elif error is None:
                raise  # Cleanup remains awaited; cancellation is never swallowed.

    @staticmethod
    def remember_sources(paths: Sequence[str], sources: SourceIdentities) -> None:
        """Retain first inode identity; generation may change size, not ownership."""
        first_error: Optional[Exception] = None
        for value in dict.fromkeys(paths):
            path = Path(value)
            if not path.is_absolute() or ".." in path.parts:
                continue
            try:
                info = path.lstat()
                if not stat.S_ISREG(info.st_mode):
                    continue
                saved = sources.get(path)
                if saved is None or (saved.st_dev, saved.st_ino) == (
                    info.st_dev,
                    info.st_ino,
                ):
                    sources[path] = info
            except FileNotFoundError:
                sources.setdefault(path, None)
            except Exception as exc:
                first_error = first_error or exc
                logger.error("Failed to inspect source %s: %s", path, exc)
        if first_error is not None:
            raise first_error

    async def cleanup_completed_sources(
        self,
        sources: SourceIdentities,
        spec: OutputRequest,
        artifacts: Sequence[str] = (),
    ) -> None:
        """Refresh generation's final sizes, then attempt cleanup even if stat fails.

        Call only after trusted termination or engine stop. The saved inode still
        defines ownership; refreshing must never claim a replacement file.
        Error precedence belongs to the lifecycle caller.
        """
        try:
            self.remember_sources([str(path) for path in sources], sources)
        finally:
            await self.cleanup_sources(sources, spec, artifacts)

    async def cleanup_sources(
        self,
        sources: SourceIdentities,
        spec: OutputRequest,
        artifacts: Sequence[str] = (),
    ) -> None:
        await await_thread_completion(
            asyncio.to_thread(
                self._remove_sources,
                spec,
                OutputResponse(url=spec.url, artifacts=list(artifacts)),
                sources,
            )
        )

    @staticmethod
    def _remove_sources(
        spec: OutputRequest,
        result: OutputResponse,
        sources: SourceIdentities,
    ) -> None:
        parsed = urlparse(spec.url)
        destinations = set()
        if parsed.scheme.lower() in LOCAL_SCHEMES:
            base = Path(local_output_path(spec.url))
            destinations = {
                (base / (unquote(name) if parsed.scheme == "file" else name)).resolve()
                for name in result.artifacts
            }
        cleanup_error: Optional[Exception] = None
        for path, saved in sources.items():
            if saved is None:
                continue
            try:
                if path.resolve() in destinations:
                    continue  # Never remove the persisted destination.
                try:
                    current = path.lstat()
                except FileNotFoundError:
                    continue
                if (
                    current.st_dev,
                    current.st_ino,
                    current.st_size,
                    current.st_mtime_ns,
                ) != (saved.st_dev, saved.st_ino, saved.st_size, saved.st_mtime_ns):
                    raise OutputWriteError(
                        f"Engine output changed during persistence: {path}"
                    )
                path.unlink()
            except Exception as exc:
                if cleanup_error is None:
                    cleanup_error = exc
                logger.error("Failed to clean source %s: %s", path, exc)
        if cleanup_error is not None:
            raise cleanup_error

    async def write(
        self,
        spec: OutputRequest,
        data: Union[bytes, BinaryIO],
        name: str,
        content_type: str = "application/json",
    ) -> OutputResponse:
        parsed = urlparse(spec.url)
        scheme = parsed.scheme.lower()
        try:
            if scheme in object_store_schemes():
                artifact = await self._write_object_store(
                    scheme, parsed.netloc, parsed.path, name, data, content_type
                )
            elif scheme in LOCAL_SCHEMES:
                base = local_output_path(spec.url)
                artifact = await self._write_local(base, name, data, content_type)
            elif scheme in HTTP_SCHEMES:
                artifact = await self._write_http(spec.url, data, content_type)
            else:  # pragma: no cover - rejected by OutputRequest validation
                raise OutputWriteError(f"Unsupported output scheme: {scheme}")
            relative = relative_artifact(spec.url, artifact)
        except OutputWriteError:
            raise
        except Exception as exc:
            raise OutputWriteError(
                f"Failed to write output to {spec.url}: {type(exc).__name__}: {exc}"
            ) from exc
        logger.info("Wrote output to %s", artifact)
        return OutputResponse(url=spec.url, artifacts=[relative])

    async def _write_object_store(
        self,
        scheme: str,
        bucket: str,
        prefix: str,
        name: str,
        data: Union[bytes, BinaryIO],
        content_type: str,
    ) -> str:
        storage = await self._env_storage()
        expected = storage_type_for_scheme(scheme)
        actual = storage.get_type()
        if actual != expected:
            raise OutputWriteError(
                f"output.url uses {scheme}:// but the configured storage is "
                f"'{actual.value}'; set STORAGE_TYPE={expected.value} with its "
                f"credentials (STORAGE_{expected.value.upper()}_*)"
            )
        configured_bucket = getattr(storage, "bucket_name", None)
        if configured_bucket and configured_bucket != bucket:
            raise OutputWriteError(
                f"output.url bucket '{bucket}' does not match the configured "
                f"storage bucket '{configured_bucket}'"
            )
        if not isinstance(data, bytes):
            data = await await_thread_completion(asyncio.to_thread(data.read))
        key = "/".join(p for p in (prefix.strip("/"), name) if p)
        if not await await_thread_completion(
            storage.put_object(key, data, content_type=content_type)
        ):
            raise OutputWriteError("Object storage rejected output")
        return f"{scheme}://{bucket}/{key}"

    async def _write_local(
        self, base: str, name: str, data: Union[bytes, BinaryIO], content_type: str
    ) -> str:
        from aibrix.storage import create_storage
        from aibrix.storage.utils import generate_filename

        if not isinstance(data, bytes):
            data = await await_thread_completion(asyncio.to_thread(data.read))
        storage = await asyncio.to_thread(create_storage, "local", base_path=base)
        if not await await_thread_completion(
            storage.put_object(name, data, content_type=content_type)
        ):
            raise OutputWriteError("Local storage rejected output")
        # Use the same key encoding and extension rule as LocalStorage.
        return str(Path(base).resolve() / generate_filename(name, content_type))

    async def _write_http(
        self, url: str, data: Union[bytes, BinaryIO], content_type: str
    ) -> str:
        async def chunks():
            assert not isinstance(data, bytes)
            while chunk := await await_thread_completion(
                asyncio.to_thread(data.read, OUTPUT_CHUNK_SIZE_BYTES)
            ):
                yield chunk

        async with httpx.AsyncClient(timeout=self._http_timeout) as client:
            resp = await client.put(
                url,
                content=data if isinstance(data, bytes) else chunks(),
                headers={"Content-Type": content_type},
            )
        if resp.status_code >= 300:
            raise OutputWriteError(
                f"PUT {url} returned HTTP {resp.status_code}: {resp.text[:200]}"
            )
        return url

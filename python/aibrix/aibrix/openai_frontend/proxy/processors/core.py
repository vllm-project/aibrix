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

"""Processor contract and registry.

A processor transforms one scoped local-media or result-payload target.
The executor owns serial execution and validates replacements; the proxy owns
request adaptation and resource retention. Processors receive neither the full
request envelope nor storage credentials.
"""

from __future__ import annotations

import asyncio
import math
import os
import stat
import time
from contextlib import contextmanager
from copy import deepcopy
from dataclasses import dataclass, field, replace
from pathlib import Path
from typing import (
    Any,
    BinaryIO,
    Dict,
    Iterable,
    Iterator,
    Mapping,
    Optional,
    Protocol,
    Tuple,
    Union,
)

from jsonschema.exceptions import ValidationError
from jsonschema.validators import validator_for

from aibrix.openai_frontend.proxy.output.path_ref import PathRef, PathRefs
from aibrix.openai_frontend.schemas.processors import (
    DEFAULT_PROCESSOR_TIMEOUT_SECONDS,
    PROCESSOR_MEDIA_TYPES,
    PROCESSOR_POST_SCOPES,
    PROCESSOR_SCOPE_INPUT_MEDIA,
    PROCESSOR_SCOPE_OUTPUT_PAYLOAD,
    PROCESSOR_STAGE_POST,
    PROCESSOR_STAGE_PRE,
    JSONSchema,
    ProcessorCall,
    ProcessorPipeline,
    ProcessorScope,
    ProcessorStage,
)
from aibrix.openai_frontend.utils.utils import ClientError, ServerError, StatusCode
from aibrix.runtime.downloaders import await_thread_completion


class ProcessorExecutionError(ServerError):
    """Processor execution failed after a valid declaration (502)."""

    status_code = StatusCode.BAD_GATEWAY


@dataclass(frozen=True)
class ProcessorMedia:
    """Ordered local media paths, read-only inputs or explicit replacements."""

    paths: Tuple[Path, ...]
    # Optional pinned identities for declared output files and prior replacements.
    # Kept on the scoped media target, never exposed to payload processors.
    source_identities: Mapping[Path, Optional[os.stat_result]] = field(
        default_factory=dict, repr=False, compare=False
    )

    input_roots: Mapping[Path, Path] = field(
        default_factory=dict, repr=False, compare=False
    )

    @contextmanager
    def open(self, path: Path) -> Iterator[BinaryIO]:
        """Open an input read-only and check its declared identity on the actual fd.

        Media handlers should use this instead of reopening a path directly.
        Readers cannot follow symlinks or consume files replaced since declaration.
        A source modified during reading also fails before a replacement is used.
        """
        if path not in self.paths or not path.is_absolute() or ".." in path.parts:
            raise ClientError("processor requires a scoped absolute local file")
        flags = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK
        root = self.input_roots.get(path)
        if root is None:
            fd = os.open(path, flags)
        else:
            # Open each parent relative to the allowed root without following
            # links: a resolve/check followed by an ordinary open has a race.
            parts = path.relative_to(root).parts
            directory_fd = os.open(root, flags | os.O_DIRECTORY)
            try:
                for part in parts[:-1]:
                    next_fd = os.open(part, flags | os.O_DIRECTORY, dir_fd=directory_fd)
                    os.close(directory_fd)
                    directory_fd = next_fd
                fd = os.open(parts[-1], flags, dir_fd=directory_fd)
            finally:
                os.close(directory_fd)
        with os.fdopen(fd, "rb") as reader:
            before = os.fstat(reader.fileno())
            if not stat.S_ISREG(before.st_mode):
                raise ClientError("processor input must be a regular file")
            if path in self.source_identities:
                saved = self.source_identities[path]
                if saved is None or (saved.st_dev, saved.st_ino) != (
                    before.st_dev,
                    before.st_ino,
                ):
                    raise ProcessorExecutionError(
                        f"Processor input was replaced: {path.name}"
                    )
            yield reader
            after = os.fstat(reader.fileno())
            if (before.st_size, before.st_mtime_ns) != (
                after.st_size,
                after.st_mtime_ns,
            ):
                raise ProcessorExecutionError(
                    f"Processor input changed while reading: {path.name}"
                )


@dataclass(frozen=True)
class ProcessorPayload:
    """Extracted result bytes, not the full engine response envelope."""

    content: bytes


ProcessorTarget = Union[ProcessorMedia, ProcessorPayload]


@dataclass(frozen=True)
class ProcessorContext:
    """Proxy-owned context for one invocation of a processor callable.

    Metadata belongs to ``ProcessorDefinition``; this context carries only
    invocation resources and the scoped target. Media processors cannot receive
    payload bytes, and payload processors cannot receive unrelated media paths.
    Treat inputs as read-only and return changes through ``ProcessorOutput``.
    The proxy owns files and their cleanup/retention after the callable returns.
    """

    # Proxy-generated correlation ID shared by the entries in this request;
    # not an engine response ID or a client-selected filename.
    req_id: str

    # Private directory for this invocation's new files. Source media may live
    # in the fetcher's directory or a preceding invocation's directory; never
    # modify it in place. New returned paths must resolve inside this workdir.
    # Unselected files may be returned unchanged, still owned by their source.
    workdir: Path

    # Exactly one kind of data, selected by the registered scope: ordered local
    # paths for input_media/output_media, or extracted bytes for output_payload.
    # Paths are filesystem Paths, not file:// URIs; bytes exclude HTTP headers
    # and envelope metadata. An empty byte string is a valid payload.
    target: ProcessorTarget

    # Values from processor.arguments, validated before forwarding. The executor
    # supplies a fresh deep copy, so nested mutation cannot change the request.
    arguments: Dict[str, Any]

    # Absolute time.monotonic() expiry in seconds for this invocation, not a Unix
    # timestamp or duration. Remaining budget is deadline - time.monotonic().
    # The executor bounds the await; child work must honor cancellation too.
    deadline: float

    # Optional invocation-level media selection. () means no explicit selection.
    # The handler must identify applicable files and preserve unselected paths;
    # the executor does not infer MIME types from filenames.
    media_types: Tuple[str, ...] = ()


@dataclass(frozen=True)
class ProcessorOutput:
    """Explicit change to the scoped target; ``None`` keeps it unchanged.

    The same rule applies to pre and post: ``ProcessorOutput()`` is a successful
    validation/no-op; a target replaces the previous value. An empty media tuple
    explicitly clears media (subject to pre input/reference constraints), and
    ``ProcessorPayload(b"")`` explicitly replaces the payload with empty bytes.
    Replacements must have the same target kind as the invocation input.
    """

    target: Optional[ProcessorTarget] = None


class Processor(Protocol):
    """Execution-only callable; all catalog metadata lives on the definition."""

    async def __call__(self, ctx: ProcessorContext, /) -> ProcessorOutput: ...


@dataclass(frozen=True)
class ProcessorDefinition:
    """A registered name plus its contract; ``run`` is None until implemented."""

    name: str
    stage: ProcessorStage
    scope: ProcessorScope
    # JSON Schema describing the arguments object, not argument values.
    # {} accepts any arguments; a strict empty object rejects all keys.
    parameters: JSONSchema
    run: Optional[Processor] = None
    # Media kinds this implementation can select; ignored for payload scope.
    media: Tuple[str, ...] = PROCESSOR_MEDIA_TYPES

    def __post_init__(self) -> None:
        if (
            self.stage == PROCESSOR_STAGE_PRE
            and self.scope != PROCESSOR_SCOPE_INPUT_MEDIA
        ) or (
            self.stage == PROCESSOR_STAGE_POST
            and self.scope not in PROCESSOR_POST_SCOPES
        ):
            raise ValueError(
                f"processor '{self.name}' has incompatible stage/scope: "
                f"{self.stage}/{self.scope}"
            )
        if self.scope != PROCESSOR_SCOPE_OUTPUT_PAYLOAD:
            unsupported = set(self.media) - set(PROCESSOR_MEDIA_TYPES)
            if unsupported or not self.media:
                detail = ", ".join(sorted(unsupported)) or "none"
                raise ValueError(
                    f"processor '{self.name}' declares unsupported media: {detail}"
                )

    @property
    def implemented(self) -> bool:
        return self.run is not None


class ProcessorRegistry:
    """Name-addressed processor catalog with an optional deployment allowlist."""

    def __init__(
        self,
        definitions: Optional[Iterable[ProcessorDefinition]] = None,
        *,
        allowlist: Optional[Iterable[str]] = None,
    ):
        if definitions is None:
            # Lazy import avoids a contract/implementation import cycle.
            from aibrix.openai_frontend.proxy.processors.builtin_processors import (
                BUILTIN_PROCESSORS,
            )

            definitions = BUILTIN_PROCESSORS
        self._definitions: Dict[Tuple[str, ProcessorStage], ProcessorDefinition] = {}
        self._validators: Dict[Tuple[str, ProcessorStage], Any] = {}
        for definition in definitions:
            self.define(definition)
        self._allowlist: Optional[Tuple[str, ...]] = (
            None if allowlist is None else tuple(allowlist)
        )
        if self._allowlist is not None:
            unknown = set(self._allowlist) - set(self.names())
            if unknown:
                raise ValueError(
                    "unknown processor(s) in allowlist: " + ", ".join(sorted(unknown))
                )

    def define(self, definition: ProcessorDefinition, *, replace: bool = False) -> None:
        """Register (or attach an implementation to) one processor."""
        key = (definition.name, definition.stage)
        existing = self._definitions.get(key)
        if existing is not None and (
            existing.stage != definition.stage or existing.scope != definition.scope
        ):
            raise ValueError(
                f"processor '{definition.name}' is already registered "
                f"for stage {existing.stage}/{existing.scope}"
            )
        if existing is not None and not replace:
            raise ValueError(
                f"processor '{definition.name}' is already registered; "
                "pass replace=True to attach an implementation"
            )
        schema = deepcopy(dict(definition.parameters))
        validator_class = validator_for(schema)
        validator_class.check_schema(schema)
        validator = validator_class(
            schema, format_checker=validator_class.FORMAT_CHECKER
        )
        self._definitions[key] = definition
        self._validators[key] = validator

    def names(self) -> Tuple[str, ...]:
        """Names this deployment exposes; the allowlist narrowing is applied."""
        names = tuple(dict.fromkeys(name for name, _ in self._definitions))
        if self._allowlist is None:
            return names
        return tuple(n for n in names if n in self._allowlist)

    def resolve(self, name: str, stage: ProcessorStage) -> ProcessorDefinition:
        definition = self._definitions.get((name, stage))
        if name not in self.names():
            available = ", ".join(self.names()) or "none"
            raise ValueError(
                f"processor '{name}' is not available; available: {available}"
            )
        if definition is None:
            stages = ", ".join(s for n, s in self._definitions if n == name)
            raise ValueError(
                f"processor '{name}' belongs to stage '{stages}', not '{stage}'"
            )
        return definition

    def validate(self, pipeline: ProcessorPipeline) -> None:
        """Reject unknown names and invalid arguments before any forwarding."""
        stages: Tuple[ProcessorStage, ...] = (
            PROCESSOR_STAGE_PRE,
            PROCESSOR_STAGE_POST,
        )
        for stage in stages:
            for index, call in enumerate(pipeline.calls(stage)):
                try:
                    definition = self.resolve(call.processor.name, stage)
                    self._check_arguments(definition, call.processor.arguments)
                    self._check_media_types(definition, call)
                except ValueError as exc:
                    raise ValueError(
                        f"processors.{stage}[{index}] ({call.processor.name}): {exc}"
                    ) from exc

    def _check_arguments(
        self, definition: ProcessorDefinition, arguments: Mapping[str, Any]
    ) -> None:
        try:
            self._validators[(definition.name, definition.stage)].validate(arguments)
        except ValidationError as exc:
            path = "arguments" + "".join(
                f"[{part}]" if isinstance(part, int) else f".{part}"
                for part in exc.absolute_path
            )
            raise ValueError(f"{path}: {exc.message}") from exc

    @staticmethod
    def _check_media_types(
        definition: ProcessorDefinition, call: ProcessorCall
    ) -> None:
        media_types = call.processor.media_types
        if media_types is None:
            return
        if definition.scope == PROCESSOR_SCOPE_OUTPUT_PAYLOAD:
            raise ValueError("media_types is only valid for media processors")
        if not media_types:
            return  # Preserve the existing no-explicit-selection semantics.
        unknown = set(media_types) - set(PROCESSOR_MEDIA_TYPES)
        if unknown:
            raise ValueError(f"unknown media type(s): {', '.join(sorted(unknown))}")
        if not set(media_types) & set(definition.media):
            raise ValueError(
                f"processor '{definition.name}' does not handle media type(s): "
                + ", ".join(sorted(media_types))
            )


@dataclass(frozen=True)
class ProcessorState:
    """Pipeline state kept by the executor, never passed whole to a processor."""

    paths: Tuple[Path, ...] = ()
    payload: Optional[bytes] = None
    replacements: Mapping[Path, Path] = field(default_factory=dict)
    # Preserve every pinned identity across the final handoff to persistence.
    source_identities: Mapping[Path, Optional[os.stat_result]] = field(
        default_factory=dict
    )


class ProcessorExecutor:
    """Run either stage with per-invocation timeout, scope and replacement checks.

    The caller owns the request directory and its retention lifetime. This class
    owns per-entry orchestration, not HTTP transport or file/URI rewriting.
    """

    def __init__(
        self,
        registry: ProcessorRegistry,
        timeout_seconds: float = DEFAULT_PROCESSOR_TIMEOUT_SECONDS,
    ) -> None:
        if not math.isfinite(timeout_seconds) or timeout_seconds <= 0:
            raise ValueError(
                "processor_timeout_seconds must be a finite positive number"
            )
        self.registry = registry
        self.timeout_seconds = timeout_seconds

    async def execute(
        self,
        req_id: str,
        workdir: Path,
        pipeline: ProcessorPipeline,
        stage: ProcessorStage,
        state: ProcessorState,
        *,
        source_identities: Optional[Mapping[Path, Optional[os.stat_result]]] = None,
        input_roots: Optional[Mapping[Path, Path]] = None,
    ) -> ProcessorState:
        identities = PathRefs()
        identities.remember(state.source_identities)
        identities.remember(source_identities or {})
        for index, call in enumerate(pipeline.calls(stage)):
            definition = self.registry.resolve(call.processor.name, stage)
            if definition.scope == PROCESSOR_SCOPE_OUTPUT_PAYLOAD:
                if state.payload is None:
                    raise ProcessorExecutionError(
                        f"Processor '{definition.name}' requires an output payload"
                    )
                target: ProcessorTarget = ProcessorPayload(state.payload)
            else:
                if not state.paths:
                    error = (
                        ClientError
                        if stage == PROCESSOR_STAGE_PRE
                        else ProcessorExecutionError
                    )
                    raise error(
                        f"Processor '{definition.name}' requires at least one local media input"
                    )
                target = ProcessorMedia(
                    state.paths,
                    {p: identities[p] for p in state.paths if p in identities},
                    {
                        p: input_roots[p]
                        for p in state.paths
                        if input_roots and p in input_roots
                    },
                )
            outdir = workdir / stage / f"{index:02d}-{definition.name}"
            outdir.mkdir(parents=True, exist_ok=True)
            timeout_seconds = (
                self.timeout_seconds
                if call.processor.timeout_seconds is None
                else call.processor.timeout_seconds
            )
            ctx = ProcessorContext(
                req_id=req_id,
                workdir=outdir,
                target=target,
                arguments=deepcopy(call.processor.arguments),
                deadline=time.monotonic() + timeout_seconds,
                media_types=tuple(call.processor.media_types or ()),
            )
            result = await self._run(definition, ctx)
            try:
                state = self._replace(state, stage, target, result.target, outdir)
                if isinstance(target, ProcessorMedia):
                    for path in state.paths:
                        if path not in target.paths and path not in identities:
                            identities.add(PathRef.from_path(path, path.lstat()))
            except (OSError, RuntimeError, ValueError) as exc:
                raise ProcessorExecutionError(
                    f"Processor '{definition.name}' returned an unavailable output"
                ) from exc
        return replace(state, source_identities=identities)

    async def _run(
        self, definition: ProcessorDefinition, ctx: ProcessorContext
    ) -> ProcessorOutput:
        if definition.run is None:
            raise ProcessorExecutionError(
                f"Processor '{definition.name}' is declared but not implemented"
            )
        try:
            if ctx.deadline <= time.monotonic():
                raise asyncio.TimeoutError
            task = asyncio.ensure_future(definition.run(ctx))
            try:
                result = await asyncio.wait_for(
                    asyncio.shield(task),
                    timeout=max(0.0, ctx.deadline - time.monotonic()),
                )
            finally:
                # wait_for's cancellation wait can itself be cancelled on 3.10/3.11.
                # The real handler still owns its files until its child work stops.
                if not task.done():
                    task.cancel()
                await await_thread_completion(
                    asyncio.gather(task, return_exceptions=True)
                )
            if not isinstance(result, ProcessorOutput):
                raise TypeError("processor must return ProcessorOutput")
            return result
        except ClientError as exc:
            if definition.stage == PROCESSOR_STAGE_PRE:
                raise ClientError(
                    f"Processor '{definition.name}' rejected input: {exc}"
                ) from exc
            raise ProcessorExecutionError(
                f"Processor '{definition.name}' rejected output: {exc}"
            ) from exc
        except asyncio.TimeoutError as exc:
            raise ProcessorExecutionError(
                f"Processor '{definition.name}' timed out"
            ) from exc
        except Exception as exc:
            raise ProcessorExecutionError(
                f"Processor '{definition.name}' failed: {type(exc).__name__}: {exc}"
            ) from exc

    @staticmethod
    def _replace(
        state: ProcessorState,
        stage: ProcessorStage,
        previous: ProcessorTarget,
        replacement: Optional[ProcessorTarget],
        workdir: Path,
    ) -> ProcessorState:
        if replacement is None:
            return state
        if isinstance(previous, ProcessorPayload):
            if not isinstance(replacement, ProcessorPayload):
                raise ProcessorExecutionError(
                    "Payload processor must return a payload replacement"
                )
            if not isinstance(replacement.content, bytes):
                raise ProcessorExecutionError("Payload replacement must contain bytes")
            return ProcessorState(state.paths, replacement.content, state.replacements)
        if not isinstance(replacement, ProcessorMedia):
            raise ProcessorExecutionError(
                "Media processor must return a media replacement"
            )
        paths = tuple(
            p if p in previous.paths else ProcessorExecutor._checked_path(p, workdir)
            for p in replacement.paths
        )
        mapping = dict(state.replacements)
        if stage == PROCESSOR_STAGE_PRE:
            if not paths:
                raise ProcessorExecutionError(
                    "Pre processor cannot clear required media inputs"
                )
            if len(paths) != len(state.paths):
                raise ProcessorExecutionError(
                    f"Pre processor returned {len(paths)} outputs for {len(state.paths)} inputs"
                )
            # Positional 1:1 replacements preserve every request reference.
            # A flat path list cannot express splitting or merging unambiguously.
            step = dict(zip(state.paths, paths))
            for original, path in mapping.items():
                mapping[original] = step.get(path, path)
            for old, new in step.items():
                mapping.setdefault(old, new)
        return ProcessorState(paths, state.payload, mapping)

    @staticmethod
    def _checked_path(path: Path, workdir: Path) -> Path:
        resolved = path.resolve()
        if not resolved.is_relative_to(workdir.resolve()):
            raise ProcessorExecutionError(
                f"Processor wrote outside its work directory: {path.name}"
            )
        if not resolved.is_file():
            raise ProcessorExecutionError(
                f"Processor returned a missing or non-file output: {path.name}"
            )
        return resolved

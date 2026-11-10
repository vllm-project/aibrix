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

"""Video output ownership, independent of its HTTP caller and engine internals."""

from __future__ import annotations

import asyncio
import logging
import tempfile
import time
from dataclasses import dataclass
from typing import Any, Protocol

import httpx
from fastapi.responses import Response

from aibrix.openai_frontend.common.artifact_fetcher import FetchResult
from aibrix.openai_frontend.proxy.output.output_writer import SourceIdentities
from aibrix.openai_frontend.schemas.generation import (
    VIDEO_JOB_UNAVAILABLE_STATUS_CODES,
    VIDEO_TERMINAL_STATUSES,
    VideoResponse,
    VideoStatus,
)
from aibrix.openai_frontend.schemas.output import OutputRequest
from aibrix.openai_frontend.schemas.processors import ProcessorPipeline
from aibrix.openai_frontend.utils.utils import ServerError
from aibrix.runtime.downloaders import await_thread_completion

logger = logging.getLogger(__name__)

VIDEO_POLL_INTERVAL_SECONDS = 1.0
VIDEO_POLL_REQUEST_TIMEOUT_SECONDS = 10.0
VIDEO_CLEANUP_TIMEOUT_SECONDS = 30 * 60.0


@dataclass(frozen=True)
class VideoJobContext:
    """Resources transferred from the POST; collections are owned, not copied."""

    req_id: str
    started_at: float
    output: OutputRequest
    inputs: list[FetchResult]
    sources: SourceIdentities
    processors: ProcessorPipeline | None = None
    processor_dir: tempfile.TemporaryDirectory | None = None


class VideoJobPort(Protocol):
    """Engine capabilities used by a job; transport/accounting stay in the adapter."""

    output_timeout: float
    writers_stopped: asyncio.Event

    def available(self) -> bool: ...
    def check_available(self) -> None: ...
    def engine_lost_error(self) -> Exception: ...
    async def poll(self, video_id: str, *, cleanup: bool = False) -> httpx.Response: ...
    def parse_response(
        self, raw: httpx.Response
    ) -> tuple[VideoResponse, dict[str, Any]]: ...
    def observe(self, raw: httpx.Response, video_id: str) -> VideoResponse | None: ...
    def cache(self, video_id: str, response: VideoResponse | None) -> None: ...
    async def persist(
        self, raw: httpx.Response, response: VideoResponse, envelope: dict[str, Any]
    ) -> Any: ...
    def register_task(
        self, video_id: str, task: asyncio.Task[Any], *, cleanup_only: bool = False
    ) -> None: ...
    def is_registered(self, video_id: str, task: asyncio.Task[Any]) -> bool: ...
    async def finish(self, video_id: str, task: asyncio.Task[Any] | None) -> None: ...


class VideoOutputJob:
    def __init__(
        self,
        port: VideoJobPort,
        context: VideoJobContext,
        response: VideoResponse,
        *,
        resources_done: asyncio.Future[None] | None = None,
    ) -> None:
        self.port = port
        self.context = context
        self.response = response
        # The engine transfers its existing resource signal, so any shutdown
        # snapshot taken before handoff keeps waiting on the same future.
        self.resources_done = (
            resources_done
            if resources_done is not None
            else asyncio.get_running_loop().create_future()
        )
        self._registered_task: asyncio.Task[Any] | None = None

    @property
    def video_id(self) -> str:
        return self.response.id

    async def run(
        self, raw: httpx.Response, envelope: dict[str, Any]
    ) -> VideoResponse | Response:
        """Wait for upload and cleanup; task cancellation is not resource completion."""
        self.port.cache(self.video_id, None)
        task = asyncio.create_task(self.upload(raw, envelope))
        self._register(task)
        task.add_done_callback(self._on_done)
        try:
            await asyncio.wait((task,))
            if task.cancelled():
                await await_thread_completion(self.resources_done)
        except asyncio.CancelledError:
            task.cancel()
            try:
                await await_thread_completion(task)
            except BaseException:
                pass  # Preserve the caller's cancellation.
            try:
                await await_thread_completion(self.resources_done)
            except asyncio.CancelledError:
                pass  # Repeated cancellation still waits for resources.
            raise
        if task.cancelled() and not self.port.available():
            raise self.port.engine_lost_error() from None
        return task.result()

    def _register(self, task: asyncio.Task[Any], *, cleanup_only: bool = False) -> None:
        self._registered_task = task
        self.port.register_task(self.video_id, task, cleanup_only=cleanup_only)

    def _on_done(self, task: asyncio.Task[Any]) -> None:
        if not task.cancelled():
            task.exception()
        if not self.resources_done.done() and self.port.is_registered(
            self.video_id, task
        ):
            # Pre-start cancellation never executes upload/finally.
            self._cache_status(
                self.response,
                VideoStatus.FAILED.value,
                {"message": "Video output job cancelled"},
            )
            self.schedule_cleanup(self.response)

    def _cache_status(
        self, response: VideoResponse, status: str, error: Any = None
    ) -> VideoResponse:
        cached = response.cache_view(status, error)
        self.port.cache(self.video_id, cached)
        return cached

    async def upload(
        self, raw: httpx.Response, envelope: dict[str, Any]
    ) -> VideoResponse | Response:
        current = self.response
        deadline = self.context.started_at + self.port.output_timeout
        background = False
        try:
            while current.status not in VIDEO_TERMINAL_STATUSES:
                self.port.check_available()
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise asyncio.TimeoutError("Video output generation timed out")
                raw = await self._poll(remaining)
                observed = self.port.observe(raw, self.video_id)
                if observed is not None:
                    current = current.model_copy(update={"status": observed.status})
                polled, envelope = self.port.parse_response(raw)
                if polled.id != self.video_id:
                    raise ServerError("Inner engine returned a different video job ID")
                current = polled
                self._cache_status(current, VideoStatus.IN_PROGRESS.value)
                if current.status not in VIDEO_TERMINAL_STATUSES:
                    await self._poll_delay(deadline)
            if current.status != VideoStatus.COMPLETED.value:
                return self._cache_status(
                    current, VideoStatus.FAILED.value, current.error
                ).model_copy(deep=True)
            return await self.port.persist(raw, current, envelope)
        except asyncio.CancelledError:
            self._cache_status(
                current,
                VideoStatus.FAILED.value,
                {"message": "Video output job cancelled"},
            )
            raise
        except Exception as exc:
            logger.exception("Video output job %s failed", self.video_id)
            message = str(exc) or "Video output polling timed out"
            self._cache_status(current, VideoStatus.FAILED.value, {"message": message})
            background = current.status not in VIDEO_TERMINAL_STATUSES
            if isinstance(exc, asyncio.TimeoutError):
                raise ServerError(message) from exc
            raise
        finally:
            cleanup = self.schedule_cleanup(current, background=background)
            if not background:
                await await_thread_completion(cleanup)

    async def _poll(self, remaining: float) -> httpx.Response:
        poll = asyncio.create_task(self.port.poll(self.video_id))
        try:
            return await asyncio.wait_for(
                asyncio.shield(poll),
                timeout=min(VIDEO_POLL_REQUEST_TIMEOUT_SECONDS, remaining),
            )
        finally:
            # wait_for's cancellation wait can itself be cancelled on 3.10/3.11.
            # Keep the real poll owned until it closes, even after repeated cancel.
            if not poll.done():
                poll.cancel()
            await await_thread_completion(asyncio.gather(poll, return_exceptions=True))

    @staticmethod
    async def _poll_delay(deadline: float) -> None:
        await asyncio.sleep(
            min(VIDEO_POLL_INTERVAL_SECONDS, max(0.0, deadline - time.monotonic()))
        )

    def schedule_cleanup(
        self,
        response: VideoResponse,
        *,
        background: bool = True,
        recover_before_start: bool = False,
    ) -> asyncio.Task[None]:
        cleanup = asyncio.create_task(self._finish(response))
        if background:
            self._register(cleanup, cleanup_only=True)
            cleanup.add_done_callback(
                self._on_done if recover_before_start else self._consume_exception
            )
        return cleanup

    @staticmethod
    def _consume_exception(task: asyncio.Task[Any]) -> None:
        if not task.cancelled():
            task.exception()

    async def _await_stop(self, response: VideoResponse) -> VideoResponse:
        await self.port.writers_stopped.wait()
        return response

    async def _wait_generation_stopped(self, response: VideoResponse) -> VideoResponse:
        """Cleanup-only polling has a separate deadline and never uploads files."""
        deadline = time.monotonic() + VIDEO_CLEANUP_TIMEOUT_SECONDS
        while response.status not in VIDEO_TERMINAL_STATUSES:
            if not self.port.available():
                return await self._await_stop(response)
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                logger.warning("Video cleanup polling expired for %s", self.video_id)
                return await self._await_stop(response)
            try:
                raw = await self._cleanup_poll(remaining)
                if raw is None:
                    break
                observed = self.port.observe(raw, self.video_id)
                if observed is not None:
                    response = observed
                if response.status in VIDEO_TERMINAL_STATUSES:
                    break  # Trusted termination takes precedence over HTTP errors.
                if raw.status_code in VIDEO_JOB_UNAVAILABLE_STATUS_CODES:
                    return await self._await_stop(response)
                raw.raise_for_status()
            except Exception:
                logger.warning(
                    "Waiting for failed video job %s to finish before cleanup",
                    self.video_id,
                    exc_info=True,
                )
            if response.status not in VIDEO_TERMINAL_STATUSES:
                await self._poll_delay(deadline)
        return response

    async def _cleanup_poll(self, remaining: float) -> httpx.Response | None:
        poll = asyncio.create_task(self.port.poll(self.video_id, cleanup=True))
        stopped = asyncio.create_task(self.port.writers_stopped.wait())
        try:
            done, _ = await asyncio.wait(
                (poll, stopped),
                timeout=min(VIDEO_POLL_REQUEST_TIMEOUT_SECONDS, remaining),
                return_when=asyncio.FIRST_COMPLETED,
            )
            if stopped in done:
                return None
            if poll not in done:
                raise asyncio.TimeoutError("Video cleanup poll timed out")
            return poll.result()
        finally:
            for task in (poll, stopped):
                if not task.done():
                    task.cancel()
            await await_thread_completion(
                asyncio.gather(poll, stopped, return_exceptions=True)
            )

    async def _finish(self, response: VideoResponse) -> None:
        try:
            await await_thread_completion(self._wait_generation_stopped(response))
        finally:
            try:
                await await_thread_completion(
                    self.port.finish(self.video_id, self._registered_task)
                )
            finally:
                if not self.resources_done.done():
                    self.resources_done.set_result(None)

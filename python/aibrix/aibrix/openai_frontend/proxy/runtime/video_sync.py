# Copyright 2026 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#       http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Use a cancellable vLLM-Omni job behind the synchronous video endpoint."""

import asyncio
import json
import logging
import os
import uuid
from typing import Any, Awaitable, Callable, Optional
from urllib.parse import quote

import anyio
import httpx

from aibrix.openai_frontend.utils.utils import ServerError
from aibrix.runtime.downloaders import await_thread_completion

logger = logging.getLogger(__name__)

VIDEO_SYNC_TIMEOUT_SECONDS = float(os.getenv("VLLM_OMNI_VIDEO_SYNC_TIMEOUT", "3600"))
VIDEO_JOB_ID_TIMEOUT_SECONDS = 10.0
VIDEO_DELETE_TIMEOUT_SECONDS = 30.0
VIDEO_POLL_INTERVAL_SECONDS = 0.5


class VideoSyncJob:
    def __init__(
        self,
        forward: Callable[..., Awaitable[httpx.Response]],
        upload_done: asyncio.Event,
    ) -> None:
        self._forward = forward
        self._upload_done = upload_done
        self._create_scope = anyio.CancelScope()
        self._create_task: Optional[asyncio.Task[httpx.Response]] = None
        self._job: dict[str, Any] = {}
        self._path: Optional[str] = None

    async def _create(self, *, query: str, **kwargs: Any) -> httpx.Response:
        with self._create_scope:
            response = await self._forward("/v1/videos" + query, True, **kwargs)
            try:
                await response.aread()
                if response.is_success:
                    self._job = self._metadata(response)
                    job_id = self._job.get("id")
                    if not isinstance(job_id, str) or not job_id:
                        raise ServerError("Inner engine returned no video job ID")
                    self._path = f"/v1/videos/{quote(job_id, safe='')}"
                    logger.info("Synchronous video uses internal job %s", job_id)
                return response
            finally:
                await await_thread_completion(response.aclose())
        raise asyncio.CancelledError()

    @staticmethod
    def _metadata(response: httpx.Response) -> dict[str, Any]:
        try:
            metadata = response.json()
            if isinstance(metadata, dict):
                return metadata
        except ValueError:
            pass
        raise ServerError("Inner engine returned invalid video job metadata")

    async def run(self, *, query: str, **kwargs: Any) -> httpx.Response:
        self._create_task = asyncio.create_task(self._create(query=query, **kwargs))
        try:
            with anyio.fail_after(VIDEO_SYNC_TIMEOUT_SECONDS):
                # After upload, the engine may already have created a job. Let
                # its ID arrive after disconnect, so aclose can DELETE it.
                response = await asyncio.shield(self._create_task)
                if not response.is_success:
                    return response
                assert self._path is not None
                while True:
                    status = self._job.get("status")
                    if status == "failed":
                        error = self._job.get("error") or {}
                        code = error.get("code", response.status_code)
                        if not isinstance(code, int) or not 400 <= code < 600:
                            code = 500
                        return httpx.Response(
                            code,
                            request=response.request,
                            json={
                                "detail": error.get(
                                    "message", "Video generation failed"
                                )
                            },
                        )
                    if not response.is_success:
                        return response
                    if status == "completed":
                        break
                    if status not in ("queued", "in_progress"):
                        raise ServerError(
                            f"Inner engine returned invalid video status: {status}"
                        )
                    await asyncio.sleep(VIDEO_POLL_INTERVAL_SECONDS)
                    response = await self._forward(self._path, True, method="GET")
                    try:
                        await response.aread()
                        try:
                            self._job = self._metadata(response)
                        except ServerError:
                            if response.is_success:
                                raise
                            return response
                    finally:
                        await await_thread_completion(response.aclose())

                headers = {
                    "X-Request-Id": f"video_sync-{uuid.uuid4().hex}",
                    "X-Model": str(self._job["model"]),
                    "X-Inference-Time-S": f"{self._job['inference_time_s']:.3f}",
                    "X-Stage-Durations": json.dumps(
                        self._job.get("stage_durations", {}), separators=(",", ":")
                    ),
                    "X-Peak-Memory-MB": f"{self._job.get('peak_memory_mb', 0.0):.3f}",
                }
                response = await self._forward(
                    self._path + "/content", True, method="GET"
                )
                if response.is_success:
                    # The native sync endpoint returns inline MP4, whereas the
                    # async download endpoint adds an attachment filename.
                    response.headers.pop("content-disposition", None)
                    response.headers.update(headers)
                return response
        except TimeoutError:
            return httpx.Response(
                504,
                request=httpx.Request("POST", "/v1/videos/sync"),
                json={
                    "detail": f"Video generation timed out after {VIDEO_SYNC_TIMEOUT_SECONDS}s."
                },
            )

    async def aclose(self) -> None:
        if self._create_task is None:
            return
        waiting = not self._create_task.done()
        try:
            if not self._upload_done.is_set():
                self._create_scope.cancel()
            with anyio.move_on_after(VIDEO_JOB_ID_TIMEOUT_SECONDS) as scope:
                await asyncio.shield(self._create_task)
            if scope.cancel_called:
                logger.warning(
                    "Timed out receiving internal video job ID; cleanup may be incomplete"
                )
        except asyncio.CancelledError:
            if not self._create_task.cancelled():
                raise
        except Exception:
            if waiting:
                logger.warning("Failed to receive internal video job ID", exc_info=True)
        finally:
            if not self._create_task.done():
                self._create_scope.cancel()
            await await_thread_completion(
                asyncio.gather(self._create_task, return_exceptions=True)
            )
        if self._path is None:
            return
        try:
            with anyio.fail_after(VIDEO_DELETE_TIMEOUT_SECONDS):
                while True:
                    response = await self._forward(self._path, True, method="DELETE")
                    try:
                        if response.is_success or response.status_code == 404:
                            return
                        if response.status_code != 409:
                            logger.warning(
                                "Internal video DELETE %s returned %s",
                                self._path,
                                response.status_code,
                            )
                            return
                    finally:
                        await await_thread_completion(response.aclose())
                    await asyncio.sleep(VIDEO_POLL_INTERVAL_SECONDS)
        except Exception:
            logger.warning(
                "Failed to delete internal video job %s", self._path, exc_info=True
            )

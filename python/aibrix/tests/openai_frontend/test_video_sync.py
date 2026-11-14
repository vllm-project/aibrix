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

"""Cancellation regressions for internally synchronous vLLM-Omni video jobs."""

import asyncio
import json

import httpx
import pytest
from starlette.requests import Request

from aibrix.openai_frontend.engine.proxy_engine import EngineState, ProxyEngine
from aibrix.openai_frontend.proxy.runtime import video_sync
from aibrix.openai_frontend.proxy.runtime.engine_spec import VLLM_OMNI_SPEC
from aibrix.openai_frontend.utils.utils import ClientError, ServerError

JOB = {
    "id": "video_gen_test",
    "status": "completed",
    "model": "minimax-h3",
    "inference_time_s": 1.25,
    "stage_durations": {"diffusion": 1.1},
    "peak_memory_mb": 2000.0,
}


def make_request(messages):
    return Request(
        {
            "type": "http",
            "asgi": {"spec_version": "2.3"},
            "method": "POST",
            "path": "/v1/videos/sync",
            "query_string": b"",
            "headers": [(b"content-type", b"multipart/form-data; boundary=b")],
        },
        messages.get,
    )


def make_engine(upstream):
    engine = ProxyEngine(["unused"], spec=VLLM_OMNI_SPEC, fetch_media=False)
    engine._set_state(EngineState.READY)
    engine._subprocess._client = httpx.AsyncClient(
        transport=httpx.MockTransport(upstream), base_url="http://inner"
    )
    return engine


def upload_messages():
    messages = asyncio.Queue()
    messages.put_nowait(
        {"type": "http.request", "body": b"--b--\r\n", "more_body": False}
    )
    return messages


@pytest.mark.asyncio
@pytest.mark.parametrize("phase", ["create", "poll", "content"])
@pytest.mark.parametrize("outcome", ["disconnect", "cancel", "shutdown"])
async def test_video_sync_abort_deletes_internal_job(monkeypatch, phase, outcome):
    monkeypatch.setattr(video_sync, "VIDEO_POLL_INTERVAL_SECONDS", 0)
    entered, release = asyncio.Event(), asyncio.Event()
    calls = []

    class VideoBody(httpx.AsyncByteStream):
        closed = False

        async def __aiter__(self):
            yield b"mp4"
            entered.set()
            await release.wait()

        async def aclose(self):
            self.closed = True

    body = VideoBody()

    async def upstream(request):
        calls.append((request.method, request.url.path))
        if request.method == "POST":
            await request.aread()
            if phase == "create":
                entered.set()
                await release.wait()
            return httpx.Response(200, json={**JOB, "status": "queued"})
        if request.method == "DELETE":
            return httpx.Response(200, json={"deleted": True})
        if request.url.path.endswith("/content"):
            return httpx.Response(
                200, stream=body, headers={"content-type": "video/mp4"}
            )
        if phase == "poll":
            entered.set()
            await release.wait()
        return httpx.Response(200, json=JOB)

    messages = upload_messages()
    request = make_request(messages)
    engine = make_engine(upstream)
    ended = []
    original_end = engine._requests_end

    def end(req_id, t0):
        ended.append(req_id)
        original_end(req_id, t0)

    engine._requests_end = end

    async def consume():
        response = await engine.forward_multipart(request.url.path, request)

        async def send(message):
            pass

        await response(request.scope, request.receive, send)

    task = asyncio.create_task(consume())
    try:
        await asyncio.wait_for(entered.wait(), 2)
        assert len(engine._in_flight) == 1
        if outcome == "disconnect":
            messages.put_nowait({"type": "http.disconnect"})
        elif outcome == "cancel":
            task.cancel()
        else:
            stopper = asyncio.create_task(engine.aclose())
        # In the creation race, cancellation must not discard the POST response.
        if phase == "create":
            await asyncio.sleep(0)
            await asyncio.sleep(0)
            release.set()
        result = (
            await asyncio.wait_for(asyncio.gather(task, return_exceptions=True), 2)
        )[0]
        if outcome == "shutdown":
            await asyncio.wait_for(stopper, 2)
        if phase != "content" and outcome != "cancel":
            assert isinstance(result, ClientError)
        assert calls.count(("DELETE", "/v1/videos/video_gen_test")) == 1
        assert len(ended) == 1
        assert (
            not engine._in_flight
            and not engine._request_done
            and not engine._stream_closers
        )
        if phase == "content":
            assert body.closed
        if outcome != "shutdown":
            assert not engine._subprocess._client.is_closed
            # A new inference after cancellation still succeeds.
            release.set()
            response = await engine.forward_multipart(
                request.url.path, make_request(upload_messages())
            )
            assert b"".join([chunk async for chunk in response.body_iterator]) == b"mp4"
            assert not engine._in_flight
    finally:
        release.set()
        await engine.aclose()
        await asyncio.gather(task, return_exceptions=True)


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "failure",
    [
        (
            400,
            {
                "status": "failed",
                "error": {"code": 400, "message": "Invalid dimensions"},
            },
        ),
        (500, {"status": "failed", "error": {"code": 500, "message": "Model failed"}}),
        (502, {"detail": "Backend failed"}),
    ],
)
async def test_video_sync_failure_preserves_status_and_message(monkeypatch, failure):
    monkeypatch.setattr(video_sync, "VIDEO_POLL_INTERVAL_SECONDS", 0)
    status_code, payload = failure
    calls = []

    async def upstream(request):
        calls.append(request.method)
        if request.method == "POST":
            await request.aread()
            return httpx.Response(200, json={**JOB, "status": "queued"})
        if request.method == "DELETE":
            return httpx.Response(200)
        return httpx.Response(status_code, json=payload)

    engine = make_engine(upstream)
    request = make_request(upload_messages())
    try:
        with pytest.raises(httpx.HTTPStatusError) as exc:
            await engine.forward_multipart(request.url.path, request)
        assert exc.value.response.status_code == status_code
        expected = payload.get("detail") or payload["error"]["message"]
        assert exc.value.response.json() == {"detail": expected}
        assert calls == ["POST", "GET", "DELETE"]
        assert not engine._in_flight
    finally:
        await engine.aclose()


@pytest.mark.asyncio
async def test_video_sync_timeout_deletes_job(monkeypatch):
    monkeypatch.setattr(video_sync, "VIDEO_SYNC_TIMEOUT_SECONDS", 0.01)
    calls = []

    async def upstream(request):
        calls.append(request.method)
        await request.aread()
        return httpx.Response(200, json={**JOB, "status": "queued"})

    engine = make_engine(upstream)
    request = make_request(upload_messages())
    try:
        with pytest.raises(httpx.HTTPStatusError) as exc:
            await engine.forward_multipart(request.url.path, request)
        assert exc.value.response.status_code == 504
        assert exc.value.response.json() == {
            "detail": "Video generation timed out after 0.01s."
        }
        assert calls == ["POST", "DELETE"]
        assert not engine._in_flight
    finally:
        await engine.aclose()


@pytest.mark.asyncio
async def test_video_sync_disconnect_before_upload_finishes():
    messages = asyncio.Queue()
    messages.put_nowait({"type": "http.request", "body": b"--b\r\n", "more_body": True})
    messages.put_nowait({"type": "http.disconnect"})
    calls = []

    async def upstream(request):
        calls.append(request.method)
        await request.aread()
        pytest.fail("A disconnected upload must not create a job")

    engine = make_engine(upstream)
    request = make_request(messages)
    try:
        with pytest.raises(ClientError, match="Client disconnected"):
            await engine.forward_multipart(request.url.path, request)
        # MockTransport consumes the upload before invoking its handler.
        assert calls == []
        assert not engine._in_flight
    finally:
        await engine.aclose()


@pytest.mark.asyncio
async def test_video_sync_repeated_cancel_waits_for_delete():
    entered, release, deleting = asyncio.Event(), asyncio.Event(), asyncio.Event()

    async def upstream(request):
        if request.method == "POST":
            await request.aread()
            return httpx.Response(200, json={**JOB, "status": "queued"})
        if request.method == "DELETE":
            deleting.set()
            await release.wait()
            return httpx.Response(200)
        entered.set()
        await asyncio.Event().wait()

    engine = make_engine(upstream)
    request = make_request(upload_messages())
    task = asyncio.create_task(engine.forward_multipart(request.url.path, request))
    try:
        await asyncio.wait_for(entered.wait(), 2)
        task.cancel()
        await asyncio.wait_for(deleting.wait(), 2)
        task.cancel()
        await asyncio.sleep(0)
        assert not task.done() and engine._in_flight
        release.set()
        with pytest.raises(asyncio.CancelledError):
            await task
        assert not engine._in_flight and not engine._request_done
    finally:
        release.set()
        await engine.aclose()
        await asyncio.gather(task, return_exceptions=True)


@pytest.mark.asyncio
@pytest.mark.parametrize("creation", [{"status": "queued"}, ["invalid"]])
async def test_video_sync_rejects_creation_without_job_id(creation):
    async def upstream(request):
        await request.aread()
        return httpx.Response(200, json=creation)

    engine = make_engine(upstream)
    request = make_request(upload_messages())
    try:
        with pytest.raises(ServerError):
            await engine.forward_multipart(request.url.path, request)
        assert not engine._in_flight
    finally:
        await engine.aclose()


@pytest.mark.asyncio
async def test_video_sync_delete_retries_pending_cancellation(monkeypatch):
    monkeypatch.setattr(video_sync, "VIDEO_POLL_INTERVAL_SECONDS", 0)
    deletes = []

    async def upstream(request):
        await request.aread()
        if request.method == "POST":
            return httpx.Response(200, json=JOB)
        if request.method == "DELETE":
            deletes.append(request)
            return httpx.Response(409 if len(deletes) == 1 else 200)
        return httpx.Response(200, content=b"mp4")

    engine = make_engine(upstream)
    request = make_request(upload_messages())
    try:
        response = await engine.forward_multipart(request.url.path, request)
        assert b"".join([chunk async for chunk in response.body_iterator]) == b"mp4"
        assert len(deletes) == 2 and not engine._in_flight
    finally:
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize("phase", ["create", "delete"])
async def test_video_sync_cleanup_has_a_deadline(monkeypatch, caplog, phase):
    monkeypatch.setattr(video_sync, "VIDEO_JOB_ID_TIMEOUT_SECONDS", 0.01)
    monkeypatch.setattr(video_sync, "VIDEO_DELETE_TIMEOUT_SECONDS", 0.01)
    monkeypatch.setattr(video_sync, "VIDEO_POLL_INTERVAL_SECONDS", 0)
    entered = asyncio.Event()
    calls = []

    async def upstream(request):
        calls.append(request.method)
        await request.aread()
        if request.method == "POST" and phase != "create":
            return httpx.Response(200, json={**JOB, "status": "queued"})
        entered.set()
        await asyncio.Event().wait()

    engine = make_engine(upstream)
    messages = upload_messages()
    request = make_request(messages)
    task = asyncio.create_task(engine.forward_multipart(request.url.path, request))
    try:
        await asyncio.wait_for(entered.wait(), 2)
        messages.put_nowait({"type": "http.disconnect"})
        with pytest.raises(ClientError):
            await asyncio.wait_for(task, 2)
        assert not engine._in_flight and not engine._request_done
        assert calls == (["POST"] if phase == "create" else ["POST", "GET", "DELETE"])
        assert "internal video job" in caplog.text
    finally:
        await engine.aclose()
        await asyncio.gather(task, return_exceptions=True)


@pytest.mark.asyncio
async def test_video_sync_shutdown_before_stream_iteration_deletes_job():
    calls = []

    async def upstream(request):
        calls.append(request.method)
        await request.aread()
        if request.method == "POST":
            return httpx.Response(200, json=JOB)
        return httpx.Response(200, content=b"mp4")

    engine = make_engine(upstream)
    request = make_request(upload_messages())
    try:
        response = await engine.forward_multipart(request.url.path, request)
        assert engine._stream_closers and len(engine._in_flight) == 1
        await asyncio.wait_for(engine.aclose(), 2)
        await response.body_iterator.aclose()
        assert calls == ["POST", "GET", "DELETE"]
        assert not engine._request_done and not engine._stream_closers
    finally:
        await engine.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize("phase", ["create", "poll"])
async def test_video_sync_abort_over_http_then_new_inference(monkeypatch, phase):
    monkeypatch.setattr(video_sync, "VIDEO_POLL_INTERVAL_SECONDS", 0)
    entered, release, aborted = asyncio.Event(), asyncio.Event(), asyncio.Event()
    creates = []

    async def upstream(reader, writer):
        try:
            head = (await reader.readuntil(b"\r\n\r\n")).decode()
            method, path, _ = head.split("\r\n", 1)[0].split()
            if method == "POST":
                length = next(
                    int(line.split(":", 1)[1])
                    for line in head.split("\r\n")
                    if line.lower().startswith("content-length:")
                )
                assert await reader.readexactly(length) == b"--b--\r\n"
                creates.append(f"video_gen_{len(creates) + 1}")
                if len(creates) == 1 and phase == "create":
                    entered.set()
                    await release.wait()
                payload = json.dumps(
                    {**JOB, "id": creates[-1], "status": "queued"}
                ).encode()
            elif method == "DELETE":
                assert path == "/v1/videos/video_gen_1" or aborted.is_set()
                aborted.set()
                payload = b'{"deleted":true}'
            elif path.endswith("/content"):
                payload = b"mp4"
            else:
                if len(creates) == 1 and phase == "poll":
                    entered.set()
                    # DELETE must arrive before this blocked status response.
                    await aborted.wait()
                payload = json.dumps({**JOB, "id": creates[-1]}).encode()
            writer.write(
                b"HTTP/1.1 200 OK\r\nConnection: close\r\n"
                + f"Content-Length: {len(payload)}\r\n\r\n".encode()
                + payload
            )
            await writer.drain()
        finally:
            writer.close()
            await writer.wait_closed()

    server = await asyncio.start_server(upstream, "127.0.0.1", 0)
    engine = ProxyEngine(
        ["unused"],
        spec=VLLM_OMNI_SPEC,
        engine_port=server.sockets[0].getsockname()[1],
        fetch_media=False,
    )

    def request(messages):
        result = make_request(messages)
        result.scope["headers"].append((b"content-length", b"7"))
        return result

    messages = upload_messages()
    first = request(messages)
    task = None
    try:
        await engine._subprocess.start_client()
        engine._set_state(EngineState.READY)
        task = asyncio.create_task(engine.forward_multipart(first.url.path, first))
        await asyncio.wait_for(entered.wait(), 2)
        messages.put_nowait({"type": "http.disconnect"})
        await asyncio.sleep(0)
        release.set()
        with pytest.raises(ClientError):
            await asyncio.wait_for(task, 2)
        assert aborted.is_set() and not engine._in_flight
        second = request(upload_messages())
        response = await engine.forward_multipart(second.url.path, second)
        assert b"".join([chunk async for chunk in response.body_iterator]) == b"mp4"
        assert creates == ["video_gen_1", "video_gen_2"]
        assert not engine._in_flight and not engine._request_done
    finally:
        release.set()
        aborted.set()
        await engine.aclose()
        if task is not None:
            await asyncio.gather(task, return_exceptions=True)
        server.close()
        await server.wait_closed()

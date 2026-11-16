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

"""Standalone mock engine for E2E tests.

Usage:
    python -m aibrix.tests.openai_frontend.mock_engine --port 18761
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import signal
import subprocess
import sys
from urllib.parse import urlparse

import uvicorn
from fastapi import FastAPI, Request, WebSocket
from fastapi.responses import JSONResponse, StreamingResponse

app = FastAPI()
_stream_gates: dict[str, asyncio.Event] = {}
app.last_chat_request = None  # type: ignore[attr-defined]
app.allowed_media_dir = None  # type: ignore[attr-defined]


@app.get("/health")
async def health():
    return JSONResponse({"status": "ok"})


# ---------------------------------------------------------------------------
# Test hooks: simulate the failure modes the proxy must survive.
# ---------------------------------------------------------------------------


@app.post("/_test/crash")
async def crash(request: Request):
    """Die abruptly (like an OOM kill or CUDA abort) with a given signal."""
    body = await request.json()
    sig = int(body.get("signal", signal.SIGKILL))
    os.kill(os.getpid(), sig)


@app.post("/_test/spawn_worker")
async def spawn_worker(request: Request):
    """Fork a long-lived child, optionally in its own session, like a vLLM
    engine-core / TP worker that would otherwise outlive the API server."""
    body = await request.json()
    child = subprocess.Popen(
        [sys.executable, "-c", "import time; time.sleep(3600)"],
        start_new_session=bool(body.get("new_session", False)),
    )
    return JSONResponse({"pid": child.pid})


def _read_local_media(url: str) -> dict:
    """Mimic vLLM's file:// loader: only files under the allowed dir."""
    parsed = urlparse(url)
    if parsed.scheme != "file":
        return {"url": url, "local": False}
    path = os.path.realpath(parsed.path)
    allowed = app.allowed_media_dir  # type: ignore[attr-defined]
    if allowed is None or not path.startswith(os.path.realpath(allowed) + os.sep):
        return {"url": url, "local": True, "error": "outside allowed dir"}
    with open(path, "rb") as f:
        return {"url": url, "local": True, "content": f.read().decode()}


def _collect_media(node, out):
    if isinstance(node, list):
        for item in node:
            _collect_media(item, out)
    elif isinstance(node, dict):
        t = node.get("type")
        if t in ("image_url", "video_url", "audio_url"):
            payload = node.get(t)
            url = payload.get("url") if isinstance(payload, dict) else payload
            out.append(_read_local_media(url))
            return
        if t == "input_image":
            out.append(_read_local_media(node.get("image_url")))
            return
        for v in node.values():
            _collect_media(v, out)


@app.post("/v1/responses")
async def responses(request: Request):
    body = await request.json()
    app.last_chat_request = body  # type: ignore[attr-defined]
    media: list = []
    _collect_media(body.get("input"), media)
    return JSONResponse(
        {
            "id": "resp_test",
            "object": "response",
            "created_at": 1,
            "model": "m",
            "status": "completed",
            "output": [
                {
                    "type": "message",
                    "role": "assistant",
                    "content": [{"type": "output_text", "text": "hello"}],
                }
            ],
            "media": media,
            "echo": body,
        }
    )


@app.post("/_test/release_stream/{gate}")
async def release_stream(gate: str):
    event = _stream_gates.get(gate)
    if event is None:
        return JSONResponse({"error": "stream not waiting"}, status_code=404)
    event.set()
    return JSONResponse({"released": True})


@app.post("/v1/audio/transcriptions")
async def transcriptions(request: Request):
    """Omni: parse the multipart form and report what arrived.

    The upload is hashed so tests can prove the bytes crossed the proxy
    untouched; the request framing is reported so tests can tell whether the
    proxy re-encoded the body.  ``stream=true`` answers with SSE, like vLLM.
    """
    import hashlib

    form = await request.form()
    upload = form.get("file")
    digest = hashlib.sha256()
    size = 0
    if upload is not None and not isinstance(upload, str):
        while chunk := await upload.read(1 << 20):
            digest.update(chunk)
            size += len(chunk)
    result = {
        "text": "transcribed",
        "model": form.get("model"),
        "filename": getattr(upload, "filename", None),
        "has_audio": upload is not None,
        "language": form.get("language"),
        "size": size,
        "sha256": digest.hexdigest(),
        "query": dict(request.query_params),
        "content_length": request.headers.get("content-length"),
        "transfer_encoding": request.headers.get("transfer-encoding"),
    }
    if form.get("stream") == "true":
        gate = request.query_params.get("gate")
        release = _stream_gates.setdefault(gate, asyncio.Event()) if gate else None

        async def events():
            try:
                yield f"data: {json.dumps({'delta': 'hello'})}\n\n"
                if release is not None:
                    await release.wait()
                yield f"data: {json.dumps({'delta': 'world'})}\n\n"
                yield f"data: {json.dumps(result)}\n\n"
                yield "data: [DONE]\n\n"
            finally:
                if gate:
                    _stream_gates.pop(gate, None)

        return StreamingResponse(events(), media_type="text/event-stream")
    return JSONResponse(result)


@app.post("/v1/audio/translations")
async def translations(request: Request):
    """Omni: reject by default so error statuses pass through the proxy."""
    form = await request.form()
    if form.get("model") != "whisper":
        return JSONResponse({"error": {"message": "unknown model"}}, status_code=400)
    return JSONResponse({"text": "translated"})


@app.websocket("/v1/realtime")
async def realtime(ws: WebSocket):
    """Omni realtime mock.

    * ``?reject=<code>`` refuses the handshake with that HTTP status.
    * The first offered subprotocol is selected.
    * Text frames are echoed, except control commands:
      ``hello`` -> JSON with the handshake info the engine received;
      ``close:<code>:<reason>`` -> close with that code;
      ``crash`` -> the engine process dies abruptly (no close frame).
    * Binary frames are echoed reversed (proves they reached the engine).
    """
    from fastapi.responses import Response

    reject = ws.query_params.get("reject")
    if reject:
        await ws.send_denial_response(
            Response(
                content=json.dumps({"error": "rejected by engine"}),
                status_code=int(reject),
                media_type="application/json",
                headers={"Location": ws.query_params["redirect"]}
                if "redirect" in ws.query_params
                else None,
            )
        )
        return
    offered = ws.scope.get("subprotocols") or []
    await ws.accept(subprotocol=offered[0] if offered else None)
    while True:
        msg = await ws.receive()
        if msg["type"] == "websocket.disconnect":
            app.last_ws_close = msg.get("code")  # type: ignore[attr-defined]
            return
        text = msg.get("text")
        if text is None:
            await ws.send_bytes(bytes(reversed(msg.get("bytes") or b"")))
        elif text == "hello":
            await ws.send_text(
                json.dumps(
                    {
                        "subprotocol": offered[0] if offered else None,
                        "authorization": ws.headers.get("authorization"),
                        "openai_beta": ws.headers.get("openai-beta"),
                        "cookie": ws.headers.get("cookie"),
                        "query": dict(ws.query_params),
                        "pid": os.getpid(),
                    }
                )
            )
        elif text.startswith("close:"):
            _, code, reason = text.split(":", 2)
            await ws.close(code=int(code), reason=reason)
            return
        elif text == "crash":
            os.kill(os.getpid(), signal.SIGKILL)
        else:
            await ws.send_text(text)


@app.get("/_test/last_ws_close")
async def last_ws_close():
    return JSONResponse({"code": getattr(app, "last_ws_close", None)})


@app.post("/v1/rerank")
async def rerank(request: Request):
    body = await request.json()
    if "query" not in body:
        return JSONResponse(
            {"error": {"message": "query is required"}}, status_code=400
        )
    return JSONResponse(
        {"id": "rerank-test", "results": [{"index": 0, "relevance_score": 0.9}]}
    )


@app.get("/_test/env")
async def echo_env(request: Request):
    names = request.query_params.getlist("name")
    return JSONResponse({n: os.environ.get(n) for n in names})


@app.get("/v1/models")
async def list_models():
    return JSONResponse(
        {
            "object": "list",
            "data": [
                {
                    "id": "test-model",
                    "object": "model",
                    "created": 0,
                    "owned_by": "test",
                }
            ],
        }
    )


@app.post("/v1/chat/completions")
async def chat_completions(request: Request):
    body = await request.json()
    app.last_chat_request = body  # type: ignore[attr-defined]
    if body.get("user") == "force-error":
        return JSONResponse(
            {"error": {"message": "forced engine error"}}, status_code=400
        )
    media: list = []
    _collect_media(body.get("messages"), media)
    is_stream = body.get("stream", False)
    if is_stream:

        async def generate():
            chunk = {
                "id": "chatcmpl-test",
                "object": "chat.completion.chunk",
                "created": 0,
                "model": "test-model",
                "choices": [
                    {
                        "index": 0,
                        "delta": {"role": "assistant", "content": "hello"},
                        "finish_reason": None,
                    }
                ],
            }
            yield f"data: {json.dumps(chunk)}\n\n"
            yield "data: [DONE]\n\n"

        return StreamingResponse(generate(), media_type="text/event-stream")

    resp = {
        "id": "chatcmpl-test",
        "object": "chat.completion",
        "created": 0,
        "model": "test-model",
        "choices": [
            {
                "index": 0,
                "message": {"role": "assistant", "content": "hello"},
                "finish_reason": "stop",
            }
        ],
        "usage": {"prompt_tokens": 5, "completion_tokens": 1, "total_tokens": 6},
        # Not part of the OpenAI schema: lets tests see what the engine got.
        "media": media,
        "received": {k: v for k, v in body.items() if k != "messages"},
    }
    return JSONResponse(resp)


@app.post("/v1/completions")
async def completions(request: Request):
    body = await request.json()
    app.last_chat_request = body  # type: ignore[attr-defined]
    resp = {
        "id": "cmpl-test",
        "object": "text_completion",
        "created": 0,
        "model": "test-model",
        "choices": [{"text": "hello", "index": 0, "finish_reason": "stop"}],
    }
    return JSONResponse(resp)


@app.post("/v1/embeddings")
async def embeddings(request: Request):
    body = await request.json()
    app.last_chat_request = body  # type: ignore[attr-defined]
    resp = {
        "object": "list",
        "data": [
            {
                "object": "embedding",
                "embedding": [0.1, 0.2, 0.3],
                "index": 0,
            }
        ],
        "model": "test-model",
        "usage": {"prompt_tokens": 3, "total_tokens": 3},
    }
    return JSONResponse(resp)


@app.get("/metrics")
async def metrics():
    return JSONResponse({})


def main():
    parser = argparse.ArgumentParser(description="Mock engine for E2E tests")
    parser.add_argument("--port", type=int, default=18761)
    parser.add_argument("--host", type=str, default="127.0.0.1")
    parser.add_argument("--allowed-local-media-path", type=str, default=None)
    parser.add_argument(
        "--startup-delay", type=float, default=0.0, help="Seconds before serving"
    )
    parser.add_argument(
        "--exit-immediately",
        type=int,
        default=None,
        help="Print a message and exit with this code instead of serving",
    )
    args = parser.parse_args()
    if args.exit_immediately is not None:
        print("mock engine: fatal startup error (CUDA out of memory)", flush=True)
        sys.exit(args.exit_immediately)
    app.allowed_media_dir = args.allowed_local_media_path  # type: ignore[attr-defined]
    if args.startup_delay:
        import time

        time.sleep(args.startup_delay)
    uvicorn.run(app, host=args.host, port=args.port, log_level="error")


if __name__ == "__main__":
    main()

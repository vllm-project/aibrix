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

"""Bidirectional WebSocket bridge for engine-native realtime endpoints.

The proxy terminates the client's WebSocket (``/v1/realtime`` on omni
engines) and relays it to a second WebSocket on the inner engine:

* The upstream connection is opened **before** the client handshake is
  accepted, so an engine rejection (401, 404, ...) is relayed to the client as
  the same HTTP status (WebSocket Denial Response extension), and the
  subprotocol the engine selected is the one the client sees.
* Text frames (the realtime JSON protocol) and binary frames (audio) are
  pumped both ways unchanged.
* Whichever side closes first ends the session; its close code and reason are
  propagated to the other side.  An abnormal engine exit (no close frame)
  reaches the client as 1011, never as a hang.
* If the bridge task is cancelled (proxy shutdown) the client is
  closed with 1012 "service restart".
"""

from __future__ import annotations

import asyncio
import logging
from typing import Dict, Optional, Sequence, Tuple

import websockets
from fastapi.responses import Response
from starlette.websockets import WebSocket, WebSocketState
from websockets.asyncio.client import ClientConnection, connect
from websockets.exceptions import ConnectionClosed, InvalidStatus

from aibrix.runtime.downloaders import await_thread_completion

logger = logging.getLogger(__name__)

UPSTREAM_OPEN_TIMEOUT_SECONDS = 10.0
UPSTREAM_PING_INTERVAL_SECONDS = 20.0

CLOSE_NORMAL = 1000
CLOSE_INTERNAL_ERROR = 1011
CLOSE_SERVICE_RESTART = 1012
# Close codes that describe a state but may never appear in a close frame.
_NO_STATUS = 1005
_ABNORMAL = (1006, 1015)
_MAX_REASON_BYTES = 123

# Request headers worth passing upstream; everything else (Host, Upgrade,
# Sec-WebSocket-*, cookies, ...) belongs to the client<->proxy hop.
FORWARDED_HEADERS = (
    "authorization",
    "api-key",
    "x-api-key",
    "openai-beta",
    "openai-organization",
    "openai-project",
    "x-request-id",
)

# (side that closed first, close code, reason)
_CloseInfo = Tuple[str, Optional[int], str]


def forwardable_headers(ws: WebSocket) -> Dict[str, str]:
    """Pick the client headers that should reach the engine."""
    return {name: ws.headers[name] for name in FORWARDED_HEADERS if name in ws.headers}


def offered_subprotocols(ws: WebSocket) -> Optional[Sequence[str]]:
    subs = ws.scope.get("subprotocols") or []
    return list(subs) or None


async def deny_websocket(
    ws: WebSocket,
    status_code: int,
    body: bytes = b"",
    content_type: str = "application/json",
) -> None:
    """Reject a not-yet-accepted handshake with ``status_code``.

    Uses the Denial Response extension when the server supports it; otherwise
    falls back to a pre-accept close (which servers render as HTTP 403).
    """
    if "websocket.http.response" in ws.scope.get("extensions", {}):
        await ws.send_denial_response(
            # websockets hands back the upstream body as a bytearray, which
            # Starlette's Response does not accept.
            Response(
                content=bytes(body), status_code=status_code, media_type=content_type
            )
        )
    else:
        await ws.close(code=1008)


def _sendable_code(code: Optional[int], fallback: int) -> int:
    """Map a received close code to one that may legally be sent."""
    if code is None or code in _ABNORMAL:
        return fallback
    if code == _NO_STATUS:
        return CLOSE_NORMAL
    return code


def _truncate_reason(reason: str) -> str:
    raw = reason.encode("utf-8")
    if len(raw) <= _MAX_REASON_BYTES:
        return reason
    return raw[:_MAX_REASON_BYTES].decode("utf-8", errors="ignore")


async def _close_client(ws: WebSocket, code: int, reason: str = "") -> None:
    if ws.application_state == WebSocketState.DISCONNECTED:
        return
    try:
        await ws.close(code=code, reason=_truncate_reason(reason))
    except (RuntimeError, OSError):
        pass  # the client went away first


async def _client_to_engine(ws: WebSocket, upstream: ClientConnection) -> _CloseInfo:
    while True:
        msg = await ws.receive()
        if msg["type"] == "websocket.disconnect":
            return "client", msg.get("code", CLOSE_NORMAL), msg.get("reason") or ""
        try:
            if (text := msg.get("text")) is not None:
                await upstream.send(text)
            elif (data := msg.get("bytes")) is not None:
                await upstream.send(data)
        except ConnectionClosed:
            return "engine", upstream.close_code, upstream.close_reason or ""


async def _engine_to_client(ws: WebSocket, upstream: ClientConnection) -> _CloseInfo:
    try:
        async for msg in upstream:
            try:
                if isinstance(msg, str):
                    await ws.send_text(msg)
                else:
                    await ws.send_bytes(msg)
            except (RuntimeError, OSError):
                return "client", None, ""
    except ConnectionClosed:
        pass
    return "engine", upstream.close_code, upstream.close_reason or ""


async def _pump(ws: WebSocket, upstream: ClientConnection) -> None:
    tasks = (
        asyncio.create_task(_client_to_engine(ws, upstream)),
        asyncio.create_task(_engine_to_client(ws, upstream)),
    )
    try:
        done, pending = await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
        for task in pending:
            task.cancel()
        # Drain both directions, including exceptions from simultaneous completion.
        results = await await_thread_completion(
            asyncio.gather(*tasks, return_exceptions=True)
        )
        errors = [result for result in results if isinstance(result, Exception)]
        if errors:
            logger.warning("realtime bridge error: %r", errors[0])
            await _close_client(ws, CLOSE_INTERNAL_ERROR, "proxy error")
            await upstream.close(code=CLOSE_INTERNAL_ERROR)
            return
        finished = next(task for task in tasks if task in done)
        side, code, reason = finished.result()
        if side == "client":
            await upstream.close(
                code=_sendable_code(code, CLOSE_NORMAL), reason=_truncate_reason(reason)
            )
        else:
            abnormal = code is None or code in _ABNORMAL
            await _close_client(
                ws,
                _sendable_code(code, CLOSE_INTERNAL_ERROR),
                "inner engine connection lost" if abnormal else reason,
            )
    except asyncio.CancelledError:

        async def close_cancelled() -> None:
            try:
                await _close_client(
                    ws, CLOSE_SERVICE_RESTART, "inner engine unavailable"
                )
            finally:
                await upstream.close(code=CLOSE_SERVICE_RESTART)

        await await_thread_completion(close_cancelled())
        raise
    finally:
        for task in tasks:
            if not task.done():
                task.cancel()
        await await_thread_completion(asyncio.gather(*tasks, return_exceptions=True))


class _EngineConnect(connect):
    def process_redirect(self, exc: Exception) -> Exception:
        # The target is the owned engine; never resend client credentials elsewhere.
        return exc


async def bridge_websocket(
    ws: WebSocket,
    engine_url: str,
    *,
    headers: Optional[Dict[str, str]] = None,
    subprotocols: Optional[Sequence[str]] = None,
) -> None:
    """Connect to ``engine_url``, accept ``ws`` and relay frames until either
    side closes.  ``ws`` must not have been accepted yet."""
    try:
        upstream = await _EngineConnect(
            engine_url,
            # websockets types Subprotocol as a NewType over str.
            subprotocols=subprotocols,  # type: ignore[arg-type]
            additional_headers=headers or {},
            max_size=None,
            open_timeout=UPSTREAM_OPEN_TIMEOUT_SECONDS,
            ping_interval=UPSTREAM_PING_INTERVAL_SECONDS,
        )
    except InvalidStatus as exc:
        resp = exc.response
        logger.info("realtime upstream rejected handshake: HTTP %d", resp.status_code)
        await deny_websocket(
            ws,
            resp.status_code,
            resp.body or b"",
            resp.headers.get("content-type", "application/json"),
        )
        return
    except (OSError, asyncio.TimeoutError, websockets.WebSocketException) as exc:
        logger.warning("realtime upstream unavailable: %r", exc)
        await deny_websocket(
            ws, 502, b'{"detail":"Inner engine realtime endpoint unavailable"}'
        )
        return

    try:
        await ws.accept(subprotocol=upstream.subprotocol)
        await _pump(ws, upstream)
    finally:
        await await_thread_completion(upstream.close())

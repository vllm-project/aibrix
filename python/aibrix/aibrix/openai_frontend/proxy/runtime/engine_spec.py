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

"""What the proxy needs to know about an inner inference engine.

An :class:`EngineSpec` is a *declarative* description: the set of paths the
engine exposes and how each is forwarded (JSON body, multipart upload,
SSE stream, WebSocket).

Versions the catalog was compiled against:

* vLLM / vLLM-omni  -- tag ``v0.33.0rc1``
* SGLang / SGLang-omni -- tag ``v0.5.22``

The ``*-omni`` specs add the audio/speech surface (multipart
``/v1/audio/transcriptions``, ``/v1/audio/translations`` and the
``/v1/realtime`` WebSocket) on top of the text/pooling surface.
The vLLM-omni spec also adds multipart ``/v1/videos/sync``.
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum
from typing import FrozenSet, Mapping, Optional, Tuple


class ForwardKind(str, Enum):
    """How the frontend should treat a forwarded path."""

    JSON = "json"  # application/json request/response
    MULTIPART = "multipart"  # file-upload form (audio/video)
    SSE = "sse"  # JSON request, server-sent-event (or JSON) response
    WEBSOCKET = "websocket"  # bidirectional upgrade (realtime)


@dataclass(frozen=True)
class EndpointSet:
    """Immutable catalog of (path, forwarding kind) pairs."""

    routes: Tuple[Tuple[str, ForwardKind], ...] = ()
    method_overrides: Tuple[Tuple[str, Tuple[str, ...]], ...] = ()

    def methods(self, path: str) -> Tuple[str, ...]:
        return dict(self.method_overrides).get(path, ("POST",))

    def kinds(self) -> Mapping[str, ForwardKind]:
        return dict(self.routes)

    def paths(self) -> FrozenSet[str]:
        return frozenset(p for p, _ in self.routes)

    def with_(self, *extra: Tuple[str, ForwardKind]) -> "EndpointSet":
        known = dict(self.routes)
        known.update(extra)
        return EndpointSet(tuple(sorted(known.items())), self.method_overrides)


# ---------------------------------------------------------------------------
# Standard OpenAI endpoints
# ---------------------------------------------------------------------------

MODELS: Tuple[Tuple[str, ForwardKind], ...] = (("/v1/models", ForwardKind.JSON),)
CHAT: Tuple[Tuple[str, ForwardKind], ...] = (("/v1/chat/completions", ForwardKind.SSE),)
COMPLETION: Tuple[Tuple[str, ForwardKind], ...] = (
    ("/v1/completions", ForwardKind.SSE),
)
EMBEDDING: Tuple[Tuple[str, ForwardKind], ...] = (("/v1/embeddings", ForwardKind.JSON),)

# ---------------------------------------------------------------------------
# vLLM surface (v0.33.0rc1)
# ---------------------------------------------------------------------------

# OpenAI Responses API, structured decisions, Anthropic Messages.
VLLM_OPENAI_EXTRA: Tuple[Tuple[str, ForwardKind], ...] = (
    ("/v1/responses", ForwardKind.SSE),
    ("/v1/responses/{response_id}", ForwardKind.JSON),
    ("/v1/responses/{response_id}/cancel", ForwardKind.JSON),
    ("/v1/decisions", ForwardKind.JSON),
    ("/v1/messages", ForwardKind.SSE),
    ("/v1/messages/count_tokens", ForwardKind.JSON),
)
# Pooling (embedding-class) models.
VLLM_POOLING: Tuple[Tuple[str, ForwardKind], ...] = (
    ("/v1/score", ForwardKind.JSON),
    ("/v1/rerank", ForwardKind.JSON),
    ("/v2/rerank", ForwardKind.JSON),
    ("/v2/embed", ForwardKind.JSON),
    ("/score", ForwardKind.JSON),
    ("/rerank", ForwardKind.JSON),
    ("/pooling", ForwardKind.JSON),
    ("/classify", ForwardKind.JSON),
)
# Tokenizer helpers, native generate, structured/system APIs.
VLLM_TEXT_EXTRA: Tuple[Tuple[str, ForwardKind], ...] = (
    ("/tokenize", ForwardKind.JSON),
    ("/detokenize", ForwardKind.JSON),
    ("/tokenizer_info", ForwardKind.JSON),
    ("/inference/v1/generate", ForwardKind.SSE),
    ("/generative_scoring", ForwardKind.JSON),
    ("/v1/systemone", ForwardKind.SSE),
    ("/cohere/v2/chat", ForwardKind.SSE),
)
# Speech / omni surface.
VLLM_SPEECH: Tuple[Tuple[str, ForwardKind], ...] = (
    ("/v1/audio/transcriptions", ForwardKind.MULTIPART),
    ("/v1/audio/translations", ForwardKind.MULTIPART),
    ("/v1/realtime", ForwardKind.WEBSOCKET),
)
# Synchronous video generation (omni only).
VLLM_VIDEO: Tuple[Tuple[str, ForwardKind], ...] = (
    ("/v1/videos/sync", ForwardKind.MULTIPART),
)

VLLM_ROUTES = EndpointSet(
    MODELS
    + CHAT
    + COMPLETION
    + EMBEDDING
    + VLLM_OPENAI_EXTRA
    + VLLM_POOLING
    + VLLM_TEXT_EXTRA,
    method_overrides=(("/tokenizer_info", ("GET",)),),
)
VLLM_OMNI_ROUTES = VLLM_ROUTES.with_(*VLLM_SPEECH, *VLLM_VIDEO)

# ---------------------------------------------------------------------------
# SGLang surface (v0.5.22)
# ---------------------------------------------------------------------------

SGLANG_OPENAI_EXTRA: Tuple[Tuple[str, ForwardKind], ...] = (
    ("/v1/responses", ForwardKind.SSE),
    ("/v1/responses/{response_id}", ForwardKind.JSON),
    ("/v1/responses/{response_id}/cancel", ForwardKind.JSON),
    ("/v1/decisions", ForwardKind.JSON),
    ("/v1/messages", ForwardKind.SSE),
    ("/v1/messages/count_tokens", ForwardKind.JSON),
    ("/v1/score", ForwardKind.JSON),
    ("/v1/rerank", ForwardKind.JSON),
    ("/v1/classify", ForwardKind.JSON),
    ("/v1/tokenize", ForwardKind.JSON),
    ("/v1/detokenize", ForwardKind.JSON),
    ("/v1/systemone", ForwardKind.SSE),
    ("/v1/loads", ForwardKind.JSON),
)
# SGLang-native HTTP API.
SGLANG_NATIVE: Tuple[Tuple[str, ForwardKind], ...] = (
    ("/generate", ForwardKind.SSE),
    ("/encode", ForwardKind.JSON),
    ("/classify", ForwardKind.JSON),
    ("/tokenize", ForwardKind.JSON),
    ("/detokenize", ForwardKind.JSON),
)
SGLANG_SPEECH: Tuple[Tuple[str, ForwardKind], ...] = (
    ("/v1/audio/transcriptions", ForwardKind.MULTIPART),
    ("/v1/realtime", ForwardKind.WEBSOCKET),
)

SGLANG_ROUTES = EndpointSet(
    MODELS + CHAT + COMPLETION + EMBEDDING + SGLANG_OPENAI_EXTRA + SGLANG_NATIVE,
    method_overrides=(("/v1/loads", ("GET",)),)
    + tuple(
        (path, ("POST", "PUT"))
        for path in ("/generate", "/encode", "/classify", "/v1/rerank")
    ),
)
SGLANG_OMNI_ROUTES = SGLANG_ROUTES.with_(*SGLANG_SPEECH)


# Standard OpenAI surface enabled on a bare EngineSpec (the old default
# behaviour). Engine-specific specs pass a larger EndpointSet.
DEFAULT_OPENAI_ENDPOINTS = EndpointSet(MODELS + CHAT + COMPLETION + EMBEDDING)


@dataclass(frozen=True)
class EngineSpec:
    name: str
    endpoints: EndpointSet = DEFAULT_OPENAI_ENDPOINTS
    chat_endpoint: str = "/v1/chat/completions"
    completion_endpoint: str = "/v1/completions"
    embedding_endpoint: str = "/v1/embeddings"
    models_endpoint: str = "/v1/models"
    health_endpoint: str = "/health"
    metrics_endpoint: str = "/metrics"
    host_arg: str = "--host"
    port_arg: str = "--port"

    # --- local-media rewriting -----------------------------------------
    supports_local_media: bool = False
    local_media_args: Tuple[str, ...] = ()

    # --- routing --------------------------------------------------------
    def kind(self, path: str) -> Optional[ForwardKind]:
        """How to forward ``path``, or None if the engine does not expose it."""
        kinds = self.endpoints.kinds()
        if path in kinds:
            return kinds[path]
        for template, kind in self.endpoints.routes:
            if "{" in template and _matches(template, path):
                return kind
        return None

    def methods(self, path: str) -> Tuple[str, ...]:
        """Allowed HTTP methods for a declared native endpoint."""
        self.require(path)
        return self.endpoints.methods(path)

    def native_response_schema(self, path: str):
        """Result contract for this engine's native endpoint (not its aliases)."""
        from aibrix.openai_frontend.schemas.sglang import SGLANG_NATIVE_RESPONSES
        from aibrix.openai_frontend.schemas.vllm import VLLM_NATIVE_RESPONSES

        catalog = {
            "vllm": VLLM_NATIVE_RESPONSES,
            "sglang": SGLANG_NATIVE_RESPONSES,
        }.get(self.name.removesuffix("-omni"), {})
        return catalog.get(path)

    def supports(self, path: str) -> bool:
        return self.kind(path) is not None

    @property
    def supports_metrics(self) -> bool:
        return True

    def require(self, path: str) -> ForwardKind:
        kind = self.kind(path)
        if kind is None:
            raise NotImplementedError(
                f"Engine '{self.name}' does not expose endpoint '{path}'"
            )
        return kind

    # Back-compat helpers used by older callers.
    def require_chat(self) -> None:
        self.require(self.chat_endpoint)

    def require_completion(self) -> None:
        self.require(self.completion_endpoint)

    def require_embedding(self) -> None:
        self.require(self.embedding_endpoint)

    def require_models(self) -> None:
        self.require(self.models_endpoint)

    def require_metrics(self) -> None:
        if not self.supports_metrics:
            raise NotImplementedError(f"Engine '{self.name}' does not support metrics")

    @property
    def passthrough_endpoints(self) -> FrozenSet[str]:
        """Static paths, kept for callers that enumerate endpoints."""
        return frozenset(p for p in self.endpoints.paths() if "{" not in p)

    def spawn_args(self, media_dir: Optional[str]) -> Tuple[str, ...]:
        if not self.supports_local_media or not media_dir:
            return ()
        return tuple(a.replace("{dir}", media_dir) for a in self.local_media_args)


def _matches(template: str, path: str) -> bool:
    """True if a concrete ``path`` matches a FastAPI-style ``{var}`` template."""
    t_parts = template.strip("/").split("/")
    p_parts = path.strip("/").split("/")
    if len(t_parts) != len(p_parts):
        return False
    return all(
        (tp.startswith("{") and tp.endswith("}")) or tp == pp
        for tp, pp in zip(t_parts, p_parts)
    )


# vLLM: file:// media requires --allowed-local-media-path
# (vllm/multimodal/media/connector.py).
VLLM_SPEC = EngineSpec(
    name="vllm",
    endpoints=VLLM_ROUTES,
    supports_local_media=True,
    local_media_args=("--allowed-local-media-path", "{dir}"),
)
VLLM_OMNI_SPEC = EngineSpec(
    name="vllm-omni",
    endpoints=VLLM_OMNI_ROUTES,
    supports_local_media=True,
    local_media_args=("--allowed-local-media-path", "{dir}"),
)

# SGLang: load_image/load_video/load_audio accept file:// directly
# (python/sglang/srt/utils/common.py); no flag needed.
SGLANG_SPEC = EngineSpec(
    name="sglang",
    endpoints=SGLANG_ROUTES,
    supports_local_media=True,
)
SGLANG_OMNI_SPEC = EngineSpec(
    name="sglang-omni",
    endpoints=SGLANG_OMNI_ROUTES,
    supports_local_media=True,
)

# TensorRT-LLM: load_image accepts "" / file:// schemes
# (tensorrt_llm/inputs/utils.py). Embedding only with `trtllm-serve embeddings`.
TRTLLM_SPEC = EngineSpec(
    name="trtllm",
    endpoints=EndpointSet(
        MODELS + CHAT + COMPLETION + (("/v1/responses", ForwardKind.SSE),)
    ),
    supports_local_media=True,
)

ENGINE_SPECS: dict[str, EngineSpec] = {
    "vllm": VLLM_SPEC,
    "vllm-omni": VLLM_OMNI_SPEC,
    "sglang": SGLANG_SPEC,
    "sglang-omni": SGLANG_OMNI_SPEC,
    "trtllm": TRTLLM_SPEC,
}


def get_engine_spec(name: str) -> EngineSpec:
    if name not in ENGINE_SPECS:
        raise ValueError(
            f"Unknown engine spec '{name}'. "
            f"Available: {', '.join(sorted(ENGINE_SPECS))}"
        )
    return ENGINE_SPECS[name]

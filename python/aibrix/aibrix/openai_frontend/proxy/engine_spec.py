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

An :class:`EngineSpec` declares the paths the engine exposes (an
:class:`EndpointSet`) and how the proxy launches it.
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum
from typing import FrozenSet, Mapping, Optional, Tuple


class ForwardKind(str, Enum):
    """How the frontend should treat a forwarded path."""

    JSON = "json"  # application/json request/response
    MULTIPART = "multipart"  # file-upload form (audio transcription)
    SSE = "sse"  # JSON request, server-sent-event (or JSON) response
    WEBSOCKET = "websocket"  # bidirectional upgrade (realtime)


@dataclass(frozen=True)
class EndpointSet:
    """Immutable catalog of (path, forwarding kind) pairs."""

    routes: Tuple[Tuple[str, ForwardKind], ...] = ()

    def kinds(self) -> Mapping[str, ForwardKind]:
        return dict(self.routes)

    def paths(self) -> FrozenSet[str]:
        return frozenset(p for p, _ in self.routes)

    def with_(self, *extra: Tuple[str, ForwardKind]) -> "EndpointSet":
        known = dict(self.routes)
        known.update(extra)
        return EndpointSet(tuple(sorted(known.items())))


# ---------------------------------------------------------------------------
# Standard OpenAI endpoints
# ---------------------------------------------------------------------------

MODELS: Tuple[Tuple[str, ForwardKind], ...] = (("/v1/models", ForwardKind.JSON),)
CHAT: Tuple[Tuple[str, ForwardKind], ...] = (("/v1/chat/completions", ForwardKind.SSE),)
COMPLETION: Tuple[Tuple[str, ForwardKind], ...] = (
    ("/v1/completions", ForwardKind.SSE),
)
EMBEDDING: Tuple[Tuple[str, ForwardKind], ...] = (("/v1/embeddings", ForwardKind.JSON),)

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
    supports_local_media=True,
    local_media_args=("--allowed-local-media-path", "{dir}"),
)
# SGLang: load_image/load_video/load_audio accept file:// directly
# (python/sglang/srt/utils/common.py); no flag needed.
SGLANG_SPEC = EngineSpec(
    name="sglang",
    supports_local_media=True,
)
# TensorRT-LLM: load_image accepts "" / file:// schemes
# (tensorrt_llm/inputs/utils.py). Embedding only with `trtllm-serve embeddings`.
TRTLLM_SPEC = EngineSpec(
    name="trtllm",
    endpoints=EndpointSet(MODELS + CHAT + COMPLETION),
    supports_local_media=True,
)

ENGINE_SPECS: dict[str, EngineSpec] = {
    "vllm": VLLM_SPEC,
    "sglang": SGLANG_SPEC,
    "trtllm": TRTLLM_SPEC,
}


def get_engine_spec(name: str) -> EngineSpec:
    if name not in ENGINE_SPECS:
        raise ValueError(
            f"Unknown engine spec '{name}'. "
            f"Available: {', '.join(sorted(ENGINE_SPECS))}"
        )
    return ENGINE_SPECS[name]

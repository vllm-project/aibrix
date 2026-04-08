# Copyright 2024 The Aibrix Team.
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

# SPDX-License-Identifier: Apache-2.0
# SPDX-FileCopyrightText: Copyright contributors to the vLLM project
"""
AIBrix PD Reuse Connector combines PD disaggregation with kvcache reuse.

This connector:
1. Supports pd disaggregation: transfer KV cache between prefiller & decoder
2. Supports kvcache reuse: use KVCacheManager to store and retrieve (reuse)
3. L2 connector (e.g., SHFS) is transparent to this connector
"""

import enum
import logging
from dataclasses import dataclass, field
from functools import wraps
from typing import TYPE_CHECKING, Any, Callable, Optional, TypeVar

import numpy as np
import torch
import torch.distributed as dist
from vllm.distributed import (
    get_tp_group,
)
from vllm.distributed.kv_transfer.kv_connector.v1.base import (
    KVConnectorBase_V1,
    KVConnectorMetadata,
    KVConnectorRole,
)
from vllm.utils.math_utils import round_down, round_up
from vllm.utils.torch_utils import get_kv_cache_torch_dtype
from vllm.v1.attention.backends.flash_attn import FlashAttentionBackend
from vllm.v1.attention.backends.flashinfer import FlashInferBackend
from vllm.v1.request import RequestStatus

import aibrix_kvcache.envs
from aibrix_kvcache import (
    BaseKVCacheManager,
    GroupAwareKVCacheManager,
    KVCacheBlockLayout,
    KVCacheBlockSpec,
    KVCacheConfig,
    KVCacheMetrics,
    KVCacheTensorSpec,
    ModelSpec,
    TokenListView,
)
from aibrix_kvcache._custom_ops import (
    reshape_and_cache_multi_layer,
    reshape_and_offload_multi_layer,
)
from aibrix_kvcache.common.absl_logging import (
    getLogger,
    log_every_n_seconds,
    log_if,
)
from aibrix_kvcache.common.cached_pyobject import CachedPyObjectBase
from aibrix_kvcache.integration.vllm.vllm_compat import get_attn_backend
from aibrix_kvcache.metrics import (
    MS_BUCKETS,
    TOKEN_BUCKETS,
    BaseMetricsExporter,
    Metrics,
)
from aibrix_kvcache.profiling import tag_wrapper
from aibrix_kvcache.utils import perf_timer

if TYPE_CHECKING:
    from vllm.config import VllmConfig
    from vllm.forward_context import ForwardContext
    from vllm.v1.attention.backend import AttentionMetadata
    from vllm.v1.core.kv_cache_manager import KVCacheBlocks
    from vllm.v1.core.sched.output import SchedulerOutput
    from vllm.v1.kv_cache_interface import VllmKVCacheConfig
    from vllm.v1.request import Request

logger = getLogger(__name__)

PD_REUSE_CONNECTOR_SKIP_THRESHOLD = 8
PD_REUSE_CONNECTOR_SUPPORTED_ATTN_BACKENDS = {
    FlashAttentionBackend.get_name(): KVCacheBlockLayout.LCND,
    FlashInferBackend.get_name(): KVCacheBlockLayout.LCND,
}

T = TypeVar("T")


def delegate_to(
    member_name: str,
) -> Callable[[Callable[..., T]], Callable[..., T]]:
    """
    Decorator that delegates a method call to a member object.

    Args:
        member_name: The name of the member attribute to delegate to.
    """

    def decorator(method: Callable[..., T]) -> Callable[..., T]:
        @wraps(method)
        def wrapper(self, *args, **kwargs) -> T:
            member = getattr(self, member_name)
            assert member is not None, f"{member_name} is not set"
            member_method = getattr(member, method.__name__)
            return member_method(*args, **kwargs)

        return wrapper

    return decorator


class AIBrixPDReuseConnectorSyncGranularity(enum.Enum):
    """The sync granularity used by AIBrix PD Reuse connectors."""

    NONE = enum.auto()
    PER_OP = enum.auto()
    PER_BATCH = enum.auto()


class AIBrixOffloadingConnectorOpMetrics(Metrics):
    """Op metrics."""

    class OP(enum.Enum):
        SEND = enum.auto()
        RECV = enum.auto()

    def __init__(
        self,
        op: OP,
        enable_time_measurement: bool = True,
    ) -> None:
        self.num_ops: int = 0
        self.total_tokens: int = 0
        self.total_sent_or_recved_tokens: int = 0
        self.num_prefixes: list[int] = []
        self.num_tokens: list[int] = []
        self.num_sent_or_recved_tokens: list[int] = []
        self.op_lat_ms: Optional[list[int]] = None

        self._op = op
        self._enable_time_measurement = enable_time_measurement
        self._init_optionals()

    def _init_optionals(self) -> None:
        if self._enable_time_measurement:
            self.op_lat_ms = []

    def add(
        self,
        num_prefix: int,
        num_tokens: int,
        num_sent_or_recved_tokens: int,
        lat_ms: int,
    ) -> None:
        self.num_ops += 1
        self.total_tokens += num_tokens
        self.total_sent_or_recved_tokens += num_sent_or_recved_tokens
        self.num_sent_or_recved_tokens.append(num_sent_or_recved_tokens)
        self.num_prefixes.append(num_prefix)
        self.num_tokens.append(num_tokens)
        if self._enable_time_measurement:
            self.op_lat_ms.append(lat_ms)  # type: ignore[union-attr]

    def reset(self) -> None:
        self.num_prefixes = []
        self.num_tokens = []
        self.num_sent_or_recved_tokens = []
        self._init_optionals()

    def summary(self) -> str:
        iter_len = len(self.num_prefixes)
        total_prefixes = sum(self.num_prefixes)
        avg_prefixes = total_prefixes / iter_len if iter_len > 0 else 0
        total_tokens = sum(self.num_tokens)
        avg_tokens = total_tokens / iter_len if iter_len > 0 else 0
        total_sent_or_recved_tokens = sum(self.num_sent_or_recved_tokens)
        avg_sent_or_recved_tokens = (
            total_sent_or_recved_tokens / iter_len if iter_len > 0 else 0
        )
        summary = (
            f"{self._op.name}: Num. of ops: {self.num_ops}, "
            f"Total num. of tokens: {self.total_tokens}, "
        )
        if self._op is AIBrixOffloadingConnectorOpMetrics.OP.SEND:
            summary += (
                f"Total num. of sent tokens: "
                f"{self.total_sent_or_recved_tokens}, "
            )
        else:
            summary += (
                f"Total num. of received tokens: "
                f"{self.total_sent_or_recved_tokens}, "
            )
        summary += (
            f"Num. of prefixes (iter): total={total_prefixes}, "
            f"avg={avg_prefixes:.2f}, "
            f"Num. of tokens (iter): total={total_tokens}, "
            f"avg={avg_tokens:.2f}"
        )
        if self._op is AIBrixOffloadingConnectorOpMetrics.OP.SEND:
            summary += (
                f", Num. of sent tokens (iter): "
                f"total={total_sent_or_recved_tokens}, "
                f"avg={avg_sent_or_recved_tokens:.2f}"
            )
        else:
            summary += (
                f", Num. of received tokens (iter): "
                f"total={total_sent_or_recved_tokens}, "
                f"avg={avg_sent_or_recved_tokens:.2f}"
            )
        if self._enable_time_measurement:
            total_lat_ms = sum(self.op_lat_ms)  # type: ignore[arg-type]
            avg_lat_ms = total_lat_ms / iter_len if iter_len > 0 else 0
            summary += (
                f", Latency (iter, ms): total={total_lat_ms:.2f}, "
                f"avg={avg_lat_ms:.2f}"
            )
        if self._op is AIBrixOffloadingConnectorOpMetrics.OP.RECV:
            hit_rate = (
                (self.total_sent_or_recved_tokens * 100 / self.total_tokens)
                if self.total_tokens > 0
                else 0
            )
            summary += f", Hit rate: {hit_rate:.2f}%"
        return summary


class AIBrixOffloadingConnectorOpMetricsExporter(BaseMetricsExporter):
    OP_TYPE_LABELNAME = "op_type"

    def __init__(
        self, *, prefix, labelnames, counter_cls, gauge_cls, histogram_cls
    ) -> None:
        labelnames = labelnames or []
        labelnames.append(self.OP_TYPE_LABELNAME)

        super().__init__(
            prefix=f"{prefix}ol_connector_",
            labelnames=labelnames,
            counter_cls=counter_cls,
            gauge_cls=gauge_cls,
            histogram_cls=histogram_cls,
        )

        self._init_exporter_fields()

    def _init_exporter_fields(self) -> None:
        self.counter_num_ops = self._counter_cls(
            name=f"{self._prefix}num_ops",
            documentation="Cumulative number of operations.",
            labelnames=self._labelnames,
        )
        self.histogram_iteration_prefixes = self._histogram_cls(
            name=f"{self._prefix}iteration_prefixes",
            documentation="Histogram of number of prefixes per iteration.",
            labelnames=self._labelnames,
            buckets=TOKEN_BUCKETS,
        )
        self.histogram_iteration_tokens = self._histogram_cls(
            name=f"{self._prefix}iteration_tokens",
            documentation="Histogram of number of tokens per iteration.",
            labelnames=self._labelnames,
            buckets=TOKEN_BUCKETS,
        )
        self.histogram_iteration_sent_tokens = self._histogram_cls(
            name=f"{self._prefix}iteration_sent_tokens",
            documentation=("Histogram of number of sent tokens per iteration."),
            labelnames=self._labelnames,
            buckets=TOKEN_BUCKETS,
        )
        self.histogram_iteration_received_tokens = self._histogram_cls(
            name=f"{self._prefix}iteration_received_tokens",
            documentation=(
                "Histogram of number of received tokens per iteration."
            ),
            labelnames=self._labelnames,
            buckets=TOKEN_BUCKETS,
        )
        self.histogram_iteration_op_lat_ms = self._histogram_cls(
            name=f"{self._prefix}iteration_op_lat_ms",
            documentation=(
                "Histogram of operation latencies per iteration in ms."
            ),
            labelnames=self._labelnames,
            buckets=MS_BUCKETS,
        )

    def export(
        self,
        labels: dict[str, str],
        metrics: AIBrixOffloadingConnectorOpMetrics,  # type: ignore[override]
    ) -> None:
        labels = labels.copy()

        labels[self.OP_TYPE_LABELNAME] = metrics._op.name.lower()
        self._export_op_metrics(labels, metrics)

    def _export_op_metrics(
        self,
        labels: dict[str, str],
        metrics: AIBrixOffloadingConnectorOpMetrics,
    ) -> None:
        self._export_counter(
            self.counter_num_ops, labels, len(metrics.num_prefixes)
        )
        self._export_histogram(
            self.histogram_iteration_prefixes, labels, metrics.num_prefixes
        )
        self._export_histogram(
            self.histogram_iteration_tokens, labels, metrics.num_tokens
        )
        if metrics._op is AIBrixOffloadingConnectorOpMetrics.OP.SEND:
            self._export_histogram(
                self.histogram_iteration_sent_tokens,
                labels,
                metrics.num_sent_or_recved_tokens,
            )
        else:
            self._export_histogram(
                self.histogram_iteration_received_tokens,
                labels,
                metrics.num_sent_or_recved_tokens,
            )
        if metrics._enable_time_measurement:
            self._export_histogram(
                self.histogram_iteration_op_lat_ms,
                labels,
                metrics.op_lat_ms,  # type: ignore[arg-type]
            )


class AIBrixOffloadingConnectorMetrics:
    def __init__(self, metrics: KVCacheMetrics) -> None:
        self._cache_metrics = metrics
        self._time_measurement_enabled = (
            self._cache_metrics.time_measurement_enabled
        )
        self._send_metrics = AIBrixOffloadingConnectorOpMetrics(
            AIBrixOffloadingConnectorOpMetrics.OP.SEND,
            enable_time_measurement=self._time_measurement_enabled,
        )
        self._recv_metrics = AIBrixOffloadingConnectorOpMetrics(
            AIBrixOffloadingConnectorOpMetrics.OP.RECV,
            enable_time_measurement=self._time_measurement_enabled,
        )

    @property
    def time_measurement_enabled(self) -> bool:
        return self._time_measurement_enabled

    def reset(self) -> None:
        self._cache_metrics.reset()
        self._send_metrics.reset()
        self._recv_metrics.reset()

    def __str__(self) -> str:
        return (
            f"AIBrixOffloadingConnector metrics: "
            f"{self._send_metrics.summary()}"
            f"\n\t{self._recv_metrics.summary()}"
            f"\n\t{self._cache_metrics.summary()}"
        )

    def log_str(self) -> str:
        ret = str(self)
        self.reset()
        return ret


class AIBrixPDReuseConnectorRequestState(enum.IntEnum):
    INIT = enum.auto()
    WAITING_FOR_ALLOC = enum.auto()
    WAITING_FOR_SEND = enum.auto()
    WAITING_FOR_RECV = enum.auto()
    SENDING = enum.auto()
    RECEIVING = enum.auto()


@dataclass
class AIBrixPDReuseConnectorCachedMeta:
    """Cached metadata for a request."""

    def __init__(
        self, prompt_len: int, cache_block_ntokens: int, enable_padding: bool
    ) -> None:
        assert cache_block_ntokens > 0, (
            "cache_block_ntokens must be > 0 to pre-allocate aligned size"
        )

        self.prompt_len = prompt_len
        # For unaligned tail support
        self.cache_block_ntokens = cache_block_ntokens
        self.enable_padding = enable_padding

        if enable_padding:
            aligned_prompt_len = round_up(prompt_len, cache_block_ntokens)
            self.context_tokens = np.zeros(aligned_prompt_len, dtype=np.int32)
        else:
            self.context_tokens = np.zeros(prompt_len, dtype=np.int32)
        self.context_tokens_offset: int = 0
        self.context_tokens_view: Optional[TokenListView] = None
        self.context_slot_mapping: Optional[torch.Tensor] = torch.empty(
            prompt_len,
            dtype=torch.long,
            device="cuda",
        )
        self.context_slot_mapping_offset: int = 0

    def get_context_tokens(self) -> np.ndarray:
        """
        Get context tokens, aligned to block boundary if padding is enabled.
        """
        actual_len = self.context_tokens_offset

        if not self.enable_padding:
            return self.context_tokens[:actual_len]
        elif actual_len < self.prompt_len:
            # we don't have all prompt tokens yet
            return self.context_tokens[:actual_len]
        else:
            aligned_len = round_up(actual_len, self.cache_block_ntokens)
            assert aligned_len <= len(self.context_tokens), (
                f"Aligned length exceeds pre-allocated size: "
                f"aligned_len={aligned_len}, "
                f"allocated_size={len(self.context_tokens)}. "
            )

            return self.context_tokens[:aligned_len]

    def get_context_tokens_view(self) -> TokenListView:
        if self.context_tokens_view is None:
            self.context_tokens_view = TokenListView(self.get_context_tokens())
        return self.context_tokens_view

    def extend(
        self,
        tokens: tuple[int, list[int]],
        slot_mapping: Optional[tuple[int, torch.Tensor]],
    ):
        if tokens:
            offset = tokens[0]
            length = len(tokens[1])
            new_offset = offset + length

            assert new_offset <= len(self.context_tokens), (
                f"Data exceeds pre-allocated size: new_offset={new_offset}, "
                f"allocated_size={len(self.context_tokens)}. "
                f"This should not happen if __init__ pre-allocated correctly."
            )

            self.context_tokens[offset : offset + length] = tokens[1]
            self.context_tokens_offset = new_offset
            self.context_tokens_view = None

        if slot_mapping is None:
            return
        offset = slot_mapping[0]
        length = min(
            slot_mapping[1].shape[0],
            self.context_slot_mapping.shape[0] - offset,  # type: ignore
        )  # type: ignore
        self.context_slot_mapping[offset : offset + length] = slot_mapping[1][  # type: ignore[index]
            :length
        ]
        self.context_slot_mapping_offset = offset + length


@dataclass
class AIBrixPDReuseConnectorRequestMetadata(CachedPyObjectBase):
    """Request metadata for AIBrix PD Reuse Connector."""

    req_id: str = ""
    prompt_len: int = -1
    context_len: int = -1
    query_len: int = -1  # num of tokens to send/recv
    load_len: int = -1  # num of tokens to load
    # a tuple of offset and
    # 1. all token ids for a new request
    # 2. new token ids for a cached request
    seq_token_ids: tuple[int, list[int]] = field(default_factory=tuple)  # type: ignore[assignment]
    # a tuple of offset and
    # 1. all slot mapping for a new request
    # 2. new slot mapping for a cached request
    seq_slot_mapping: Optional[tuple[int, torch.Tensor]] = None
    state: AIBrixPDReuseConnectorRequestState = (
        AIBrixPDReuseConnectorRequestState.INIT
    )
    resumed_from_preemption: bool = False
    # PD disaggregation related fields
    do_remote_prefill: bool = False
    do_remote_decode: bool = False
    remote_block_ids: list[int] = field(default_factory=list)
    remote_engine_id: str = ""
    remote_host: str = ""
    remote_port: int = -1
    tp_size: int = 1

    def __str__(self) -> str:
        seq_slot_mapping_off = (
            "None"
            if self.seq_slot_mapping is None
            else str(self.seq_slot_mapping[0])
        )
        seq_slot_mapping_len = (
            "None"
            if self.seq_slot_mapping is None
            else str(self.seq_slot_mapping[1].shape[0])
        )
        seq_token_ids_off = (
            "None" if not self.seq_token_ids else str(self.seq_token_ids[0])
        )
        seq_token_ids_len = (
            "None"
            if not self.seq_token_ids or len(self.seq_token_ids) < 2
            else str(len(self.seq_token_ids[1]))
        )
        return (
            f"AIBrixPDReuseConnectorRequestMetadata["
            f"req_id={self.req_id}, "
            f"prompt_len={self.prompt_len}, "
            f"context_len={self.context_len}, "
            f"query_len={self.query_len}, "
            f"load_len={self.load_len}, "
            f"offset(seq_token_ids)={seq_token_ids_off}, "
            f"len(seq_token_ids)={seq_token_ids_len}, "
            f"offset(seq_slot_mapping)={seq_slot_mapping_off}, "
            f"len(seq_slot_mapping)={seq_slot_mapping_len}, "
            f"state={self.state.name}, "
            f"resumed_from_preemption={self.resumed_from_preemption}, "
            f"do_remote_prefill={self.do_remote_prefill}, "
            f"do_remote_decode={self.do_remote_decode}]"
        )

    def __repr__(self):
        return self.__str__()


class AIBrixPDReuseConnectorMetadata(KVConnectorMetadata):
    """Metadata for AIBrix PD Reuse Connector."""

    def __init__(
        self, requests: dict[str, AIBrixPDReuseConnectorRequestMetadata]
    ):
        self.requests = requests
        self.finished_requests_ids: set[str] = set()
        self.total_num_scheduled_tokens: int = 0

    def __getitem__(self, key: str) -> AIBrixPDReuseConnectorRequestMetadata:
        return self.requests[key]

    def __contains__(self, key: str) -> bool:
        return key in self.requests

    def __iter__(self):
        return iter(self.requests)

    def __next__(self):
        return next(self.requests)  # type: ignore[call-overload]

    def __len__(self):
        return len(self.requests)

    def items(self):
        return self.requests.items()

    def upsert_request(
        self,
        request_id: str,
        **kwargs,
    ) -> None:
        self.requests.setdefault(
            request_id, AIBrixPDReuseConnectorRequestMetadata()
        ).__dict__.update(req_id=request_id, **kwargs)

    def pop_request(
        self,
        request_id: str,
    ) -> AIBrixPDReuseConnectorRequestMetadata | None:
        return self.requests.pop(request_id, None)

    def finish_request(self, request_id: str) -> None:
        self.finished_requests_ids.add(request_id)
        self.pop_request(request_id)

    def get(self, predicate: Callable) -> "AIBrixPDReuseConnectorMetadata":
        requests = {k: v for k, v in self.requests.items() if predicate(v)}
        return AIBrixPDReuseConnectorMetadata(requests=requests)

    def filter_requests(self, predicate: Callable) -> None:
        self.requests = {
            k: v for k, v in self.requests.items() if not predicate(v)
        }

    def extend(self, other: "AIBrixPDReuseConnectorMetadata") -> None:
        self.requests.update(other.requests)

    def clear(self) -> None:
        self.requests.clear()
        self.finished_requests_ids.clear()

    def __str__(self) -> str:
        return f"AIBrixPDReuseConnectorMetadata: {self.__dict__}"


class AIBrixPDReuseConnectorScheduler:
    """Scheduler-side implementation for AIBrix PD Reuse Connector."""

    def __init__(self, config: "VllmConfig"):
        self.kv_role = config.kv_transfer_config.kv_role
        self.engine_block_ntokens = config.cache_config.block_size
        self.vllm_config = config
        assert config.kv_transfer_config.engine_id is not None
        self.engine_id = config.kv_transfer_config.engine_id

        # AIBrixPDReuseConnector only communicates with KVCacheManager,
        # which handles L2 cache (SHFS, HPKV, PrisKV, etc.) internally.
        # No side channel (ZMQ) is needed for KVCacheManager communication.

        self._scheduler_meta = AIBrixPDReuseConnectorMetadata({})

    def get_num_new_matched_tokens(
        self,
        request: "Request",
        num_computed_tokens: int,
    ) -> tuple[int, bool]:
        """
        Get number of new tokens that can be loaded from the
        external KV cache (KVCacheManager) beyond the num_computed_tokens.
        """
        params = request.kv_transfer_params
        if not params:
            return 0, False

        if params.get("do_remote_decode"):
            return 0, False

        if params.get("do_remote_prefill"):
            num_external_tokens = (
                request.num_prompt_tokens - num_computed_tokens
            )
            if num_external_tokens > 0:
                return num_external_tokens, True
        return 0, False

    def update_state_after_alloc(
        self,
        request: "Request",
        blocks: "KVCacheBlocks",
        num_external_tokens: int,
    ):
        """Update state after block allocation."""
        params = request.kv_transfer_params
        if not params:
            return

        # Handle PD disaggregation: update metadata if needed
        self._scheduler_meta.upsert_request(
            request.request_id,
            do_remote_prefill=params.get("do_remote_prefill", False),
            do_remote_decode=params.get("do_remote_decode", False),
            remote_block_ids=params.get("remote_block_ids", []),
            remote_engine_id=params.get("remote_engine_id", ""),
            remote_host=params.get("remote_host", ""),
            remote_port=params.get("remote_port", -1),
            tp_size=params.get("tp_size", -1),
        )

        if params.get("do_remote_prefill") and num_external_tokens > 0:
            req_id = request.request_id
            prompt_len = len(request.prompt_token_ids)
            context_len = request.num_computed_tokens
            query_len = 0
            if context_len >= prompt_len:
                return

            block_ids = blocks.get_unhashed_block_ids()
            slot_mapping = self._block_ids_to_slot_mapping(block_ids)

            self._scheduler_meta.upsert_request(
                req_id,
                prompt_len=prompt_len,
                context_len=context_len,
                query_len=query_len,
                seq_token_ids=(0, request.prompt_token_ids),
                seq_slot_mapping=(0, slot_mapping),
                state=AIBrixPDReuseConnectorRequestState.WAITING_FOR_RECV,
            )

    def build_connector_meta(
        self,
        scheduler_output: "SchedulerOutput",
    ) -> KVConnectorMetadata:
        """Build connector metadata for this step."""
        # 1. Remove finished requests
        for req_id in scheduler_output.finished_req_ids:
            self._scheduler_meta.pop_request(req_id)

        # 2. handle preempted requests for pd
        cached_reqs = scheduler_output.scheduled_cached_reqs
        for req_id in cached_reqs.resumed_req_ids:
            if self._scheduler_meta[req_id].do_remote_prefill:
                self._scheduler_meta.upsert_request(
                    req_id,
                    state=AIBrixPDReuseConnectorRequestState.WAITING_FOR_RECV,
                )

        # 3. Process new requests
        for req in scheduler_output.scheduled_new_reqs:
            req_id = req.req_id

            # In PD mode, after decoder receiving all n prompt tokens, it starts
            # the computation from prefilling the nth prompt token (which has
            # already been loaded) and thus we need to skip this case
            if (
                req_id in self._scheduler_meta
                and self._scheduler_meta[req_id].do_remote_prefill
            ):
                continue

            prompt_len = len(req.prompt_token_ids)
            context_len = req.num_computed_tokens
            query_len = scheduler_output.num_scheduled_tokens[req_id]

            if context_len >= prompt_len:
                continue

            (block_ids,) = req.block_ids
            slot_mapping = self._block_ids_to_slot_mapping(block_ids)

            # Get PD disaggregation params from request metadata if it was
            # already set in update_state_after_alloc.
            if req_id in self._scheduler_meta:
                existing_meta = self._scheduler_meta[req_id]
                do_remote_prefill = existing_meta.do_remote_prefill
                do_remote_decode = existing_meta.do_remote_decode
                remote_block_ids = existing_meta.remote_block_ids
                remote_engine_id = existing_meta.remote_engine_id
                remote_host = existing_meta.remote_host
                remote_port = existing_meta.remote_port
                tp_size = existing_meta.tp_size
            else:
                # initialize with defaults
                do_remote_prefill = False
                do_remote_decode = False
                remote_block_ids = []
                remote_engine_id = ""
                remote_host = ""
                remote_port = -1
                tp_size = self.vllm_config.parallel_config.tensor_parallel_size

            self._scheduler_meta.upsert_request(
                req_id,
                prompt_len=prompt_len,
                context_len=context_len,
                query_len=query_len,
                seq_token_ids=(0, req.prompt_token_ids),
                seq_slot_mapping=(0, slot_mapping),
                state=AIBrixPDReuseConnectorRequestState.WAITING_FOR_RECV,
                do_remote_prefill=do_remote_prefill,
                do_remote_decode=do_remote_decode,
                remote_block_ids=remote_block_ids,
                remote_engine_id=remote_engine_id,
                remote_host=remote_host,
                remote_port=remote_port,
                tp_size=tp_size,
            )

        # 4. Process cached requests
        req_ids = cached_reqs.req_ids
        for i in range(len(req_ids)):
            req_id = req_ids[i]

            if req_id not in self._scheduler_meta:
                continue

            # In PD mode, after decoder receiving all n prompt tokens, it starts
            # the computation from prefilling the nth prompt token (which has
            # already been loaded) and thus we need to skip this case
            if self._scheduler_meta[req_id].do_remote_prefill:
                continue

            req_meta = self._scheduler_meta[req_id]

            prompt_len = req_meta.prompt_len
            context_len = min(cached_reqs.num_computed_tokens[i], prompt_len)
            query_len = min(
                scheduler_output.num_scheduled_tokens[req_id],
                prompt_len - context_len,
            )

            if context_len >= prompt_len:
                continue

            if req_id in cached_reqs.resumed_req_ids:
                logger.debug(
                    "Got preempt Request[id=%s, context_len=%d, query_len=%d]",
                    req_id,
                    context_len,
                    query_len,
                )
                (block_ids,) = cached_reqs.new_block_ids[i]
                seq_slot_mapping = (
                    0,
                    self._block_ids_to_slot_mapping(block_ids),
                )
            elif cached_reqs.new_block_ids[i] is not None:
                (block_ids,) = cached_reqs.new_block_ids[i]
                seq_slot_mapping = (
                    round_up(context_len, self.engine_block_ntokens),
                    self._block_ids_to_slot_mapping(block_ids),
                )
            else:
                seq_slot_mapping = None

            self._scheduler_meta.upsert_request(
                req_id,
                prompt_len=prompt_len,
                context_len=context_len,
                query_len=query_len,
                seq_token_ids=None,
                seq_slot_mapping=seq_slot_mapping,
                state=AIBrixPDReuseConnectorRequestState.WAITING_FOR_RECV,
                resumed_from_preemption=req_id in cached_reqs.resumed_req_ids,
            )

        # 5. Keep requests that are in the WAITING_FOR_RECV state
        meta = self._scheduler_meta.get(
            lambda req: (
                req.state == AIBrixPDReuseConnectorRequestState.WAITING_FOR_RECV
            )
        )
        logger.debug("SCHEDULER: build_connector_meta, meta=%s", meta.__dict__)

        # 6. Update scheduled requests
        for req_id in meta:
            self._scheduler_meta.upsert_request(
                req_id,
                state=AIBrixPDReuseConnectorRequestState.RECEIVING,
            )

        # 7. Attach finished requests
        meta.finished_requests_ids = self._scheduler_meta.finished_requests_ids
        self._scheduler_meta.finished_requests_ids = set()

        logger.debug(
            "Num. of scheduled requests: %s",
            len(
                self._scheduler_meta.get(
                    lambda req: req.state
                    == AIBrixPDReuseConnectorRequestState.RECEIVING
                )
            ),
        )
        return meta

    def request_finished(
        self,
        request: "Request",
        block_ids: list[int],
    ) -> tuple[bool, Optional[dict[str, Any]]]:
        """
        Called when a request is finished.

        For prefiller: return metadata to router for decoder to use.
        For decoder: just clean up.
        """
        req_id = request.request_id

        params = request.kv_transfer_params
        if not params:
            logger.debug("SCHEDULER: Request[id=%s] finished", req_id)
            self._scheduler_meta.finish_request(req_id)
            return False, None

        # Handle PD disaggregation: if this is a prefiller finishing, return
        # metadata for decoder.
        if (
            params.get("do_remote_decode")
            and request.status == RequestStatus.FINISHED_LENGTH_CAPPED
        ):
            # Do NOT delay_free_blocks because KV cache is already saved to
            # KVCacheManager (L2 cache) in wait_for_save(). Decoder will read
            # from KVCacheManager, not from prefiller's GPU memory.
            # So we can free the GPU blocks immediately.
            #
            # NOTE: remote_host and remote_port are set to empty values because
            # AIBrixPDReuseConnector only uses KVCacheManager for communication,
            # not direct network connections. These fields are included for
            # router compatibility but are not used by the decoder.
            self._scheduler_meta.finish_request(req_id)
            logger.debug(
                "SCHEDULER: Request[id=%s] finished prefilling", req_id
            )
            return False, dict(
                do_remote_prefill=True,
                do_remote_decode=False,
                remote_block_ids=[],
                remote_engine_id=self.engine_id,
                remote_host="",
                remote_port=-1,
                tp_size=self.vllm_config.parallel_config.tensor_parallel_size,
            )

        logger.debug("SCHEDULER: Request[id=%s] finished", req_id)
        self._scheduler_meta.finish_request(req_id)
        return False, None

    def _block_ids_to_slot_mapping(self, block_ids: list[int]) -> torch.Tensor:
        """Convert block IDs to slot mapping."""
        block_ids_tensor = torch.tensor(block_ids)
        num_blocks = block_ids_tensor.shape[0]
        block_offsets = torch.arange(0, self.engine_block_ntokens)
        slot_mapping = (
            block_offsets.reshape((1, self.engine_block_ntokens))
            + block_ids_tensor.reshape((num_blocks, 1))
            * self.engine_block_ntokens
        )
        return slot_mapping.flatten()


class AIBrixPDReuseConnectorWorker:
    """Worker-side implementation for AIBrix PD Reuse Connector."""

    def __init__(self, config: "VllmConfig"):
        self.kv_role = config.kv_transfer_config.kv_role
        self.vllm_config = config
        self._init_worker(config)

    def _init_worker(
        self,
        config: "VllmConfig",
        max_num_batched_tokens: int = -1,
        tp_aware: bool = True,
        multi_threaded: bool = False,
    ):
        cache_config = config.cache_config
        model_config = config.model_config
        parallel_config = config.parallel_config

        tp_size = parallel_config.tensor_parallel_size
        num_kv_heads = model_config.get_num_kv_heads(parallel_config)
        head_size = model_config.get_head_size()

        rank = get_tp_group().rank_in_group
        kv_head_ids = list(
            range(num_kv_heads * rank, num_kv_heads * (rank + 1))
        )
        layer_ids = list(
            range(*model_config.get_layers_start_end_indices(parallel_config))
        )
        num_layers = len(layer_ids)

        block_ntokens = cache_config.block_size
        block_dtype = get_kv_cache_torch_dtype(
            cache_config.cache_dtype, model_config.dtype
        )

        kv_cache_dtype = cache_config.cache_dtype

        self.attn_backend = get_attn_backend(
            model_config.get_head_size(),
            model_config.dtype,
            kv_cache_dtype,
            block_ntokens,
            use_mla=model_config.use_mla,
        )

        # Check if the first dimension of the cache is kv or num_blocks
        test_shape = self.attn_backend.get_kv_cache_shape(
            num_blocks=1111, block_size=16, num_kv_heads=8, head_size=256
        )
        self.kv_layout_blocks_first = test_shape[0] == 1111

        block_spec = KVCacheBlockSpec(
            block_ntokens=block_ntokens,
            block_dtype=block_dtype,
            block_layout=self._get_block_layout(),
            tensor_spec=KVCacheTensorSpec(
                heads=kv_head_ids,
                layers=layer_ids,
                head_size=head_size,
            ),
        )

        kv_config = KVCacheConfig(
            block_spec=block_spec,
            model_spec=ModelSpec(
                model_config.max_model_len,
                max_num_batched_tokens,
            ),
            multi_threaded=multi_threaded,
        )

        self.kv_group: dist.ProcessGroup | None = None
        if parallel_config.tensor_parallel_size == 1 or not tp_aware:
            self.cache = BaseKVCacheManager(config=kv_config)
        else:
            kv_group = get_tp_group()

            sync_granularity = aibrix_kvcache.envs.VLLM_AIBRIX_SYNC_GRANULARITY
            if (
                AIBrixPDReuseConnectorSyncGranularity.NONE.name
                == sync_granularity
            ):
                self.cache = BaseKVCacheManager(config=kv_config)
            elif (
                AIBrixPDReuseConnectorSyncGranularity.PER_OP.name
                == sync_granularity
            ):
                self.cache = GroupAwareKVCacheManager(
                    config=kv_config, process_group=kv_group.cpu_group
                )
            elif (
                AIBrixPDReuseConnectorSyncGranularity.PER_BATCH.name
                == sync_granularity
            ):
                self.cache = BaseKVCacheManager(config=kv_config)
                self.kv_group = kv_group.cpu_group
                self._coll_tensor = torch.empty(
                    (config.scheduler_config.max_num_seqs * 3),
                    dtype=torch.int32,
                )
            else:
                raise ValueError(f"Unknown sync granularity {sync_granularity}")

        self.rank = rank
        self.head_size = head_size
        self.tp_size = tp_size
        self.num_kv_heads = num_kv_heads
        self.num_layers = num_layers
        self.kv_head_ids = kv_head_ids
        self.layer_ids = layer_ids
        self.engine_block_ntokens = block_ntokens
        self.cache_block_ntokens = self.cache.block_size
        self.block_dtype = block_dtype
        self.block_shape = block_spec.block_shape
        self.block_spec = block_spec
        self.block_layout = block_spec.block_layout
        self.chunk_size = self.cache.chunk_size
        self.cache_feature = self.cache.feature
        self.kv_cache_dtype = kv_cache_dtype

        # KV caches and kv scales will be init'ed later
        self.no_compile_layers = (
            config.compilation_config.static_forward_context
        )
        self.kv_caches: dict[str, torch.Tensor] | None = None
        self.layers_kv_caches: list[torch.Tensor] | None = None
        self.k_scales: list[torch.Tensor] | None = None
        self.v_scales: list[torch.Tensor] | None = None

        self._meta_cache: dict[str, AIBrixPDReuseConnectorCachedMeta] = {}
        # used by decoder in the pd mode
        self._received_requests: set[str] = set()
        # metrics
        self._metrics = AIBrixOffloadingConnectorMetrics(self.cache.metrics)
        logger.info(
            "AIBrixPDReuseConnector is initialized, "
            "engine_block_ntokens=%d, cache_block_ntokens=%d",
            self.engine_block_ntokens,
            self.cache_block_ntokens,
        )

    def shutdown(self) -> None:
        if getattr(self, "cache", None) is not None:
            self.cache.close()
            self.cache = None  # type: ignore[assignment]

    def _get_block_layout(self) -> KVCacheBlockLayout:
        backend_name = self.attn_backend.get_name()
        supported = PD_REUSE_CONNECTOR_SUPPORTED_ATTN_BACKENDS
        if backend_name in supported:
            return KVCacheBlockLayout(supported[backend_name])
        raise NotImplementedError(
            f"Only support attn backends in "
            f"{list(PD_REUSE_CONNECTOR_SUPPORTED_ATTN_BACKENDS.keys())}. "
            f"{self.attn_backend.get_name()} is used."
        )

    def _update_meta_cache(
        self,
        metadata: AIBrixPDReuseConnectorMetadata,
    ) -> None:
        # Remove finished requests
        for req_id in metadata.finished_requests_ids:
            if req_id in self._meta_cache:
                self._meta_cache.pop(req_id)

        # Update metadata cache
        for req_id, meta in metadata.items():
            if req_id not in self._meta_cache:
                is_pd_mode = meta.do_remote_prefill or meta.do_remote_decode
                self._meta_cache[req_id] = AIBrixPDReuseConnectorCachedMeta(
                    meta.prompt_len,
                    cache_block_ntokens=self.cache_block_ntokens,
                    enable_padding=is_pd_mode,
                )
            elif meta.resumed_from_preemption:
                self._meta_cache[req_id].context_slot_mapping_offset = 0
                self._meta_cache[req_id].context_slot_mapping.zero_()  # type: ignore[union-attr]

            seq_meta_cache = self._meta_cache[req_id]
            seq_meta_cache.extend(meta.seq_token_ids, meta.seq_slot_mapping)

    def register_kv_caches(self, kv_caches: dict[str, torch.Tensor]) -> None:
        self.kv_caches = kv_caches
        layer_names = self.no_compile_layers.keys()
        self.layers_kv_caches = [
            self.kv_caches[layer_name] for layer_name in layer_names
        ]
        self.layer_name_idx_mapping = {
            layer_name: idx for idx, layer_name in enumerate(layer_names)
        }
        layers = self.no_compile_layers.values()
        self.k_scales = [layer._k_scale for layer in layers]
        self.v_scales = [layer._v_scale for layer in layers]

    @tag_wrapper(
        {
            "connector": "AIBrixPDReuseConnector",
            "func": "start_load_kv_before_update",
        }
    )
    def start_load_kv_before_update(
        self,
        metadata: AIBrixPDReuseConnectorMetadata,
    ) -> dict[str, int]:
        """
        Start loading KV cache.
        """
        self._update_meta_cache(metadata)

        stats = {}
        for seq_request_id, seq_request_meta in metadata.items():
            num_fetched_tokens = self._recv_kv_from_cache_impl(seq_request_meta)

            if not seq_request_meta.do_remote_prefill:
                # decoder should not update stats
                stats[seq_request_id] = num_fetched_tokens

        if len(stats) > 0 and self.kv_group is not None:
            for idx, (seq_request_id, seq_request_meta) in enumerate(
                metadata.items()
            ):
                self._coll_tensor[idx] = stats.get(seq_request_id, 0)
            dist.all_reduce(
                self._coll_tensor[idx], dist.ReduceOp.MIN, self.kv_group
            )

            for idx, (seq_request_id, seq_request_meta) in enumerate(
                metadata.items()
            ):
                if self._coll_tensor[idx] > 0:
                    stats[seq_request_id] = self._coll_tensor[idx].item()
                else:
                    stats.pop(seq_request_id, None)

        for seq_request_id, num_fetched_tokens in stats.items():
            seq_request_meta = metadata[seq_request_id]
            # Update seq_request_meta
            seq_request_meta.query_len -= num_fetched_tokens
            seq_request_meta.context_len += num_fetched_tokens
            seq_request_meta.state = (
                AIBrixPDReuseConnectorRequestState.WAITING_FOR_SEND
            )

        return stats

    def _recv_kv_from_cache_impl(
        self,
        seq_request_meta: AIBrixPDReuseConnectorRequestMetadata,
    ) -> int:
        """
        Receive KV cache from KVCacheManager (kvcache reuse).
        """
        logger.debug("_recv_kv_from_cache_impl: %s", seq_request_meta)
        seq_request_id = seq_request_meta.req_id
        seq_cached_meta = self._meta_cache[seq_request_id]
        seq_all_tokens = seq_cached_meta.get_context_tokens_view()
        assert seq_all_tokens is not None, "seq_all_tokens is None"
        seq_context_len = seq_request_meta.context_len
        is_pd_mode = (
            seq_request_meta.do_remote_prefill
            or seq_request_meta.do_remote_decode
        )

        prompt_len = seq_request_meta.prompt_len

        # Align to block boundary
        aligned_context_len = round_down(
            seq_context_len, self.cache_block_ntokens
        )
        shift_len = seq_context_len - aligned_context_len
        if not seq_request_meta.do_remote_prefill:
            query_len = seq_request_meta.query_len
            actual_query_len = seq_context_len + query_len - aligned_context_len
            aligned_query_len = round_down(
                actual_query_len, self.cache_block_ntokens
            )

            assert prompt_len >= aligned_context_len + aligned_query_len, (
                f"{prompt_len}<{aligned_context_len}+{aligned_query_len}"
            )
        else:
            # Decoder in pd mode needs to load kvcache for the entire prompt not
            # the scheduled tokens
            actual_query_len = prompt_len - aligned_context_len
            aligned_query_len = round_up(
                actual_query_len, self.cache_block_ntokens
            )
            # mark this request as received
            self._received_requests.add(seq_request_id)

        prefix = seq_all_tokens[:aligned_context_len]
        tokens = seq_all_tokens[
            aligned_context_len : aligned_context_len + aligned_query_len
        ]

        if not is_pd_mode:
            # KV cache reuse only (no PD disaggregation): Use threshold to
            # avoid loading very small chunks
            threshold = max(
                PD_REUSE_CONNECTOR_SKIP_THRESHOLD * self.engine_block_ntokens,
                self.cache_block_ntokens,
            )
            if aligned_query_len < threshold:
                logger.debug(
                    "Skip Request[id=%s, context_len=%d, query_len=%d]",
                    seq_request_id,
                    aligned_context_len,
                    aligned_query_len,
                )
                return 0

        if self._metrics.time_measurement_enabled:
            start = torch.cuda.Event(enable_timing=True)
            end = torch.cuda.Event(enable_timing=True)
            start.record()

        seq_recv_len = 0
        for (
            chunk_prefix,
            chunk_tokens,
            next_tokens,
            _,
        ) in self.cache.cache_chunk_keys(prefix, tokens):
            if next_tokens and len(next_tokens) > 0:
                # Prefetch
                self.cache.prefetch(chunk_prefix + chunk_tokens, next_tokens)

            # Get KV caches from KVCacheManager
            status = self.cache.acquire(chunk_prefix, chunk_tokens)

            if not status.is_ok():
                if not status.is_not_found():
                    log_every_n_seconds(
                        logger,
                        logging.ERROR,
                        "Failed to get from KVCacheManager: %s",
                        3,
                        str(status),
                    )
                break

            num_fetched_tokens, handle = status.value
            kv_blocks = handle.to_tensors()

            offset = len(chunk_prefix)
            length = num_fetched_tokens

            chunk_slot_mapping = seq_cached_meta.context_slot_mapping[
                offset : offset
                + length  # type: ignore[index]
            ]

            with perf_timer() as get_kernel_onload_dur_ms:
                reshape_and_cache_multi_layer(
                    kv_blocks,
                    self.layers_kv_caches,  # type: ignore[arg-type]
                    chunk_slot_mapping,
                    self.engine_block_ntokens,
                    "auto",
                    self.k_scales,  # type: ignore[arg-type]
                    self.v_scales,  # type: ignore[arg-type]
                    self.block_layout.name,
                    self.kv_layout_blocks_first,
                )

            logger.info(
                "Request[id=%s] onloads %d tokens in %.4f ms",
                seq_request_id,
                length,
                get_kernel_onload_dur_ms(),
            )

            # Update recv_len
            if len(chunk_prefix) + num_fetched_tokens <= prompt_len:
                seq_recv_len += num_fetched_tokens - shift_len
            else:
                seq_recv_len += prompt_len - len(chunk_prefix) - shift_len
                logger.debug(
                    "Request[id=%s] padding chunk: %d tokens %d padding",
                    seq_request_id,
                    prompt_len - len(chunk_prefix),
                    num_fetched_tokens + len(chunk_prefix) - prompt_len,
                )
            # Reset shift_len
            shift_len = 0

            # Release handle
            handle.release()

            if num_fetched_tokens < len(chunk_tokens):
                # Didn't receive all tokens for current chunk, break
                break

        log_if(
            logger,
            logging.INFO,
            "Request[id=%s, prompt_len=%d, context_len=%d] reused %d tokens",
            seq_recv_len > 0,
            seq_request_id,
            prompt_len,
            seq_context_len,
            seq_recv_len,
        )

        if self._metrics.time_measurement_enabled:
            end.record()
            end.synchronize()
            lat_ms = start.elapsed_time(end)
            self._metrics._recv_metrics.add(
                aligned_context_len, aligned_query_len, seq_recv_len, lat_ms
            )

        return seq_recv_len

    @tag_wrapper(
        {"connector": "AIBrixPDReuseConnector", "func": "wait_for_save"}
    )
    def wait_for_save(
        self,
        metadata: AIBrixPDReuseConnectorMetadata,
    ) -> None:
        """
        Save newly generated KV cache to KVCacheManager (for kvcache reuse).

        NOTE: After this function completes, KV cache is saved to L2
        cache (e.g., SHFS). The GPU blocks can be freed immediately in
        request_finished() because decoder will read from L2 cache, not from
        prefiller's GPU memory.
        """
        assert self.layers_kv_caches is not None, "layers_kv_caches is None"

        is_prefiller = False
        for seq_request_id, seq_request_meta in metadata.items():
            if seq_request_meta.do_remote_decode:
                is_prefiller = True
            if seq_request_meta.query_len == 0:
                continue
            self._send_kv_to_cache_impl(seq_request_meta)

        if is_prefiller:
            # ensure all async ops are completed
            self.cache.flush()

        if self._metrics.time_measurement_enabled:
            log_every_n_seconds(
                logger,
                logging.INFO,
                self._metrics.log_str(),
                10,
            )

    def _send_kv_to_cache_impl(
        self,
        seq_request_meta: AIBrixPDReuseConnectorRequestMetadata,
    ) -> None:
        """
        Send KV cache to KVCacheManager (kvcache reuse).
        """
        logger.debug("_send_kv_to_cache_impl: %s", seq_request_meta)
        seq_request_id = seq_request_meta.req_id
        seq_context_len = seq_request_meta.context_len
        seq_cached_meta = self._meta_cache[seq_request_id]
        seq_all_tokens = seq_cached_meta.get_context_tokens_view()
        assert seq_all_tokens is not None, "seq_all_tokens is None"
        prompt_len = seq_request_meta.prompt_len
        query_len = seq_request_meta.query_len
        is_pd_mode = (
            seq_request_meta.do_remote_prefill
            or seq_request_meta.do_remote_decode
        )
        have_all_prompt_tokens = seq_context_len + query_len >= prompt_len

        # Align to block boundary
        aligned_context_len = round_down(
            seq_context_len, self.cache_block_ntokens
        )
        actual_query_len = seq_context_len + query_len - aligned_context_len

        if is_pd_mode and have_all_prompt_tokens:
            aligned_query_len = round_up(
                actual_query_len, self.cache_block_ntokens
            )
        else:
            aligned_query_len = round_down(
                actual_query_len, self.cache_block_ntokens
            )

            assert prompt_len >= aligned_context_len + aligned_query_len, (
                f"{prompt_len}<{aligned_context_len}+{aligned_query_len}"
            )

        prefix = seq_all_tokens[:aligned_context_len]
        tokens = seq_all_tokens[
            aligned_context_len : aligned_context_len + aligned_query_len
        ]

        if self._metrics.time_measurement_enabled:
            start = torch.cuda.Event(enable_timing=True)
            end = torch.cuda.Event(enable_timing=True)
            start.record()

        total_sent = 0
        for (
            chunk_prefix,
            chunk_tokens,
            _,
            all,
        ) in self.cache.cache_chunk_keys(prefix, tokens):
            # |----------- seq_context_len ---------|- offset -|
            # |- aligned_context_len -|- shift_len -|
            #                         |--- n * chunk_size -----|
            # |----------------- chunk_prefix -----------------|- tokens -|
            #                         |-------- (n + 1) * chunk_size -----|
            chunk_size = len(chunk_tokens)
            offset = len(chunk_prefix)
            length = chunk_size

            # Check if already exists in cache
            exists_status = self.cache.exists(chunk_prefix, chunk_tokens)
            if exists_status.is_ok():
                num_existing_tokens = exists_status.value
                logger.info(
                    "Request[id=%s] send(%d) encounters %d existing tokens",
                    seq_request_id,
                    length,
                    num_existing_tokens,
                )
                if chunk_size == num_existing_tokens:
                    continue
                else:
                    # Partially exists
                    offset += num_existing_tokens
                    length -= num_existing_tokens
                    new_chunk_prefix_len = (
                        len(chunk_prefix) + num_existing_tokens
                    )
                    chunk_prefix = all[:new_chunk_prefix_len]
                    chunk_tokens = all[
                        new_chunk_prefix_len : new_chunk_prefix_len + length
                    ]

            # Allocate space for KV caches
            status = self.cache.allocate_for(chunk_prefix, chunk_tokens)
            if not status.is_ok():
                log_every_n_seconds(
                    logger,
                    logging.ERROR,
                    "Failed to allocate: %s",
                    3,
                    str(status),
                )
                break
            handle = status.value
            tensors = handle.to_tensors()
            length = len(tensors) * self.cache_block_ntokens

            chunk_slot_mapping = seq_cached_meta.context_slot_mapping[
                offset : offset
                + length  # type: ignore[index]
            ]

            with perf_timer() as get_kernel_offload_dur_ms:
                reshape_and_offload_multi_layer(
                    tensors,
                    self.layers_kv_caches,  # type: ignore[arg-type]
                    chunk_slot_mapping,
                    self.engine_block_ntokens,
                    "auto",
                    self.k_scales,  # type: ignore[arg-type]
                    self.v_scales,  # type: ignore[arg-type]
                    self.block_layout.name,
                    self.kv_layout_blocks_first,
                )

            logger.info(
                "Request[id=%s] offloads %d tokens in %.4f ms",
                seq_request_id,
                length,
                get_kernel_offload_dur_ms(),
            )

            # Put KV caches to KVCacheManager (L2 cache, e.g., SHFS)
            status = self.cache.put(chunk_prefix, chunk_tokens[:length], handle)
            if not status.is_ok():
                log_every_n_seconds(
                    logger,
                    logging.ERROR,
                    "Failed to put to KVCacheManager: %s",
                    3,
                    str(status),
                )
                break

            put_ntokens = status.get()
            total_sent += put_ntokens
            if len(chunk_prefix) + put_ntokens > prompt_len:
                logger.debug(
                    "Request[id=%s] padding chunk: %d tokens %d padding",
                    seq_request_id,
                    prompt_len - len(chunk_prefix),
                    put_ntokens + len(chunk_prefix) - prompt_len,
                )

            if put_ntokens != length:
                break

        log_if(
            logger,
            logging.INFO,
            "Request[id=%s, prompt_len=%d, context_len=%d] sent %d tokens",
            total_sent > 0,
            seq_request_id,
            prompt_len,
            seq_context_len,
            total_sent,
        )

        if self._metrics.time_measurement_enabled:
            end.record()
            end.synchronize()
            lat_ms = start.elapsed_time(end)
            self._metrics._send_metrics.add(
                aligned_context_len, aligned_query_len, total_sent, lat_ms
            )

    @tag_wrapper(
        {"connector": "AIBrixPDReuseConnector", "func": "get_finished"}
    )
    def get_finished(
        self,
        metadata: AIBrixPDReuseConnectorMetadata,
    ) -> None:
        received_requests = self._received_requests
        self._received_requests = set()
        return None, received_requests  # type: ignore[return-value]


class AIBrixPDReuseConnector(KVConnectorBase_V1):
    """
    AIBrixPDReuseConnector combines PD disaggregation with kvcache reuse.

    This connector:
    1. Supports pd disaggregation: transfer KV cache between prefiller & decoder
    2. Supports kvcache reuse: use KVCacheManager to store and retrieve (reuse)
    3. L2 connector (e.g., SHFS) is transparent to this connector
    """

    def __init__(
        self,
        config: "VllmConfig",
        role: KVConnectorRole,
        kv_cache_config: Optional["VllmKVCacheConfig"] = None,
    ):
        super().__init__(
            vllm_config=config, role=role, kv_cache_config=kv_cache_config
        )

        self.connector_scheduler: Optional[AIBrixPDReuseConnectorScheduler] = (
            None
        )
        self.connector_worker: Optional[AIBrixPDReuseConnectorWorker] = None

        if role == KVConnectorRole.SCHEDULER:
            self.connector_scheduler = AIBrixPDReuseConnectorScheduler(config)
        elif role == KVConnectorRole.WORKER:
            self.connector_worker = AIBrixPDReuseConnectorWorker(config)

    # ==============================
    # Scheduler-side methods
    # ==============================

    @delegate_to("connector_scheduler")
    def get_num_new_matched_tokens(  # type: ignore
        self,
        request: "Request",
        num_computed_tokens: int,
    ) -> tuple[int, bool]:
        """Get number of new matched tokens."""
        pass

    @delegate_to("connector_scheduler")
    def update_state_after_alloc(
        self,
        request: "Request",
        blocks: "KVCacheBlocks",
        num_external_tokens: int,
    ):
        """Update state after block allocation."""
        pass

    @delegate_to("connector_scheduler")
    def build_connector_meta(
        self,
        scheduler_output: "SchedulerOutput",
    ) -> KVConnectorMetadata:
        """Build connector metadata."""
        pass

    @delegate_to("connector_scheduler")
    def request_finished(  # type: ignore
        self,
        request: "Request",
        block_ids: list[int],
    ) -> tuple[bool, Optional[dict[str, Any]]]:
        """Handle request finished."""
        pass

    # ==============================
    # Worker-side methods
    # ==============================

    @delegate_to("connector_worker")
    def shutdown(self):
        pass

    @delegate_to("connector_worker")
    def register_kv_caches(self, kv_caches: dict[str, torch.Tensor]):
        """Register KV caches."""
        pass

    def start_load_kv_before_update(self, **kwargs) -> dict[str, int]:
        """
        Start loading the KV cache from the connector to vLLM's paged
        KV buffer before gpu runner updating its states.
        """
        assert self.connector_worker is not None
        assert isinstance(
            self._connector_metadata,
            AIBrixPDReuseConnectorMetadata,
        )
        return self.connector_worker.start_load_kv_before_update(
            self._connector_metadata
        )

    def start_load_kv(
        self, forward_context: "ForwardContext", **kwargs
    ) -> None:
        """Start loading KV cache."""
        pass

    def wait_for_layer_load(self, layer_name: str) -> None:
        """Wait for layer load."""
        pass

    def save_kv_layer(
        self,
        layer_name: str,
        kv_layer: torch.Tensor,
        attn_metadata: "AttentionMetadata",
        **kwargs,
    ) -> None:
        """Save KV layer."""
        pass

    def wait_for_save(self, **kwargs) -> None:
        """
        Wait for all KV cache saves to complete.
        """
        assert self.connector_worker is not None
        assert isinstance(
            self._connector_metadata,
            AIBrixPDReuseConnectorMetadata,
        )
        self.connector_worker.wait_for_save(self._connector_metadata)

    def get_finished(
        self,
        finished_req_ids: set[str],
    ) -> tuple[set[str], set[str]]:
        """Get finished requests."""
        assert self.connector_worker is not None
        assert isinstance(
            self._connector_metadata,
            AIBrixPDReuseConnectorMetadata,
        )
        return self.connector_worker.get_finished(self._connector_metadata)

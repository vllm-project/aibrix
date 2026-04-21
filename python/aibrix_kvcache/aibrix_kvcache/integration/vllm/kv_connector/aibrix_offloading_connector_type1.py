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
import enum
import logging
from dataclasses import dataclass, field
from functools import wraps
from typing import TYPE_CHECKING, Any, Callable, Optional, TypeVar

import numpy as np
import torch
import torch.distributed as dist

# from vllm.v1.attention.backends.flashinfer import FlashInferBackend
# from vllm.v1.attention.backends.flex_attention import FlexAttentionBackend
# from vllm.v1.attention.backends.triton_attn import TritonAttentionBackend
from vllm.distributed import (
    get_tp_group,
)
from vllm.distributed.kv_transfer.kv_connector.v1.base import (
    KVConnectorBase_V1,
    KVConnectorMetadata,
    KVConnectorRole,
    KVConnectorWorkerMetadata,
)
from vllm.utils.math_utils import round_down, round_up
from vllm.utils.torch_utils import get_kv_cache_torch_dtype
from vllm.v1.attention.backends.flash_attn import FlashAttentionBackend
from vllm.v1.attention.backends.flashinfer import FlashInferBackend

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

OFFLOADING_CONNECTOR_SKIP_THRESHOLD = 8
OFFLOADING_CONNECTOR_SUPPORTED_ATTN_BACKENDS = {
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


class AIBrixOffloadingConnectorSyncGranularity(enum.Enum):
    """The sync granularity used by AIBrix offloading connectors.
    NONE: no synchronization among TP participants.
    PER_OP: sync up the min num. of tokens fetched by each TP participant after
            each GET/ACUQUIRE operation.
    PER_BATCH: sync up the min num. of tokens fetched by each TP participant
               after each batch.
    """

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
        self.op_lat_ms: Optional[list[float]] = None

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
        lat_ms: float,
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


@dataclass
class AIBrixOffloadingConnectorCachedMeta:
    def __init__(self, prompt_len: int) -> None:
        self.context_tokens: np.ndarray = np.empty(prompt_len, dtype=np.int32)
        self.context_tokens_offset: int = 0
        self.context_tokens_view: Optional[TokenListView] = None
        self.context_slot_mapping: Optional[torch.Tensor] = torch.empty(
            prompt_len,
            dtype=torch.long,
            device="cuda",
        )
        self.context_slot_mapping_offset: int = 0

    def get_context_tokens(self) -> list[int]:
        return self.context_tokens[: self.context_tokens_offset]

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
            self.context_tokens[offset : offset + length] = tokens[1]
            self.context_tokens_offset = offset + length
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


class AIBrixOffloadingConnectorRequestState(enum.IntEnum):
    INIT = enum.auto()
    WAITING_FOR_ALLOC = enum.auto()
    WAITING_FOR_SEND = enum.auto()
    WAITING_FOR_RECV = enum.auto()
    SENDING = enum.auto()
    RECEIVING = enum.auto()


@dataclass
class AIBrixOffloadingConnectorRequestMetadata(CachedPyObjectBase):
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
    state: AIBrixOffloadingConnectorRequestState = (
        AIBrixOffloadingConnectorRequestState.INIT
    )
    resumed_from_preemption: bool = False

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
            f"AIBrixOffloadingConnectorRequestMetadata["
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
            f"resumed_from_preemption={self.resumed_from_preemption}]"
        )

    def __repr__(self):
        return self.__str__()


class AIBrixOffloadingConnectorMetadata(KVConnectorMetadata):
    def __init__(
        self, requests: dict[str, AIBrixOffloadingConnectorRequestMetadata]
    ):
        # Requests that need to load from external kvcache.
        self.requests = requests
        self.finished_requests_ids: set[str] = set()
        self.total_num_scheduled_tokens: int = 0
        self.side_channel_host: str = ""
        self.side_channel_port: int = -1

    def __getitem__(self, key: str) -> AIBrixOffloadingConnectorRequestMetadata:
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
            request_id, AIBrixOffloadingConnectorRequestMetadata()
        ).__dict__.update(req_id=request_id, **kwargs)

    def pop_request(
        self,
        request_id: str,
    ) -> AIBrixOffloadingConnectorRequestMetadata | None:
        return self.requests.pop(request_id, None)

    def finish_request(self, request_id: str) -> None:
        self.finished_requests_ids.add(request_id)
        self.pop_request(request_id)

    def get(self, predicate: Callable) -> "AIBrixOffloadingConnectorMetadata":
        requests = {k: v for k, v in self.requests.items() if predicate(v)}
        return AIBrixOffloadingConnectorMetadata(requests=requests)

    def filter_requests(self, predicate: Callable) -> None:
        self.requests = {
            k: v for k, v in self.requests.items() if not predicate(v)
        }

    def extend(self, other: "AIBrixOffloadingConnectorMetadata") -> None:
        self.requests.update(other.requests)

    def clear(self) -> None:
        self.requests.clear()
        self.finished_requests_ids.clear()

    def __str__(self) -> str:
        return f"AIBrixOffloadingConnectorMetadata: {self.__dict__}"


@dataclass
class AIBrixWorkerMeta(KVConnectorWorkerMetadata):
    """Worker-to-scheduler metadata reporting L1 cache state changes.

    - saved_tokens: tokens newly written to the external L1 cache
    - failed_load_tokens: tokens the scheduler promised as cached but the
      worker could not actually load (eviction race). The scheduler
      removes the corresponding block hashes from its tracker so
      subsequent get_num_new_matched_tokens lookups stay consistent.
    """

    saved_tokens: dict[str, int] = field(default_factory=dict)
    failed_load_tokens: dict[str, int] = field(default_factory=dict)

    def aggregate(
        self, other: "KVConnectorWorkerMetadata"
    ) -> "KVConnectorWorkerMetadata":
        """Aggregate across TP ranks.

        saved_tokens uses intersection + min(): a block only counts as
        cached when ALL ranks reported saving it. Union would mark
        blocks cached when only some ranks saved, causing load failures
        on ranks that missed the save.

        failed_load_tokens uses union + max(): if ANY rank failed to
        load a block, the block is unusable for the whole TP group —
        the scheduler must invalidate the corresponding hash for every
        rank, not just the one that reported the failure.
        """
        if not isinstance(other, AIBrixWorkerMeta):
            return self
        saved_merged = {
            req_id: min(num_tokens, other.saved_tokens[req_id])
            for req_id, num_tokens in self.saved_tokens.items()
            if req_id in other.saved_tokens
        }
        failed_merged = dict(self.failed_load_tokens)
        for req_id, num_tokens in other.failed_load_tokens.items():
            failed_merged[req_id] = max(
                failed_merged.get(req_id, 0), num_tokens
            )
        return AIBrixWorkerMeta(
            saved_tokens=saved_merged,
            failed_load_tokens=failed_merged,
        )


class AIBrixOffloadingConnectorScheduler:
    def __init__(self, config: "VllmConfig"):
        self.kv_role = config.kv_transfer_config.kv_role
        self.engine_block_ntokens = config.cache_config.block_size

        self._scheduler_meta = AIBrixOffloadingConnectorMetadata({})

        # Track which vLLM block hashes are in external L1 cache.
        # Uses vLLM's BlockHash (bytes), NOT AIBrix's FarmHash32 strings.
        self._cached_block_hashes: set[bytes] = set()
        # Keep block_hashes per request so we can map worker reports
        # (req_id -> num_tokens_saved) to vLLM block hashes.
        self._request_block_hashes: dict[str, list] = {}
        # num_external_tokens promised to the scheduler per request.
        # Populated by update_state_after_alloc, consumed by
        # build_connector_meta to set load_len in worker metadata.
        self._request_external_tokens: dict[str, int] = {}

    def build_connector_meta(
        self, scheduler_output: "SchedulerOutput"
    ) -> KVConnectorMetadata:
        # 1. remove finished requests
        for req_id in scheduler_output.finished_req_ids:
            self._scheduler_meta.pop_request(req_id)

        # 2. new requests
        for req in scheduler_output.scheduled_new_reqs:
            req_id = req.req_id

            prompt_len = len(req.prompt_token_ids)
            context_len = req.num_computed_tokens
            query_len = scheduler_output.num_scheduled_tokens[req_id]

            if context_len >= prompt_len:
                continue

            (block_ids,) = req.block_ids
            slot_mapping = self._block_ids_to_slot_mapping(block_ids)

            # load_len = num_external_tokens promised by scheduler via
            # get_num_new_matched_tokens. Tells the worker how many tokens
            # to load from L1 cache (vs recomputing).
            load_len = self._request_external_tokens.pop(req_id, 0)

            self._scheduler_meta.upsert_request(
                req_id,
                prompt_len=prompt_len,
                context_len=context_len,
                query_len=query_len,
                load_len=load_len,
                seq_token_ids=(0, req.prompt_token_ids),
                seq_slot_mapping=(0, slot_mapping),
                state=AIBrixOffloadingConnectorRequestState.WAITING_FOR_RECV,
            )

        # 3. cached requests
        cached_reqs = scheduler_output.scheduled_cached_reqs
        req_ids = cached_reqs.req_ids
        for i in range(len(req_ids)):
            req_id = req_ids[i]

            if req_id not in self._scheduler_meta:
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

            load_len = self._request_external_tokens.pop(req_id, 0)

            self._scheduler_meta.upsert_request(
                req_id,
                prompt_len=prompt_len,
                context_len=context_len,
                query_len=query_len,
                load_len=load_len,
                seq_token_ids=None,
                seq_slot_mapping=seq_slot_mapping,
                state=AIBrixOffloadingConnectorRequestState.WAITING_FOR_RECV,
                resumed_from_preemption=req_id in cached_reqs.resumed_req_ids,
            )

        # 4. keep requests that are in the WAITING_FOR_RECV state
        meta = self._scheduler_meta.get(
            lambda req: req.state
            == AIBrixOffloadingConnectorRequestState.WAITING_FOR_RECV
        )

        logger.debug("SCHEDULER: build_connector_meta, meta=%s", meta.__dict__)

        # 5. update scheduled requests
        for req_id in meta:
            self._scheduler_meta.upsert_request(
                req_id,
                state=AIBrixOffloadingConnectorRequestState.RECEIVING,
            )

        # 6. attach finished requests
        meta.finished_requests_ids = self._scheduler_meta.finished_requests_ids
        self._scheduler_meta.finished_requests_ids = set()

        logger.debug(
            "Num. of scheduled requests: %s",
            len(
                self._scheduler_meta.get(
                    lambda req: req.state
                    == AIBrixOffloadingConnectorRequestState.RECEIVING
                )
            ),
        )
        return meta

    def request_finished(
        self,
        request: "Request",
        block_ids: list[int],
    ) -> tuple[bool, Optional[dict[str, Any]]]:
        req_id = request.request_id
        logger.debug("SCHEDULER: Request[id=%s] finished", req_id)

        self._scheduler_meta.finish_request(req_id)
        self._request_block_hashes.pop(req_id, None)
        self._request_external_tokens.pop(req_id, None)
        return False, None

    def receive_connector_worker_meta(
        self, worker_meta: Optional[KVConnectorWorkerMetadata]
    ) -> None:
        """Process worker metadata to update the scheduler-side cache tracker.

        saved_tokens: maps {req_id: num_tokens_saved} from the worker to
        vLLM block hashes, marking those blocks as cached for future
        get_num_new_matched_tokens lookups.

        failed_load_tokens: maps {req_id: num_tokens_failed_to_load} from
        the worker. The scheduler removes the corresponding block hashes
        from its tracker so it stops promising stale entries. Partial
        loads always fail at the tail of the promised range, so the
        failing hashes are the last N of the request's block_hashes.
        """
        if worker_meta is None or not isinstance(worker_meta, AIBrixWorkerMeta):
            return
        block_size = self.engine_block_ntokens
        for req_id, num_tokens in worker_meta.saved_tokens.items():
            block_hashes = self._request_block_hashes.get(req_id, [])
            num_blocks = num_tokens // block_size
            for i in range(min(num_blocks, len(block_hashes))):
                self._cached_block_hashes.add(block_hashes[i])
        for req_id, num_tokens in worker_meta.failed_load_tokens.items():
            block_hashes = self._request_block_hashes.get(req_id, [])
            if not block_hashes:
                continue
            failed_blocks = num_tokens // block_size
            if failed_blocks <= 0:
                continue
            # Remove the last failed_blocks entries — partial loads always
            # fail at the tail of the range the scheduler promised.
            for h in block_hashes[-failed_blocks:]:
                self._cached_block_hashes.discard(h)

    def _block_ids_to_slot_mapping(self, block_ids: list[int]) -> torch.Tensor:
        block_ids_tensor = torch.tensor(block_ids)
        num_blocks = block_ids_tensor.shape[0]
        block_offsets = torch.arange(0, self.engine_block_ntokens)
        slot_mapping = (
            block_offsets.reshape((1, self.engine_block_ntokens))
            + block_ids_tensor.reshape((num_blocks, 1))
            * self.engine_block_ntokens
        )
        return slot_mapping.flatten()


class AIBrixOffloadingConnectorWorker:
    """AIBrixOffloadingConnectorWorker carries out the data-plane operations."""

    def __init__(self, config: "VllmConfig"):
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
                AIBrixOffloadingConnectorSyncGranularity.NONE.name
                == sync_granularity
            ):
                self.cache = BaseKVCacheManager(config=kv_config)
            elif (
                AIBrixOffloadingConnectorSyncGranularity.PER_OP.name
                == sync_granularity
            ):
                self.cache = GroupAwareKVCacheManager(
                    config=kv_config, process_group=kv_group.cpu_group
                )
            elif (
                AIBrixOffloadingConnectorSyncGranularity.PER_BATCH.name
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

        self._meta_cache: dict[str, AIBrixOffloadingConnectorCachedMeta] = {}
        # Track tokens saved to L1 cache per request (for scheduler reporting)
        self._newly_saved_tokens: dict[str, int] = {}
        # Track tokens the scheduler promised as cached but the worker
        # could not actually load (partial load / eviction race). Reported
        # to the scheduler via AIBrixWorkerMeta.failed_load_tokens so it
        # can remove the corresponding block hashes from its tracker.
        self._newly_failed_load_tokens: dict[str, int] = {}
        # Track vLLM block_ids where cache.acquire() returned less than the
        # scheduler promised (eviction race). These are reported via
        # get_block_ids_with_load_errors() so vLLM can roll back
        # num_computed_tokens and re-schedule for recompute
        # (when kv_load_failure_policy="recompute", the default).
        self._failed_load_block_ids: set[int] = set()
        # metrics
        self._metrics = AIBrixOffloadingConnectorMetrics(self.cache.metrics)
        logger.info(
            "AIBrixOffloadingConnector is initialized, "
            "engine_block_ntokens=%d, cache_block_ntokens=%d",
            self.engine_block_ntokens,
            self.cache_block_ntokens,
        )

    def __del__(self) -> None:
        if getattr(self, "cache", None) is not None:
            self.cache.close()
            self.cache = None  # type: ignore[assignment]

    def _get_block_layout(self) -> KVCacheBlockLayout:
        if (
            self.attn_backend.get_name()
            in OFFLOADING_CONNECTOR_SUPPORTED_ATTN_BACKENDS
        ):
            return KVCacheBlockLayout(
                OFFLOADING_CONNECTOR_SUPPORTED_ATTN_BACKENDS[
                    self.attn_backend.get_name()
                ]
            )
        raise NotImplementedError(
            f"Only support attn backends in "
            f"{list(OFFLOADING_CONNECTOR_SUPPORTED_ATTN_BACKENDS.keys())}. "
            f"{self.attn_backend.get_name()} is used."
        )

    def _update_meta_cache(
        self,
        metadata: AIBrixOffloadingConnectorMetadata,
    ) -> None:
        # remove finished requests
        for req_id in metadata.finished_requests_ids:
            if req_id in self._meta_cache:
                self._meta_cache.pop(req_id)

        for req_id, meta in metadata.items():
            if req_id not in self._meta_cache:
                self._meta_cache[req_id] = AIBrixOffloadingConnectorCachedMeta(
                    meta.prompt_len
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
            "connector": "AIBrixOffloadingConnectorV1Type1",
            "func": "start_load_kv_before_update",
        }
    )
    def start_load_kv_before_update(
        self,
        metadata: AIBrixOffloadingConnectorMetadata,
    ) -> dict[str, int]:
        self._update_meta_cache(metadata)

        stats = {}
        for seq_request_id, seq_request_meta in metadata.items():
            num_fetched_tokens = self._recv_kv_sync_impl(seq_request_meta)
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
            # NOTE: In the new flow (get_num_new_matched_tokens reports
            # external hits to the scheduler), context_len and query_len
            # are ALREADY adjusted by vLLM scheduler before we get here.
            # We must NOT double-adjust them. We only transition state.
            seq_request_meta.state = (
                AIBrixOffloadingConnectorRequestState.WAITING_FOR_SEND
            )

        return stats

    def _recv_kv_sync_impl(
        self,
        seq_request_meta: AIBrixOffloadingConnectorRequestMetadata,
    ) -> int:
        logger.debug("_recv_kv_sync_impl: %s", seq_request_meta)
        seq_request_id = seq_request_meta.req_id
        seq_cached_meta = self._meta_cache[seq_request_id]
        seq_all_tokens = seq_cached_meta.get_context_tokens_view()
        assert seq_all_tokens is not None, "seq_all_tokens is None"

        # load_len is the number of tokens the scheduler promised are in
        # the external L1 cache (via get_num_new_matched_tokens). These
        # are already counted in context_len. We need to LOAD them from
        # L1 into GPU KV buffer. Tokens to the LEFT of these are local
        # (computed in a previous step).
        load_len = seq_request_meta.load_len
        seq_context_len = seq_request_meta.context_len
        prompt_len = seq_request_meta.prompt_len

        if load_len <= 0:
            # Nothing to load from external cache
            return 0

        # Split: local_context is tokens computed locally; the last
        # load_len tokens of context_len come from external cache.
        local_context_len = seq_context_len - load_len
        assert local_context_len >= 0, (
            f"local_context_len={local_context_len} "
            f"(context_len={seq_context_len}, load_len={load_len})"
        )

        # Align local context DOWN to block boundary. The load range
        # starts here and must span full cache blocks.
        aligned_context_len = round_down(
            local_context_len, self.cache_block_ntokens
        )
        # Account for unaligned portion of local context (partial block)
        shift_len = local_context_len - aligned_context_len
        # Total load range aligned to full cache blocks
        aligned_query_len = round_down(
            shift_len + load_len, self.cache_block_ntokens
        )

        assert prompt_len >= aligned_context_len + aligned_query_len, (
            f"{prompt_len}<{aligned_context_len}+{aligned_query_len}"
        )

        threshold = max(
            OFFLOADING_CONNECTOR_SKIP_THRESHOLD * self.engine_block_ntokens,
            self.cache_block_ntokens,
        )
        if aligned_query_len < threshold:
            logger.debug(
                "Skip Request[id=%s, context_len=%d, load_len=%d]",
                seq_request_id,
                aligned_context_len,
                aligned_query_len,
            )
            return 0

        prefix = seq_all_tokens[:aligned_context_len]
        tokens = seq_all_tokens[
            aligned_context_len : aligned_context_len + aligned_query_len
        ]

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
                # prefetch
                self.cache.prefetch(chunk_prefix + chunk_tokens, next_tokens)

            # get KV caches from offloading service
            status = self.cache.acquire(chunk_prefix, chunk_tokens)

            if not status.is_ok():
                if not status.is_not_found():
                    log_every_n_seconds(
                        logger,
                        logging.ERROR,
                        "Failed to get from offloading service: %s",
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

            # update recv_len
            seq_recv_len += num_fetched_tokens - shift_len
            # reset shift_len
            shift_len = 0

            # release handle
            handle.release()

            if num_fetched_tokens < len(chunk_tokens):
                # didn't receive all tokens for current chunk, break
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

        # Detect partial load (eviction race): scheduler promised more
        # tokens than the worker was able to load. Report the vLLM
        # block_ids we failed to load so vLLM rolls back
        # num_computed_tokens and re-schedules those tokens for compute.
        if seq_recv_len < aligned_query_len:
            # Report failed tokens to the scheduler so it can invalidate
            # the corresponding block hashes in _cached_block_hashes.
            failed_tokens = aligned_query_len - seq_recv_len
            if failed_tokens > 0:
                prev = self._newly_failed_load_tokens.get(seq_request_id, 0)
                self._newly_failed_load_tokens[seq_request_id] = (
                    prev + failed_tokens
                )
            slot_mapping = seq_cached_meta.context_slot_mapping
            if slot_mapping is not None:
                block_size = self.engine_block_ntokens
                first_failed = aligned_context_len + seq_recv_len
                last_failed = aligned_context_len + aligned_query_len
                first_block = first_failed // block_size
                last_block = last_failed // block_size
                for blk_idx in range(first_block, last_block):
                    token_offset = blk_idx * block_size
                    if 0 <= token_offset < len(slot_mapping):
                        slot = int(slot_mapping[token_offset].item())
                        block_id = slot // block_size
                        self._failed_load_block_ids.add(block_id)

        if self._metrics.time_measurement_enabled:
            end.record()
            end.synchronize()
            lat_ms = start.elapsed_time(end)
            self._metrics._recv_metrics.add(
                aligned_context_len, aligned_query_len, seq_recv_len, lat_ms
            )

        return seq_recv_len

    @tag_wrapper(
        {
            "connector": "AIBrixOffloadingConnectorV1Type1",
            "func": "wait_for_save",
        }
    )
    def wait_for_save(
        self,
        metadata: AIBrixOffloadingConnectorMetadata,
    ) -> None:
        assert self.layers_kv_caches is not None, "layers_kv_caches is None"

        for seq_request_id, seq_request_meta in metadata.items():
            if seq_request_meta.query_len == 0:
                continue
            self._send_kv_sync_impl(seq_request_meta)

        if self._metrics.time_measurement_enabled:
            log_every_n_seconds(
                logger,
                logging.INFO,
                self._metrics.log_str(),
                10,
            )

    def _send_kv_sync_impl(
        self,
        seq_request_meta: AIBrixOffloadingConnectorRequestMetadata,
    ) -> None:
        logger.debug("_send_kv_sync_impl: %s", seq_request_meta)
        seq_request_id = seq_request_meta.req_id
        seq_context_len = seq_request_meta.context_len
        seq_cached_meta = self._meta_cache[seq_request_id]
        seq_all_tokens = seq_cached_meta.get_context_tokens_view()
        assert seq_all_tokens is not None, "seq_all_tokens is None"
        prompt_len = seq_request_meta.prompt_len
        query_len = seq_request_meta.query_len

        # align to block boundary
        aligned_context_len = round_down(
            seq_context_len, self.cache_block_ntokens
        )
        actual_query_len = seq_context_len + query_len - aligned_context_len
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

            exists_status = self.cache.exists(chunk_prefix, chunk_tokens)
            if exists_status.is_ok():
                num_existing_tokens = exists_status.value
                logger.info(
                    "Request[id=%s] send(%d) encounters %d existing tokens",
                    seq_request_id,
                    length,
                    num_existing_tokens,
                )
                if chunk_size - num_existing_tokens < self.cache_block_ntokens:
                    continue
                else:
                    # partially exists
                    offset += num_existing_tokens
                    length -= num_existing_tokens
                    new_chunk_prefix_len = (
                        len(chunk_prefix) + num_existing_tokens
                    )
                    chunk_prefix = all[:new_chunk_prefix_len]
                    chunk_tokens = all[
                        new_chunk_prefix_len : new_chunk_prefix_len + length
                    ]

            # allocate space for KV caches
            status = self.cache.allocate_for(chunk_prefix, chunk_tokens)
            if not status.is_ok():
                log_every_n_seconds(
                    logger,
                    logging.ERROR,
                    "Failed to allocate : %s",
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

            # put KV caches to offloading service
            status = self.cache.put(chunk_prefix, chunk_tokens[:length], handle)
            if not status.is_ok():
                # TODO: notify other ranks in the group to stop sending
                # if this is a fatal error
                log_every_n_seconds(
                    logger,
                    logging.ERROR,
                    "Failed to put to offloading service: %s",
                    3,
                    str(status),
                )
                break

            put_ntokens = status.get()
            total_sent += put_ntokens
            if put_ntokens != length:
                break

        # Track saved tokens for scheduler reporting via
        # build_connector_worker_meta
        if total_sent > 0:
            prev = self._newly_saved_tokens.get(seq_request_id, 0)
            self._newly_saved_tokens[seq_request_id] = prev + total_sent

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


class AIBrixOffloadingConnector(KVConnectorBase_V1):
    """AIBrixOffloadingConnector is a KVConnector that offloads KV caches
    to the kv cache offloading service.
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

        self.connector_scheduler: Optional[
            AIBrixOffloadingConnectorScheduler
        ] = None

        self.connector_worker: Optional[AIBrixOffloadingConnectorWorker] = None
        if role == KVConnectorRole.SCHEDULER:
            self.connector_scheduler = AIBrixOffloadingConnectorScheduler(
                config
            )
        elif role == KVConnectorRole.WORKER:
            self.connector_worker = AIBrixOffloadingConnectorWorker(config)

    # ==============================
    # Worker-side methods
    # ==============================

    @delegate_to("connector_worker")
    def register_kv_caches(self, kv_caches: dict[str, torch.Tensor]):
        """
        Initialize with the KV caches. Useful for pre-registering the
        KV Caches in the KVConnector (e.g. for NIXL).

        Args: kv_caches:
            dictionary of layer names, kv cache
        """
        pass

    def start_load_kv_before_update(self, **kwargs) -> dict[str, int]:
        """
        Start loading the KV cache from the connector to vLLM's paged
        KV buffer before gpu runner updating its states.

        Args:
            **kwargs: additional arguments for the load operation

        Returns:
            dict[str, int]: a dictionary of request ids and the number of
            tokens loaded for each request.
        """
        assert self.connector_worker is not None
        assert isinstance(
            self._connector_metadata,
            AIBrixOffloadingConnectorMetadata,
        )
        return self.connector_worker.start_load_kv_before_update(
            self._connector_metadata
        )

    def start_load_kv(
        self, forward_context: "ForwardContext", **kwargs
    ) -> None:
        """
        Start loading the KV cache from the connector to vLLM's paged
        KV buffer. This is called from the forward context before the
        forward pass to enable async loading during model execution.

        Args:
            forward_context (ForwardContext): the forward context.
            **kwargs: additional arguments for the load operation

        Note:
            The number of elements in kv_caches and layer_names should be
            the same.

        """
        return

    def wait_for_layer_load(self, layer_name: str) -> None:
        """
        Block until the KV for a specific layer is loaded into vLLM's
        paged buffer. This is called from within attention layer to ensure
        async copying from start_load_kv is complete.

        This interface will be useful for layer-by-layer pipelining.

        Args:
            layer_name: the name of that layer
        """
        """Not supported yet"""
        pass

    def save_kv_layer(
        self,
        layer_name: str,
        kv_layer: torch.Tensor,
        attn_metadata: "AttentionMetadata",
        **kwargs,
    ) -> None:
        """
        Start saving a layer of KV cache from vLLM's paged buffer
        to the connector. This is called from within attention layer to
        enable async copying during execution.

        Args:
            layer_name (str): the name of the layer.
            kv_layer (torch.Tensor): the paged KV buffer of the current
                layer in vLLM.
            attn_metadata (AttentionMetadata): the attention metadata.
            **kwargs: additional arguments for the save operation.
        """
        """Not supported yet"""
        pass

    def wait_for_save(self) -> None:
        """
        Block until all the save operations is done. This is called
        as the forward context exits to ensure that the async saving
        from save_kv_layer is complete before finishing the forward.

        This prevents overwrites of paged KV buffer before saving done.
        """
        assert self.connector_worker is not None
        assert isinstance(
            self._connector_metadata,
            AIBrixOffloadingConnectorMetadata,
        )
        self.connector_worker.wait_for_save(self._connector_metadata)

    def get_finished(
        self, finished_req_ids: set[str]
    ) -> tuple[Optional[set[str]], Optional[set[str | tuple[str, int]]]]:
        """
        Notifies worker-side connector ids of requests that have
        finished generating tokens.
        """
        return None, None

    def get_block_ids_with_load_errors(self) -> set[int]:
        """Report vLLM block_ids that the scheduler promised as cached
        but the worker failed to load (e.g., evicted from L1 between the
        scheduler tracker update and the worker acquire call).

        vLLM invalidates these blocks, rolls back num_computed_tokens,
        and re-schedules the affected tokens for recomputation
        (when kv_load_failure_policy="recompute", the default).
        This keeps the scheduler-side _cached_block_hashes tracker
        eventually-consistent with the actual L1 cache state without
        requiring a tight eviction-notification channel.
        """
        if self.connector_worker is None:
            return set()
        result = set(self.connector_worker._failed_load_block_ids)
        self.connector_worker._failed_load_block_ids.clear()
        return result

    def build_connector_worker_meta(
        self,
    ) -> Optional[KVConnectorWorkerMetadata]:
        """Report L1 cache state changes back to the scheduler:

        - saved_tokens: tokens newly written to L1, so the scheduler can
          mark the corresponding block hashes as cached.
        - failed_load_tokens: tokens the scheduler promised as cached
          but the worker could not load, so the scheduler can remove
          those stale hashes from _cached_block_hashes.
        """
        if self.connector_worker is None:
            return None
        saved = self.connector_worker._newly_saved_tokens
        failed = self.connector_worker._newly_failed_load_tokens
        if not saved and not failed:
            return None
        meta = AIBrixWorkerMeta(
            saved_tokens=dict(saved),
            failed_load_tokens=dict(failed),
        )
        saved.clear()
        failed.clear()
        return meta

    def update_connector_output(self, connector_output) -> None:
        """Process worker-side output to update scheduler cache tracker.

        Called by vLLM scheduler after each engine step. Extracts
        kv_connector_worker_meta (AIBrixWorkerMeta) and forwards it to
        the scheduler's receive_connector_worker_meta.
        """
        worker_meta = getattr(
            connector_output, "kv_connector_worker_meta", None
        )
        if self.connector_scheduler is not None and worker_meta is not None:
            self.connector_scheduler.receive_connector_worker_meta(worker_meta)

    # ==============================
    # Scheduler-side methods
    # ==============================

    def get_num_new_matched_tokens(
        self,
        request: "Request",
        num_computed_tokens: int,
    ) -> tuple[int, bool]:
        """
        Get number of new tokens that can be loaded from the
        external KV cache beyond the num_computed_tokens.

        Uses the scheduler-side cache tracker (populated via
        build_connector_worker_meta) to determine how many consecutive
        blocks starting from num_computed_tokens are available in the
        external L1 cache.
        """
        if self.connector_scheduler is None:
            return 0, False

        scheduler = self.connector_scheduler

        block_hashes = getattr(request, "block_hashes", None)
        if not block_hashes:
            return 0, False

        # Save block_hashes for this request so we can map worker
        # reports (req_id -> num_tokens_saved) to vLLM block hashes
        req_id = getattr(request, "request_id", None)
        if req_id is not None:
            scheduler._request_block_hashes[req_id] = list(block_hashes)

        block_size = scheduler.engine_block_ntokens

        # Count how many consecutive blocks starting from
        # num_computed_tokens are available in the external cache
        start_block = num_computed_tokens // block_size
        num_matched_blocks = 0

        for i in range(start_block, len(block_hashes)):
            if block_hashes[i] not in scheduler._cached_block_hashes:
                break
            num_matched_blocks += 1

        num_matched_tokens = num_matched_blocks * block_size

        # Don't exceed request length
        max_matchable = request.num_tokens - num_computed_tokens
        num_matched_tokens = min(num_matched_tokens, max_matchable)

        return num_matched_tokens, False

    def update_state_after_alloc(
        self,
        request: "Request",
        blocks: "KVCacheBlocks",
        num_external_tokens: int,
    ):
        """
        Update KVConnector state after block allocation.

        Stores num_external_tokens so build_connector_meta can pass
        it to the worker via load_len (instructing the worker exactly
        how many tokens to load from the external L1 cache).
        """
        if self.connector_scheduler is None:
            return
        if num_external_tokens > 0:
            self.connector_scheduler._request_external_tokens[
                request.request_id
            ] = num_external_tokens

    @delegate_to("connector_scheduler")
    def build_connector_meta(
        self, scheduler_output: "SchedulerOutput"
    ) -> KVConnectorMetadata:
        """
        Build the connector metadata for this step.

        This function should NOT modify fields in the scheduler_output.
        Also, calling this function will reset the state of the connector.

        Args:
            scheduler_output (SchedulerOutput): the scheduler output object.
        """
        pass

    @delegate_to("connector_scheduler")
    def request_finished(  # type: ignore
        self,
        request: "Request",
        block_ids: list[int],
    ) -> tuple[bool, Optional[dict[str, Any]]]:
        """
        Called when a request has finished, before its blocks are freed.

        Returns:
            True if the request is being saved/sent asynchronously and blocks
            should not be freed until the request_id is returned from
            get_finished().
            Optional KVTransferParams to be included in the request outputs
            returned by the engine.
        """
        pass

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
import logging
import time
from typing import TYPE_CHECKING, Any, Optional

import torch
import torch.distributed as dist

# from vllm.v1.attention.backends.flashinfer import FlashInferBackend
# from vllm.v1.attention.backends.flex_attention import FlexAttentionBackend
# from vllm.v1.attention.backends.triton_attn import TritonAttentionBackend
from vllm.distributed.kv_transfer.kv_connector.v1.base import (
    KVConnectorBase_V1,
    KVConnectorMetadata,
    KVConnectorRole,
)
from vllm.utils.math_utils import round_down
from vllm.v1.attention.backends.flash_attn import FlashAttentionBackend
from vllm.v1.attention.backends.flashinfer import FlashInferBackend

from aibrix_kvcache import KVCacheBlockLayout, KVCacheHandle
from aibrix_kvcache._custom_ops import (
    reshape_and_cache_multi_layer,
    reshape_and_offload_multi_layer,
)
from aibrix_kvcache.common.absl_logging import (
    getLogger,
    log_every_n_seconds,
    log_if,
)
from aibrix_kvcache.profiling import tag_wrapper

from .aibrix_offloading_connector_type1 import (
    AIBrixOffloadingConnectorMetadata,
    AIBrixOffloadingConnectorRequestMetadata,
    AIBrixOffloadingConnectorRequestState,
    AIBrixWorkerMeta,
    delegate_to,
)

if TYPE_CHECKING:
    from vllm.distributed.kv_transfer.kv_connector.v1.base import (
        KVConnectorWorkerMetadata,
    )
from .aibrix_offloading_connector_type1 import (
    AIBrixOffloadingConnectorScheduler as AIBrixOffloadingConnectorSchedulerType1,  # noqa: E501
)
from .aibrix_offloading_connector_type1 import (
    AIBrixOffloadingConnectorWorker as AIBrixOffloadingConnectorWorkerType1,
)

if TYPE_CHECKING:
    from vllm.config import VllmConfig
    from vllm.forward_context import ForwardContext
    from vllm.v1.attention.backend import AttentionMetadata
    from vllm.v1.core.kv_cache_manager import KVCacheBlocks
    from vllm.v1.core.sched.output import SchedulerOutput
    from vllm.v1.kv_cache_interface import KVCacheConfig
    from vllm.v1.request import Request

logger = getLogger(__name__)

OFFLOADING_CONNECTOR_SKIP_THRESHOLD = 8
OFFLOADING_CONNECTOR_SUPPORTED_ATTN_BACKENDS = {
    FlashAttentionBackend.get_name(): KVCacheBlockLayout.LCND,
    FlashInferBackend.get_name(): KVCacheBlockLayout.LCND,
}


class AIBrixOffloadingConnectorScheduler(
    AIBrixOffloadingConnectorSchedulerType1
):
    def __init__(self, config: "VllmConfig"):
        super().__init__(config)


class AIBrixOffloadingConnectorWorker(AIBrixOffloadingConnectorWorkerType1):
    """AIBrixOffloadingConnectorWorker carries out the data-plane operations."""

    def __init__(self, config: "VllmConfig"):
        self._init_worker(
            config,
            max_num_batched_tokens=config.scheduler_config.max_num_batched_tokens,
        )
        # create streams on current device that are dedicated for sends and
        # recvs
        self._send_stream = torch.cuda.Stream()
        self._recv_stream = torch.cuda.Stream()
        self._send_slot_mapping = torch.empty(
            config.scheduler_config.max_num_batched_tokens,
            dtype=torch.long,
            device="cuda",
        )
        self._recv_slot_mapping = torch.empty(
            config.scheduler_config.max_num_batched_tokens,
            dtype=torch.long,
            device="cuda",
        )

        self._allocated_kvcache_handles: dict[str, KVCacheHandle] = {}
        self._acquired_kvcache_handles: dict[str, KVCacheHandle] = {}
        self._send_lengths: dict[str, tuple[int, int]] = {}

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

    def _release_allocated_kvcache_handles(self):
        for seq_request_id in self._allocated_kvcache_handles:
            self._allocated_kvcache_handles[seq_request_id].release()
        self._allocated_kvcache_handles.clear()

    def _release_acquired_kvcache_handles(self):
        for seq_request_id in self._acquired_kvcache_handles:
            self._acquired_kvcache_handles[seq_request_id].release()
        self._acquired_kvcache_handles.clear()

    @tag_wrapper(
        {
            "connector": "AIBrixOffloadingConnectorV1Type2",
            "func": "start_load_kv_before_update",
        }
    )
    def start_load_kv_before_update(
        self,
        metadata: AIBrixOffloadingConnectorMetadata,
    ) -> dict[str, int]:
        self._update_meta_cache(metadata)

        stats = {}
        if self.kv_group is not None:
            for seq_request_id, seq_request_meta in metadata.items():
                num_fetched_tokens = self._recv_kv_impl(seq_request_meta)
                if num_fetched_tokens > 0:
                    stats[seq_request_id] = num_fetched_tokens

            if len(stats) > 0:
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

        for seq_request_id, seq_request_meta in metadata.items():
            if self.kv_group is None:
                num_fetched_tokens = self._recv_kv_impl(seq_request_meta)
            else:
                num_fetched_tokens = stats.get(seq_request_id, 0)

            # Scheduler already set context_len/query_len correctly
            # (via get_num_new_matched_tokens). Don't double-adjust.
            if num_fetched_tokens > 0:
                stats[seq_request_id] = num_fetched_tokens
            self._send_lengths[seq_request_id] = (
                seq_request_meta.context_len,
                seq_request_meta.query_len,
            )

            seq_request_meta.state = (
                AIBrixOffloadingConnectorRequestState.WAITING_FOR_SEND
            )

            if seq_request_id not in self._acquired_kvcache_handles:
                continue

        # load the first layer
        layer_name = list(self.no_compile_layers.keys())[0]
        self._start_load_kv(metadata, layer_name)

        return stats

    def _recv_kv_impl(
        self,
        seq_request_meta: AIBrixOffloadingConnectorRequestMetadata,
    ) -> int:
        logger.debug("_recv_kv_impl: %s", seq_request_meta)
        seq_request_id = seq_request_meta.req_id
        seq_cached_meta = self._meta_cache[seq_request_id]
        seq_all_tokens = seq_cached_meta.get_context_tokens_view()
        assert seq_all_tokens is not None, "seq_all_tokens is None"

        # Use load_len (tokens promised to scheduler as external hits)
        # instead of query_len (tokens to compute). See Type1 comment.
        load_len = seq_request_meta.load_len
        seq_context_len = seq_request_meta.context_len
        prompt_len = seq_request_meta.prompt_len

        if load_len <= 0:
            return 0

        local_context_len = seq_context_len - load_len
        assert local_context_len >= 0, (
            f"local_context_len={local_context_len} "
            f"(context_len={seq_context_len}, load_len={load_len})"
        )

        aligned_context_len = round_down(
            local_context_len, self.cache_block_ntokens
        )
        shift_len = local_context_len - aligned_context_len
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
            start = time.perf_counter()

        seq_recv_len = 0

        # get KV caches from offloading service
        status = self.cache.acquire(prefix, tokens)

        if not status.is_ok():
            if not status.is_not_found():
                log_every_n_seconds(
                    logger,
                    logging.ERROR,
                    "Failed to get from offloading service: %s",
                    3,
                    str(status),
                )
            if self._metrics.time_measurement_enabled:
                end = time.perf_counter()
                lat_ms = (end - start) * 1000
                self._metrics._recv_metrics.add(
                    aligned_context_len, aligned_query_len, 0, lat_ms
                )
            return 0

        num_fetched_tokens, handle = status.value
        self._acquired_kvcache_handles[seq_request_id] = handle

        # update recv_len
        seq_recv_len += num_fetched_tokens - shift_len

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

        # Detect partial load (eviction race) — see Type1 comment
        if seq_recv_len < aligned_query_len:
            # Report failed tokens to scheduler so it can remove the
            # corresponding block hashes from its tracker.
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
            end = time.perf_counter()
            lat_ms = (end - start) * 1000
            self._metrics._recv_metrics.add(
                aligned_context_len, aligned_query_len, seq_recv_len, lat_ms
            )

        return seq_recv_len

    def _start_load_kv(
        self,
        metadata: AIBrixOffloadingConnectorMetadata,
        layer_name: str,
    ):
        lid = self.layer_name_idx_mapping[layer_name]
        layer_tensors: list[torch.Tensor] = []
        slot_mapping_offset = 0

        for seq_request_id in self._acquired_kvcache_handles:
            if self._acquired_kvcache_handles[seq_request_id] is None:
                continue
            handle = self._acquired_kvcache_handles[seq_request_id]
            seq_cached_meta = self._meta_cache[seq_request_id]
            seq_context_len = metadata[seq_request_id].context_len
            aligned_context_len = round_down(
                seq_context_len, self.cache_block_ntokens
            )

            kv_blocks = handle.to_tensors()
            num_fetched_tokens = len(kv_blocks) * self.cache_block_ntokens

            offset = aligned_context_len
            length = num_fetched_tokens

            self._recv_slot_mapping[
                slot_mapping_offset : slot_mapping_offset + length
            ].copy_(
                seq_cached_meta.context_slot_mapping[offset : offset + length]  # type: ignore[index]
            )
            slot_mapping_offset += length
            # We are using LCND layout, so the tensors can be split by layers
            layer_tensors.extend([t[lid : lid + 1] for t in kv_blocks])

        if slot_mapping_offset == 0:
            return

        with torch.cuda.stream(self._recv_stream):
            reshape_and_cache_multi_layer(
                layer_tensors,
                [self.layers_kv_caches[lid]],  # type: ignore[index]
                self._recv_slot_mapping[:slot_mapping_offset],
                self.engine_block_ntokens,
                "auto",
                [self.k_scales[lid]],  # type: ignore[index]
                [self.v_scales[lid]],  # type: ignore[index]
                self.block_layout.name,
                self.kv_layout_blocks_first,
            )

    def wait_for_layer_load(
        self, metadata: AIBrixOffloadingConnectorMetadata, layer_name: str
    ) -> None:
        # wait until kv layer is loaded
        self._recv_stream.synchronize()
        curr_layer_idx = self.layer_name_idx_mapping[layer_name]
        if curr_layer_idx == self.num_layers - 1:
            # release acquired handles once we finished onloading the last
            # layer
            self._release_acquired_kvcache_handles()
        else:
            # start to load next layer
            next_layer_idx = curr_layer_idx + 1
            next_layer = list(self.no_compile_layers.keys())[next_layer_idx]
            self._start_load_kv(metadata, next_layer)

    def save_kv_layer(
        self,
        metadata: AIBrixOffloadingConnectorMetadata,
        layer_name: str,
        kv_layer: torch.Tensor,
        attn_metadata: "AttentionMetadata",
    ) -> None:
        is_first_layer = self.layer_name_idx_mapping[layer_name] == 0
        lid = self.layer_name_idx_mapping[layer_name]
        layer_tensors: list[torch.Tensor] = []
        slot_mapping_offset = 0

        if is_first_layer:
            for seq_req_id in self._send_lengths:
                seq_context_len, seq_query_len = self._send_lengths[seq_req_id]
                if seq_query_len == 0:
                    continue
                # allocate staging buffers that can hold kvcache for all layers
                seq_request_meta = metadata[seq_req_id]
                handle = self._allocate_for_request(seq_request_meta)
                if handle is None:
                    continue
                else:
                    self._allocated_kvcache_handles[seq_req_id] = handle

        for seq_req_id, _ in self._allocated_kvcache_handles.items():
            if (
                seq_req_id not in self._allocated_kvcache_handles
                or self._allocated_kvcache_handles[seq_req_id] is None
            ):
                return

            seq_cached_meta = self._meta_cache[seq_req_id]
            seq_context_len, _ = self._send_lengths[seq_req_id]
            aligned_context_len = round_down(
                seq_context_len, self.cache_block_ntokens
            )

            seq_allocated_handle = self._allocated_kvcache_handles[seq_req_id]
            seq_allocated_tensors = seq_allocated_handle.to_tensors()
            seq_num_tokens = (
                len(seq_allocated_tensors) * self.cache_block_ntokens
            )
            self._send_slot_mapping[
                slot_mapping_offset : slot_mapping_offset + seq_num_tokens
            ].copy_(
                seq_cached_meta.context_slot_mapping[
                    aligned_context_len : aligned_context_len
                    + seq_num_tokens  # type: ignore[index]
                ]
            )
            slot_mapping_offset += seq_num_tokens

            # We are using LCND layout, so the tensors can be split by layers
            layer_tensors.extend(
                [t[lid : lid + 1] for t in seq_allocated_tensors]
            )

        if slot_mapping_offset == 0:
            return

        # wait for compute on current stream
        curr_stream = torch.cuda.current_stream()
        self._send_stream.wait_stream(curr_stream)

        # use send stream to carry out async copy from HBM to DRAM
        with torch.cuda.stream(self._send_stream):
            reshape_and_offload_multi_layer(
                layer_tensors,
                [kv_layer],
                self._send_slot_mapping[:slot_mapping_offset],
                self.engine_block_ntokens,
                "auto",
                [self.k_scales[lid]],  # type: ignore[index]
                [self.v_scales[lid]],  # type: ignore[index]
                self.block_layout.name,
                self.kv_layout_blocks_first,
            )

    def _allocate_for_request(
        self,
        seq_request_meta: AIBrixOffloadingConnectorRequestMetadata,
    ) -> Optional[KVCacheHandle]:
        seq_request_id = seq_request_meta.req_id
        seq_context_len, query_len = self._send_lengths[seq_request_id]
        seq_cached_meta = self._meta_cache[seq_request_id]
        seq_all_tokens = seq_cached_meta.get_context_tokens_view()
        assert seq_all_tokens is not None, "seq_all_tokens is None"
        prompt_len = seq_request_meta.prompt_len

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

        if aligned_query_len < self.cache_block_ntokens:
            return None

        prefix = seq_all_tokens[:aligned_context_len]
        tokens = seq_all_tokens[
            aligned_context_len : aligned_context_len + aligned_query_len
        ]
        status = self.cache.allocate_for(prefix, tokens)
        if not status.is_ok():
            log_every_n_seconds(
                logger, logging.ERROR, "Failed to allocate : %s", 3, str(status)
            )
            return None
        return status.get()

    @tag_wrapper(
        {
            "connector": "AIBrixOffloadingConnectorV1Type2",
            "func": "wait_for_save",
        }
    )
    def wait_for_save(
        self,
        metadata: AIBrixOffloadingConnectorMetadata,
    ) -> None:
        # wait for send stream to finish
        self._send_stream.synchronize()

        for seq_request_id in self._send_lengths:
            seq_context_len, seq_query_len = self._send_lengths[seq_request_id]
            if seq_query_len == 0:
                continue
            if (
                seq_request_id not in self._allocated_kvcache_handles
                or self._allocated_kvcache_handles[seq_request_id] is None
            ):
                continue
            seq_request_meta = metadata[seq_request_id]
            self._send_kv_impl(seq_request_meta)

        # release all allocated handles
        self._release_allocated_kvcache_handles()
        self._send_lengths.clear()

        if self._metrics.time_measurement_enabled:
            log_every_n_seconds(
                logger,
                logging.INFO,
                self._metrics.log_str(),
                10,
            )

    def _send_kv_impl(
        self,
        seq_request_meta: AIBrixOffloadingConnectorRequestMetadata,
    ) -> None:
        logger.debug("_send_kv_impl: %s", seq_request_meta)
        seq_request_id = seq_request_meta.req_id
        seq_context_len, query_len = self._send_lengths[seq_request_id]
        seq_cached_meta = self._meta_cache[seq_request_id]
        seq_all_tokens = seq_cached_meta.get_context_tokens_view()
        assert seq_all_tokens is not None, "seq_all_tokens is None"
        seq_allocated_handle = self._allocated_kvcache_handles.pop(
            seq_request_id
        )
        seq_allocated_tensors = seq_allocated_handle.to_tensors()
        prompt_len = seq_request_meta.prompt_len
        query_len = min(
            query_len,
            len(seq_allocated_tensors) * self.cache_block_ntokens,
        )

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
            start = time.perf_counter()

        total_sent = 0
        length = aligned_query_len

        # put KV caches to offloading service
        status = self.cache.put(prefix, tokens, seq_allocated_handle)
        if not status.is_ok():
            log_every_n_seconds(
                logger,
                logging.ERROR,
                "Failed to put to offloading service: %s",
                3,
                str(status),
            )
        else:
            total_sent += length

            # Track saved tokens for scheduler reporting via
            # build_connector_worker_meta. Use per-block length (not
            # cumulative total_sent) so the accounting stays correct
            # if chunking is ever added to this path.
            if length > 0:
                prev = self._newly_saved_tokens.get(seq_request_id, 0)
                self._newly_saved_tokens[seq_request_id] = prev + length

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
            end = time.perf_counter()
            lat_ms = (end - start) * 1000
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
        kv_cache_config: Optional["KVCacheConfig"] = None,
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
        pass

    def wait_for_layer_load(self, layer_name: str) -> None:
        """
        Block until the KV for a specific layer is loaded into vLLM's
        paged buffer. This is called from within attention layer to ensure
        async copying from start_load_kv is complete.

        This interface will be useful for layer-by-layer pipelining.

        Args:
            layer_name: the name of that layer
        """
        assert self.connector_worker is not None
        assert isinstance(
            self._connector_metadata,
            AIBrixOffloadingConnectorMetadata,
        )
        return self.connector_worker.wait_for_layer_load(
            self._connector_metadata, layer_name
        )

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
        assert self.connector_worker is not None
        assert isinstance(
            self._connector_metadata,
            AIBrixOffloadingConnectorMetadata,
        )
        self.connector_worker.save_kv_layer(
            self._connector_metadata, layer_name, kv_layer, attn_metadata
        )

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

    def get_block_ids_with_load_errors(self) -> set[int]:
        """Report vLLM block_ids the worker failed to load (eviction race).
        See AIBrixOffloadingConnector (type1) for full doc.
        """
        if self.connector_worker is None:
            return set()
        result = set(self.connector_worker._failed_load_block_ids)
        self.connector_worker._failed_load_block_ids.clear()
        return result

    def get_finished(
        self, finished_req_ids: set[str]
    ) -> tuple[Optional[set[str]], Optional[set[str | tuple[str, int]]]]:
        """
        Notifies worker-side connector ids of requests that have
        finished generating tokens.
        """
        return None, None

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

    def build_connector_worker_meta(
        self,
    ) -> Optional["KVConnectorWorkerMetadata"]:
        """Report tokens saved to L1 cache back to the scheduler."""
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
        """Process worker-side output to update scheduler cache tracker."""
        worker_meta = getattr(
            connector_output, "kv_connector_worker_meta", None
        )
        if self.connector_scheduler is not None and worker_meta is not None:
            self.connector_scheduler.receive_connector_worker_meta(worker_meta)

    def update_state_after_alloc(
        self,
        request: "Request",
        blocks: "KVCacheBlocks",
        num_external_tokens: int,
    ):
        """Store num_external_tokens so build_connector_meta can pass it
        to the worker via load_len (instructing the worker exactly how
        many tokens to load from the external L1 cache)."""
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

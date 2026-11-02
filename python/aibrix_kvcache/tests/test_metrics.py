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


from aibrix_kvcache.metrics import KVCacheMetrics, MetricRecorder
from aibrix_kvcache.status import Status

BLOCK_NTOKENS = 16


def _metrics() -> KVCacheMetrics:
    return KVCacheMetrics(
        block_ntokens=BLOCK_NTOKENS,
        capacity_nbytes=0,
        enable_l1=True,
        enable_l2=True,
    )


def test_l1_and_l2_get_results_are_scaled_by_block_size():
    metrics = _metrics()
    # L1 returns one memory region per block, L2 returns a number of blocks.
    metrics.l1.record(
        MetricRecorder.OP.GET, 0, 64, Status.ok([object()] * 4), 0
    )
    metrics.l2.record(MetricRecorder.OP.GET, 0, 64, Status.ok(4), 0)

    for level in (metrics.l1, metrics.l2):
        assert level.num_tokens_hit == 4 * BLOCK_NTOKENS
        assert level.num_tokens_miss == 0
        assert level.get_metrics.num_fetched_tokens == [4 * BLOCK_NTOKENS]


def test_mgr_get_and_acquire_results_are_counted_as_tokens():
    metrics = _metrics()
    # The manager's get() returns a number of tokens.
    metrics.mgr.record(MetricRecorder.OP.GET, 0, 64, Status.ok(64), 0)
    # acquire() returns (number of tokens, handle).
    metrics.mgr.record(
        MetricRecorder.OP.ACQUIRE, 0, 64, Status.ok((32, object())), 0
    )

    assert metrics.mgr.num_tokens_hit == 64 + 32
    assert metrics.mgr.num_tokens_miss == 32
    assert metrics.mgr.get_metrics.num_fetched_tokens == [64]
    assert metrics.mgr.acquire_metrics.num_fetched_tokens == [32]

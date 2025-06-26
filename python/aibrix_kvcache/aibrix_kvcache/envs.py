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

import os
from typing import TYPE_CHECKING, Any, Callable, Dict, List, Tuple

if TYPE_CHECKING:
    # (minimum number of blocks, ratio)
    #
    # If both L1 and L2 caches are enabled, we will issue a second get request
    # (i.e., double get) to L2 cache to fetch the missing cache blocks if:
    # 1. The number of missing blocks is greater than or equal to
    #    AIBRIX_KV_CACHE_OL_DOUBLE_GET_THRESHOLD[0]
    # 2. The ratio of missing blocks to the total number of blocks is greater
    #    than or equal to AIBRIX_KV_CACHE_OL_DOUBLE_GET_THRESHOLD[1]
    # Otherwise, we will not issue a second get request to L2 cache.
    #
    # Note that the second rule is only applicable if the ratio threshold is
    # set, otherwise we only consider the first rule. For example, if
    # AIBRIX_KV_CACHE_OL_DOUBLE_GET_THRESHOLD is set to "4", we will ignore the
    # second rule.
    AIBRIX_KV_CACHE_OL_DOUBLE_GET_THRESHOLD: Tuple[int, float] = (4, 0.1)
    AIBRIX_KV_CACHE_OL_CHUNK_SIZE: int = 512
    AIBRIX_KV_CACHE_OL_TIME_MEASUREMENT_ENABLED: bool = True
    AIBRIX_KV_CACHE_OL_BREAKDOWN_MEASUREMENT_ENABLED: bool = True
    # Whether to validate the token in L2 cache. Defaults to False.
    #
    # Note:
    # If disabled, we will use a tight memory layout for both L1 and L2
    # cache. I.e., we will not pack tokens in the cache entry.
    AIBRIX_KV_CACHE_OL_TOKEN_VALIDATION_ENABLED: bool = False

    AIBRIX_KV_CACHE_OL_L1_CACHE_ENABLED: bool = True
    AIBRIX_KV_CACHE_OL_L1_CACHE_EVICTION_POLICY: str = "S3FIFO"
    AIBRIX_KV_CACHE_OL_L1_CACHE_CAPACITY_GB: float = 10
    AIBRIX_KV_CACHE_OL_DEVICE: str = "cpu"

    # S3FIFO Env Vars
    AIBRIX_KV_CACHE_OL_S3FIFO_SMALL_TO_MAIN_PROMO_THRESHOLD: int = 1
    AIBRIX_KV_CACHE_OL_S3FIFO_SMALL_FIFO_CAPACITY_RATIO: float = 0.3

    AIBRIX_KV_CACHE_OL_L2_CACHE_BACKEND: str = ""
    AIBRIX_KV_CACHE_OL_L2_CACHE_NAMESPACE: str = "dummy"
    AIBRIX_KV_CACHE_OL_L2_CACHE_COMPRESSION: str = ""
    AIBRIX_KV_CACHE_OL_L2_CACHE_OP_BATCH: int = 32
    AIBRIX_KV_CACHE_OL_L2_CACHE_PER_TOKEN_TIMEOUT_MS: int = 20
    # L2 cache placement policy. Defaults to "SIMPLE".
    # Only applicable if using meta service.
    AIBRIX_KV_CACHE_OL_L2_CACHE_PLACEMENT_POLICY: str = "SIMPLE"

    # L2 cache key builder. Defaults to raw.
    AIBRIX_KV_CACHE_OL_L2_CACHE_KEY_BUILDER: str = "RAW"

    # If meta service backend is not set, L2 cache backend will use direct
    # mode to access the given cache server.
    # Otherwise, we will get membership information from meta service
    # and construct the L2 cache cluster.
    AIBRIX_KV_CACHE_OL_META_SERVICE_BACKEND: str = ""
    # L2 cache meta service refresh interval in seconds. Defaults to 30.
    AIBRIX_KV_CACHE_OL_META_SERVICE_REFRESH_INTERVAL_S: int = 30
    AIBRIX_KV_CACHE_OL_META_SERVICE_URL: str = ""
    AIBRIX_KV_CACHE_OL_META_SERVICE_CLUSTER_META_KEY: str = ""

    # Ingestion type, only applicable if L1 cache is enabled. Defaults to "HOT".
    # "ALL": all newly put data in L1 cache will be put onto L2 cache.
    # "HOT": hot data in L1 cache will be put onto L2 cache.
    # "EVICTED": evicted data in L1 cache will be put onto L2 cache.
    AIBRIX_KV_CACHE_OL_L2_CACHE_INGESTION_TYPE: str = "HOT"
    # Max number of inflight writes to L2 cache in terms of tokens.
    # Defaults to 0.
    # If the number of inflight writes reaches the limit, new writes
    # will be discarded. Set it to zero to use synchronous writes.
    AIBRIX_KV_CACHE_OL_L2_CACHE_INGESTION_MAX_INFLIGHT_TOKENS: int = 0

    AIBRIX_KV_CACHE_OL_L2_CACHE_NUM_ASYNC_WORKERS: int = 8

    # Mock Connector
    AIBRIX_KV_CACHE_OL_MOCK_USE_RDMA: bool = False
    AIBRIX_KV_CACHE_OL_MOCK_USE_MPUT_MGET: bool = False

    # RocksDB Env Vars
    AIBRIX_KV_CACHE_OL_ROCKSDB_ROOT: str = os.path.expanduser(
        os.path.join(os.path.expanduser("~"), ".kv_cache_ol", "rocksdb")
    )
    AIBRIX_KV_CACHE_OL_ROCKSDB_TTL_S: int = 600
    AIBRIX_KV_CACHE_OL_ROCKSDB_WRITE_BUFFER_SIZE: int = 64 * 1024 * 1024
    AIBRIX_KV_CACHE_OL_ROCKSDB_TARGET_FILE_SIZE_BASE: int = 64 * 1024 * 1024
    AIBRIX_KV_CACHE_OL_ROCKSDB_MAX_WRITE_BUFFER_NUMBER: int = 3
    AIBRIX_KV_CACHE_OL_ROCKSDB_MAX_TOTAL_WAL_SIZE: int = 128 * 1024 * 1024
    AIBRIX_KV_CACHE_OL_ROCKSDB_MAX_BACKGROUND_JOBS: int = 8

    # InfiniStore Env Vars
    AIBRIX_KV_CACHE_OL_INFINISTORE_HOST_ADDR: str = "127.0.0.1"
    AIBRIX_KV_CACHE_OL_INFINISTORE_SERVICE_PORT: int = 12345
    AIBRIX_KV_CACHE_OL_INFINISTORE_CONNECTION_TYPE: str = "RDMA"
    AIBRIX_KV_CACHE_OL_INFINISTORE_IB_PORT: int = 1
    AIBRIX_KV_CACHE_OL_INFINISTORE_LINK_TYPE: str = "Ethernet"
    # Since 0.2.42, InfiniStore supports RDMA GID index in client config,
    # users can specify the GID index of each device in this format:
    # "mlx5_0:gid0,mlx5_1:gid1,mlx5_2:gid2"
    AIBRIX_KV_CACHE_OL_INFINISTORE_VISIBLE_DEV_LIST: List[str] = []
    AIBRIX_KV_CACHE_OL_INFINISTORE_USE_GDR: bool = True

    # HPKV Env Vars
    AIBRIX_KV_CACHE_OL_HPKV_REMOTE_ADDR: str = "127.0.0.1"
    AIBRIX_KV_CACHE_OL_HPKV_REMOTE_PORT: int = 12346
    AIBRIX_KV_CACHE_OL_HPKV_LOCAL_ADDR: str = "127.0.0.1"
    AIBRIX_KV_CACHE_OL_HPKV_LOCAL_PORT: int = 12345
    AIBRIX_KV_CACHE_OL_HPKV_USE_GDR: bool = True

    # RDMA Auto-Detection Env Vars
    # Defines the range of valid GIDs. Similar to NVSHMEM_IB_ADDR_RANGE
    # for NVSHMEM. It must be a valid CIDR.
    AIBRIX_KV_CACHE_OL_TRANSPORT_RDMA_ADDR_RANGE: str = "::/0"

# The begin-* and end* here are used by the documentation generator
# to extract the used env vars.

# begin-env-vars-definition

kv_cache_ol_environment_variables: Dict[str, Callable[[], Any]] = {
    "AIBRIX_KV_CACHE_OL_DOUBLE_GET_THRESHOLD": lambda: tuple(
        map(
            lambda x, t: t(x),
            map(
                str.strip,
                os.getenv(
                    "AIBRIX_KV_CACHE_OL_DOUBLE_GET_THRESHOLD", "4,0.1"
                ).split(","),
            ),
            (int, float),
        )
    ),
    "AIBRIX_KV_CACHE_OL_CHUNK_SIZE": lambda: int(
        os.getenv("AIBRIX_KV_CACHE_OL_CHUNK_SIZE", "512")
    ),
    "AIBRIX_KV_CACHE_OL_TIME_MEASUREMENT_ENABLED": lambda: (
        os.getenv("AIBRIX_KV_CACHE_OL_TIME_MEASUREMENT_ENABLED", "1")
        .strip()
        .lower()
        in ("1", "true")
    ),
    "AIBRIX_KV_CACHE_OL_BREAKDOWN_MEASUREMENT_ENABLED": lambda: (
        os.getenv("AIBRIX_KV_CACHE_OL_BREAKDOWN_MEASUREMENT_ENABLED", "1")
        .strip()
        .lower()
        in ("1", "true")
    ),
    "AIBRIX_KV_CACHE_OL_TOKEN_VALIDATION_ENABLED": lambda: (
        os.getenv("AIBRIX_KV_CACHE_OL_TOKEN_VALIDATION_ENABLED", "0")
        .strip()
        .lower()
        in ("1", "true")
    ),
    # ================== L1Cache Env Vars ==================
    "AIBRIX_KV_CACHE_OL_L1_CACHE_ENABLED": lambda: (
        os.getenv("AIBRIX_KV_CACHE_OL_L1_CACHE_ENABLED", "1").strip().lower()
        in ("1", "true")
    ),
    "AIBRIX_KV_CACHE_OL_L1_CACHE_EVICTION_POLICY": lambda: (
        os.getenv("AIBRIX_KV_CACHE_OL_L1_CACHE_EVICTION_POLICY", "S3FIFO")
        .strip()
        .upper()
    ),
    "AIBRIX_KV_CACHE_OL_L1_CACHE_CAPACITY_GB": lambda: float(
        os.getenv("AIBRIX_KV_CACHE_OL_L1_CACHE_CAPACITY_GB", "10")
    ),
    "AIBRIX_KV_CACHE_OL_DEVICE": lambda: (
        os.getenv("AIBRIX_KV_CACHE_OL_DEVICE", "cpu").strip().lower()
    ),
    # ================== S3FIFO Env Vars ==================
    # Promotion threshold of small fifo to main fifo
    "AIBRIX_KV_CACHE_OL_S3FIFO_SMALL_TO_MAIN_PROMO_THRESHOLD": lambda: int(
        os.getenv(
            "AIBRIX_KV_CACHE_OL_S3FIFO_SMALL_TO_MAIN_PROMO_THRESHOLD", "1"
        )
    ),
    # Small fifo capacity ratio
    "AIBRIX_KV_CACHE_OL_S3FIFO_SMALL_FIFO_CAPACITY_RATIO": lambda: float(
        os.getenv("AIBRIX_KV_CACHE_OL_S3FIFO_SMALL_FIFO_CAPACITY_RATIO", "0.3")
    ),
    # ================== L2Cache Env Vars ==================
    "AIBRIX_KV_CACHE_OL_L2_CACHE_BACKEND": lambda: (
        os.getenv("AIBRIX_KV_CACHE_OL_L2_CACHE_BACKEND", "").strip().upper()
    ),
    "AIBRIX_KV_CACHE_OL_L2_CACHE_NAMESPACE": lambda: (
        os.getenv("AIBRIX_KV_CACHE_OL_L2_CACHE_NAMESPACE", "aibrix")
        .strip()
        .lower()
    ),
    "AIBRIX_KV_CACHE_OL_L2_CACHE_COMPRESSION": lambda: (
        os.getenv("AIBRIX_KV_CACHE_OL_L2_CACHE_COMPRESSION", "").strip().upper()
    ),
    "AIBRIX_KV_CACHE_OL_L2_CACHE_OP_BATCH": lambda: int(
        os.getenv("AIBRIX_KV_CACHE_OL_L2_CACHE_OP_BATCH", "32")
    ),
    "AIBRIX_KV_CACHE_OL_L2_CACHE_PER_TOKEN_TIMEOUT_MS": lambda: int(
        os.getenv("AIBRIX_KV_CACHE_OL_L2_CACHE_PER_TOKEN_TIMEOUT_MS", "20")
    ),
    "AIBRIX_KV_CACHE_OL_META_SERVICE_BACKEND": lambda: (
        os.getenv("AIBRIX_KV_CACHE_OL_META_SERVICE_BACKEND", "").strip().upper()
    ),
    "AIBRIX_KV_CACHE_OL_META_SERVICE_REFRESH_INTERVAL_S": lambda: int(
        os.getenv("AIBRIX_KV_CACHE_OL_META_SERVICE_REFRESH_INTERVAL_S", "30")
    ),
    "AIBRIX_KV_CACHE_OL_META_SERVICE_URL": lambda: (
        os.getenv("AIBRIX_KV_CACHE_OL_META_SERVICE_URL", "").strip()
    ),
    "AIBRIX_KV_CACHE_OL_META_SERVICE_CLUSTER_META_KEY": lambda: (
        os.getenv(
            "AIBRIX_KV_CACHE_OL_META_SERVICE_CLUSTER_META_KEY", ""
        ).strip()
    ),
    "AIBRIX_KV_CACHE_OL_L2_CACHE_PLACEMENT_POLICY": lambda: (
        os.getenv("AIBRIX_KV_CACHE_OL_L2_CACHE_PLACEMENT_POLICY", "SIMPLE")
        .strip()
        .upper()
    ),
    "AIBRIX_KV_CACHE_OL_L2_CACHE_KEY_BUILDER": lambda: (
        os.getenv("AIBRIX_KV_CACHE_OL_L2_CACHE_KEY_BUILDER", "RAW")
        .strip()
        .upper()
    ),
    "AIBRIX_KV_CACHE_OL_L2_CACHE_INGESTION_TYPE": lambda: (
        os.getenv("AIBRIX_KV_CACHE_OL_L2_CACHE_INGESTION_TYPE", "HOT")
        .strip()
        .upper()
    ),
    "AIBRIX_KV_CACHE_OL_L2_CACHE_INGESTION_MAX_INFLIGHT_TOKENS": lambda: int(
        os.getenv(
            "AIBRIX_KV_CACHE_OL_L2_CACHE_INGESTION_MAX_INFLIGHT_TOKENS", "0"
        )
    ),
    "AIBRIX_KV_CACHE_OL_L2_CACHE_NUM_ASYNC_WORKERS": lambda: int(
        os.getenv("AIBRIX_KV_CACHE_OL_L2_CACHE_NUM_ASYNC_WORKERS", "8")
    ),
    # ================== Mock Connector Env Vars ==================
    "AIBRIX_KV_CACHE_OL_MOCK_USE_RDMA": lambda: (
        os.getenv("AIBRIX_KV_CACHE_OL_MOCK_USE_RDMA", "0").strip().lower()
        in ("1", "true")
    ),
    "AIBRIX_KV_CACHE_OL_MOCK_USE_MPUT_MGET": lambda: (
        os.getenv("AIBRIX_KV_CACHE_OL_MOCK_USE_MPUT_MGET", "0").strip().lower()
        in ("1", "true")
    ),
    # ================== RocksDB Env Vars ==================
    "AIBRIX_KV_CACHE_OL_ROCKSDB_ROOT": lambda: os.path.expanduser(
        os.getenv(
            "AIBRIX_KV_CACHE_OL_ROCKSDB_ROOT",
            os.path.join(os.path.expanduser("~"), ".kv_cache_ol", "rocksdb"),
        )
    ),
    "AIBRIX_KV_CACHE_OL_ROCKSDB_TTL_S": lambda: int(
        os.getenv("AIBRIX_KV_CACHE_OL_ROCKSDB_TTL_S", "600")
    ),
    "AIBRIX_KV_CACHE_OL_ROCKSDB_WRITE_BUFFER_SIZE": lambda: int(
        os.getenv(
            "AIBRIX_KV_CACHE_OL_ROCKSDB_WRITE_BUFFER_SIZE",
            f"{64 * 1024 * 1024}",
        )
    ),
    "AIBRIX_KV_CACHE_OL_ROCKSDB_TARGET_FILE_SIZE_BASE": lambda: int(
        os.getenv(
            "AIBRIX_KV_CACHE_OL_ROCKSDB_TARGET_FILE_SIZE_BASE",
            f"{64 * 1024 * 1024}",
        )
    ),
    "AIBRIX_KV_CACHE_OL_ROCKSDB_MAX_WRITE_BUFFER_NUMBER": lambda: int(
        os.getenv("AIBRIX_KV_CACHE_OL_ROCKSDB_MAX_WRITE_BUFFER_NUMBER", "3")
    ),
    "AIBRIX_KV_CACHE_OL_ROCKSDB_MAX_TOTAL_WAL_SIZE": lambda: int(
        os.getenv(
            "AIBRIX_KV_CACHE_OL_ROCKSDB_MAX_TOTAL_WAL_SIZE",
            f"{128 * 1024 * 1024}",
        )
    ),
    "AIBRIX_KV_CACHE_OL_ROCKSDB_MAX_BACKGROUND_JOBS": lambda: int(
        os.getenv("AIBRIX_KV_CACHE_OL_ROCKSDB_MAX_BACKGROUND_JOBS", "8")
    ),
    # ================== InfiniStore Env Vars ==================
    "AIBRIX_KV_CACHE_OL_INFINISTORE_HOST_ADDR": lambda: (
        os.getenv(
            "AIBRIX_KV_CACHE_OL_INFINISTORE_HOST_ADDR", "127.0.0.1"
        ).strip()
    ),
    "AIBRIX_KV_CACHE_OL_INFINISTORE_SERVICE_PORT": lambda: int(
        os.getenv("AIBRIX_KV_CACHE_OL_INFINISTORE_SERVICE_PORT", "12345")
    ),
    "AIBRIX_KV_CACHE_OL_INFINISTORE_CONNECTION_TYPE": lambda: (
        os.getenv("AIBRIX_KV_CACHE_OL_INFINISTORE_CONNECTION_TYPE", "RDMA")
        .strip()
        .upper()
    ),
    "AIBRIX_KV_CACHE_OL_INFINISTORE_IB_PORT": lambda: int(
        os.getenv("AIBRIX_KV_CACHE_OL_INFINISTORE_IB_PORT", "12345")
    ),
    "AIBRIX_KV_CACHE_OL_INFINISTORE_LINK_TYPE": lambda: (
        os.getenv(
            "AIBRIX_KV_CACHE_OL_INFINISTORE_LINK_TYPE", "Ethernet"
        ).strip()
    ),
    "AIBRIX_KV_CACHE_OL_INFINISTORE_VISIBLE_DEV_LIST": lambda: list(
        filter(
            None,
            os.getenv("AIBRIX_KV_CACHE_OL_INFINISTORE_VISIBLE_DEV_LIST", "")
            .strip()
            .split(","),
        )
    ),
    "AIBRIX_KV_CACHE_OL_INFINISTORE_USE_GDR": lambda: (
        os.getenv("AIBRIX_KV_CACHE_OL_INFINISTORE_USE_GDR", "1").strip().lower()
        in ("1", "true")
    ),
    # ================== HPKV Env Vars ==================
    "AIBRIX_KV_CACHE_OL_HPKV_REMOTE_ADDR": lambda: (
        os.getenv("AIBRIX_KV_CACHE_OL_HPKV_REMOTE_ADDR", "127.0.0.1").strip()
    ),
    "AIBRIX_KV_CACHE_OL_HPKV_REMOTE_PORT": lambda: int(
        os.getenv("AIBRIX_KV_CACHE_OL_HPKV_REMOTE_PORT", "12346")
    ),
    "AIBRIX_KV_CACHE_OL_HPKV_LOCAL_ADDR": lambda: (
        os.getenv("AIBRIX_KV_CACHE_OL_HPKV_LOCAL_ADDR", "127.0.0.1").strip()
    ),
    "AIBRIX_KV_CACHE_OL_HPKV_LOCAL_PORT": lambda: int(
        os.getenv("AIBRIX_KV_CACHE_OL_HPKV_LOCAL_PORT", "12345")
    ),
    "AIBRIX_KV_CACHE_OL_HPKV_USE_GDR": lambda: (
        os.getenv("AIBRIX_KV_CACHE_OL_HPKV_USE_GDR", "1").strip().lower()
        in ("1", "true")
    ),
    # ================== RDMA Auto-Detection Env Vars ==================
    "AIBRIX_KV_CACHE_OL_TRANSPORT_RDMA_ADDR_RANGE": lambda: (
        os.getenv(
            "AIBRIX_KV_CACHE_OL_TRANSPORT_RDMA_ADDR_RANGE", "::/0"
        ).strip()
    ),
}

# end-env-vars-definition


def __getattr__(name: str):
    # lazy evaluation of environment variables
    if name in kv_cache_ol_environment_variables:
        return kv_cache_ol_environment_variables[name]()
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")


def __dir__():
    return list(kv_cache_ol_environment_variables.keys())

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

from typing import Any, Tuple

from .cache_hashable import (
    BlockHashes,
    KVCacheKey,
    KVCacheKeyTypes,
    TokenListView,
)
from .memory import MemoryRegion


def parse_kvcache_api_args(
    *args, **kwargs
) -> Tuple[
    KVCacheKeyTypes | None,
    KVCacheKeyTypes,
    Any,
]:
    """
    Parse arguments and return normalized parameters.

    Returns:
        Tuple of (prefix, query, kv_tensors) where one of the following will
        be None:
        - If called with (prefix, query): kv_tensors will be None
        - If called with (prefix, query, kv_tensors): none will be None
        - If called with (cache_key): kv_tensors will be None
        - If called with (cache_key, kv_tensors): none will be None
    """

    def validate_cache_key_type(cache_key):
        assert isinstance(cache_key, KVCacheKey), (
            f"cache_key must be of type KVCacheKey, got {type(cache_key)}"
        )
        if not isinstance(cache_key.query, TokenListView):
            assert MemoryRegion.use_compact_layout(), (
                "AIBRIX_KV_CACHE_OL_TOKEN_VALIDATION_ENABLED is not "
                "supported if using block hashes or block ids as kvcache "
                "keys"
            )

    def validate_prefix_query_type(prefix, query):
        if prefix is not None:
            assert isinstance(prefix, (TokenListView, BlockHashes)), (
                "prefix must be TokenListView or BlockHashes, "
                f"got {type(prefix)}"
            )
        assert isinstance(query, (TokenListView, BlockHashes)), (
            f"query must be TokenListView or BlockHashes, got {type(query)}"
        )
        if not isinstance(query, TokenListView):
            assert MemoryRegion.use_compact_layout(), (
                "AIBRIX_KV_CACHE_OL_TOKEN_VALIDATION_ENABLED is not "
                "supported if using block hashes or block ids as kvcache "
                "keys"
            )

    # Check for keyword arguments first
    if kwargs:
        if "prefix" in kwargs and "query" in kwargs:
            prefix = kwargs["prefix"]
            query = kwargs["query"]
            validate_prefix_query_type(prefix, query)
            if "kv_tensors" in kwargs:
                kv_tensors = kwargs["kv_tensors"]
                return prefix, query, kv_tensors
            return prefix, query, None
        elif "query" in kwargs and len(args) == 1:
            prefix = args[0]
            query = kwargs["query"]
            validate_prefix_query_type(prefix, query)
            if "kv_tensors" in kwargs:
                kv_tensors = kwargs["kv_tensors"]
                return prefix, query, kv_tensors
            return prefix, query, None
        elif "cache_key" in kwargs:
            cache_key = kwargs["cache_key"]
            validate_cache_key_type(cache_key)
            if "kv_tensors" in kwargs:
                kv_tensors = kwargs["kv_tensors"]
                return cache_key.prefix, cache_key.query, kv_tensors  # type: ignore
            return cache_key.prefix, cache_key.query, None  # type: ignore
        elif "kv_tensors" in kwargs and len(args) == 1:
            cache_key = args[0]
            validate_cache_key_type(cache_key)
            kv_tensors = kwargs["kv_tensors"]
            return cache_key.prefix, cache_key.query, kv_tensors  # type: ignore
        elif "kv_tensors" in kwargs and len(args) == 2:
            prefix = args[0]
            query = args[1]
            validate_prefix_query_type(prefix, query)
            kv_tensors = kwargs["kv_tensors"]
            return prefix, query, kv_tensors

    # Check positional arguments
    if len(args) == 1:
        # Single argument - should be KVCacheKey
        cache_key = args[0]
        validate_cache_key_type(cache_key)
        return cache_key.prefix, cache_key.query, None  # type: ignore
    elif len(args) == 2:
        # Two arguments - should be (prefix, query) or
        # (cache_key, kv_tensors)
        if isinstance(args[0], KVCacheKey):
            cache_key, kv_tensors = args
            validate_cache_key_type(cache_key)
            return cache_key.prefix, cache_key.query, kv_tensors  # type: ignore
        else:
            prefix, query = args
            validate_prefix_query_type(prefix, query)
            return prefix, query, None
    elif len(args) == 3:
        # Three arguments - should be (prefix, query, kv_tensors)
        prefix, query, kv_tensors = args
        validate_prefix_query_type(prefix, query)
        return prefix, query, kv_tensors

    raise ValueError("Invalid arguments")

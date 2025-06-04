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

from abc import ABC, abstractmethod
from typing import Iterable

import numpy as np
from farmhash import FarmHash64

from .memory import MemoryRegion
from .utils import np_array_concat


class KVCacheHashable(ABC):
    """
    A hashable object that can be used to index a KV cache block.
    """

    @abstractmethod
    def __hash__(self) -> int:
        raise NotImplementedError

    @abstractmethod
    def __eq__(self, other) -> bool:
        raise NotImplementedError

    @abstractmethod
    def __len__(self) -> int:
        raise NotImplementedError


class BaseKVCacheHashable(KVCacheHashable):
    """
    Base class for a hashable object that uses all tokens to compute the hash.
    """

    @abstractmethod
    def all_tokens_memoryview(self) -> memoryview:
        """Memoryview of the PACKED bytes representation of all tokens."""
        raise NotImplementedError

    def __hash__(self) -> int:
        return FarmHash64(self.all_tokens_memoryview())

    def __eq__(self, other) -> bool:
        if not isinstance(other, BaseKVCacheHashable):
            return False
        return self.all_tokens_memoryview() == other.all_tokens_memoryview()


class TokenCacheKey(BaseKVCacheHashable):
    """
    A cache key that use numpy ndarray's bytes representation to compute
    the hash.
    Args:
        prefix (np.ndarray | None): The prefix tokens of the kv tensors.
        tokens (np.ndarray): The tokens of the kv tensors.
    """

    def __init__(self, *, tokens: np.ndarray, prefix: np.ndarray | None = None):
        self._storage = np_array_concat(prefix, tokens)

        self.prefix = prefix
        self.tokens = tokens

    def all_tokens(self) -> np.ndarray:
        return self._storage

    def all_tokens_memoryview(self) -> memoryview:
        return memoryview(self._storage)  # type: ignore

    def __len__(self) -> int:
        return len(self._storage)

    def shift(self, shift: int) -> "TokenCacheKey":
        """
        Shifts the split of prefix and tokens.
        Args:
            shift (int): The number of tokens to shift.
        Returns:
            TokenCacheKey: The cache key with shifted tokens.
        """
        orig_prefix_len = len(self.prefix) if self.prefix is not None else 0
        new_prefix_len = min(max(0, orig_prefix_len + shift), len(self))
        new_prefix = self._storage[:new_prefix_len]
        new_tokens = self._storage[new_prefix_len:]
        return TokenCacheKey(prefix=new_prefix, tokens=new_tokens)

    def shrink(self, n: int) -> "TokenCacheKey":
        """
        Shrinks n from tokens.
        Args:
            n (int): The number of tokens to shrink.
        Returns:
            TokenCacheKey: The new cache key.
        """
        new_tokens = self.tokens[:-n]
        return TokenCacheKey(prefix=self.prefix, tokens=new_tokens)

    def batched(self, batch_size: int) -> Iterable["TokenCacheKey"]:
        """
        Batches the tokens into a list of TokenCacheKey.
        Args:
            batch_size (int): The batch size.
        Returns:
            Iterable[TokenCacheKey]: The batched tokens.
        """
        prefix_len = len(self.prefix) if self.prefix is not None else 0
        num_batches = len(self.tokens) // batch_size
        for _ in range(num_batches):
            yield (
                TokenCacheKey(
                    prefix=self._storage[:prefix_len],
                    tokens=self._storage[prefix_len : prefix_len + batch_size],
                )
            )
            prefix_len += batch_size


class MemoryRegionCacheEntry(BaseKVCacheHashable):
    """
    A cache entry that stores a memory region and uses MR's all tokens
    to compute the hash.
    """

    def __init__(self, mr: MemoryRegion):
        # Take the ownership of the memory region
        self._mr: MemoryRegion | None = mr
        assert mr.is_sealed, "Memory region must be sealed"

        self.ref_up = self._mr.ref_up
        self.ref_down = self._mr.ref_down

    def all_tokens_memoryview(self) -> memoryview:
        assert self._mr is not None
        return memoryview(np_array_concat(*self._mr.unpack_tokens()))  # type: ignore

    def __len__(self) -> int:
        assert self._mr is not None
        return self._mr.length

    def cache_key(self) -> TokenCacheKey:
        assert self._mr is not None
        prefix, tokens = self._mr.unpack_tokens()
        prefix = prefix.copy() if prefix is not None else None
        tokens = tokens.copy()
        return TokenCacheKey(prefix=prefix, tokens=tokens)

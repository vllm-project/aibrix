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
from typing import List, Sequence, TypeAlias

import numpy as np
from farmhash import FarmHash32

from .common import CachedPyObjectBase
from .utils import hash_combine_128


class KVCacheHashable(ABC):
    """
    A hashable object that can be used to index a KV cache block.
    """

    @property
    @abstractmethod
    def prefix(self):
        raise NotImplementedError

    @property
    @abstractmethod
    def query(self):
        raise NotImplementedError

    @abstractmethod
    def __hash__(self) -> int:
        raise NotImplementedError

    @abstractmethod
    def __eq__(self, other) -> bool:
        raise NotImplementedError

    @abstractmethod
    def __len__(self) -> int:
        """Number of tokens"""
        raise NotImplementedError


class KVCacheIds(ABC):
    """
    A KVCacheIds is a sequence of block ids (e.g., block hashes) or tokens.
    """

    @abstractmethod
    def __getitem__(self, index):
        """Get item by token index"""
        raise NotImplementedError

    @abstractmethod
    def __add__(self, other):
        raise NotImplementedError

    @abstractmethod
    def __iter__(self):
        raise NotImplementedError

    @abstractmethod
    def __hash__(self) -> int:
        raise NotImplementedError

    @abstractmethod
    def __eq__(self, other) -> bool:
        raise NotImplementedError

    @abstractmethod
    def __len__(self) -> int:
        """Number of tokens"""
        raise NotImplementedError


class TokenListViewMeta:
    """
    The meta data of a TokenListView. Key builders will use this metadata
    to cache its internal results.
    """

    pass


class TokenListView(CachedPyObjectBase, KVCacheIds):
    """
    A TokenListView is a view of a list of tokens.
    """

    def __init__(
        self,
        data: Sequence[int] | np.ndarray,
        start=None,
        stop=None,
        *,
        meta=None,
    ):
        if isinstance(data, np.ndarray):
            self._data = data
        else:
            self._data = np.array(data, dtype=np.int32)

        self._start = start or 0
        self._stop = len(data) if stop is None else stop

        # normalize negative indices
        if self._start < 0:
            self._start = max(0, len(self._data) + self._start)
        if self._stop < 0:
            self._stop = max(0, len(self._data) + self._stop)

        # ensure boundaries are valid
        self._start = min(self._start, len(self._data))
        self._stop = min(self._stop, len(self._data))
        self._start, self._stop = (
            min(self._start, self._stop),
            max(self._start, self._stop),
        )
        self._meta_ = meta or TokenListViewMeta()

    def __len__(self):
        """Number of tokens"""
        return self._stop - self._start

    def __repr__(self):
        return (
            f"TokenListView(data=[len={len(self._data)}], start={self._start}, "
            f"stop={self._stop})"
        )

    def __str__(self):
        return self.__repr__()

    def __getitem__(self, index):
        """Indexing returns a new TokenListView with adjusted boundaries"""
        if isinstance(index, slice):
            # Calculate new start/stop based on the slice
            start, stop, step = index.indices(len(self))
            if step != 1:
                raise ValueError("TokenListView only supports step=1 slicing")
            new_start = self._start + start
            new_stop = self._start + stop
            return TokenListView(
                self._data, new_start, new_stop, meta=self._meta_
            )

        # Handle single index
        if index < 0:
            index += len(self)
        if not 0 <= index < len(self):
            raise IndexError("TokenListView index out of range")
        return self._data[self._start + index]

    def __add__(self, other):
        """Support for + operator with another TokenListView"""
        if not isinstance(other, TokenListView):
            raise TypeError(
                "Can only concatenate TokenListView with another TokenListView"
            )

        if self._data is not other._data:
            raise ValueError(
                "Cannot concatenate TokenListViews with different underlying "
                "data"
            )

        if self._stop == other._start:
            return TokenListView(
                self._data, self._start, other._stop, meta=self._meta_
            )
        elif other._stop == self._start:
            return TokenListView(
                self._data, other._start, self._stop, meta=self._meta_
            )
        else:
            raise ValueError(
                "Cannot concatenate TokenListViews without overlapping "
                "boundaries"
            )

    def __iter__(self):
        for i in range(len(self)):
            yield self[i]

    def __eq__(self, other):
        if not isinstance(other, TokenListView):
            return False
        if self._data is other._data:
            return self._start == other._start and self._stop == other._stop
        elif len(self) == len(other):
            return all(self.to_numpy() == other.to_numpy())
        else:
            return False

    def __ne__(self, value):
        return not self.__eq__(value)

    def __hash__(self) -> int:
        return FarmHash32(self.memoryview())

    def memoryview(self) -> memoryview:
        return memoryview(self.to_numpy().data)

    def to_numpy(self) -> np.ndarray:
        return self._data[self._start : self._stop]

    @staticmethod
    def from_numpy(data: np.ndarray) -> "TokenListView":
        return TokenListView(data)

    def nbytes(self) -> int:
        return self.to_numpy().nbytes

    @staticmethod
    def calculate_size(ntokens: int) -> int:
        bytes_per_token = np.dtype(np.int32).itemsize
        return bytes_per_token * ntokens


BlockHashList: TypeAlias = List[str]


def is_block_hash_list_type(data) -> bool:
    if not isinstance(data, List):
        return False

    if not all(isinstance(item, str) for item in data):
        return False

    return True


class BlockHashes(CachedPyObjectBase, KVCacheIds):
    """
    A BlockHashes is a sequence of block hashes
    """

    def __init__(self, data: BlockHashList, block_ntokens: int):
        self._data = data
        self.block_ntokens = block_ntokens

        if not is_block_hash_list_type(data):
            raise TypeError(f"Only support {BlockHashList}, got {type(data)}")

    def __len__(self):
        return len(self._data) * self.block_ntokens

    def __repr__(self):
        return f"BlockHashes(data=[len={len(self._data)}])"

    def __str__(self):
        return self.__repr__()

    def __getitem__(self, index):
        if isinstance(index, slice):
            start, stop, _ = index.indices(len(self))
            assert start % self.block_ntokens == 0, (
                f"start ({start}) of token index must be multiple of block "
                f"size ({self.block_ntokens})"
            )
            assert stop % self.block_ntokens == 0, (
                f"stop ({stop}) of token index must be multiple of block "
                f"size ({self.block_ntokens})"
            )
            if index.step is None:
                step = self.block_ntokens
            else:
                step = index.step
                assert step % self.block_ntokens == 0, (
                    f"step ({step}) of token index must be multiple of block "
                    f"size ({self.block_ntokens})"
                )

            start = start // self.block_ntokens
            stop = stop // self.block_ntokens
            step = step // self.block_ntokens
            return BlockHashes(self._data[start:stop:step], self.block_ntokens)

        # Handle single index
        return self._data[index]

    def __add__(self, other):
        if not isinstance(other, BlockHashes):
            raise TypeError(
                "Can only concatenate BlockHashes with another BlockHashes"
            )
        if self.block_ntokens != other.block_ntokens:
            raise ValueError(
                "self and other should have the same block_ntokens"
            )
        return BlockHashes(self._data + other._data, self.block_ntokens)

    def __iter__(self):
        yield from self._data.__iter__()

    def __eq__(self, other):
        if not isinstance(other, BlockHashes):
            return False
        # we assume a block hash includes the information of both position and
        # all tokens belonging to this block, therefore, we only need to check
        # the last block hash and num of blocks to see if two BlockHashes are
        # the same
        return self._data[-1] == other._data[-1] and len(self._data) == len(
            other._data
        )

    def __ne__(self, value):
        return not self.__eq__(value)

    def __hash__(self) -> int:
        # use the last block hash and num of blocks for calculating the hash
        return hash_combine_128(hash(self._data[-1]), len(self._data))


class TokenCacheKey(KVCacheHashable, CachedPyObjectBase):
    """
    A cache key that compounds prefix and query tokens.
    Args:
        prefix (TokenListView | None): The prefix tokens of the kv tensors.
        query (TokenListView): The query tokens of the kv tensors.
    """

    def __init__(self, prefix: TokenListView | None, query: TokenListView):
        self._prefix = prefix
        self._query = query

        if prefix is not None:
            self._all_tokens = prefix + query
        else:
            self._all_tokens = query

    @property
    def prefix(self):
        return self._prefix

    @property
    def query(self):
        return self._query

    def __hash__(self) -> int:
        return hash(self._all_tokens)

    def __eq__(self, other) -> bool:
        if not isinstance(other, TokenCacheKey):
            return False
        return self._all_tokens == other._all_tokens

    def __len__(self) -> int:
        return len(self._all_tokens)

    def __str__(self) -> str:
        return f"TokenCacheKey(prefix={self._prefix}, query={self._query})"


class BlockCacheKey(KVCacheHashable, CachedPyObjectBase):
    """
    A cache key that compounds prefix and query blocks.
    Args:
        prefix: The prefix block hashes of the kv tensors.
        query: The query block hashes of the kv tensors.
        block_ntokens: Number of tokens in a block.
    """

    def __init__(
        self,
        prefix: BlockHashes | BlockHashList | None,
        query: BlockHashes | BlockHashList,
        block_ntokens: int,
    ):
        if isinstance(prefix, BlockHashes) or prefix is None:
            self._prefix = prefix
        else:
            self._prefix = BlockHashes(prefix, block_ntokens)

        if isinstance(query, BlockHashes):
            self._query = query
        else:
            self._query = BlockHashes(query, block_ntokens)

    @property
    def prefix(self):
        return self._prefix

    @property
    def query(self):
        return self._query

    def __hash__(self) -> int:
        return hash(self.query)

    def __eq__(self, other) -> bool:
        if not isinstance(other, BlockCacheKey):
            return False
        return self.query == other.query

    def __len__(self) -> int:
        prefix_len = len(self._prefix) if self._prefix is not None else 0
        return prefix_len + len(self._query)

    def __str__(self) -> str:
        return f"BlockCacheKey(prefix={self._prefix}, query={self._query})"


KVCacheKeyTypes: TypeAlias = TokenListView | BlockHashes


class KVCacheKey(KVCacheHashable):
    """
    A cache key that compounds prefix and query tokens.
    Args:
        prefix: The prefix tokens or prefix block hashes.
        query: The query tokens or query block hashes.
    """

    def __init__(
        self,
        prefix: KVCacheKeyTypes | None,
        query: KVCacheKeyTypes,
    ):
        assert query is not None, "query should not be None"

        query_type = type(query)
        if prefix is not None:
            assert type(prefix) is query_type, (
                f"{type(prefix)} is not {query_type}"
            )

        if query_type is TokenListView:
            self._key: KVCacheHashable = TokenCacheKey(prefix, query)  # type: ignore
        elif query_type is BlockHashes:
            if prefix is not None:
                assert prefix.block_ntokens == query.block_ntokens  # type: ignore
            self._key = BlockCacheKey(prefix, query, query.block_ntokens)  # type: ignore
        else:
            raise TypeError(
                "Only TokenListView and BlockHashes are support, "
                f"got {query_type}"
            )

    @property
    def prefix(self):
        return self._key.prefix

    @property
    def query(self):
        return self._key.query

    def __hash__(self) -> int:
        return hash(self._key)

    def __eq__(self, other) -> bool:
        return self._key == other._key

    def __len__(self) -> int:
        return len(self._key)

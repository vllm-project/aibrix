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
from typing import Sequence

import numpy as np
from farmhash import FarmHash32

from .common import CachedPyObjectBase


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


class TokenListViewMeta:
    """
    The meta data of a TokenListView. Key builders will use this metadata
    to cache its internal results.
    """

    pass


class TokenListView(CachedPyObjectBase):
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
        return self._stop - self._start

    def __contains__(self, item):
        raise TypeError("contains is not supported for TokenListView")

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


class BaseKVCacheHashable(KVCacheHashable, CachedPyObjectBase):
    """
    Base class for a hashable object that uses all tokens to compute the hash.
    """

    def __init__(self, prefix: TokenListView | None, tokens: TokenListView):
        self.prefix = prefix
        self.tokens = tokens

        if prefix is not None:
            self._all_tokens = prefix + tokens
        else:
            self._all_tokens = tokens

    def __hash__(self) -> int:
        return self._all_tokens.__hash__()

    def __eq__(self, other) -> bool:
        if not isinstance(other, BaseKVCacheHashable):
            return False
        return self._all_tokens == other._all_tokens


class TokenCacheKey(BaseKVCacheHashable):
    """
    A cache key that compounds prefix and tokens.
    Args:
        prefix (TokenListView | None): The prefix tokens of the kv tensors.
        tokens (TokenListView): The tokens of the kv tensors.
    """

    def __init__(self, prefix: TokenListView | None, tokens: TokenListView):
        super().__init__(prefix, tokens)

    def __len__(self) -> int:
        return len(self._all_tokens)

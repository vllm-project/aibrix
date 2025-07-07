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
from typing import Tuple

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


class BaseKVCacheHashable(KVCacheHashable, CachedPyObjectBase):
    """
    Base class for a hashable object that uses all tokens to compute the hash.
    """

    def __init__(self, prefix: Tuple[int, ...] | None, tokens: Tuple[int, ...]):
        self.prefix = prefix or tuple()
        self.tokens = tokens

    def __hash__(self) -> int:
        return hash((self.prefix, self.tokens))

    def __eq__(self, other) -> bool:
        if not isinstance(other, BaseKVCacheHashable):
            return False
        return (self.prefix, self.tokens) == (other.prefix, other.tokens)


class TokenCacheKey(BaseKVCacheHashable):
    """
    A cache key that compounds prefix and tokens.
    Args:
        prefix (np.ndarray | None): The prefix tokens of the kv tensors.
        tokens (np.ndarray): The tokens of the kv tensors.
    """

    def __init__(self, prefix: Tuple[int, ...] | None, tokens: Tuple[int, ...]):
        super().__init__(prefix, tokens)

    def __len__(self) -> int:
        return len(self.prefix) + len(self.tokens)

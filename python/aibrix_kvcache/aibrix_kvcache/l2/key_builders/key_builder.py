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
from typing import Sequence, Tuple


class KeyBuilder(ABC):
    """KeyBuilder is used to build a sequence of keys from given tokens."""

    def __init__(self, block_size: int):
        self.block_size = block_size

    @staticmethod
    def create(
        name: str,
        **kwargs,
    ) -> "KeyBuilder":
        """Create a KeyBuilder."""
        if name == "RAW":
            from .raw_key_builder import RawKeyBuilder

            return RawKeyBuilder(**kwargs)
        elif name == "ROLLING_HASH":
            from .hasher import FarmHasher
            from .rolling_hash_key_builder import RollingHashKeyBuilder

            return RollingHashKeyBuilder(FarmHasher(), **kwargs)
        elif name == "SIMPLE_HASH":
            from .hasher import FarmHasher
            from .simple_hash_key_builder import SimpleHashKeyBuilder

            return SimpleHashKeyBuilder(FarmHasher(), **kwargs)
        else:
            raise ValueError(f"Unknown key builder type: {name}")

    @property
    @abstractmethod
    def signature(self) -> str:
        raise NotImplementedError

    @abstractmethod
    def build(
        self, prefix: Sequence[int] | None, tokens: Sequence[int]
    ) -> Tuple[Tuple[Tuple[int, ...], bytes], ...]:
        """Build a sequence of keys from given tokens.
        Args:
            prefix (Sequence[int] | None): prefix tokens
            tokens (Sequence[int]): tokens
        Returns:
            A sequence of keys.
        """
        raise NotImplementedError

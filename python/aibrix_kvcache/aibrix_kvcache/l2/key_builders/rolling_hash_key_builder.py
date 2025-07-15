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

from typing import Tuple

from ...cache_hashable import TokenListView
from ...utils import hash_combine_128
from .hasher import Hasher
from .key_builder import KeyBuilder


class RollingHashKeyBuilder(KeyBuilder):
    def __init__(self, hasher: Hasher, block_size: int):
        super().__init__(block_size)
        self.hasher = hasher

    @property
    def signature(self) -> str:
        return "ro"

    def build(
        self, prefix: TokenListView | None, tokens: TokenListView
    ) -> Tuple[Tuple[TokenListView, bytes], ...]:
        assert prefix is None or len(prefix) % self.block_size == 0

        token_size = len(tokens) - len(tokens) % self.block_size
        if token_size < self.block_size:
            return tuple()

        if prefix is not None:
            all = prefix + tokens
        else:
            all = tokens

        all_bytes = memoryview(all._data.data)
        attr_name = f"{self.__class__.__name__}.hash_bytes"
        if hasattr(all._meta_, attr_name):
            hash_bytes = getattr(all._meta_, attr_name)
        else:
            hash_bytes = []
            prev_hash = -1

            # calculate hashes for all._data and cache them in all._meta
            for i in range(0, len(all._data), self.block_size):
                data = all_bytes[i : i + self.block_size]
                curr_hash = self.hasher.hash(data)

                if i > 0:
                    curr_hash = hash_combine_128(prev_hash, curr_hash)

                prev_hash = curr_hash
                hash_bytes.append(curr_hash.to_bytes(16))

            setattr(all._meta_, attr_name, hash_bytes)

        prefix_len = len(prefix) if prefix is not None else 0
        results = []

        for i in range(0, token_size, self.block_size):
            keys = all[: prefix_len + i + self.block_size]

            curr_hash_bytes = hash_bytes[(prefix_len + i) // self.block_size]

            results.append((keys, curr_hash_bytes))

        return tuple(results)

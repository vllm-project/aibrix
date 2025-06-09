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

import array
from typing import Sequence, Tuple

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
        self, prefix: Sequence[int] | None, tokens: Sequence[int]
    ) -> Tuple[Tuple[Tuple[int, ...], bytes], ...]:
        assert prefix is None or len(prefix) % self.block_size == 0

        token_size = len(tokens) - len(tokens) % self.block_size
        if token_size < self.block_size:
            return tuple()

        results = []

        prefix_len = len(prefix) if prefix is not None else 0
        prefix_hash = -1
        all = (tuple(prefix) if prefix is not None else ()) + tuple(tokens)
        all_bytes = memoryview(array.array("I", all).tobytes())
        itemsize = array.array("I").itemsize
        for i in range(0, prefix_len, self.block_size):
            start = i * itemsize
            end = (i + self.block_size) * itemsize
            data = all_bytes[start:end]
            curr_hash = self.hasher.hash(data)

            if prefix_hash != -1:
                curr_hash = hash_combine_128(prefix_hash, curr_hash)

            prefix_hash = curr_hash

        for i in range(0, token_size, self.block_size):
            keys = all[: prefix_len + i + self.block_size]

            start = (prefix_len + i) * itemsize
            end = (prefix_len + i + self.block_size) * itemsize
            data = all_bytes[start:end]
            curr_hash = self.hasher.hash(data)

            if prefix_hash != -1:
                curr_hash = hash_combine_128(prefix_hash, curr_hash)

            prefix_hash = curr_hash

            results.append((keys, curr_hash.to_bytes(16)))

        return tuple(results)

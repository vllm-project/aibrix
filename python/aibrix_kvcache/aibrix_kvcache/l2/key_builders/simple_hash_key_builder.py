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
from .hasher import Hasher
from .key_builder import KeyBuilder


class SimpleHashKeyBuilder(KeyBuilder):
    def __init__(self, hasher: Hasher, block_size: int):
        super().__init__(block_size)
        self.hasher = hasher

    @property
    def signature(self) -> str:
        return "sim"

    def build(
        self, prefix: TokenListView | None, tokens: TokenListView
    ) -> Tuple[Tuple[TokenListView, bytes], ...]:
        assert prefix is None or len(prefix) % self.block_size == 0

        token_size = len(tokens) - len(tokens) % self.block_size
        if token_size < self.block_size:
            return tuple()

        results = []

        prefix_len = len(prefix) if prefix is not None else 0

        if prefix is not None:
            all = prefix + tokens
        else:
            all = tokens

        all_bytes = all.memoryview()
        for i in range(0, token_size, self.block_size):
            keys = all[: prefix_len + i + self.block_size]

            data = all_bytes[: (prefix_len + i + self.block_size)]
            curr_hash = self.hasher.hash(data)

            results.append((keys, curr_hash.to_bytes(16)))

        return tuple(results)

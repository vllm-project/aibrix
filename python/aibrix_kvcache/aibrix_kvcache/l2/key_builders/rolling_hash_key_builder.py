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

import numpy as np

from ...utils import np_array_concat
from .hasher import Hasher
from .key_builder import KeyBuilder


class RollingHashKeyBuilder(KeyBuilder):
    def __init__(self, hasher: Hasher, block_size: int):
        super().__init__()
        self.hasher = hasher
        self.block_size = block_size

    def build(
        self, prefix: np.ndarray | None, tokens: np.ndarray
    ) -> Tuple[Tuple[np.ndarray, bytes], ...]:
        assert prefix is None or len(prefix) % self.block_size == 0

        token_size = len(tokens) - len(tokens) % self.block_size
        if token_size < self.block_size:
            return tuple()

        results = []
        prev_hash: int = -1
        candidates = np.empty(self.block_size + 1)

        if prefix is not None:
            for i in range(0, len(prefix), self.block_size):
                if i > 0:
                    candidates[0] = prev_hash
                    start = 0
                else:
                    start = 1
                candidates[1:] = prefix[i : i + self.block_size]

                prev_hash = self.hasher.hash(memoryview(candidates[start:]))  # type: ignore

        all = np_array_concat(prefix, tokens)
        prefix_len = len(prefix) if prefix is not None else 0
        for i in range(0, token_size, self.block_size):
            if prev_hash > 0:
                candidates[0] = prev_hash
                start = 0
            else:
                start = 1
            candidates[1:] = tokens[i : i + self.block_size]
            keys = all[: prefix_len + i + self.block_size]

            curr_hash = self.hasher.hash(memoryview(candidates[start:]))  # type: ignore

            results.append((keys, curr_hash.to_bytes(16)))
            prev_hash = curr_hash

        return tuple(results)

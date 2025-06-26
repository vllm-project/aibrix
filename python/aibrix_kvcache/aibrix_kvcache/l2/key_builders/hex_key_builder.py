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

from .key_builder import KeyBuilder


class HexKeyBuilder(KeyBuilder):
    def __init__(self, block_size: int):
        super().__init__(block_size)

    @property
    def signature(self) -> str:
        return "hex"

    def build(
        self, prefix: Sequence[int] | None, tokens: Sequence[int]
    ) -> Tuple[Tuple[Tuple[int, ...], bytes], ...]:
        assert prefix is None or len(prefix) % self.block_size == 0

        token_size = len(tokens) - len(tokens) % self.block_size
        if token_size < self.block_size:
            return tuple()

        results = []

        all = (tuple(prefix) if prefix is not None else ()) + tuple(tokens)
        assert len(all) % self.block_size == 0
        prefix_len = len(prefix) if prefix is not None else 0

        all_bytes = memoryview(array.array("I", all).tobytes())
        itemsize = array.array("I").itemsize
        for i in range(0, token_size, self.block_size):
            keys = all[: prefix_len + i + self.block_size]
            end = (prefix_len + i + self.block_size) * itemsize
            block_hex = all_bytes[:end].hex()
            results.append((keys, block_hex.encode("utf-8")))

        return tuple(results)

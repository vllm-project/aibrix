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


class RawKeyBuilder(KeyBuilder):
    def __init__(self, block_size: int):
        super().__init__(block_size)

    @property
    def signature(self) -> str:
        return "raw"

    def build(
        self, prefix: Sequence[int] | None, tokens: Sequence[int]
    ) -> Tuple[Tuple[Tuple[int, ...], bytes], ...]:
        assert prefix is None or len(prefix) % self.block_size == 0

        token_size = len(tokens) - len(tokens) % self.block_size
        if token_size < self.block_size:
            return tuple()

        results = []

        not_none_prefix = tuple() if prefix is None else tuple(prefix)
        all = tuple(not_none_prefix + tuple(tokens[:token_size]))
        assert len(all) % self.block_size == 0
        prefix_len = len(not_none_prefix)

        all_bytes = array.array("I", all).tobytes()
        itemsize = array.array("I").itemsize
        for i in range(0, token_size, self.block_size):
            keys = all[: prefix_len + i + self.block_size]
            block_bytes = all_bytes[
                : (prefix_len + i + self.block_size) * itemsize
            ]
            results.append((keys, block_bytes))

        return tuple(results)

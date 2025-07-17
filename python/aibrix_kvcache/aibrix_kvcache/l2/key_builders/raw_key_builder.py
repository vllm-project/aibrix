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
from .key_builder import KeyBuilder


class RawKeyBuilder(KeyBuilder):
    def __init__(self, block_size: int):
        super().__init__(block_size)

    @property
    def signature(self) -> str:
        return "raw"

    def build(
        self, prefix: TokenListView | None, tokens: TokenListView
    ) -> Tuple[Tuple[TokenListView, bytes], ...]:
        assert prefix is None or len(prefix) % self.block_size == 0

        token_size = len(tokens) - len(tokens) % self.block_size
        if token_size < self.block_size:
            return tuple()

        results = []

        if prefix is not None:
            all = prefix + tokens
        else:
            all = tokens
        assert len(all) % self.block_size == 0
        prefix_len = len(prefix) if prefix is not None else 0

        attr_name = f"{self.__class__.__name__}.data_bytes"
        if hasattr(all._meta_, attr_name):
            all_bytes = getattr(all._meta_, attr_name)
        else:
            all_bytes = all._data.tobytes()
            setattr(all._meta_, attr_name, all_bytes)

        itemsize = all.to_numpy().itemsize
        for i in range(0, token_size, self.block_size):
            keys = all[: prefix_len + i + self.block_size]
            block_bytes = all_bytes[
                : (prefix_len + i + self.block_size) * itemsize
            ]
            results.append((keys, block_bytes))

        return tuple(results)

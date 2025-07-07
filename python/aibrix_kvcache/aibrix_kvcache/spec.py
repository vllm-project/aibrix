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

import enum
from dataclasses import dataclass
from typing import List, Tuple

import torch


@dataclass
class KVCacheTensorSpec:
    """The specification of the kv cache tensor.
    Args:
        heads: head ids. To support tensor parallelism.
        layers: layer ids. To support pipeline parallelism.
        head_size: head size.
    """

    heads: List[int]
    layers: List[int]
    head_size: int

    def __post_init__(self):
        if len(self.heads) <= 0:
            raise ValueError("The number of heads must be greater than 0.")
        if len(self.layers) <= 0:
            raise ValueError("The number of layers must be greater than 0.")
        if self.head_size <= 0:
            raise ValueError("Head size must be greater than 0.")


class KVCacheBlockLayout(enum.Enum):
    """The layout of the kv cache block.

    Args:
        NCLD:
            This layout signifies that the shape would be
            [num_tokens, 2 (k & v), num_layers, layer_dim].
            |<------------------------ Block i ------------------------------>|
            |...|<------------------------ Token j ---------------------->|...|
            |...|<------------ K ---------->|<------------ V ------------>|...|
            |...|<-- Layer 0 ->||<-- L 1 ->|...|<-- L 0 ->||<-- L 1 ->|...|...|

        LCND:
            This layout signifies that the shape would be
            [num_layers, 2 (k & v), num_tokens, layer_dim].
            |<------------------------ Block i ------------------------------>|
            |...|<------------------------ Layer j ---------------------->|...|
            |...|<------------ K ---------->|<------------ V ------------>|...|
            |...|<-- Token 0 ->||<-- T 1 ->|...|<-- T 0 ->||<-- T 1 ->|...|...|

    """

    NCLD = enum.auto()
    LCND = enum.auto()


@dataclass
class KVCacheBlockSpec:
    """The specification of the kv cache block.
    Args:
        block_ntokens: The number of tokens in each block.
        block_dtype: The dtype of the kv cache block.
        block_layout: The layout of the kv cache block.
        tensor_spec: The specification of the kv cache tensor.
    """

    block_ntokens: int
    block_dtype: torch.dtype
    block_layout: KVCacheBlockLayout
    tensor_spec: KVCacheTensorSpec

    def __post_init__(self):
        if self.block_ntokens <= 0:
            raise ValueError("block_ntokens must be greater than 0.")
        self.block_nbytes: int = (
            2
            * self.block_ntokens
            * len(self.tensor_spec.layers)
            * len(self.tensor_spec.heads)
            * self.tensor_spec.head_size
            * self.block_dtype.itemsize
        )
        self.block_shape: Tuple[int, ...] = self._get_block_shape()
        self.block_shape_token_dim: int = 0
        if self.block_layout == KVCacheBlockLayout.NCLD:
            self.block_shape_token_dim = 0
        else:
            self.block_shape_token_dim = 2

    def _get_block_shape(self) -> Tuple[int, ...]:
        if self.block_layout == KVCacheBlockLayout.NCLD:
            return (
                self.block_ntokens,
                2,
                len(self.tensor_spec.layers),
                len(self.tensor_spec.heads),
                self.tensor_spec.head_size,
            )
        elif self.block_layout == KVCacheBlockLayout.LCND:
            return (
                len(self.tensor_spec.layers),
                2,
                self.block_ntokens,
                len(self.tensor_spec.heads),
                self.tensor_spec.head_size,
            )


@dataclass
class ModelSpec:
    """Model specification.

    Args:
        max_model_len: Max length of a sequence (including prompt and output).
    """

    max_model_len: int

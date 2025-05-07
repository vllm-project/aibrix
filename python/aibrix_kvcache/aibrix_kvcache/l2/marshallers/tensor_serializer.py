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

import io
import struct
from typing import Sequence, Tuple

import torch

from ...utils import tensor_to_bytes
from . import BaseMarshaller, Marshaller


class TensorSerializer(BaseMarshaller):
    def __init__(self, marshaller: Marshaller | None = None) -> None:
        super().__init__(marshaller)

    def _marshal(
        self, obj: torch.Tensor | Tuple[Sequence[int], torch.Tensor]
    ) -> bytes:
        buffer = io.BytesIO()
        if isinstance(obj, torch.Tensor):
            # 0 indicates no indices
            buffer.write(struct.pack("i", 0))
            buffer.write(tensor_to_bytes(obj))
        else:
            indices, tensor = obj
            # non-zero indicates we have indices before tensor bytes
            buffer.write(struct.pack("i", len(indices)))
            for index in indices:
                buffer.write(struct.pack("i", index))
            buffer.write(tensor_to_bytes(tensor))
        return buffer.getvalue()

    def _unmarshal(
        self, data: bytes
    ) -> torch.Tensor | Tuple[Sequence[int], torch.Tensor]:
        buffer = io.BytesIO(data)
        have_indices = struct.unpack("i", buffer.read(4))[0]

        if have_indices == 0:
            tensor = torch.frombuffer(
                buffer.read(),
                dtype=torch.uint8,
            )
            setattr(tensor, "__kv_cache_offloading_data_ref", data)
            return tensor
        else:
            length_of_indices = have_indices
            indices = [
                struct.unpack("i", buffer.read(4))[0]
                for _ in range(length_of_indices)
            ]
            tensor = torch.frombuffer(
                buffer.read(),
                dtype=torch.uint8,
            )
            setattr(tensor, "__kv_cache_offloading_data_ref", data)
            return (indices, tensor)

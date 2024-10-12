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

import concurrent.futures
import json
import queue
import struct
import threading
from typing import Generator, List, Tuple, Union

import torch

from aibrix.loader.file import LoadFile
from aibrix.loader.utils import TensorMeta, split_continue_tensors


def get_safetensors_metas(file: LoadFile):
    LENTH_COUNT = 8
    length_bytes = file.load_to_bytes(offset=0, count=LENTH_COUNT)
    length_of_header = struct.unpack("<Q", length_bytes)[0]
    base_offset = length_of_header + LENTH_COUNT
    
    meta_bytes = file.load_to_bytes(offset=LENTH_COUNT, count=length_of_header)
    
    tensors_meta = json.loads(meta_bytes.decode("utf-8"))
    del tensors_meta["__metadata__"]
    
    metas: List[TensorMeta] = []
    for name, tensor_meta in tensors_meta.items():
        metas.append(TensorMeta(
            name=name,
            base_offset=base_offset,
            dtype=tensor_meta["dtype"],
            shape=tensor_meta["shape"],
            data_offsets=tensor_meta["data_offsets"],
        ))
    # Ensure tensors chunks could be split continuously
    sorted_metas = sorted(metas, key=lambda obj: obj.real_offset)
    return sorted_metas 


class StreamLoader():
    def __init__(self, 
                 file: LoadFile, 
                 num_thread: int = 32, 
                 use_pinmem: bool = False, 
                 use_direct_io: bool = False):
        self.file = file
        self.num_thread = num_thread
        self.use_pinmem = use_pinmem
        self.use_direct_io = use_direct_io
        # TODO assert file type is safetensors
        self.tensors_metas: List[TensorMeta] = get_safetensors_metas(file)

    def load_safetensors(self, device: Union[torch.device, str] = "cpu"):
        return dict(self.get_weights_iterator(device=device))
    
    
    def _tensors_reader(self, 
                       thread_idx, 
                       barrier, 
                       device: Union[torch.device, str], 
                       tensor_metas: Tuple[TensorMeta], 
                       transfer_out_queue: queue.SimpleQueue[Union[Exception, TensorMeta]]
                       ):
        device = torch.device(device)
        is_cuda = device.type == "cuda"
        # TODO use stream nonblocking IO
        for tensor_meta in tensor_metas:
            tensor_buffer = self.file.load_to_buffer(offset=tensor_meta.real_offset, count=tensor_meta.count)
            tensor = torch.frombuffer(
                tensor_buffer,
                dtype=tensor_meta.dtype
                ).view(tensor_meta.shape)
            if is_cuda:
                tensor = tensor.to(device, non_blocking=True)
            tensor_meta.set_tensor(tensor)
            transfer_out_queue.put(tensor_meta)
    
    def get_weights_iterator(
        self,
        device: Union[torch.device, str] = "cpu"
    ) -> Generator[Tuple[str, torch.Tensor], None, None]:
        tensors_per_reader: List[Tuple[TensorMeta]] = split_continue_tensors(self.tensors_metas, self.num_thread)
        
        effective_num_readers = len(tensors_per_reader)
        self._reader_pool = concurrent.futures.ThreadPoolExecutor(
            max_workers=effective_num_readers,
            thread_name_prefix="SafetensorsReader",
        )
        transfer_out_queue: queue.SimpleQueue[Union[Exception, TensorMeta]] = queue.SimpleQueue() # type: ignore
        futures: List[concurrent.futures.Future] = []

        barrier = threading.Barrier(effective_num_readers)

        for thread_idx, tensor_metas in enumerate(tensors_per_reader):
            future = self._reader_pool.submit(
                self._tensors_reader,
                thread_idx,
                barrier,
                device,
                tensor_metas,
                transfer_out_queue,
            )
            futures.append(future)
            
        try:
            for _ in range(len(self.tensors_metas)):
                tensor_meta: TensorMeta = transfer_out_queue.get(timeout=3600)
                if isinstance(tensor_meta, Exception):
                    raise tensor_meta
                yield tensor_meta.name, tensor_meta.tensor
        except BaseException:
            raise

    def get_weights_iterator_wo_threads(
        self,
        device: Union[torch.device, str] = "cpu"
    ) -> Generator[Tuple[str, torch.Tensor], None, None]:

        device = torch.device(device)
        is_cuda = device.type == "cuda"
        # TODO use stream nonblocking IO
        for tensor_meta in self.tensors_metas:
            tensor_buffer = self.file.load_to_bytes(offset=tensor_meta.real_offset, count=tensor_meta.count)
            tensor = torch.frombuffer(
                tensor_buffer,
                dtype=tensor_meta.dtype
                ).view(tensor_meta.shape)
            
            if is_cuda:
                tensor = tensor.to(device, non_blocking=True)
            # tensor_meta.set_tensor(tensor)
            yield tensor_meta.name, tensor

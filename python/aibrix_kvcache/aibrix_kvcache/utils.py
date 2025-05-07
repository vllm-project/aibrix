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

import os
import time
from contextlib import contextmanager

import torch


def tensor_to_bytes(tensor: torch.Tensor) -> bytes:
    """Convert a PyTorch tensor (CPU/GPU) to raw bytes."""
    if tensor.is_cuda:
        tensor = tensor.cpu()  # Move to CPU if on GPU
    return tensor.view(torch.uint8).numpy().tobytes()


def bytes_to_tensor(data: bytes) -> torch.Tensor:
    """Convert raw bytes to a PyTorch tensor."""
    return torch.frombuffer(data, dtype=torch.uint8)


@contextmanager
def cpu_perf_timer(enabled: bool = True):
    if not enabled:
        yield lambda: 0
    else:
        start = time.perf_counter()
        end = start
        yield lambda: (end - start) * 1000
        end = time.perf_counter()


if torch.cuda.is_available():

    @contextmanager
    def perf_timer():
        start = torch.cuda.Event(enable_timing=True)
        end = torch.cuda.Event(enable_timing=True)

        start.record()
        yield lambda: start.elapsed_time(end)
        end.record()

        end.synchronize()
else:
    perf_timer = cpu_perf_timer


def ensure_dir_exist(path: str) -> None:
    dir = os.path.dirname(path)
    if not os.path.exists(dir):
        os.makedirs(dir)

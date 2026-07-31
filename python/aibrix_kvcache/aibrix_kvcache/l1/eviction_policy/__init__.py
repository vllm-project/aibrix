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

from .base_eviction_policy import BaseEvictionPolicy, Functor
from .fifo import FIFO
from .lru import LRU
from .s3fifo import S3FIFO

__all__ = ["BaseEvictionPolicy", "Functor", "FIFO", "LRU", "S3FIFO"]

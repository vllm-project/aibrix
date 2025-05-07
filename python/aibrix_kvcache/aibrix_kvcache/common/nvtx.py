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

import asyncio
import functools
import hashlib

_NVTX_COLORS = ["green", "blue", "yellow", "purple", "rapids", "red"]


@functools.lru_cache
def _nvtx_get_color(name: str):
    m = hashlib.sha256()
    m.update(name.encode())
    hash_value = int(m.hexdigest(), 16)
    idx = hash_value % len(_NVTX_COLORS)
    return _NVTX_COLORS[idx]


try:
    import nvtx

    def nvtx_range(msg: str, domain: str):
        """
        Decorator and context manager for NVTX profiling.
        Supports both sync and async functions.

        Args:
            msg (str): Message associated with the NVTX range.
            domain (str): NVTX domain.
        """

        def decorator(func):
            @functools.wraps(func)
            async def async_wrapper(*args, **kwargs):
                nvtx.push_range(
                    message=msg, domain=domain, color=_nvtx_get_color(msg)
                )
                try:
                    return await func(*args, **kwargs)
                finally:
                    nvtx.pop_range()

            @functools.wraps(func)
            def sync_wrapper(*args, **kwargs):
                nvtx.push_range(
                    message=msg, domain=domain, color=_nvtx_get_color(msg)
                )
                try:
                    return func(*args, **kwargs)
                finally:
                    nvtx.pop_range()

            return (
                async_wrapper
                if asyncio.iscoroutinefunction(func)
                else sync_wrapper
            )

        return decorator

except ImportError:

    def nvtx_range(msg: str, domain: str):
        def decorator(func):
            return func

        return decorator

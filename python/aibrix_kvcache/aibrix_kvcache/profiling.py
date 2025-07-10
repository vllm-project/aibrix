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
from contextlib import contextmanager

from . import envs

_NVTX_COLORS = ["green", "blue", "yellow", "purple", "rapids", "red"]


@functools.lru_cache
def _nvtx_get_color(name: str):
    m = hashlib.sha256()
    m.update(name.encode())
    hash_value = int(m.hexdigest(), 16)
    idx = hash_value % len(_NVTX_COLORS)
    return _NVTX_COLORS[idx]


@contextmanager
def _nvtx_range_context(msg: str, domain: str):
    nvtx.push_range(message=msg, domain=domain, color=_nvtx_get_color(msg))
    try:
        yield
    finally:
        nvtx.pop_range()


if envs.AIBRIX_KV_CACHE_OL_PROFILING_ENABLED:
    # ================ CPU profiling ===============
    import pyroscope

    pyroscope.configure(
        application_name="aibrix.kvcache",
        server_address=envs.AIBRIX_KV_CACHE_OL_PROFILING_SERVER_ADDRESS,
        sample_rate=100,
        # detect subprocesses started by the main process; default is False
        detect_subprocesses=False,
        oncpu=True,  # report cpu time only; default is True
        # only include traces for threads that are holding on to the Global
        # Interpreter Lock; default is True
        gil_only=True,
        tags={
            "namespace": envs.AIBRIX_KV_CACHE_OL_L2_CACHE_NAMESPACE,
        },
    )

    # create an alias
    tag_wrapper = pyroscope.tag_wrapper

    # ================ NVTX profiling ===============
    try:
        import nvtx

        def nvtx_range(msg: str, domain: str):
            """
            Decorator for NVTX profiling.
            Supports both sync and async functions.

            Args:
                msg (str): Message associated with the NVTX range.
                domain (str): NVTX domain.
            """

            def decorator(func):
                @functools.wraps(func)
                async def async_wrapper(*args, **kwargs):
                    with _nvtx_range_context(msg, domain):
                        return await func(*args, **kwargs)

                @functools.wraps(func)
                def sync_wrapper(*args, **kwargs):
                    with _nvtx_range_context(msg, domain):
                        return func(*args, **kwargs)

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

else:

    @contextmanager
    def tag_wrapper(tags):
        yield

    def nvtx_range(msg: str, domain: str):
        def decorator(func):
            return func

        return decorator

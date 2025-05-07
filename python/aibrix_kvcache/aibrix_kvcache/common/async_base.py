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
from concurrent.futures import Executor


class AsyncBase:
    def __init__(
        self,
        executor: Executor,
        event_loop: asyncio.AbstractEventLoop | None = None,
    ):
        self._executor = executor
        self._event_loop = event_loop

    @property
    def event_loop(self):
        return self._event_loop or asyncio.get_running_loop()

    @staticmethod
    def async_wrap(**kwargs):
        def make_async_method(method_name):
            async def method(self, *args, **kwargs):
                cb = functools.partial(
                    getattr(self, method_name), *args, **kwargs
                )
                return await self.event_loop.run_in_executor(self._executor, cb)

            return method

        def cls_builder(cls):
            for async_method_name, orig_method_name in kwargs.items():
                setattr(
                    cls, async_method_name, make_async_method(orig_method_name)
                )
            return cls

        return cls_builder

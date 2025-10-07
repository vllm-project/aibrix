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
import threading
from concurrent.futures import Future
from typing import Any, Coroutine, Optional, TypeVar

# Define a TypeVar to represent the generic return type.
T = TypeVar("T")


class AsyncLoopThread(threading.Thread):
    """
    A class to run and manage an asyncio event loop in a dedicated thread.
    """

    def __init__(self, name: str) -> None:
        super().__init__(daemon=True)
        self.loop: Optional[asyncio.AbstractEventLoop] = None
        # Use an event to signal when the loop in the new thread is ready.
        self._loop_started = threading.Event()
        self._name = name

    def run(self) -> None:
        """
        This method is the entry point for the new thread. It creates, sets,
        and runs the event loop forever.
        """
        self.loop = asyncio.new_event_loop()
        self.loop.name = self._name  # type: ignore[attr-defined]
        print(f"AsyncLoopThread using: {type(self.loop)}")
        asyncio.set_event_loop(self.loop)

        # Signal that the loop is set up and ready.
        self._loop_started.set()

        # This will run until loop.stop() is called from another thread.
        self.loop.run_forever()

        # Cleanly close the loop when it's stopped.
        self.loop.close()

    def start(self) -> None:
        """
        Starts the thread and blocks until the event loop inside is ready.
        """
        super().start()
        self._loop_started.wait()

    def stop(self) -> None:
        """
        Gracefully stops the event loop and waits for the thread to exit.
        """
        if self.loop:
            # This is a thread-safe way to schedule loop.stop() to be called.
            self.loop.call_soon_threadsafe(self.loop.stop)
        self.join()

    async def run_coroutine(self, coro: Coroutine[Any, Any, T]) -> T:
        """
        Submits a coroutine to the event loop and returns an awaitable Future.
        This method itself MUST be awaited. (For use from async code)
        """
        # 1. Submit the coroutine to the background loop thread-safely.
        #    This returns a concurrent.futures.Future, which is not awaitable.
        concurrent_future = self.submit_coroutine(coro)

        # 2. Wrap the concurrent future in an asyncio future, which IS awaitable
        #    in the current (caller's) event loop.
        asyncio_future = asyncio.wrap_future(concurrent_future)

        # 3. Await the asyncio future and return its result.
        return await asyncio_future

    def submit_coroutine(self, coro: Coroutine[Any, Any, T]) -> Future[T]:
        """
        Submits a coroutine to the event loop from current thread and returns
        a future that can be used to get the result.

        Args:
            coro: The coroutine to execute.
        """
        if not self.loop:
            raise RuntimeError("Loop is not running.")

        return asyncio.run_coroutine_threadsafe(coro, self.loop)

    def create_task(self, coro: Coroutine[Any, Any, T]) -> asyncio.Task[T]:
        """
        Submits a task to the event loop from current thread and returns
        a task that can be handled later

        Args:
            coro: The coroutine to execute.

        Returns:
            A asyncio.Task object for the result.
        """
        if not self.loop:
            raise RuntimeError("Loop is not running.")

        return self.loop.create_task(coro)

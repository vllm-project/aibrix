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

import contextlib
import logging
import threading
from queue import Queue
from typing import Callable, List, Optional, TypeVar

T = TypeVar("T")

logger = logging.getLogger(__name__)


class ObjectPool:
    """
    Thread-safe object pool with size limits and pre-allocation.

    Args:
        klass: The class type for pool objects (alternative to object_creator)
        object_creator: Function that creates new objects
        min_pool_size: Minimum number of objects to maintain
        max_pool_size: Maximum number of objects allowed
    """

    def __init__(
        self,
        *,
        klass=None,
        object_creator: Optional[Callable[[], T]] = None,
        min_pool_size: int = 3,
        max_pool_size: int = 20,
    ):
        if klass is None and object_creator is None:
            raise ValueError("Must provide either klass or object_creator")

        self.object_creator = object_creator or klass
        self.min_pool_size = min_pool_size
        self.max_pool_size = max_pool_size
        self._pool: Queue = Queue(maxsize=max_pool_size)
        self._lock = threading.Lock()
        # Tracks available objects in pool
        self._current_size = 0
        # Tracks total objects (in pool + checked out)
        self._current_capacity = 0

        self._initialize_pool()

    def _initialize_pool(self):
        """Pre-fill the pool with min_pool_size objects."""
        with self._lock, contextlib.suppress(Exception):
            for _ in range(self.min_pool_size):
                self._pool.put(self.object_creator(), block=False)
                self._current_size += 1
                self._current_capacity += 1

    def get(self, n: int = 1) -> Optional[List[T]]:
        """
        Get objects from the pool.

        Args:
            n: The number of objects to get from the pool.

        Returns:
            List of object if available, None if capacity limit reached.
        """
        objs = []
        with self._lock:
            num_checked_out = self._current_capacity - self._current_size
            if num_checked_out + n > self.max_pool_size:
                return None  # Capacity limit reached

            # Prefer to get objects from pool
            num_from_pool = min(n, self._current_size)
            objs.extend([self._pool.get_nowait() for _ in range(num_from_pool)])
            self._current_size -= num_from_pool

            # If not enough objects in pool, create new ones
            if num_from_pool < n:
                objs.extend(
                    [self.object_creator() for _ in range(n - num_from_pool)]
                )
                self._current_capacity += n - num_from_pool

            return objs

    def put(self, objs: T | List[T]) -> None:
        """
        Return an object or objects to the pool.

        Args:
            obj: The object or objects to return
        """
        if objs is None:
            return

        if not isinstance(objs, list):
            objs = [objs]

        with self._lock and contextlib.suppress(Exception):
            for o in objs:
                if self._current_size < self._current_capacity:
                    self._pool.put_nowait(o)
                    self._current_size += 1

    def size(self) -> int:
        """Get current number of available objects in pool."""
        with self._lock:
            return self._current_size

    def capacity(self) -> int:
        """Get total number of objects (checked out + in pool)."""
        with self._lock:
            return self._current_capacity

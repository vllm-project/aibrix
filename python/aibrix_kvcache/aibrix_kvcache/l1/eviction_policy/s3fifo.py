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

from typing import Iterator, cast

from ... import envs
from ...cache_hashable import (
    KVCacheHashable,
    MemoryRegionCacheEntry,
)
from ...memory import MemoryRegion
from ...status import Status, StatusCodes
from .base_eviction_policy import (
    BaseEvictionPolicy,
    BaseEvictionPolicyNode,
    Functor,
)


class S3FIFONode(BaseEvictionPolicyNode):
    __slots__ = ("next", "prev", "queue")

    def __init__(self, payload: KVCacheHashable) -> None:
        super().__init__(payload)
        self.next: S3FIFONode | None = None
        self.prev: S3FIFONode | None = None
        self.queue: S3FIFOQueue | None = None


class S3FIFOQueue:
    def __init__(self) -> None:
        self._head: S3FIFONode | None = None
        self._tail: S3FIFONode | None = None
        self._size_nbytes: int = 0
        self._len: int = 0

    def __len__(self) -> int:
        """Return the number of items in the queue."""
        return self._len

    def nbytes(self) -> int:
        """Return queue's usage in bytes."""
        return self._size_nbytes

    def append(self, node: S3FIFONode) -> None:
        node.next = self._head
        node.prev = None
        node.queue = self
        if self._head:
            self._head.prev = node
        self._head = node
        if self._tail is None:
            self._tail = node
        self._len += 1
        self._size_nbytes += len(node.payload)

    def pop(self) -> S3FIFONode | None:
        node = self._tail
        if node is None:
            return None

        self.erase(node)
        return node

    def erase(self, node: S3FIFONode) -> None:
        if node.prev:
            node.prev.next = node.next
        if node.next:
            node.next.prev = node.prev
        if self._head == node:
            self._head = node.next
        if self._tail == node:
            self._tail = node.prev
        node.next = None
        node.prev = None
        node.queue = None
        self._len -= 1
        self._size_nbytes -= len(node.payload)


class S3FIFO(BaseEvictionPolicy[S3FIFONode]):
    def __init__(
        self,
        capacity_nbytes: int,
        on_put: Functor | None = None,
        on_evict: Functor | None = None,
        on_hot_access: Functor | None = None,
    ) -> None:
        super().__init__(
            name="S3FIFO",
            capacity_nbytes=capacity_nbytes,
            on_put=on_put,
            on_evict=on_evict,
            on_hot_access=on_hot_access,
        )

        self._small_to_main_promo_threshold: int = (
            envs.AIBRIX_KV_CACHE_OL_S3FIFO_SMALL_TO_MAIN_PROMO_THRESHOLD
        )
        self._small_fifo_capacity_ratio: float = (
            envs.AIBRIX_KV_CACHE_OL_S3FIFO_SMALL_FIFO_CAPACITY_RATIO
        )

        self._small_fifo_capacity_nbytes: int = int(
            capacity_nbytes * self._small_fifo_capacity_ratio
        )
        self._main_fifo_capacity_nbytes: int = (
            capacity_nbytes - self._small_fifo_capacity_nbytes
        )

        self._small_fifo: S3FIFOQueue = S3FIFOQueue()
        self._main_fifo: S3FIFOQueue = S3FIFOQueue()
        self._ghost_fifo: S3FIFOQueue = S3FIFOQueue()

        self._check_params()

    def _check_params(self) -> None:
        assert all(
            [
                self._small_to_main_promo_threshold >= 1,
                self._small_to_main_promo_threshold <= 3,
            ]
        ), (
            "AIBRIX_KV_CACHE_OL_S3FIFO_SMALL_TO_MAIN_PROMO_THRESHOLD "
            "must be in [1, 3]"
        )
        assert all(
            [
                self._small_fifo_capacity_ratio > 0,
                self._small_fifo_capacity_ratio < 1,
            ]
        ), (
            "AIBRIX_KV_CACHE_OL_S3FIFO_SMALL_FIFO_CAPACITY_RATIO "
            "must be in (0, 1)"
        )

    def __len__(self) -> int:
        """Return the usage in the eviction policy."""
        return self._small_fifo.nbytes() + self._main_fifo.nbytes()

    def __contains__(self, key: KVCacheHashable) -> bool:
        """Return True if the key is in the eviction policy."""
        return (
            key in self._hashmap
            and self._hashmap[key].queue != self._ghost_fifo
        )

    def __iter__(self) -> Iterator[KVCacheHashable]:
        """Return an iterator over the entries in the eviction policy."""
        return iter(
            {
                key
                for key in self._hashmap
                if self._hashmap[key].queue != self._ghost_fifo
            }
        )

    def put(
        self,
        entry: MemoryRegionCacheEntry,
    ) -> Status:
        node = self._hashmap.pop(entry, None)
        if node is not None:
            if node.queue == self._ghost_fifo:
                # We hit on a ghost entry, let's promote it to main fifo.

                # Remove it from ghost fifo
                self._ghost_fifo.erase(node)

                # Assign new entry
                node.payload = entry
                node.hotness = 0

                # Insert into main fifo
                self._main_fifo.append(node)

                if self._on_hot_access:
                    entry = cast(MemoryRegionCacheEntry, node.payload)
                    entry.ref_up()
                    self._on_hot_access(entry)
            else:
                # Hit on small or main fifo
                node.payload.ref_down()  # type: ignore

                node.payload = entry
        else:
            # New key always goes to small fifo
            node = S3FIFONode(entry)
            self._small_fifo.append(node)
            if self._on_put is not None:
                entry = cast(MemoryRegionCacheEntry, node.payload)
                entry.ref_up()
                self._on_put(entry)

        self._hashmap[entry] = node

        if len(self) > self._capacity_nbytes:
            self.evict()

        return Status.ok()

    def get(
        self,
        key: KVCacheHashable,
    ) -> Status[MemoryRegion]:
        if key not in self._hashmap:
            return Status(StatusCodes.NOT_FOUND)

        node = self._hashmap[key]

        if node.queue == self._ghost_fifo:
            # Hit on a ghost entry, return None
            return Status(StatusCodes.NOT_FOUND)

        entry = cast(MemoryRegionCacheEntry, node.payload)
        # Invoke on_hot_access callback on the item that will be promoted
        # to main fifo
        if all(
            [
                node.queue == self._small_fifo,
                node.hotness == self._small_to_main_promo_threshold - 1,
                self._on_hot_access is not None,
            ]
        ):
            entry.ref_up()
            self._on_hot_access(entry)  # type: ignore

        node.hotness = min(node.hotness + 1, 3)
        entry.ref_up()
        return Status.ok(entry._mr)

    def delete(self, key: KVCacheHashable) -> Status:
        node = self._hashmap.pop(key, None)
        if node:
            assert node.queue is not None
            node.queue.erase(node)
            if node.queue != self._ghost_fifo:
                node.payload.ref_down()  # type: ignore

        return Status.ok()

    def evict(self, nbytes: int = 1) -> Status:
        target_usage = max(0, min(len(self), self.capacity_nbytes) - nbytes)
        while len(self) > target_usage:
            if (
                self._small_fifo.nbytes() > self._small_fifo_capacity_nbytes
                or len(self._main_fifo) == 0
            ):
                self._evict_one_from_small_fifo()
            else:
                self._evict_one_from_main_fifo()

        return Status.ok()

    def assert_consistency(self) -> None:
        total_in_list = 0
        for queue in [self._small_fifo, self._main_fifo, self._ghost_fifo]:
            curr = queue._head
            while curr is not None and curr.next != queue._head:
                total_in_list += 1
                key = curr.payload
                assert self._hashmap.get(key, None) == curr
                assert self._hashmap[key].queue == queue
                curr = curr.next
        assert total_in_list == len(
            self._hashmap
        ), f"{total_in_list} != {len(self._hashmap)}"

    def _evict_one_from_small_fifo(self) -> None:
        node = self._small_fifo.pop()
        if node is None:
            return

        if node.hotness >= self._small_to_main_promo_threshold:
            # Promote to main fifo
            node.hotness = 0
            self._main_fifo.append(node)
            # Trigger eviction on main fifo if needed
            if self._main_fifo.nbytes() > self._main_fifo_capacity_nbytes:
                self._evict_one_from_main_fifo()
        else:
            entry = cast(MemoryRegionCacheEntry, node.payload)
            del self._hashmap[entry]

            if self._on_evict:
                self._on_evict(entry)
            else:
                entry.ref_down()
            # Insert into ghost fifo
            node.hotness = -1
            node.payload = entry.cache_key()
            self._ghost_fifo.append(node)
            # Update the key in hashmap
            self._hashmap[node.payload] = node
            # Trigger eviction on ghost fifo if needed
            if self._ghost_fifo.nbytes() > self._main_fifo_capacity_nbytes:
                self._evict_ghost_fifo()

    def _evict_one_from_main_fifo(self) -> None:
        node = self._main_fifo.pop()
        if node is None:
            return

        if node.hotness >= 1:
            node.hotness -= 1
            self._main_fifo.append(node)
        else:
            entry = cast(MemoryRegionCacheEntry, node.payload)
            del self._hashmap[node.payload]

            if self._on_evict:
                self._on_evict(entry)
            else:
                entry.ref_down()

    def _evict_ghost_fifo(self) -> None:
        while self._ghost_fifo.nbytes() > self._main_fifo_capacity_nbytes:
            node = self._ghost_fifo.pop()
            assert node is not None
            del self._hashmap[node.payload]

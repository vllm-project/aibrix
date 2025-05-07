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

from typing import Hashable, Iterator, Tuple

from ... import envs
from ...memory import RefCountedObj
from ...status import Status, StatusCodes
from .base_eviction_policy import (
    BaseEvictionPolicy,
    BaseEvictionPolicyNode,
    Functor,
    V,
)


class S3FIFONode(BaseEvictionPolicyNode[V]):
    __slots__ = ("next", "prev", "queue")

    def __init__(self, key: Hashable, value: V):
        super().__init__(key, value)
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


class S3FIFO(BaseEvictionPolicy[S3FIFONode, V]):
    def __init__(
        self,
        capacity: int,
        evict_size: int = 1,
        on_put: Functor | None = None,
        on_evict: Functor | None = None,
        on_hot_access: Functor | None = None,
    ) -> None:
        super().__init__(
            name="S3FIFO",
            capacity=capacity,
            evict_size=evict_size,
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

        self._small_fifo_capacity: int = int(
            capacity * self._small_fifo_capacity_ratio
        )
        self._main_fifo_capacity: int = capacity - self._small_fifo_capacity

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
        """Return the number of items in the eviction policy."""
        return len(self._small_fifo) + len(self._main_fifo)

    def __contains__(self, key: Hashable) -> bool:
        """Return True if the key is in the eviction policy."""
        return (
            key in self._hashmap
            and self._hashmap[key].queue != self._ghost_fifo
        )

    def __iter__(self) -> Iterator[Hashable]:
        """Return an iterator over the keys in the eviction policy."""
        return iter(
            {
                key
                for key in self._hashmap
                if self._hashmap[key].queue != self._ghost_fifo
            }
        )

    def items(self) -> Iterator[Tuple[Hashable, V]]:
        """Return an iterator over the key-value pairs in the
        eviction policy.
        """
        return iter(
            {
                (key, node.value)
                for key, node in self._hashmap.items()
                if self._hashmap[key].queue != self._ghost_fifo
            }
        )

    def keys(self) -> Iterator[Hashable]:
        """Return an iterator over the keys in the eviction policy."""
        return iter(self)

    def values(self) -> Iterator[V]:
        """Return an iterator over the values in the eviction policy."""
        return iter({value for _, value in self.items()})

    def put(
        self,
        key: Hashable,
        value: V,
    ) -> Status:
        if key in self._hashmap:
            node = self._hashmap[key]

            if node.queue == self._ghost_fifo:
                # We hit on a ghost entry, let's promote it to main fifo.

                # Remove it from ghost fifo
                self._ghost_fifo.erase(node)

                # Assign new value
                node.value = value
                node.hotness = 0

                # Insert into main fifo
                self._main_fifo.append(node)

                if self._on_hot_access:
                    if isinstance(node.value, RefCountedObj):
                        node.value.ref_up()
                    self._on_hot_access(node.key, node.value)
            else:
                # Replace the value
                if node.value is not None and isinstance(
                    node.value, RefCountedObj
                ):
                    node.value.ref_down()
                node.value = value
        else:
            # New key always goes to small fifo
            node = S3FIFONode(key, value)
            self._hashmap[key] = node
            self._small_fifo.append(node)
            if self._on_put is not None:
                if isinstance(node.value, RefCountedObj):
                    node.value.ref_up()
                self._on_put(node.key, node.value)

        if len(self) > self._capacity:
            self.evict(self.evict_size)

        return Status.ok()

    def get(
        self,
        key: Hashable,
    ) -> Status[V]:
        if key not in self._hashmap:
            return Status(StatusCodes.NOT_FOUND)

        node = self._hashmap[key]

        if node.queue == self._ghost_fifo:
            # Hit on a ghost entry, return None
            return Status(StatusCodes.NOT_FOUND)

        # Invoke on_hot_access callback on the item that will be promoted
        # to main fifo
        if all(
            [
                node.queue == self._small_fifo,
                node.hotness == self._small_to_main_promo_threshold - 1,
                self._on_hot_access is not None,
            ]
        ):
            if isinstance(node.value, RefCountedObj):
                node.value.ref_up()
            self._on_hot_access(node.key, node.value)  # type: ignore[misc]

        node.hotness = min(node.hotness + 1, 3)
        if isinstance(node.value, RefCountedObj):
            node.value.ref_up()
        return Status.ok(node.value)

    def delete(self, key: Hashable) -> Status:
        node = self._hashmap.pop(key, None)
        if node:
            assert node.queue is not None
            node.queue.erase(node)

            if node.value is not None:
                if isinstance(node.value, RefCountedObj):
                    node.value.ref_down()
                node.value = None

        return Status.ok()

    def evict(self, size: int = 1) -> Status:
        orig_usage = len(self)
        target_usage = max(0, orig_usage - size)

        curr_usage = orig_usage
        while curr_usage > target_usage:
            if (
                len(self._small_fifo) > self._small_fifo_capacity
                or len(self._main_fifo) == 0
            ):
                self._evict_one_from_small_fifo()
            else:
                self._evict_one_from_main_fifo()

            curr_usage = len(self)

        return Status.ok()

    def assert_consistency(self) -> None:
        total_in_list = 0
        for queue in [self._small_fifo, self._main_fifo, self._ghost_fifo]:
            curr = queue._head
            while curr is not None and curr.next != queue._head:
                total_in_list += 1
                assert self._hashmap.get(curr.key, None) == curr
                assert self._hashmap[curr.key].queue == queue
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
            if len(self._main_fifo) > self._main_fifo_capacity:
                self._evict_one_from_main_fifo()
        else:
            if self._on_evict:
                self._on_evict(node.key, node.value)
            elif isinstance(node.value, RefCountedObj):
                node.value.ref_down()
            # Insert into ghost fifo
            node.hotness = -1
            node.value = None
            self._ghost_fifo.append(node)
            # Trigger eviction on ghost fifo if needed
            if len(self._ghost_fifo) > self._main_fifo_capacity:
                self._evict_ghost_fifo()

    def _evict_one_from_main_fifo(self) -> None:
        node = self._main_fifo.pop()
        if node is None:
            return

        if node.hotness >= 1:
            node.hotness -= 1
            self._main_fifo.append(node)
        else:
            if self._on_evict:
                self._on_evict(node.key, node.value)
            elif isinstance(node.value, RefCountedObj):
                node.value.ref_down()
            del self._hashmap[node.key]

    def _evict_ghost_fifo(self) -> None:
        while len(self._ghost_fifo) > self._main_fifo_capacity:
            node = self._ghost_fifo.pop()
            assert node is not None
            assert node.key is not None
            del self._hashmap[node.key]
            if node.value is not None:
                del node.value
                node.value = None

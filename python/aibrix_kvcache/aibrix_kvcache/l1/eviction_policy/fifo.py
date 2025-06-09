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

from ...cache_hashable import KVCacheHashable
from ...memory import MemoryRegion
from ...status import Status, StatusCodes
from .base_eviction_policy import (
    BaseEvictionPolicy,
    BaseEvictionPolicyNode,
    Functor,
)


class FIFONode(BaseEvictionPolicyNode):
    __slots__ = ("next", "prev")

    def __init__(self, key: KVCacheHashable, value: MemoryRegion):
        super().__init__(key, value)
        self.next: FIFONode | None = None
        self.prev: FIFONode | None = None


class FIFO(BaseEvictionPolicy[FIFONode]):
    def __init__(
        self,
        capacity_nbytes: int,
        on_put: Functor | None = None,
        on_evict: Functor | None = None,
        on_hot_access: Functor | None = None,
    ) -> None:
        super().__init__(
            name="FIFO",
            capacity_nbytes=capacity_nbytes,
            on_put=on_put,
            on_evict=on_evict,
            on_hot_access=on_hot_access,
        )
        self._head: FIFONode | None = None
        self._tail: FIFONode | None = None

    def put(
        self,
        key: KVCacheHashable,
        value: MemoryRegion,
    ) -> Status:
        if key in self._hashmap:
            node = self._hashmap[key]

            node_value_len = len(node.value)
            node.value.ref_down()
            usage = len(value) - node_value_len

            node.value = value
            node.hotness = 0
        else:
            node = FIFONode(key, value)
            self._hashmap[key] = node
            self._prepend_to_head(node)
            usage = len(value)
            if self._on_put is not None:
                value.ref_up()
                self._on_put(key, value)

        self._used_nbytes += usage

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
        mr = node.value
        # The item becomes hot after the first access
        if node.hotness == 0 and self._on_hot_access:
            mr.ref_up()
            self._on_hot_access(key, mr)

        node.hotness = 1
        mr.ref_up()
        return Status.ok(mr)

    def delete(self, key: KVCacheHashable) -> Status:
        node = self._hashmap.pop(key, None)
        if node:
            self._used_nbytes -= len(node.value)
            self._remove_from_list(node)
            node.value.ref_down()

        return Status.ok()

    def evict(self, nbytes: int = 1) -> Status:
        target_usage = max(0, min(len(self), self.capacity_nbytes) - nbytes)
        while len(self) > target_usage:
            if not self._tail:
                break
            if self._on_evict:
                key = self._tail.key
                mr = self._tail.value
                mr.ref_up()
                self._on_evict(key, mr)
            evicted_node = self._tail
            self.delete(evicted_node.key)
        return Status.ok()

    def assert_consistency(self) -> None:
        total_in_list = 0
        curr = self._head
        while curr is not None and curr.next != self._head:
            total_in_list += 1
            assert self._hashmap.get(curr.key, None) == curr
            curr = curr.next
        assert total_in_list == len(
            self._hashmap
        ), f"{total_in_list} != {len(self._hashmap)}"

    def _prepend_to_head(self, node: FIFONode) -> None:
        node.next = self._head
        node.prev = None
        if self._head:
            self._head.prev = node
        self._head = node
        if self._tail is None:
            self._tail = node

    def _remove_from_list(self, node: FIFONode) -> None:
        if node.prev:
            node.prev.next = node.next
        if node.next:
            node.next.prev = node.prev
        if self._head == node:
            self._head = node.next
        if self._tail == node:
            self._tail = node.prev

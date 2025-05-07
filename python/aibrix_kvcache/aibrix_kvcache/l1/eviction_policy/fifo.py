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

from typing import Hashable

from ...memory import RefCountedObj
from ...status import Status, StatusCodes
from .base_eviction_policy import (
    BaseEvictionPolicy,
    BaseEvictionPolicyNode,
    Functor,
    V,
)


class FIFONode(BaseEvictionPolicyNode[V]):
    __slots__ = ("next", "prev")

    def __init__(self, key: Hashable, value: V):
        super().__init__(key, value)
        self.next: FIFONode | None = None
        self.prev: FIFONode | None = None


class FIFO(BaseEvictionPolicy[FIFONode, V]):
    def __init__(
        self,
        capacity: int,
        evict_size: int = 1,
        on_put: Functor | None = None,
        on_evict: Functor | None = None,
        on_hot_access: Functor | None = None,
    ) -> None:
        super().__init__(
            name="FIFO",
            capacity=capacity,
            evict_size=evict_size,
            on_put=on_put,
            on_evict=on_evict,
            on_hot_access=on_hot_access,
        )
        self._head: FIFONode | None = None
        self._tail: FIFONode | None = None

    def put(
        self,
        key: Hashable,
        value: V,
    ) -> Status:
        if key in self._hashmap:
            node = self._hashmap[key]
            if node.value is not None:
                if isinstance(node.value, RefCountedObj):
                    node.value.ref_down()
                node.value = None

            node.value = value
            node.hotness = 0
        else:
            node = FIFONode(key, value)
            self._hashmap[key] = node
            self._prepend_to_head(node)
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
        # The item becomes hot after the first access
        if node.hotness == 0 and self._on_hot_access:
            if isinstance(node.value, RefCountedObj):
                node.value.ref_up()
            self._on_hot_access(node.key, node.value)

        node.hotness = 1
        if isinstance(node.value, RefCountedObj):
            node.value.ref_up()
        return Status.ok(node.value)

    def delete(self, key: Hashable) -> Status:
        node = self._hashmap.pop(key, None)
        if node:
            self._remove_from_list(node)

            if node.value is not None:
                if isinstance(node.value, RefCountedObj):
                    node.value.ref_down()
                node.value = None

        return Status.ok()

    def evict(self, size: int = 1) -> Status:
        for _ in range(size):
            if not self._tail:
                break
            if self._on_evict:
                if isinstance(self._tail.value, RefCountedObj):
                    self._tail.value.ref_up()
                self._on_evict(self._tail.key, self._tail.value)
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

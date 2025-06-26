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

from abc import ABC, abstractmethod
from typing import Callable, Dict, Generic, Iterator, Tuple, TypeVar

from ...cache_hashable import KVCacheHashable
from ...memory import MemoryRegion
from ...status import Status
from ...utils import human_readable_bytes

N = TypeVar("N", bound="BaseEvictionPolicyNode")

Functor = Callable[
    [KVCacheHashable, MemoryRegion],
    None,
]


class BaseEvictionPolicyNode(ABC):
    __slots__ = ("key", "value", "hotness")

    def __init__(self, key: KVCacheHashable, value: MemoryRegion):
        self.key: KVCacheHashable = key
        self.value: MemoryRegion = value
        self.hotness: int = 0

    def __repr__(self) -> str:
        derived_members = ", ".join(
            {f"{slot}={getattr(self, slot, None)}" for slot in self.__slots__}
        )
        return (
            f"Node(key={self.key}, value={self.value}, "
            f"hotness={self.hotness}, {derived_members})"
        )

    def __str__(self) -> str:
        return self.__repr__()


class BaseEvictionPolicy(Generic[N]):
    """Base class for eviction policies."""

    def __init__(
        self,
        name: str,
        capacity_nbytes: int,
        on_put: Functor | None = None,
        on_evict: Functor | None = None,
        on_hot_access: Functor | None = None,
    ) -> None:
        """Initialize the eviction policy.
        Args:
            name (str): The name of the eviction policy.
            capacity_nbytes (int): The capacity of the eviction policy in bytes.
            on_put (Functor): The put function to call when putting new items.
                              Defaults to None.
            on_evict (Functor): The evict function to call when evicting items.
                                Defaults to None.
            on_hot_access (Functor): The callback function to call when a cache
                                     item becomes hot. Defaults to None.
        """

        self._name: str = name
        self._capacity_nbytes: int = capacity_nbytes
        self._used_nbytes: int = 0
        self._on_put: Functor | None = on_put
        self._on_evict: Functor | None = on_evict
        self._on_hot_access: Functor | None = on_hot_access

        self._hashmap: Dict[KVCacheHashable, N] = {}

    @staticmethod
    def create(name: str, *args, **kwargs) -> "BaseEvictionPolicy":
        """Return the eviction policy with the given name."""
        if name == "LRU":
            from .lru import LRU

            return LRU(*args, **kwargs)
        elif name == "FIFO":
            from .fifo import FIFO

            return FIFO(*args, **kwargs)
        elif name == "S3FIFO":
            from .s3fifo import S3FIFO

            return S3FIFO(*args, **kwargs)
        else:
            raise ValueError(f"Unknown eviction policy: {name}")

    @property
    def name(self) -> str:
        """Return the name of the eviction policy."""
        return self._name

    @property
    def capacity_nbytes(self) -> int:
        """Return the capacity of the eviction policy in bytes."""
        return self._capacity_nbytes

    def __del__(self) -> None:
        for _, node in self._hashmap.items():
            if node.value is not None:
                node.value.ref_down()

    def __len__(self) -> int:
        """Return the usage in the eviction policy."""
        return self._used_nbytes

    def __contains__(self, key: KVCacheHashable) -> bool:
        """Return True if the key is in the eviction policy."""
        return key in self._hashmap

    def __getitem__(self, key: KVCacheHashable) -> MemoryRegion:
        """Return the value of the key."""
        status = self.get(key)
        if not status.is_ok():
            raise KeyError(key)
        return status.get()

    def __setitem__(self, key: KVCacheHashable, value: MemoryRegion) -> None:
        """Set the value of the key."""
        self.put(key, value)

    def __delitem__(self, key: KVCacheHashable) -> None:
        """Delete the key."""
        self.delete(key)

    def __iter__(self) -> Iterator[KVCacheHashable]:
        """Return an iterator over the entries in the eviction policy."""
        return iter(self._hashmap.keys())

    def set_on_put_callback(self, functor: Functor) -> None:
        """Set the callback function to call when putting new items."""
        self._on_put = functor

    def set_on_evict_callback(self, functor: Functor) -> None:
        """Set the callback function to call when evicting items."""
        self._on_evict = functor

    def set_on_hot_access_callback(self, functor: Functor) -> None:
        """Set the callback function to call when a cache
        item becomes hot.
        """
        self._on_hot_access = functor

    def items(self) -> Iterator[Tuple[KVCacheHashable, MemoryRegion]]:
        """Return an iterator over the key-value pairs in the
        eviction policy.
        """
        return iter({(key, node.value) for key, node in self._hashmap.items()})

    def keys(self) -> Iterator[KVCacheHashable]:
        """Return an iterator over the keys in the eviction policy."""
        return iter(self._hashmap.keys())

    def values(self) -> Iterator[MemoryRegion]:
        """Return an iterator over the values in the eviction policy."""
        return iter({node.value for node in self._hashmap.values()})

    def __repr__(self) -> str:
        return (
            f"{self._name}("
            f"capacity_nbytes={human_readable_bytes(self._capacity_nbytes)}"
            f", size={human_readable_bytes(len(self))}"
            f")"
        )

    def __str__(self) -> str:
        return self.__repr__()

    @abstractmethod
    def put(self, key: KVCacheHashable, value: MemoryRegion) -> Status:
        """Put a key into the eviction policy.
        Args:
            key (KVCacheHashable): The key of the item.
            value: The value of the item.
        Returns:
            Status: The status of the operation.
        """
        raise NotImplementedError

    @abstractmethod
    def get(
        self,
        key: KVCacheHashable,
    ) -> Status[MemoryRegion]:
        """Get the cache entry corresponding to key from the eviction policy.
        Args:
            key (KVCacheHashable): The key of the item.
        Returns:
            Status: The status of the operation.
        """
        raise NotImplementedError

    @abstractmethod
    def delete(self, key: KVCacheHashable) -> Status:
        """Delete an entry from the eviction policy.
        Args:
            key (KVCacheHashable): The key of the item.
        Returns:
            Status: The status of the operation.
        """
        raise NotImplementedError

    @abstractmethod
    def evict(self, nbytes: int = 1) -> Status:
        """Evict entries from the eviction policy.
        Args:
            nbytes (int, optional): The size to be evicted in bytes.
        """
        raise NotImplementedError

    @abstractmethod
    def assert_consistency(self) -> None:
        """Check the consistency of the eviction policy. Only
        for test purpose.
        """
        raise NotImplementedError

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

import weakref
from typing import Dict, List, Type, TypeVar

T = TypeVar("T", bound="CachedPyObjectBase")


class CachedPyObjectBase:
    """Base class that provides per-class object caching for derived classes.

    Each derived class maintains its own free list of instances for reuse.
    """

    # Dictionary to hold free lists for each class
    _free_lists: Dict[
        Type["CachedPyObjectBase"], List["CachedPyObjectBase"]
    ] = {}
    _free_list_max_sizes: Dict[Type["CachedPyObjectBase"], int] = {}

    def __new__(cls: Type[T], *args, **kwargs):
        """Custom __new__ that checks the class's free list before creating
        a new instance.
        """
        if cls not in cls._free_lists:
            # Initialize free list for this class if it doesn't exist
            cls._free_lists[cls] = []
            cls._free_list_max_sizes[cls] = 80

        free_list = cls._free_lists[cls]

        if free_list:
            # Reuse an instance from the free list
            instance = free_list.pop()
            # Clear any weakrefs that might be lingering
            if hasattr(instance, "__weakref__"):
                for ref in weakref.getweakrefs(instance):
                    weakref.ref(instance).__callback__(ref)
            return instance

        # No available instances in free list, create a new one
        return super().__new__(cls)

    def __init__(self, *args, **kwargs):
        """Initialize the instance.

        Note: __init__ is called both for new instances and recycled ones.
        """
        super().__init__(*args, **kwargs)

    @classmethod
    def set_free_list_max_size(cls: Type[T], size: int) -> None:
        """Set the maximum size of the free list for this class."""
        if cls not in cls._free_list_max_sizes:
            cls._free_lists[cls] = []
        cls._free_list_max_sizes[cls] = size

    @classmethod
    def _add_to_free_list(cls: Type[T], instance: T) -> None:
        """Add an instance to this class's free list for potential reuse."""
        if cls not in cls._free_lists:
            cls._free_lists[cls] = []

        free_list = cls._free_lists[cls]
        max_size = cls._free_list_max_sizes.get(cls, 80)

        if len(free_list) < max_size:
            # Clear the instance's state before adding to free list
            if hasattr(instance, "__dict__"):
                instance.__dict__.clear()
            free_list.append(instance)

    def __del__(self):
        """Add the instance to its class's free list when it's garbage
        collected.
        """
        self.__class__._add_to_free_list(self)

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

import threading
from abc import ABC, abstractmethod


class RefCountedObj(ABC):
    def __init__(self):
        """Initializes the RefCountedObj."""
        self.ref_count = 1
        self._lock = threading.Lock()

    def ref_up(self):
        """Increments the reference count."""
        with self._lock:
            self.ref_count += 1

    def ref_down(self):
        """Decrements the reference count and invokes destroy if the count
        reaches zero.
        """
        with self._lock:
            if self.ref_count == 0:
                raise ValueError("Reference count is already zero.")
            self.ref_count -= 1
            if self.ref_count == 0:
                self.destroy_unsafe()

    @abstractmethod
    def destroy_unsafe(self):
        """Destroys the object."""
        pass

    def __repr__(self):
        with self._lock:
            return f"RefCountedObj(ref_count={self.ref_count})"

    def __str__(self) -> str:
        return self.__repr__()

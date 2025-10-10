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

from threading import RLock
from types import TracebackType


class _DummyLock:
    """A dummy lock that does nothing"""

    __slots__ = ()

    def acquire(self, blocking: bool = True, timeout: float = -1) -> bool:
        return True

    def release(self) -> None:
        return

    __enter__ = acquire

    def __exit__(
        self,
        t: type[BaseException] | None,
        v: BaseException | None,
        tb: TracebackType | None,
    ) -> None:
        return


class ConditionalLock:
    """Conditional lock that makes lock operations no-ops when `thread_safe`
    is False
    """

    __slots__ = ("_lock", "__enter__", "__exit__", "acquire", "release")

    def __init__(self, thread_safe: bool):
        self._lock = RLock() if thread_safe else _DummyLock()

        # method binding
        self.__enter__ = self._lock.__enter__  # type: ignore
        self.__exit__ = self._lock.__exit__  # type: ignore
        self.acquire = self._lock.acquire  # type: ignore
        self.release = self._lock.release  # type: ignore

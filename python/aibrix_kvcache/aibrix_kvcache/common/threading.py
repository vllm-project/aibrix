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


class ConditionalLock:
    """Conditional lock that makes lock operations no-ops when `thread_safe`
    is False
    """

    def __init__(self, thread_safe: bool):
        self._lock = RLock() if thread_safe else None

    def __enter__(self):
        if self._lock is not None:
            self._lock.__enter__()
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        if self._lock is not None:
            self._lock.__exit__(exc_type, exc_val, exc_tb)

    def acquire(self, *args, **kwargs):
        if self._lock is not None:
            return self._lock.acquire(*args, **kwargs)
        return True

    def release(self):
        if self._lock is not None:
            self._lock.release()

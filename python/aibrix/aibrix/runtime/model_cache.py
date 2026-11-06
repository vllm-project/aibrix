# Copyright 2026 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Bounded asynchronous discovery; cache hints never block runtime snapshots."""

import threading
import time
from typing import Callable, List, Optional

from aibrix.logger import init_logger

logger = init_logger(__name__)


def scan_prepared_artifacts() -> List[str]:
    # One registered-result index for all backends; never inventory model caches.
    from aibrix.downloader.metadata import discover_download_metadata

    return discover_download_metadata()


class PreparedCacheIndex:
    """One in-flight scan per runtime, with a bounded age for returned hints.

    The runtime supervisor periodically requests refreshes. Snapshot-only
    callers can also trigger them, without owning a persistent background loop.
    """

    def __init__(
        self,
        scanner: Callable[[], List[str]] = scan_prepared_artifacts,
        interval: float = 10.0,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        if interval <= 0:
            raise ValueError("cache refresh interval must be positive")
        self._scanner = scanner
        self._interval = interval
        self._clock = clock
        self._lock = threading.Lock()
        self._artifacts: List[str] = []
        self._completed: Optional[float] = None
        self._scanning = False

    def get_artifacts(self) -> List[str]:
        self.refresh_if_due()
        with self._lock:
            if (
                self._completed is None
                or self._clock() - self._completed >= 2 * self._interval
            ):
                return []
            return list(self._artifacts)

    def refresh_if_due(self) -> None:
        with self._lock:
            if self._scanning or (
                self._completed is not None
                and self._clock() - self._completed < self._interval
            ):
                return
            self._scanning = True
        try:
            threading.Thread(
                target=self._refresh, name="aibrix-model-cache-scan", daemon=True
            ).start()
        except Exception:
            with self._lock:
                self._scanning = False
                self._artifacts = []
                self._completed = self._clock()
            logger.warning("could not start model metadata scan", exc_info=True)

    def _refresh(self) -> None:
        try:
            artifacts = sorted(set(self._scanner()))
        except Exception:
            logger.warning("model cache scan failed", exc_info=True)
            artifacts = []
        with self._lock:
            self._artifacts = artifacts
            self._completed = self._clock()
            self._scanning = False

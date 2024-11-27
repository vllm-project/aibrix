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

import logging
import math
import sys
from datetime import datetime
from typing import Callable, Iterable, List, Optional, Protocol, Tuple, Union

from incdbscan import IncrementalDBSCAN

from aibrix.gpu_optimizer.utils import DelayedLog

from .helpers import Centeroid, DataPoints

logger = logging.getLogger("aibrix.gpuoptimizer.clusterer")


class Clusterer(Protocol):
    def insert(self, points):
        """Pass in a list of data points to be clustered"""

    def reset(self):
        """Reset the clusterer"""

    def get_cluster_labels(
        self, points: DataPoints, uncategorized: Optional[List] = None
    ) -> Tuple[Iterable[int], Iterable[Centeroid]]:
        """Get the cluster labels for the given data points"""

    @property
    def length(self):
        """Get the number of data points in the clusterer"""


class DBSCANClusterer:
    def __init__(self, eps: float, min_pts: int):
        self.eps = eps
        self.min_pts = min_pts
        self.reset()

    def insert(self, points: DataPoints):
        self.clusterer.insert(points.signatures)
        self._length += len(points)

    def reset(self):
        self.clusterer = IncrementalDBSCAN(eps=self.eps, min_pts=self.min_pts)
        self._length = 0
        self.created = datetime.now().timestamp()

    def clone(self):
        return DBSCANClusterer(self.eps, self.min_pts)

    def get_cluster_labels(
        self, points: DataPoints, uncategorized: Optional[List] = None
    ) -> Tuple[Iterable[int], Iterable[Centeroid]]:
        labels = self.clusterer.get_cluster_labels(points.signatures)
        centers = {}
        start_label = sys.maxsize
        for i, label in enumerate(labels):
            if math.isnan(label):
                continue
            if label < 0:
                if uncategorized is not None:
                    uncategorized.append(points.datapoint(i))
                continue
            start_label = min(start_label, label)
            if label not in centers:
                centers[label] = Centeroid()
            if len(centers) > 10:
                print(f"unepxected label:{label}")
            centers[label].add(points.datapoint(i))
        # Try fixing label index.
        if start_label == sys.maxsize:
            start_label = 0
        if start_label != 0:
            for i, label in enumerate(labels):
                if label >= 0:
                    labels[i] -= start_label
        return labels, centers.values()

    @property
    def length(self):
        return self._length


class MovingDBSCANClusterer:
    """MovingCluster uses extra buffer space to store a moving DBSCAN cluster"""

    def __init__(
        self,
        eps: float,
        min_pts: int,
        buffer_size=4,
        window: Union[int, float, Callable[[DBSCANClusterer], bool]] = 4000,
    ):
        if isinstance(window, int):
            self.window_cb = self._get_points_window_cb(window)
        elif isinstance(window, float):
            self.window_cb = self._get_time_window_cb(window)
        else:
            self.window_cb = window

        self.buffer_size = buffer_size
        self.frontier = 0
        self.clusterers = [DBSCANClusterer(eps, min_pts)]
        self._reason = None

    def validate(self) -> bool:
        """Do necessary window rotating and return if data refreshing is necessary"""
        current = (self.frontier + 1) % len(self.clusterers)
        if not self.window_cb(self.clusterers[self.frontier]):
            return False

        # Reset next slot in buffer.
        if len(self.clusterers) < self.buffer_size:
            self.clusterers.append(self.clusterers[current].clone())
            self.frontier = len(self.clusterers) - 1
            logger.debug("test")
            logger.debug(
                "moving buffer created: %s, buffers: %s",
                self._reason,
                DelayedLog(lambda: [cluster.length for cluster in self.clusterers]),
            )
            return False
        else:
            self.clusterers[current].reset()
            self.frontier = current
            current = (current + 1) % self.buffer_size
            logger.debug(
                "moving buffer created: %s, now available: %s, buffers: %s",
                self._reason,
                self.clusterer.length,
                DelayedLog(lambda: [cluster.length for cluster in self.clusterers]),
            )
            return True
            # data.trim_head(-cluster['clusterers'][0][current].length)

    def insert(self, points: DataPoints):
        for clusterer in self.clusterers:
            clusterer.insert(points)

    def reset(self):
        self.clusterers = [self.clusterers[0].clone()]
        self.frontier = 0

    def get_cluster_labels(
        self, points: DataPoints, uncategorized: Optional[List] = None
    ) -> Tuple[Iterable[int], Iterable[Centeroid]]:
        return self.clusterer.get_cluster_labels(points, uncategorized=uncategorized)

    @property
    def length(self):
        return self.clusterer.length

    @property
    def clusterer(self):
        return self.clusterers[(self.frontier + 1) % len(self.clusterers)]

    def _get_points_window_cb(self, window: int) -> Callable[[DBSCANClusterer], bool]:
        return (
            lambda clusterer: clusterer.length >= window / self.buffer_size
            and self.reason(f"reached {round(window / self.buffer_size)} points")
        )

    def _get_time_window_cb(self, window: float) -> Callable[[DBSCANClusterer], bool]:
        return (
            lambda clusterer: datetime.now().timestamp() - clusterer.created
            >= window / self.buffer_size
            and self.reason(
                f"timeout after {round(window / self.buffer_size, 2)} seconds"
            )
        )

    def reason(self, msg) -> bool:
        """Provide a reason for the window to be rotated, always return True."""
        self._reason = msg
        return True

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

from typing import Callable, List, Optional, Tuple, Union

import numpy as np
from typing_extensions import TypeAlias

DataSignatures: TypeAlias = np.ndarray
"""ndarray of shape(n, 2)"""

DataSignature: TypeAlias = np.ndarray
"""ndarray of shape(2,)"""


class DataPoint(np.ndarray):
    def __new__(
        cls,
        *args,
        age: Union[int, float] = 0,
        ndarray: Optional[np.ndarray] = None,
        **kwargs,
    ):
        if ndarray is not None:
            return ndarray.view(cls)

        ins = np.empty((3,))
        if len(args) > 0:
            ins[0] = args[0]
        if len(args) > 1:
            ins[1] = args[1]
        ins[2] = age
        return ins

    @property
    def age(self):
        return self[2]

    @property
    def signature(self) -> DataSignature:
        return self[:2]


class DataPoints(np.ndarray):
    def __new__(cls, ndarray: np.ndarray):
        return ndarray.view(cls)

    @property
    def signatures(self) -> DataSignatures:
        return self[:, :2]

    def datapoint(self, idx):
        return DataPoint(ndarray=self[idx])


class DataBuffer:
    def __init__(self, cap: int):
        self._xy = np.empty((cap, 3), dtype=float)
        self._commited = 0
        """The length of data that has been processed and ready to read"""
        self._head = 0
        """All length of all data that includes processing data points."""

    def reconcile(self, cap: int):
        if cap < self._xy.shape[0]:
            # We do not shrink
            return

        new_cap = self._xy.shape[0] * 2
        while new_cap < cap:
            new_cap *= 2
        self._xy = np.resize(self._xy, (new_cap, 3))

    def append(self, tokens: List[DataPoint], commit: bool = False) -> DataPoints:
        """Append data points to the buffer. If commit is True, the data points will be committed immediately and could lead to data inconsistent if it takes a long time to process new data."""
        size_gap = self._commited + len(tokens) - self._xy.shape[0]
        # Check buffer size.
        if size_gap > 0:
            # We do not expand the buffer automatically, simply evicting some records to make room for new data.
            self.trim_head(size_gap)

        # Check tokens size. If tokens is too large, buffer has been cleared at this point.
        if len(tokens) > self._xy.shape[0]:
            tokens = tokens[-self._xy.shape[0] :]

        self._xy[self._commited : self._commited + len(tokens)] = tokens
        ret = DataPoints(self._xy[self._commited : self._commited + len(tokens)])
        self._head += len(tokens)
        if commit:
            self.commit()
        return ret

    def commit(self):
        self._commited = self._head

    def trim_head(self, start):
        if self._head != self._commited:
            raise Exception("Cannot trim head when there are uncommited data points.")

        if start >= self._commited:
            # empty(), skip data moving
            self._head = self._commited = 0
            return
        elif start <= -self._commited:
            # keep all data
            return

        # Do compacting.
        # implement self._xy[:-start] = self._xy[start:] with variable buffer length.
        if start > 0:
            self._xy[: self._commited - start] = self._xy[start : self._commited]
            self._commited -= start
        else:
            self._xy[:-start] = self._xy[self._commited + start : self._commited]
            self._commited = -start
        self._head = self._commited

    def clear(self):
        self._commited = 0
        self._head = 0

    @property
    def x(self):
        return self._xy[: self._commited, 0]

    @property
    def y(self):
        return self._xy[: self._commited, 1]

    @property
    def datapoints(self) -> DataPoints:
        return DataPoints(self._xy[: self._commited])

    @property
    def len(self):
        return self._commited

    @property
    def cap(self):
        return self._xy.shape[0]


class Centeroid:
    def __init__(self):
        """Centeroid calculates the mass center, radius, and size of data points."""
        self._sum_center = None
        self._range_max = None
        self._range_min = None
        self._span_max = 0
        self._span_min = 0
        self._size = 0
        self._signature = None
        self._up_to_date = False  # Whether the signature is up to date.

    def add(self, point: DataPoint):
        if self._sum_center is None:
            self._sum_center = list(point.signature)
            self._range_min = list(point.signature)
            self._range_max = list(point.signature)
            self._span_max = point.age
            self._span_min = point.age
            self._signature = np.zeros_like(self._sum_center, dtype=int)
        else:
            for i, val in enumerate(point.signature):  # type: ignore
                self._sum_center[i] += val
                self._range_min[i] = min(self._range_min[i], val)
                self._range_max[i] = max(self._range_max[i], val)
            self._span_min = min(self._span_min, point.age)
            self._span_max = max(self._span_max, point.age)
            self._up_to_date = False

        self._size += 1

    def get_signature(
        self,
        indexes: List[List[float]],
        error_suppressor: Optional[
            Callable[[int, float, float, float, float], None]
        ] = None,
    ) -> Tuple[int]:
        """Generate the index signature of the centroid within the indexes' range.

        Args:
            indexes: A list of list of float, each list is a range of values.
            error_suppressor: A function to handle the error with parameters(value, index assigned, value of index, offset). If None, raise an exception.
        """
        if len(self._signature) != len(indexes):
            raise Exception(
                f"Indexes and centeroid signature size mismatch, {len(self._signature)}:{len(indexes)}"
            )

        if self._up_to_date:
            return self.signature

        for i, value in enumerate(self.center):
            if len(indexes[i]) == 0:
                raise Exception("Indexes size mismatch, at least 1.")
            elif len(indexes[i]) == 1:
                self._signature[i] = 0
                continue

            # Assuming indexes are ascending ordered.
            distance = (indexes[i][-1] - indexes[i][0]) / (len(indexes[i]) - 1)
            if value < indexes[i][0] - distance / 2:
                if error_suppressor is not None:
                    self._signature[i] = 0
                    error_suppressor(i, value, 0, indexes[i][0], -distance / 2)
                else:
                    raise Exception(
                        f"Centeroid is out of range: {i}:{value} and accepted minimum {indexes[i][0]} (with offset {- distance / 2})."
                    )
            elif value > indexes[i][-1] + distance / 2:
                if error_suppressor is not None:
                    self._signature[i] = len(indexes[i]) - 1
                    error_suppressor(
                        i, value, len(indexes[i]) - 1, indexes[i][-1], distance / 2
                    )
                else:
                    raise Exception(
                        f"Centeroid is out of range: {i}:{value} and accepted maximum {indexes[i][-1]} (with offset {distance / 2})."
                    )
            else:
                # Find the index using binary search.
                left, right = 0, len(indexes[i]) - 1
                found = False
                while left < right - 1:
                    mid = (left + right) // 2
                    if value < indexes[i][mid]:
                        right = mid
                    elif value > indexes[i][mid]:
                        left = mid
                    else:
                        self._signature[i] = mid
                        found = True
                        break
                if not found:
                    self._signature[i] = (
                        left
                        if value < (indexes[i][left] + indexes[i][left]) / 2
                        else right
                    )

        self._up_to_date = True
        return self.signature

    @property
    def center(self):
        return tuple(val / self._size for val in self._sum_center)

    @property
    def radius(self):
        return max(
            (val - self._range_min[i]) / 2 for i, val in enumerate(self._range_max)
        )

    @property
    def size(self):
        return self._size

    @property
    def span(self):
        return self._span_max - self._span_min + 1

    @property
    def signature(self) -> Tuple[int]:
        if not self._up_to_date:
            raise Exception("Signature is not up to date.")
        return tuple(self._signature.tolist())

    @property
    def rate(self):
        return self._size / self.span

    def to_array(self):
        ret = list(self.center)
        ret.append(self.radius)
        ret.append(self.rate)
        return ret

    def __str__(self) -> str:
        return f"Centeroid(center={self.center}, rps={self.rate})"

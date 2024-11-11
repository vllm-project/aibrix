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

from typing import Union, Tuple, List
import numpy as np

DataSignatures = np.ndarray
"""ndarray of shape(n, 2)"""

DataSignature = np.ndarray
"""ndarray of shape(2,)"""

class DataPoint(np.ndarray):
    def __new__(cls, *args, age:Union[int, float]=0, ndarray:np.ndarray=None, **kwargs):
        if ndarray is not None:
            return ndarray.view(cls)

        ins = ndarray = np.empty((3,), *kwargs)
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
    def __new__(cls, ndarray:np.ndarray):
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

    def append(self, tokens: List[DataPoint], commit: bool=False) -> DataPoints:
        """Append data points to the buffer. If commit is True, the data points will be committed immediately and could lead to data inconsistent if it takes a long time to process new data."""
        size_gap = self._commited + len(tokens) - self._xy.shape[0]
        # Check buffer size.
        if size_gap > 0:
            # We do not expand the buffer automatically, simply evicting some records to make room for new data.
            self.trim_head(size_gap)

        # Check tokens size. If tokens is too large, buffer has been cleared at this point.
        if len(tokens) > self._xy.shape[0]:
            tokens = tokens[-self._xy.shape[0]:]
        
        self._xy[self._commited: self._commited+len(tokens)] = tokens
        ret = DataPoints(self._xy[self._commited: self._commited+len(tokens)])
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
            self._xy[:self._commited-start] = self._xy[start:self._commited]
            self._commited -= start
        else:
            self._xy[:-start] = self._xy[self._commited+start:self._commited]
            self._commited = -start
        self._head = self._commited
    
    def clear(self):
        self._commited = 0
        self._head = 0

    @property
    def x(self):
        return self._xy[:self._commited, 0]
    
    @property
    def y(self):
        return self._xy[:self._commited, 1]
    
    @property
    def datapoints(self) -> DataPoints:
        return DataPoints(self._xy[:self._commited])

    @property
    def len(self):
        return self._commited
    
    @property
    def cap(self):
        return self._xy.shape[0]

class Centeroid:
    def __init__(self):
        """Centeroid calculates the mass center, radius, and size of data points. """
        self._sum_center = None
        self._range_max = None
        self._range_min = None
        self._span_max = 0
        self._span_min = 0
        self._size = 0

    def add(self, point: DataPoint):
        if self._sum_center is None:
            self._sum_center = list(point.signature)
            self._range_min = list(point.signature)
            self._range_max = list(point.signature)
            self._span_max = point.age
            self._span_min = point.age
        else:
            for i, val in enumerate(point.signature):
                self._sum_center[i] += val
                self._range_min[i] = min(self._range_min[i], val)
                self._range_max[i] = max(self._range_max[i], val)
            self._span_min = min(self._span_min, point.age)
            self._span_max = max(self._span_max, point.age)
            
        self._size += 1

    @property
    def center(self):
        return tuple(val / self._size for val in self._sum_center)

    @property
    def radius(self):
        return max((val - self._range_min[i])/2 for i, val in enumerate(self._range_max))
    
    @property
    def size(self):
        return self._size
    
    @property
    def span(self):
        return self._span_max - self._span_min + 1
    
    @property
    def signature(self) -> Tuple[float]:
        return (0, 0)
    
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
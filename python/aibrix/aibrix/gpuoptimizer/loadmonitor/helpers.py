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

from typing import Union
import matplotlib.pyplot as plt
import numpy as np

class DataPoint(np.ndarray):
    def __new__(cls, data, age: Union[int, float]):
        obj = np.array(data).view(cls)
        obj.age = age
        return obj

    def __array_finalize__(self, obj):
        # see InfoArray.__array_finalize__ for comments
        if obj is None: return
        self.age = getattr(obj, 'age', None)

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
            self._sum_center = list(point)
            self._range_min = list(point)
            self._range_max = list(point)
            self._span_max = point.age
            self._span_min = point.age
        else:
            for i, val in enumerate(point):
                self._sum_center[i] += val
                self._range_min[i] = min(self._range_min[i], val)
                self._range_max[i] = max(self._range_max[i], val)
            self._span_min = min(self._span_min, point.age)
            self._span_max = max(self._span_max, point.age)
            
        self._size += 1

    @property
    def center(self):
        return (val / self.size for val in self._sum_center)

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
    def moving_size(self):
        return self._size / self.span
    
    def to_array(self, span=1):
        ret = list(self.center)
        ret.append(self.radius)
        ret.append(self.moving_size)
        return ret
    
def make_color(color, alpha=1):
    rgb = plt.matplotlib.colors.to_rgb(color)
    return f"rgba({rgb[0]*255}, {rgb[1]*255}, {rgb[2]*255}, {alpha})"
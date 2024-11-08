from typing import List
import pandas as pd
import os
from dataclasses import dataclass, field

# Convert max throughput profiling to a mapping from request size to load
def tputs_to_loads_2d(max_tputs: List[List[float]]):
    loads = []
    for i in range(len(max_tputs)):
        loads.append([])
        for j in range(len(max_tputs[0])):
            load = 1 / max_tputs[i][j]
            loads[-1].append(load)
    return loads


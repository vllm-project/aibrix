from typing import List


# Convert max throughput profiling to a mapping from request size to load
def tputs_to_loads_2d(max_tputs: List[List[float]]):
    loads: List[List[float]] = []
    for i in range(len(max_tputs)):
        loads.append([])
        for j in range(len(max_tputs[0])):
            load = 1000000.0  # inf
            if max_tputs[i][j] > 0:
                load = 1.0 / max_tputs[i][j]
            loads[-1].append(load)
    return loads

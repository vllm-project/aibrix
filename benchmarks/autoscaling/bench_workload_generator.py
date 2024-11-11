import logging
import math
import os
import random
from typing import List, Any

import matplotlib.pyplot as plt
import numpy as np


def generate_workload(input_requests: List[Any], A=1, B=1,
                      sigma=0.1,
                      only_rise: bool = False,
                      omega: float = None,
                      period=0.25,
                      length: int = None,
                      duration_sec: int = None,
                      interval_sec: int = None,
                      ) -> List[List[Any]]:
    """
    Generates a workload based on a given list of input requests and a concurrency function.

    Reference: https://bytedance.larkoffice.com/docx/C114dkyJioiFDzxoaTScWqh8nof

    The concurrency function is defined as:
        concurrency(t) = trend(t) + noise
        trend(t) = A * sin(omega * t) + B
        noise ~ N(0, sigma^2)

    Args:
        input_requests (list): The list of all requests to be sent.
        A (float, optional): The amplitude of the sine wave in the concurrency function. Defaults to 1.
        B (float, optional): The vertical shift of the sine wave in the concurrency function. Defaults to 1.
        sigma (float, optional): The standard deviation of the normal distribution for the noise. Defaults to 0.1.
        omega (float, optional): if None, omega = pi / (2 * length / period)
        period (float, optional): See omega. Defaults to 0.25.
        only_rise: if True, the concurrency will monotonically increase
        length (int, optional): if None, length = test_duration_sec / interval_sec
        duration_sec (int, optional): See param: length
        interval_sec (int, optional): See param: length

    Returns:
        list: A list of items, where each item is a list of requests to be sent concurrently.
    """

    def math_function(t):
        """
        Calculates the concurrency value based on the given concurrency function.

        The concurrency function is defined as:
        concurrency(t) = trend(t) + noise
        trend(t) = A * sin(omega * t) + B
        noise ~ N(0, sigma^2)

        Args:
            t (int): The discrete integer value of t, starting from 0.

        Returns:
            int: The concurrency value rounded to the nearest integer.
        """
        trend = A * math.sin(omega * t) + B
        noise = random.gauss(0, sigma)
        return round(trend + noise)

    assert length is not None or (duration_sec is not None and interval_sec is not None), \
        "duration_sec and interval_sec must be specified if length is not None"
    if length is None:
        length = int(duration_sec // interval_sec)
    assert omega is not None or period is not None, "period must be specified if length is not None"
    if omega is None:
        omega = 2 * math.pi / (length / period)
    workload = []
    t = 0
    input_length = len(input_requests)

    previous_concurrency = -1
    start_index, end_index = 0, 0
    while t < length:
        current_concurrency = math_function(t)
        if only_rise:
            current_concurrency = max(previous_concurrency, current_concurrency)
            previous_concurrency = current_concurrency

        # start from last end index
        start_index = end_index
        end_index += current_concurrency
        workload.append([input_requests[i % input_length] for i in range(start_index, end_index)])
        t += 1

    return workload


def plot_workload(workload_dict, interval_sec, output_path: str = None):
    """
    Plots the concurrency (item length) of the generated workload.

    Args:
        workload_dict (dict): A dictionary where the keys are workload names (labels) and the values are lists of lists representing the workload.
        interval_sec (int):
    """
    for workload_name, workload in workload_dict.items():
        concurrency_values = [len(item) for item in workload]
        plt.plot(np.arange(len(concurrency_values)) * interval_sec, concurrency_values, label=workload_name)

    plt.xlabel('Time (Sec)')
    plt.ylabel('Concurrency')
    plt.title('Workload Concurrency')
    plt.legend()
    if output_path is None:
        plt.show()
    else:
        os.makedirs(os.path.dirname(output_path), exist_ok=True)
        plt.savefig(output_path)
        logging.info(f'Saved workload plot to {output_path}')


if __name__ == '__main__':
    # Generate input requests (ascending integers)
    demo_requests = list(range(1, 10001))
    interval = 30
    # Generate workloads with different parameters
    workload_dict = {
        'Quick Rising':
            generate_workload(demo_requests, duration_sec=600, interval_sec=interval, A=5, period=5, only_rise=True),
        'Slow Rising':
            generate_workload(demo_requests, duration_sec=600, interval_sec=interval, A=5, period=0.25,
                              only_rise=True),
        'Slight Fluctuation':
            generate_workload(demo_requests, duration_sec=600, interval_sec=interval, A=5, B=5, period=1,
                              only_rise=False),
        'Severe Fluctuation':
            generate_workload(demo_requests, duration_sec=600, interval_sec=interval, A=5, B=10, period=12,
                              only_rise=False),
    }

    # Plot the workloads
    plot_workload(workload_dict, interval_sec=interval)

import logging
import math
import os
import random
import json
from typing import Tuple, Optional, List, Any
from transformers import PreTrainedTokenizerBase
import matplotlib.pyplot as plt
import numpy as np
import argparse
import csv


def sample_sharegpt_requests(
        dataset_path: str,
        num_requests: int,
        tokenizer: Optional[PreTrainedTokenizerBase] = None,
        fixed_output_len: Optional[int] = None,
) -> List[Tuple[str, int, int, None]]:
    # Load the dataset
    with open(dataset_path, encoding='utf-8') as f:
        dataset = json.load(f)
    dataset = [data for data in dataset if len(data["conversations"]) >= 2]
    dataset = [(data["conversations"][0]["value"], data["conversations"][1]["value"]) for data in dataset]

    filtered_dataset: List[Tuple[str, int, int]] = []
    for i in range(len(dataset)):
        if len(filtered_dataset) == num_requests:
            break
        prompt = dataset[i][0]
        if tokenizer is not None:
            prompt_token_ids = tokenizer(prompt).input_ids
            completion = dataset[i][1]
            completion_token_ids = tokenizer(completion).input_ids
            prompt_len = len(prompt_token_ids)
            output_len = len(completion_token_ids) if fixed_output_len is None else fixed_output_len
            if prompt_len < 4 or (fixed_output_len is None and output_len < 4):
                continue
            if prompt_len > 1024 or prompt_len + output_len > 2048:
                continue
            filtered_dataset.append((prompt, prompt_len, output_len, None))
        else:
            filtered_dataset.append((prompt, -1, -1, None))

    return filtered_dataset

def generate_from_QPS_csv(input_requests: List[Any],
                          file_path: str,
                          sampling_granularity_seconds: int = 15,
                        ) -> List[List[Any]]:
    total_requests_from_trace = []
    with open(file_path, 'r') as file:
        reader = csv.DictReader(file)
        for row in reader:
            if 'Total' in row:
                total_value = row['Total']
                if total_value:
                    total_requests_from_trace.append(float(total_value))
    workloads = []
    base = 0
    end = False
    for interval_requests in total_requests_from_trace:
        if end:
            break
        mean_rate = round(interval_requests/sampling_granularity_seconds)
        print(f"mean_rate {mean_rate} for next {sampling_granularity_seconds} interval" )
        for _ in range(0, sampling_granularity_seconds):
            bound = min(base + mean_rate, len(input_requests))
            workloads.append(input_requests[base : bound])
            base += mean_rate
            if base >= len(input_requests):
                end = True
                break
    print(f"workloads {workloads}")
    return workloads

def generate_synthetic(input_requests: List[Any], A=1, B=1,
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
    print(workload)
    return workload

def pair_requests_with_prompts(workload: List[List[Any]], prompts: List[Tuple[str, int, int, None]], output_file: str = 'trace_output.json') -> List[List[Tuple[Any, str]]]:
    paired_workload = []
    prompt_count = len(prompts)

    for requests in workload:
        requests_with_prompts = [
            prompts[request % prompt_count] for request in requests
        ]
        paired_workload.append(requests_with_prompts)

    # Save to file
    with open(output_file, 'w') as file:
        json.dump(paired_workload, file)

    return paired_workload

def plot_workload(workload_dict, interval_sec, output_path: str = None):
    """
    Plots the concurrency (item length) of the generated workload.

    Args:
        workload_dict (dict): A dictionary where the keys are workload names (labels) and the values are lists of lists representing the workload.
        interval_sec (int):
    """
    fig, ax = plt.subplots()
    for workload_name, workload in workload_dict.items():
        concurrency_values = [len(item) for item in workload]
        ax.plot(np.arange(len(concurrency_values)) * interval_sec, concurrency_values, label=workload_name)

    ax.set_ylim(0,)
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
    parser = argparse.ArgumentParser(description='Workload Generator')
    parser.add_argument('--prompt-file', type=str, required=True, help='File containing prompts')
    parser.add_argument('--num-prompts', type=int, default=100, help='Number of prompts to sample')
    parser.add_argument('--num-requests', type=int, default=10000, help='Number of requests in total')
    parser.add_argument('--trace-file', type=str, required=False, default=None, help='File containing trace CSV')
    parser.add_argument('--output', type=str, required=False, default="output.json", help='Output file name')
    args = parser.parse_args()

    # Load prompts from a file
    prompts = sample_sharegpt_requests(args.prompt_file, args.num_prompts)

    # Generate input requests (ascending integers)
    demo_requests = list(range(1, 1 + args.num_requests))
    interval = 30
    # Generate workloads and pair with prompts
    workload_dict = {}
    if args.trace_file is None:
        # Generate workloads with different parameters
        scenarios = {
            'Quick Rising': {'duration_sec': 600, 'interval_sec': interval, 'A': 5, 'period': 5, 'only_rise': True},
            'Slow Rising': {'duration_sec': 600, 'interval_sec': interval, 'A': 5, 'period': 0.25, 'only_rise': True},
            'Slight Fluctuation': {'duration_sec': 600, 'interval_sec': interval, 'A': 5, 'B': 5, 'period': 1, 'only_rise': False},
            'Severe Fluctuation': {'duration_sec': 600, 'interval_sec': interval, 'A': 5, 'B': 10, 'period': 12, 'only_rise': False},
        }
        for scenario_name, params in scenarios.items():
            generated_workload = generate_synthetic(demo_requests, **params)
            paired_workload = pair_requests_with_prompts(generated_workload, prompts, f'traces/{scenario_name}.json')
            workload_dict[scenario_name] = paired_workload
    else:
        generated_workload = generate_from_QPS_csv(demo_requests, file_path=args.trace_file, sampling_granularity_seconds=15)
        paired_workload = pair_requests_with_prompts(generated_workload, prompts, f'traces/{args.output}')
        workload_dict["from_csv"] = paired_workload

    # Plot the workloads
    plot_workload(workload_dict, interval_sec=interval)

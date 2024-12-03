import logging
import math
import random
import json
import pandas as pd
import argparse
import csv

from typing import Tuple, List, Any
from transformers import PreTrainedTokenizerBase
from datetime import datetime, timedelta
from utils import (sample_sharegpt_requests, load_sharegpt_requests,sample_sharegpt_requests_len_range, get_tokenizer, plot_workload, make_serializable)

def generate_from_summary_csv(input_requests: List[Any],
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
        for _ in range(0, sampling_granularity_seconds):
            bound = min(base + mean_rate, len(input_requests))
            workloads.append(input_requests[base : bound])
            base += mean_rate
            if base >= len(input_requests):
                end = True
                break
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
    return workload

def pair_requests_with_prompts(workload: List[List[Any]], prompts: List[Tuple[str, int, int, None]], output_file: str = 'output/output.json') -> List[List[Tuple[Any, str]]]:
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

# generated_workload = generate_from_azure_csv(demo_requests, file_path=args.trace_file, sampling_granularity_seconds=15, output_file=args.output)
def generate_from_azure_csv(file_path: str,
                            prompt_file_path: str,
                            tokenizer: PreTrainedTokenizerBase,
                            group_interval_seconds: int = 1,
                            output_file: str = 'output/output.json',
                        ) -> List[List[Any]]:
    # Load the CSV file
    df = pd.read_csv(file_path)

    # Ensure TIMESTAMP is a datetime object
    df['TIMESTAMP'] = pd.to_datetime(df['TIMESTAMP'])

    # Define the grouping time range (e.g., 1 second)
    time_range = timedelta(seconds=group_interval_seconds)

    # Initialize a list to hold the grouped requests
    grouped_requests = []

    # Group requests by the time range
    df.set_index('TIMESTAMP', inplace=True)
    current_time = df.index.min()
    end_time = df.index.max()
    logging.INFO(f"Start generation from time {current_time} to {end_time}")
   
    sharegpt_df = load_sharegpt_requests(dataset_path = prompt_file_path, tokenizer = tokenizer)
    
    while current_time <= end_time:
        # Select requests within the current time range
        mask = (df.index >= current_time) & (df.index < current_time + time_range)
        group = df.loc[mask]
        input_lens = []
        output_lens = []
        for _, row in group.iterrows():
            input_lens.append(int(row['ContextTokens']))
            output_lens.append(int(row['GeneratedTokens']))
        logging.info(f"Sample iteration {len(grouped_requests)} for {len(input_lens)} requests")
        sampled_requests = sample_sharegpt_requests_len_range(
                df = sharegpt_df,
                num_requests = len(input_lens),
                input_lens = input_lens, 
                output_lens = output_lens,
                initial_err_perc = 0.5,
                err_step = 0.05
                )
        
        if sampled_requests:  # Only add non-empty groups
            grouped_requests.append(sampled_requests)
        
        # Move to the next time range
        current_time += time_range

    # Print or process grouped_requests as needed
    # Save to file
    grouped_requests = make_serializable(grouped_requests)
    with open(output_file, 'w') as file:
        json.dump(grouped_requests, file)
    
    print(grouped_requests)
    return grouped_requests
    

if __name__ == '__main__':
    parser = argparse.ArgumentParser(description='Workload Generator')
    parser.add_argument('--prompt-file', type=str, required=True, help='File containing prompts')
    parser.add_argument('--num-prompts', type=int, default=100, help='Number of prompts to sample')
    parser.add_argument('--num-requests', type=int, default=10000, help='Number of requests in total')
    parser.add_argument('--group-interval-seconds', type=int, default=1, help='Grouping interval seconds')
    parser.add_argument('--trace-type', type=str, required=True, default="synthetic", help='Type of trace consumed')
    parser.add_argument('--trace-file', type=str, required=False, default=None, help='File containing trace CSV')
    parser.add_argument('--model', type=str, required=False, default="meta-llama/Llama-2-7b-hf", help='Target model tokenizer')
    parser.add_argument('--output', type=str, required=False, default="output/output", help='Output path')
    args = parser.parse_args()

    # Load prompts from a file
    prompts = sample_sharegpt_requests(args.prompt_file, args.num_prompts)

    # Generate input requests (ascending integers)quit
    demo_requests = list(range(1, 1 + args.num_requests))
    interval = 30
    # Generate workloads and pair with prompts
    workload_dict = {}
    if args.trace_type == "synthetic":
        # Generate workloads with different parameters
        scenarios = {
            'Quick Rising': {'duration_sec': 600, 'interval_sec': interval, 'A': 5, 'period': 5, 'only_rise': True},
            'Slow Rising': {'duration_sec': 600, 'interval_sec': interval, 'A': 5, 'period': 0.25, 'only_rise': True},
            'Slight Fluctuation': {'duration_sec': 600, 'interval_sec': interval, 'A': 5, 'B': 5, 'period': 1, 'only_rise': False},
            'Severe Fluctuation': {'duration_sec': 600, 'interval_sec': interval, 'A': 5, 'B': 10, 'period': 12, 'only_rise': False},
        }
        for scenario_name, params in scenarios.items():
            generated_workload = generate_synthetic(demo_requests, **params)
            paired_workload = pair_requests_with_prompts(generated_workload, prompts, f"{args.output}/{scenario_name}.json")
            workload_dict[scenario_name] = paired_workload
        # Plot the workloads
        plot_workload(workload_dict, interval_sec=interval, output_file=f"plot/synthetic.pdf")
    elif args.trace_type == "summary":
        generated_workload = generate_from_summary_csv(demo_requests, file_path=args.trace_file, sampling_granularity_seconds=15)
        generated_workload = pair_requests_with_prompts(generated_workload, prompts, f"{args.output}/summary.json")
        workload_dict["summary"] = generated_workload
        # Plot the workloads
        plot_workload(workload_dict, interval_sec=interval, output_file=f"plot/summary.pdf")
    elif args.trace_type == "azure":
        tokenizer = get_tokenizer(pretrained_model_name_or_path = args.model, trust_remote_code = True)
        generated_workload = generate_from_azure_csv(file_path=args.trace_file, prompt_file_path = args.prompt_file, tokenizer = tokenizer, group_interval_seconds=1, output_file=f"{args.output}/azure.json")
        workload_dict["azure"] = generated_workload
        # Plot the workloads
        plot_workload(workload_dict, interval_sec=interval, output_file=f"plot/azure.pdf")

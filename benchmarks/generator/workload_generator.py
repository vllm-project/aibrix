import logging
import math
import random
import pandas as pd
import argparse
import csv

from typing import Tuple, List, Any
from transformers import PreTrainedTokenizerBase
from datetime import timedelta
from sample_request import (load_sharegpt_requests, sample_sharegpt_requests, sample_sharegpt_requests_len_range)
from utils import (get_tokenizer, plot_workload, make_serializable, save_workload)

def generate_from_internal_csv(file_path: str,
                          duration_ms: int,
                          summary_interval_ms: int,
                          interval_ms: int = 1000,
                        ) -> List[List[Any]]:
    total_requests_from_summary = []
    with open(file_path, 'r') as file:
        reader = csv.DictReader(file)
        for row in reader:
            if 'Total' in row:
                total_value = row['Total']
                if total_value:
                    total_requests_from_summary.append(float(total_value))
    workloads = []
    base = 0
    ts = 0
    for interval_requests in total_requests_from_summary:
        mean_rate = round(interval_requests/(summary_interval_ms / interval_ms))
        for ts_delta in list(range(0, summary_interval_ms, interval_ms)):
            workloads.append((ts + ts_delta, range(base, base + mean_rate)))
            base += mean_rate
        ts += summary_interval_ms
        if ts > duration_ms:
            break     
    return workloads

def generate_synthetic(A=1, B=1,
                      sigma=0.1,
                      only_rise: bool = False,
                      omega: float = None,
                      period=0.25,
                      length: int = None,
                      duration_ms: int = None,
                      interval_ms: int = None,
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
        length (int, optional): if None, length = duration_ms / interval_ms
        duration_ms (int, optional): See param: length
        interval_ms (int, optional): See param: length

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

    assert length is not None or (duration_ms is not None and interval_ms is not None), \
        "duration_ms and interval_ms must be specified if length is not None"
    if length is None:
        length = int(duration_ms // interval_ms) + 1
    assert omega is not None or period is not None, "period must be specified if length is not None"
    if omega is None:
        omega = 2 * math.pi / (length / period)
    workload = []
    t = 0

    previous_concurrency = -1
    start_index, end_index = 0, 0
    ts = 0
    base_req_id = 0
    while t < length:
        current_concurrency = math_function(t)
        if only_rise:
            current_concurrency = max(previous_concurrency, current_concurrency)
            previous_concurrency = current_concurrency

        # start from last end index
        start_index = end_index
        end_index += current_concurrency
        workload.append((ts, [base_req_id + i for i in range(start_index, end_index)]))
        base_req_id += current_concurrency
        ts += interval_ms
        t += 1
    return workload

def pair_requests_with_prompts_round_robin(workload: List[List[Any]], 
                                           prompts: List[Tuple[str, int, int, None]], 
                                           output_file: str = 'output/output', 
                                           to_jsonl: bool = False
                                        ) -> List[List[Tuple[Any, str]]]:
    paired_workload = []
    prompt_count = len(prompts)
    for ts, requests in workload:
        requests_with_prompts = [
            prompts[request % prompt_count] for request in requests
        ]
        paired_workload.append({"Timestamp": ts, "Requests": requests_with_prompts})

    # Save to file
    save_workload(paired_workload, output_file, use_jsonl = to_jsonl)
    
    return paired_workload

# generated_workload = generate_from_azure_csv(demo_requests, file_path=args.trace_file, sampling_granularity_seconds=15, output_file=args.output)
def generate_from_azure_csv(file_path: str,
                            prompt_file_path: str,
                            duration_ms: int,
                            tokenizer: PreTrainedTokenizerBase,
                            interval_ms: int,
                            output_file: str = 'output/output.json',
                            to_jsonl: bool = False,
                        ) -> List[List[Any]]:
    # Load the CSV file
    df = pd.read_csv(file_path)

    # Ensure TIMESTAMP is a datetime object
    df['TIMESTAMP'] = pd.to_datetime(df['TIMESTAMP'])

    # Define the grouping time range (e.g., 1 second)
    time_range = timedelta(milliseconds=interval_ms)

    # Initialize a list to hold the grouped requests
    grouped_requests = []

    # Group requests by the time range
    df.set_index('TIMESTAMP', inplace=True)
    current_time = df.index.min()
    end_time = df.index.max()
    logging.warn(f"Start generation from time {current_time} to {end_time}")
   
    sharegpt_df = load_sharegpt_requests(dataset_path = prompt_file_path, tokenizer = tokenizer)
    
    ts = 0
    while current_time <= end_time:
        # Select requests within the current time range
        mask = (df.index >= current_time) & (df.index < current_time + time_range)
        group = df.loc[mask]
        input_lens = []
        output_lens = []
        for _, row in group.iterrows():
            input_lens.append(int(row['ContextTokens']))
            output_lens.append(int(row['GeneratedTokens']))
        sampled_requests = sample_sharegpt_requests_len_range(
                df = sharegpt_df,
                num_requests = len(input_lens),
                input_lens = input_lens, 
                output_lens = output_lens,
                initial_err_perc = 0.5,
                err_step = 0.05
                )
        
        if sampled_requests:  # Only add non-empty groups
            grouped_requests.append({"Timestamp": ts, "Requests": sampled_requests})
        ts += interval_ms
        if ts > duration_ms:
            break
        # Move to the next time range
        current_time += time_range

    # Print or process grouped_requests as needed
    # Save to file
    grouped_requests = make_serializable(grouped_requests)
    save_workload(grouped_requests, output_file, use_jsonl = to_jsonl)
    
    return grouped_requests
    

if __name__ == '__main__':
    parser = argparse.ArgumentParser(description='Workload Generator')
    parser.add_argument('--prompt-file', type=str, required=True, help='File containing prompts.')
    parser.add_argument('--num-prompts', type=int, default=100, help='Number of prompts to sample.')
    parser.add_argument('--group-interval-seconds', type=int, default=1, help='Grouping interval seconds.')
    parser.add_argument('--trace-type', type=str, required=True, default="synthetic", help='Type of trace consumed. Choose among: synthetic, internal, azure')
    parser.add_argument('--trace-file', type=str, required=False, default=None, help='File containing original trace file csv, which workload generator depends upon to convert to workload format. This is only needed for for internal/azure trace type. ')
    parser.add_argument('--model', type=str, required=False, default="Qwen/Qwen2.5-Coder-7B-Instruct", help='Target model tokenizer.')
    parser.add_argument('--output', type=str, required=False, default="output", help='Output path to the workload.')
    parser.add_argument('--interval-ms', type=int, required=False, default=1000, help='Granularity of request injection interval in milliseconds.')
    parser.add_argument('--duration-ms', type=int, default=60000, help='Duration of the trace generated.')
    parser.add_argument('--to-jsonl', dest='to_jsonl', action='store_true', help='Set output data format to .jsonl (default .json).')
    args = parser.parse_args()

    # Generate workloads and pair with prompts
    workload_dict = {}
    tokenizer = get_tokenizer(pretrained_model_name_or_path = args.model, trust_remote_code = True)
    if args.trace_type == "synthetic":
        # Load prompts from a file
        prompts = sample_sharegpt_requests(dataset_path = args.prompt_file, num_requests = args.num_prompts, tokenizer = tokenizer)
        # Generate workloads with different parameters
        scenarios = {
            'Quick Rising': {'duration_ms': args.duration_ms, 'interval_ms': args.interval_ms, 'A': 5, 'period': 5, 'only_rise': True},
            'Slow Rising': {'duration_ms': args.duration_ms, 'interval_ms': args.interval_ms, 'A': 5, 'period': 0.25, 'only_rise': True},
            'Slight Fluctuation': {'duration_ms': args.duration_ms, 'interval_ms': args.interval_ms, 'A': 5, 'B': 5, 'period': 1, 'only_rise': False},
            'Severe Fluctuation': {'duration_ms': args.duration_ms, 'interval_ms': args.interval_ms, 'A': 5, 'B': 10, 'period': 12, 'only_rise': False},
        }
        for scenario_name, params in scenarios.items():
            generated_workload = generate_synthetic(**params)
            paired_workload = pair_requests_with_prompts_round_robin(workload = generated_workload, 
                                                                     prompts = prompts, 
                                                                     output_file = f"{args.output}/{scenario_name}",
                                                                     to_jsonl = args.to_jsonl)
            workload_dict[scenario_name] = paired_workload
        # Plot the workloads
        plot_workload(workload_dict, interval_ms=args.interval_ms, output_file=f"plot/synthetic.pdf")
    elif args.trace_type == "internal":
        # Load prompts from a file
        prompts = sample_sharegpt_requests(dataset_path = args.prompt_file, num_requests = args.num_prompts, tokenizer = tokenizer)
        # Generate input requests (ascending integers)quit
        generated_workload = generate_from_internal_csv(file_path=args.trace_file, duration_ms = args.duration_ms, summary_interval_ms=15000, interval_ms=args.interval_ms)
        generated_workload = pair_requests_with_prompts_round_robin(workload = generated_workload, 
                                                                    prompts = prompts, 
                                                                    output_file = f"{args.output}/internal",
                                                                    to_jsonl = args.to_jsonl)
        workload_dict["internal"] = generated_workload
        # Plot the workloads
        plot_workload(workload_dict, interval_ms=args.interval_ms, output_file=f"plot/internal.pdf")
    elif args.trace_type == "azure":
        generated_workload = generate_from_azure_csv(file_path=args.trace_file, 
                                                     prompt_file_path = args.prompt_file, 
                                                     duration_ms = args.duration_ms, 
                                                     tokenizer = tokenizer, 
                                                     interval_ms = args.interval_ms, 
                                                     output_file = f"{args.output}/azure",
                                                     to_jsonl = args.to_jsonl)
        workload_dict["azure"] = generated_workload
        # Plot the workloads
        plot_workload(workload_dict, interval_ms=args.interval_ms, output_file=f"plot/azure.pdf")

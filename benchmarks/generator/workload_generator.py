import logging
import math
import random
import argparse
import time
import pandas as pd
import numpy as np

from pandas import Timedelta
from typing import List, Tuple, Dict, Any
from transformers import PreTrainedTokenizerBase
from datetime import timedelta
from sample_request import (load_requests,  
                            sample_requests_len_range, 
                            sample_requests_all,
                            )
from distribution import (generate_poisson_dist,
                          generate_token_len_from_percentiles,
                          to_fluctuate_pattern_config,
                          )
                          
from utils import (convert_to_stat_df,
                   read_distribution_stats,
                   get_tokenizer, 
                   plot_workload, 
                   make_serializable, 
                   load_config,
                   save_workload, 
                   )

# Set up logging to print only warning and above level messages
logging.basicConfig(level=logging.INFO)


def generate_from_internal_csv(prompt_file_path: str, 
                            duration_ms: int,
                            tokenizer: PreTrainedTokenizerBase,       
                            qps_stat: str = None,
                            input_stat: str = None,
                            output_stat: str = None,
                            qps_scale: float = 1.0,
                            input_scale: float = 1.0,
                            output_scale: float = 1.0,
                            internal_trace_type: str = 'maas',
                            model: str = None,
                            output_file: str = 'output/output',
                            to_jsonl: bool = False,
                            ) -> Dict[str, Any]:
    merged_df = convert_to_stat_df(qps_stat, input_stat, output_stat, internal_trace_type)
    input_len_configs, output_len_configs, rps_configs = read_distribution_stats(merged_df)
    input_len_dist = []
    output_len_dist = []
    rps_dist = []
    for rps_config in rps_configs:
        rps_segment = generate_poisson_dist(target = rps_config['mean_rps'], sample_size = rps_config['total_seconds'], generate_poisson_dist = 10)
        rps_dist.extend(rps_segment)
    if internal_trace_type == "maas":
        for config in input_len_configs:
            config['scale'] = input_scale
            input_segment = generate_token_len_from_percentiles(**config)
            input_len_dist.extend(input_segment)
        for config in output_len_configs:
            config['scale'] = output_scale
            output_segment = generate_token_len_from_percentiles(**config)
            output_len_dist.extend(output_segment)
    elif internal_trace_type == "cloudide":
        for config in input_len_configs:
            config['scale'] = input_scale
            input_segment = generate_token_len_from_percentiles(**config)
            input_len_dist.extend(input_segment)
            output_segment = generate_token_len_from_percentiles(**config)
            output_len_dist.extend(output_segment)
    
    workload = generate_synthetic_from_dist(
        prompt_file_path = prompt_file_path,
        tokenizer = tokenizer,
        duration_ms =  duration_ms,
        rps_dist = rps_dist,
        input_token_len_dist = input_len_dist,
        output_token_len_dist = output_len_dist,
        qps_scale = qps_scale,
        input_scale = input_scale,
        output_scale = output_scale,
        model = model,
    )
    
    workload = make_serializable(workload)
    save_workload(workload, output_file, use_jsonl=to_jsonl)
    return workload
    
def generate_synthetic_from_dist(
        prompt_file_path: str,
        tokenizer: PreTrainedTokenizerBase,
        duration_ms: int,
        rps_dist: List[int],
        input_token_len_dist: List[int],
        output_token_len_dist: List[int],
        qps_scale: float,
        input_scale: float,
        output_scale: float,
        model: str = None,
    ) -> List[Dict[str, Any]]:
    
    if not (len(rps_dist) == len(input_token_len_dist) == len(output_token_len_dist)):
        raise ValueError(f"All distributions must have the same length, len(rps_dist): {len(rps_dist)}, len(input_token_len_dist): {len(input_token_len_dist)}, len(output_token_len_dist): {len(output_token_len_dist)}")
    workload = []
    current_time = 0
    total_seconds = len(rps_dist)
    ts = time.time()
    prompt_df = load_requests(dataset_path=prompt_file_path, tokenizer=tokenizer)
    logging.info(f"Load requests took {int(time.time() - ts)}s")
    while current_time < total_seconds * 1000:
        time_idx = int(current_time / 1000)
        if time_idx >= total_seconds:
            time_idx = total_seconds - 1
        current_rate = rps_dist[time_idx] / qps_scale
        current_input_len = input_token_len_dist[time_idx] / input_scale
        current_output_len = output_token_len_dist[time_idx] / output_scale
        inter_arrival_time = 1000 if current_rate == 0 else np.random.exponential(scale=1000/current_rate) 
        current_time += inter_arrival_time
        if current_time < total_seconds * 1000:
            request = sample_requests_len_range(
                df=prompt_df,
                num_requests=1,
                input_lens=[current_input_len],
                output_lens=[current_output_len],
                initial_err_perc=0.5,
                err_step=0.05,
                model=model,
            )
            workload.append({"timestamp": int(current_time), "requests": request})  
            if current_time > duration_ms:
                break
        
    return workload

def generate_constant(prompt_file_path: str,
                       qps: int, 
                       duration_ms: int = None,
                       interval_ms: int = None,
                       model: str = None,
                       output_file: str = 'output/output',
                       to_jsonl: bool = False,
                       ) -> List[List[Any]]:
    workload = []
    ts = 0
    sharegpt_df = load_requests(dataset_path=prompt_file_path, tokenizer=tokenizer)
    while ts < duration_ms:
        concurrent_reqs = sample_requests_len_range(
            df=sharegpt_df,
            num_requests=qps,
            input_lens=[None] * qps, 
            output_lens=[None] * qps, 
            initial_err_perc=0.5,
            err_step=0.05,
            model=model,
        )
        if concurrent_reqs:  # Only add non-empty groups
            workload.append({"timestamp": ts, "requests": concurrent_reqs})  
        else:
            logging.error(f"sampled return {concurrent_reqs}")
        ts += interval_ms
    ### Generate constant load for all requests
    # idx = 0
    # while idx < len(sharegpt_df):
    #     concurrent_reqs = sample_requests_all(df=sharegpt_df, start_idx=idx, qps=qps)
    #     workload.append({"timestamp": ts, "requests": concurrent_reqs})  
    #     idx += qps
    #     ts += interval_ms
   
    workload = make_serializable(workload)
    save_workload(workload, output_file, use_jsonl=to_jsonl)
    return workload

def generate_synthetic(prompt_file_path: str,
                       qps_pattern_config: Dict[str, Any],
                       input_pattern_config: Dict[str, Any],
                       output_pattern_config: Dict[str, Any],
                       duration_ms: int = None,
                       interval_ms: int = None,
                       model: str = None,
                       output_file: str = 'output/output',
                       to_jsonl: bool = False,
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

    def math_function(t, pattern_config, length, prev_value):
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
        assert length is not None, \
        "length cannot be None"
        if pattern_config['omega'] is None:
            omega = 2 * math.pi / (length / pattern_config['period'])
        trend = pattern_config['A'] * math.sin(omega * t) + pattern_config['B']
        noise = random.gauss(0, pattern_config['sigma'])
        current_value = round(trend + noise)
        if pattern_config['only_rise']:
            current_value = max(prev_value, current_value)
            prev_value = current_value
        return current_value, prev_value

    assert duration_ms is not None and interval_ms is not None, \
        "duration_ms and interval_ms must be specified."
    length = int(duration_ms // interval_ms) + 1
    workload = []
    t = 0
    previous_concurrency = -1
    previous_input_len = -1
    previous_output_len = -1
    ts = 0
    base_req_id = 0
    
    sharegpt_df = load_requests(dataset_path=prompt_file_path, tokenizer=tokenizer)
    while t < length:
        current_concurrency, previous_concurrency = math_function(t, qps_pattern_config, length, previous_concurrency)
        current_input_len, previous_input_len = math_function(t, input_pattern_config, length, previous_input_len)
        current_output_len, previous_output_len = math_function(t, output_pattern_config, length, previous_output_len)
        current_concurrency_pois = generate_poisson_dist(target = current_concurrency, sample_size = 1)
        current_input_len_pois = generate_poisson_dist(target = current_input_len, sample_size = 1)
        current_output_len_pois = generate_poisson_dist(target = current_output_len, sample_size = 1)
        # start from last end index
        logging.debug(f"search requests for current_concurrency {current_concurrency} : {current_concurrency_pois} input_lens {current_input_len} : {current_input_len_pois} output_lens {current_output_len} : {current_output_len_pois}")
        concurrent_reqs = sample_requests_len_range(
            df=sharegpt_df,
            num_requests=current_concurrency,
            input_lens=[current_input_len] * current_concurrency, 
            output_lens=[current_output_len] * current_concurrency, 
            initial_err_perc=0.1,
            err_step=0.05,
            model=model,
        )
        workload.append({"timestamp": ts, "requests": concurrent_reqs})  
        base_req_id += current_concurrency
        ts += interval_ms
        t += 1
   
    workload = make_serializable(workload)
    save_workload(workload, output_file, use_jsonl=to_jsonl)
    return workload


def generate_from_azure_csv(file_path: str,
                            prompt_file_path: str,
                            duration_ms: int,
                            tokenizer: PreTrainedTokenizerBase,
                            interval_ms: int,
                            model: str = None,
                            output_file: str = 'output/output',
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
    tracing_file_end_time = df.index.max()
    end_time = current_time + Timedelta(milliseconds=duration_ms)
    if tracing_file_end_time < end_time:
        logging.warning(f"{tracing_file_end_time} can not cover duration {duration_ms}, cap to end time of tracing file")
        end_time = tracing_file_end_time

    logging.info(f"Start generation from time {current_time} to {end_time}")
    sharegpt_df = load_requests(dataset_path=prompt_file_path, tokenizer=tokenizer)

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
        sampled_requests = sample_requests_len_range(
            df=sharegpt_df,
            num_requests=len(input_lens),
            input_lens=input_lens,
            output_lens=output_lens,
            initial_err_perc=0.5,
            err_step=0.05,
            model=model,
        )

        if sampled_requests:  # Only add non-empty groups
            grouped_requests.append({"timestamp": ts, "requests": sampled_requests})
        ts += interval_ms
        if ts > duration_ms:
            break
        # Move to the next time range
        current_time += time_range

    # Save to file
    grouped_requests = make_serializable(grouped_requests)
    save_workload(grouped_requests, output_file, use_jsonl=to_jsonl)

    return grouped_requests



if __name__ == '__main__':
    parser = argparse.ArgumentParser(description='Workload Generator')
    parser.add_argument('--prompt-file', type=str, required=True, help='File containing sampling prompts.')
    parser.add_argument('--trace-type', type=str, required=True, choices=['constant','synthetic', 'internal', 'azure'],
                        help='Type of trace consumed. Choose among: synthetic, internal, azure.')
    parser.add_argument('--model', type=str, required=False, default="Qwen/Qwen2.5-Coder-7B-Instruct",
                        help='Target model for the workload.')
    parser.add_argument('--tokenizer', type=str, required=False, default="Qwen/Qwen2.5-Coder-7B-Instruct",
                        help='Target model tokenizer.')
    parser.add_argument('--interval-ms', type=int, required=False, default=1000,
                        help='Granularity of request injection interval in milliseconds.')
    parser.add_argument('--duration-ms', type=int, default=60000, help='Duration of the trace generated.')
    parser.add_argument('--group-interval-seconds', type=int, default=1, help='Grouping interval seconds.')
    parser.add_argument('--internal-trace-type', type=str, choices=['maas', 'cloudide'], default="maas", help='Type of internal traces.')
    parser.add_argument('--adapter-name', type=str, required=False, default=None, help='Adapter name associated with workload (if applied).')
    parser.add_argument('--output-dir', type=str, required=False, default="output", help='Output directory to save.'
                                                                                         'the workload.')
    parser.add_argument('--output-format', type=str, choices=['json', 'jsonl'], default='json',
                        help='Set output data format to either .json or .jsonl (default is .json).')
    
    ###### Synthetic and constant workload
    parser.add_argument('--target-qps', type=int, required=False, default=1, help='Target QPS for the workload.')
    parser.add_argument('--traffic-pattern', type=str, required=False, choices=['quick_rising', 'slow_rising', 'slight_fluctuation', 'severe_fluctuation'], default=None,
                        help='Traffic patterns used for synthetic workload type.')
    parser.add_argument('--prompt-len-pattern', type=str, required=False, choices=['quick_rising', 'slow_rising', 'slight_fluctuation', 'severe_fluctuation'], default=None,
                        help='Prompt lengths patterns used for synthetic workload type.')
    parser.add_argument('--completion-len-pattern', type=str, required=False, choices=['quick_rising', 'slow_rising', 'slight_fluctuation', 'severe_fluctuation'], default=None,
                        help='Prompt lengths patterns used for synthetic workload type.')
    parser.add_argument('--traffic-pattern-config', type=str, required=False, default=None,
                        help='Traffic configuration file used for synthetic workload type.')
    parser.add_argument('--prompt-len-pattern-config', type=str, required=False, default=None,
                        help='Prompt lengths configuration file used for synthetic workload type.')
    parser.add_argument('--completion-len-pattern-config', type=str, required=False, default=None,
                        help='Completion lengths configuration file used for synthetic workload type.')
    
    ##### Trace and stats-driven workload
    parser.add_argument('--traffic-file', type=str, required=False, default=None,
                        help='Traffic file containing times of arrival, which workload generator depends upon to'
                             'convert to traffic used in workload. This is only needed for for internal and azure trace type.')
    parser.add_argument('--prompt-len-file', type=str, required=False, default=None,
                        help='File containing request input lengths varied by time, which workload generator depends upon to '
                             'select input prompt. This is only needed for for internal trace type. ')
    parser.add_argument('--completion-len-file', type=str, required=False, default=None,
                        help='File containing request output lengths varied by time, which workload generator depends upon to '
                             'select input prompt. This is only needed for for internal trace type. ')
    parser.add_argument('--qps-scale', type=float, required=False, default=1.0, help='QPS scaling factor.')
    parser.add_argument('--input-scale', type=float, required=False, default=1.0, help='Input length scaling factor.')
    parser.add_argument('--output-scale', type=float, required=False, default=1.0, help='Output length scaling factor.')
    
    args = parser.parse_args()

    # Generate workloads and pair with prompts
    workload_dict = {}
    tokenizer = get_tokenizer(pretrained_model_name_or_path=args.tokenizer, trust_remote_code=True)

    if args.trace_type == "synthetic":
        if args.traffic_pattern and args.prompt_len_pattern and args.completion_len_pattern:
            logging.info(f"Generating synthetic workload with traffic pattern: {args.traffic_pattern}, prompt length pattern: {args.prompt_len_pattern}, completion length pattern: {args.completion_len_pattern}")
            comp_pattern_type = f"synthetic_QPS_{args.traffic_pattern}_INPUT_{args.prompt_len_pattern}_OUTPUT_{args.completion_len_pattern}"
            qps_pattern_config = to_fluctuate_pattern_config(config_type = args.traffic_pattern, mean = 6)
            input_pattern_config = to_fluctuate_pattern_config(config_type = args.prompt_len_pattern, mean = 1024)
            output_pattern_config = to_fluctuate_pattern_config(config_type = args.completion_len_pattern, mean = 1024)
            logging.debug(f"qps_pattern_config {qps_pattern_config}")
            logging.debug(f"input_pattern_config {input_pattern_config}")
            logging.debug(f"output_pattern_config {output_pattern_config}")
            generated_workload = generate_synthetic(prompt_file_path = args.prompt_file,
                                                    qps_pattern_config = qps_pattern_config,
                                                    input_pattern_config = input_pattern_config,
                                                    output_pattern_config = output_pattern_config,
                                                    duration_ms=args.duration_ms,
                                                    interval_ms=args.interval_ms,
                                                    model=args.model,
                                                    output_file=f"{args.output_dir}/{comp_pattern_type}",
                                                    to_jsonl=(args.output_format == "jsonl"),
                                                )
            workload_dict[comp_pattern_type] = generated_workload
        elif args.traffic_pattern_config and args.prompt_len_pattern_config and args.completion_len_pattern_config:
            logging.info(f"Generating synthetic workload with traffic pattern config: {args.traffic_pattern_config}, prompt length pattern config: {args.prompt_len_pattern_config}, completion length pattern config: {args.completion_len_pattern_config}")
            comp_pattern_type = f"synthetic_manual_config"
            qps_pattern_config = load_config(args.traffic_pattern_config)
            input_pattern_config = load_config(args.prompt_len_pattern_config)
            output_pattern_config = load_config(args.completion_len_pattern_config)
            logging.debug(f"qps_pattern_config {qps_pattern_config}")
            logging.debug(f"input_pattern_config {input_pattern_config}")
            logging.debug(f"output_pattern_config {output_pattern_config}")
            generated_workload = generate_synthetic(prompt_file_path = args.prompt_file,
                                                    qps_pattern_config = qps_pattern_config,
                                                    input_pattern_config = input_pattern_config,
                                                    output_pattern_config = output_pattern_config,
                                                    duration_ms=args.duration_ms,
                                                    interval_ms=args.interval_ms,
                                                    model=args.model,
                                                    output_file=f"{args.output_dir}/{comp_pattern_type}",
                                                    to_jsonl=(args.output_format == "jsonl"),
                                                )
            workload_dict[comp_pattern_type] = generated_workload
    else:
        # Process for 'internal' and 'azure'
        if args.trace_type == "constant":
            generated_workload = generate_constant(prompt_file_path=args.prompt_file, 
                                                    qps=1,
                                                    duration_ms=args.duration_ms, 
                                                    interval_ms=args.interval_ms,
                                                    adapter_name=args.adapter_name,
                                                    output_file=f"{args.output_dir}/{args.trace_type}",
                                                    to_jsonl=(args.output_format == "jsonl"),
                                                    )
        elif args.trace_type == "internal":
            generated_workload = generate_from_internal_csv(prompt_file_path=args.prompt_file, 
                                                            duration_ms=args.duration_ms, 
                                                            tokenizer=tokenizer,
                                                            qps_stat=args.traffic_file, 
                                                            input_stat=args.prompt_len_file, 
                                                            output_stat=args.completion_len_file,
                                                            qps_scale=args.qps_scale,
                                                            input_scale=args.input_scale,
                                                            output_scale=args.output_scale,
                                                            internal_trace_type=args.internal_trace_type,
                                                            model=args.model,
                                                            output_file=f"{args.output_dir}/{args.trace_type}",
                                                            to_jsonl=(args.output_format == "jsonl"),
                                                            )

        elif args.trace_type == "azure":
            generated_workload = generate_from_azure_csv(file_path=args.traffic_file, 
                                                         prompt_file_path=args.prompt_file,
                                                         duration_ms=args.duration_ms, 
                                                         tokenizer=tokenizer,
                                                         interval_ms=args.interval_ms, 
                                                         model=args.model,
                                                         output_file=f"{args.output_dir}/{args.trace_type}",
                                                         to_jsonl=(args.output_format == "jsonl"),
                                                         )

        workload_dict[args.trace_type] = generated_workload

    if workload_dict:
        # Plot the workloads
        for workload_name, workload in workload_dict.items():
            plot_workload(
                workload_name = workload_name, 
                workload = workload, 
                bin_size_sec = int(args.interval_ms/1000), 
                output_dir = f"./plot")

import logging
import argparse
import time
import pandas as pd
import numpy as np

from pandas import Timedelta
from typing import List, Dict, Any
from transformers import PreTrainedTokenizerBase
from datetime import timedelta
from generator.workload_generator.sample_request import (load_requests,  
                            RequestFinder,
                        )
from generator.workload_generator.distribution import (generate_poisson_dist,
                          generate_token_len_from_percentiles,
                          to_fluctuate_pattern_config,
                          user_to_synthetic_config,
                          sine_fluctuation,
                          )
                          
from generator.workload_generator.utils import (if_sessioned_dataset,
                   convert_to_stat_df,
                   read_distribution_stats,
                   get_tokenizer, 
                   plot_workload, 
                   make_serializable, 
                   load_config,
                   save_workload, 
                   )

# Set up logging to print only warning and above level messages
logging.basicConfig(level=logging.INFO)


def generate_from_stat_csv(prompt_file_path: str, 
                            duration_ms: int,
                            tokenizer: PreTrainedTokenizerBase,       
                            qps_stat: str = None,
                            input_stat: str = None,
                            output_stat: str = None,
                            qps_scale: float = 1.0,
                            input_scale: float = 1.0,
                            output_scale: float = 1.0,
                            stat_trace_type: str = 'maas',
                            max_concurrent_sessions: int = None,
                            output_file: str = 'output/output',
                            to_jsonl: bool = False,
                            ) -> Dict[str, Any]:
    merged_df = convert_to_stat_df(qps_stat, input_stat, output_stat, stat_trace_type)
    input_len_configs, output_len_configs, rps_configs = read_distribution_stats(merged_df)
    input_len_dist = []
    output_len_dist = []
    rps_dist = []
    for rps_config in rps_configs:
        rps_segment = generate_poisson_dist(target = rps_config['mean_rps'], sample_size = rps_config['total_seconds'], smooth_window_size = 10)
        rps_dist.extend(rps_segment)
    if stat_trace_type == "maas":
        for config in input_len_configs:
            config['scale'] = input_scale
            input_segment = generate_token_len_from_percentiles(**config)
            input_len_dist.extend(input_segment)
        for config in output_len_configs:
            config['scale'] = output_scale
            output_segment = generate_token_len_from_percentiles(**config)
            output_len_dist.extend(output_segment)
    elif stat_trace_type == "cloudide":
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
        max_concurrent_sessions = max_concurrent_sessions,
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
        max_concurrent_sessions: int,
    ) -> List[Dict[str, Any]]:
    
    if input_token_len_dist is not None and output_token_len_dist is not None: 
        if not (len(rps_dist) == len(input_token_len_dist) == len(output_token_len_dist)):
            raise ValueError(f"All distributions must have the same length, len(rps_dist): {len(rps_dist)}, len(input_token_len_dist): {len(input_token_len_dist)}, len(output_token_len_dist): {len(output_token_len_dist)}")
    workload = []
    current_time = 0
    total_seconds = len(rps_dist)
    logging.debug(f"total_seconds {total_seconds} rps_dist {rps_dist}")
    ts = time.time()
    prompt_df = load_requests(dataset_path=prompt_file_path, tokenizer=tokenizer)
    logging.info(f"Load requests took {int(time.time() - ts)}s")
    request_finder = RequestFinder(df=prompt_df)
    while current_time < total_seconds * 1000:
        time_idx = int(current_time / 1000)
        if time_idx >= total_seconds:
            time_idx = total_seconds - 1
        current_rate = rps_dist[time_idx] / qps_scale
        current_input_len = None
        current_output_len = None
        if input_token_len_dist is not None:
            current_input_len = input_token_len_dist[time_idx] / input_scale if input_token_len_dist[time_idx] else None 
        if output_token_len_dist is not None: 
            current_output_len = output_token_len_dist[time_idx] / output_scale if output_token_len_dist[time_idx] else None
        inter_arrival_time = 1000 if current_rate == 0 else np.random.exponential(scale=1000/current_rate) 
        current_time += inter_arrival_time
        if current_time < total_seconds * 1000:
            if current_rate != 0:
                if max_concurrent_sessions and if_sessioned_dataset(prompt_df):
                    request = request_finder.find_requests_max_session(
                        num_requests=1,
                        max_concurrent_session = max_concurrent_sessions,
                    )
                else:
                    request = request_finder.find_requests_len_range(
                        num_requests=1,
                        input_lens=[current_input_len],
                        output_lens=[current_output_len],
                        initial_err_perc=0.5,
                        err_step=0.05,
                    )
            else:
                request = []
            workload.append({"timestamp": int(current_time), "requests": request})  
            if current_time > duration_ms:
                break
        
    return workload

def generate_constant(prompt_file_path: str,
                    tokenizer: PreTrainedTokenizerBase,
                    qps: int, 
                    input_len: int = None,
                    output_len: int = None,
                    duration_ms: int = None,
                    interval_ms: int = None,
                    max_concurrent_sessions: int = None,
                    output_file: str = 'output/output',
                    to_jsonl: bool = False,
                    ) -> List[List[Any]]:
    workload = []
    ts = 0
    
    # if input_len != None and output_len != None:
    rps_dist = []
    input_token_len_dist = None
    if input_len != None:
        input_token_len_dist = []
    if output_len != None:
        output_token_len_dist = []
    output_token_len_dist = None
    while ts < duration_ms:
        rps_dist.append(qps)
        if input_len != None:
            input_token_len_dist.append(input_len)
        if output_len != None:
            output_token_len_dist.append(output_len)
        ts += interval_ms
    workload = generate_synthetic_from_dist(
        prompt_file_path = prompt_file_path,
        tokenizer = tokenizer,
        duration_ms =  duration_ms,
        rps_dist = rps_dist,
        input_token_len_dist = input_token_len_dist,
        output_token_len_dist = output_token_len_dist,
        qps_scale = 1.0,
        input_scale = 1.0,
        output_scale = 1.0,
        max_concurrent_sessions = max_concurrent_sessions
    )
        
    
    ### Generate constant load for all requests
    # idx = 0
    # request_finder = RequestFinder(df=sharegpt_df)
    # while idx < len(sharegpt_df):
    #     concurrent_reqs = request_finder.sample_requests_all(start_idx=idx, qps=qps)
    #     workload.append({"timestamp": ts, "requests": concurrent_reqs})  
    #     idx += qps
    #     ts += interval_ms
   
    workload = make_serializable(workload)
    save_workload(workload, output_file, use_jsonl=to_jsonl)
    return workload

def generate_synthetic(prompt_file_path: str,
                       tokenizer: PreTrainedTokenizerBase,
                       qps_pattern_config: Dict[str, Any],
                       input_pattern_config: Dict[str, Any],
                       output_pattern_config: Dict[str, Any],
                       duration_ms: int = None,
                       interval_ms: int = None,
                       max_concurrent_sessions: int = None,
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


    assert duration_ms is not None and interval_ms is not None, \
        "duration_ms and interval_ms must be specified."
    num_intervals = int(duration_ms // interval_ms) + 1
    workload = []
    interval = 0
    previous_rate = -1
    previous_input_len = None
    previous_output_len = None
    ts = 0
    
    rps_dist = []
    input_token_len_dist = []
    output_token_len_dist = []
    while interval < num_intervals:
        current_rate, previous_rate = sine_fluctuation(interval, qps_pattern_config, num_intervals, previous_rate)
        current_input_len = None
        current_output_len = None
        if input_pattern_config:
            current_input_len, previous_input_len = sine_fluctuation(interval, input_pattern_config, num_intervals, previous_input_len) 
            current_input_len = current_input_len if current_input_len > 0 else 1
        if output_pattern_config:
            current_output_len, previous_output_len = sine_fluctuation(interval, output_pattern_config, num_intervals, previous_output_len)
            current_output_len = current_output_len if current_output_len > 0 else 1
        rps_dist.append(current_rate)
        input_token_len_dist.append(current_input_len)
        output_token_len_dist.append(current_output_len)
        ts += interval_ms
        interval += 1
        
    workload = generate_synthetic_from_dist(
        prompt_file_path = prompt_file_path,
        tokenizer = tokenizer,
        duration_ms =  duration_ms,
        rps_dist = rps_dist,
        input_token_len_dist = input_token_len_dist,
        output_token_len_dist = output_token_len_dist,
        qps_scale = 1.0,
        input_scale = 1.0,
        output_scale = 1.0,
        max_concurrent_sessions = max_concurrent_sessions,
    )
    workload = make_serializable(workload)
    save_workload(workload, output_file, use_jsonl=to_jsonl)
    return workload


def generate_from_azure_csv(file_path: str,
                            prompt_file_path: str,
                            duration_ms: int,
                            tokenizer: PreTrainedTokenizerBase,
                            interval_ms: int,
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
    request_finder = RequestFinder(df=sharegpt_df)
    while current_time <= end_time:
        # Select requests within the current time range
        mask = (df.index >= current_time) & (df.index < current_time + time_range)
        group = df.loc[mask]
        input_lens = []
        output_lens = []
        for _, row in group.iterrows():
            input_lens.append(int(row['ContextTokens']))
            output_lens.append(int(row['GeneratedTokens']))
        sampled_requests = request_finder.find_requests_len_range(
            num_requests=len(input_lens),
            input_lens=input_lens,
            output_lens=output_lens,
            initial_err_perc=0.1,
            err_step=0.05,
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

def main(args):
    # Generate workloads and pair with prompts
    workload_dict = {}
    tokenizer = get_tokenizer(pretrained_model_name_or_path=args.model, trust_remote_code=True)

    if args.trace_type == "synthetic":
        qps_pattern_config = None
        input_pattern_config = None
        output_pattern_config = None
        comp_pattern_type = f"synthetic_manual_config"
        if args.traffic_pattern:
            qps_pattern_config = to_fluctuate_pattern_config(config_type = args.traffic_pattern, mean = 6)
        elif args.traffic_pattern_config:
            qps_pattern_config = user_to_synthetic_config(user_config = load_config(args.traffic_pattern_config), duration_ms = args.duration_ms)
            
        if args.prompt_len_pattern:
            input_pattern_config = to_fluctuate_pattern_config(config_type = args.prompt_len_pattern, mean = 1024)
        elif args.prompt_len_pattern_config:
            input_pattern_config = user_to_synthetic_config(user_config = load_config(args.prompt_len_pattern_config), duration_ms = args.duration_ms)
            
        if args.completion_len_pattern:
            output_pattern_config = to_fluctuate_pattern_config(config_type = args.completion_len_pattern, mean = 1024)
        elif args.completion_len_pattern_config:
            output_pattern_config = user_to_synthetic_config(user_config = load_config(args.completion_len_pattern_config), duration_ms = args.duration_ms)
        
        if qps_pattern_config is None:
            raise ValueError(f"qps_pattern_config cannot be None")
        
        generated_workload = generate_synthetic(prompt_file_path = args.prompt_file,
                                                tokenizer=tokenizer,
                                                qps_pattern_config = qps_pattern_config,
                                                input_pattern_config = input_pattern_config,
                                                output_pattern_config = output_pattern_config,
                                                duration_ms=args.duration_ms,
                                                interval_ms=args.interval_ms,
                                                max_concurrent_sessions=args.max_concurrent_sessions,
                                                output_file=f"{args.output_dir}/workload",
                                                to_jsonl=(args.output_format == "jsonl"),
                                            )
        workload_dict[comp_pattern_type] = generated_workload
    else:
        # Process for 'stat' and 'azure'
        if args.trace_type == "constant":
            generated_workload = generate_constant(prompt_file_path=args.prompt_file, 
                                                   tokenizer=tokenizer,
                                                    qps=args.target_qps,
                                                    input_len=args.target_prompt_len,
                                                    output_len=args.target_completion_len,
                                                    duration_ms=args.duration_ms, 
                                                    interval_ms=args.interval_ms,
                                                    max_concurrent_sessions=args.max_concurrent_sessions,
                                                    output_file=f"{args.output_dir}/workload",
                                                    to_jsonl=(args.output_format == "jsonl"),
                                                )
        elif args.trace_type == "stat":
            generated_workload = generate_from_stat_csv(prompt_file_path=args.prompt_file, 
                                                            duration_ms=args.duration_ms, 
                                                            tokenizer=tokenizer,
                                                            qps_stat=args.traffic_file, 
                                                            input_stat=args.prompt_len_file, 
                                                            output_stat=args.completion_len_file,
                                                            qps_scale=args.qps_scale,
                                                            input_scale=args.input_scale,
                                                            output_scale=args.output_scale,
                                                            stat_trace_type=args.stat_trace_type,
                                                            max_concurrent_sessions=args.max_concurrent_sessions,
                                                            output_file=f"{args.output_dir}/workload",
                                                            to_jsonl=(args.output_format == "jsonl"),
                                                            )

        elif args.trace_type == "azure":
            generated_workload = generate_from_azure_csv(file_path=args.traffic_file, 
                                                         prompt_file_path=args.prompt_file,
                                                         duration_ms=args.duration_ms, 
                                                         tokenizer=tokenizer,
                                                         interval_ms=args.interval_ms, 
                                                         output_file=f"{args.output_dir}/workload",
                                                         to_jsonl=(args.output_format == "jsonl"),
                                                         )

        workload_dict[args.trace_type] = generated_workload

    if workload_dict:
        # Plot the workloads
        for workload_name, workload in workload_dict.items():
            plot_workload(
                workload = workload, 
                bin_size_sec = 1, 
                output_dir = f"{args.output_dir}")


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description='Workload Generator')
    parser.add_argument('--prompt-file', type=str, required=True, help='File containing sampling prompts.')
    parser.add_argument('--trace-type', type=str, required=True, choices=['constant','synthetic', 'stat', 'azure'],
                        help='Type of trace consumed. Choose among: synthetic, stat, azure.')
    parser.add_argument('--model', type=str, required=False, default="Qwen/Qwen2.5-Coder-7B-Instruct",
                        help='Target model for the workload.')
    parser.add_argument('--interval-ms', type=int, required=False, default=1000,
                        help='Granularity of request injection interval in milliseconds.')
    parser.add_argument('--duration-ms', type=int, default=60000, help='Duration of the trace generated.')
    parser.add_argument('--group-interval-seconds', type=int, default=1, help='Grouping interval seconds.')
    parser.add_argument('--stat-trace-type', type=str, choices=['maas', 'cloudide'], default="maas", help='Type of stat traces.')
    parser.add_argument('--output-dir', type=str, required=False, default="output", help='Output directory to save.'
                                                                                         'the workload.')
    parser.add_argument('--output-format', type=str, choices=['json', 'jsonl'], default='json',
                        help='Set output data format to either .json or .jsonl (default is .json).')
    
    ###### Synthetic and constant workload
    parser.add_argument('--target-qps', type=int, required=False, default=1, help='Target QPS for the workload.')
    parser.add_argument('--target-prompt-len', type=int, required=False, default=None, help='Target prompt length for the workload.')
    parser.add_argument('--target-completion-len', type=int, required=False, default=None, help='Target completion length for the workload.')
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
                             'convert to traffic used in workload. This is only needed for for stat and azure trace type.')
    parser.add_argument('--prompt-len-file', type=str, required=False, default=None,
                        help='File containing request input lengths varied by time, which workload generator depends upon to '
                             'select input prompt. This is only needed for for stat trace type. ')
    parser.add_argument('--completion-len-file', type=str, required=False, default=None,
                        help='File containing request output lengths varied by time, which workload generator depends upon to '
                             'select input prompt. This is only needed for for stat trace type. ')
    parser.add_argument('--qps-scale', type=float, required=False, default=1.0, help='QPS scaling factor.')
    parser.add_argument('--input-scale', type=float, required=False, default=1.0, help='Input length scaling factor.')
    parser.add_argument('--output-scale', type=float, required=False, default=1.0, help='Output length scaling factor.')
    parser.add_argument('--max-concurrent-sessions', type=int, required=False, default=1, help='Maximum number of overlapping sessions.')
    
    args = parser.parse_args()
    main(args)
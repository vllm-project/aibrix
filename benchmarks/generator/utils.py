import logging
import json
import os
import csv

import numpy as np
import matplotlib.pyplot as plt
import pandas as pd

from typing import List, Union, Any, Optional, Tuple, Dict
from transformers import (AutoTokenizer, PreTrainedTokenizer,
                          PreTrainedTokenizerFast)
from datetime import datetime
from scipy import stats
from scipy.optimize import minimize

def convert_to_stat_df(qps_file: str, 
                       input_file: str, 
                       output_file: str,
                       internal_trace_type: str) -> pd.DataFrame:
    if internal_trace_type == "maas":
        # Load CSV files into DataFrames
        qps_df = pd.read_csv(qps_file)
        input_len_df = pd.read_csv(input_file)
        output_len_df = pd.read_csv(output_file)

        # Rename columns for merging and clarity
        input_len_df.rename(columns={"P50": "input_len_p50", "P70": "input_len_p70", "P90": "input_len_p90", "P99": "input_len_p99"}, inplace=True)
        output_len_df.rename(columns={"P50": "output_len_p50", "P70": "output_len_p70", "P95": "output_len_p90", "P99": "output_len_p99"}, inplace=True)
        qps_df.rename(columns={"Success": "qps_success"}, inplace=True)

        # Merge DataFrames on the 'Time' column (now renamed to 'timestamp')
        merged_df = pd.merge(input_len_df, output_len_df, on="Time")
        merged_df = pd.merge(merged_df, qps_df, on="Time")

        # Drop unwanted columns (if needed)
        merged_df.drop(columns=["Total", "5xx Error", "4xx Error"], inplace=True)

        # Rename the 'Time' column to 'timestamp'
        merged_df.rename(columns={"Time": "timestamp"}, inplace=True)

        # Rearrange columns to match the desired order
        merged_df = merged_df[[
            "timestamp",
            "input_len_p50", "input_len_p70", "input_len_p90", "input_len_p99",
            "output_len_p50", "output_len_p70", "output_len_p90", "output_len_p99",
            "qps_success"
        ]]
        merged_df['timestamp'] = pd.to_datetime(merged_df['timestamp'])
    elif internal_trace_type == "cloudide":
        if input_file != output_file:
            logging.error(f"input file {input_file} does not match output_file {output_file}")
        df = pd.read_csv(input_file, parse_dates=['Time'])
        df = df.replace("undefined", 0)
        df['Time'] = pd.to_datetime(df['Time'], unit = 'ms')  # Ensure timestamp is a datetime object
        df = df.set_index('Time')  # Set 'Time' as index for rolling window calculation
        df_rate = pd.read_csv(qps_file, parse_dates=['Time'])
        df_rate.columns.values[1] = "Rate"
        df_rate = df_rate.replace("undefined", 0)
        df_rate['Time'] = pd.to_datetime(df_rate['Time'], unit = 'ms') 
        df_rate = df_rate.set_index('Time')
        
        sent_columns = df.filter(regex = r'^sent_bytes.rate@')
        sent_columns = sent_columns.apply(pd.to_numeric, errors='coerce').fillna(0)
        df['sent'] = sent_columns.sum(axis = 1)
        
        recv_columns = df.filter(regex = r'^recv_bytes.rate@')
        recv_columns = recv_columns.apply(pd.to_numeric, errors='coerce').fillna(0)
        df['recv'] = recv_columns.sum(axis = 1)
        
        df_merged = pd.merge(df, df_rate, left_index=True, right_index=True, how='outer')
        df_merged = df_merged.fillna(0)
        df_merged = df_merged.apply(pd.to_numeric, errors='coerce').fillna(0)
        
        df_merged['sent_rate'] = df_merged.apply(lambda row : 0 if row['Rate'] == 0 else row['sent'] / row['Rate'], axis=1)
        df_merged['recv_rate'] = df_merged.apply(lambda row : 0 if row['Rate'] == 0 else row['recv'] / row['Rate'], axis=1)
        
        df_merged = df_merged.reset_index()
        merged_df = pd.DataFrame({
            "timestamp": df_merged['Time'],
            "input_len_p50": df_merged['recv_rate'], 
            "input_len_p70": df_merged['recv_rate'],
            "input_len_p90": df_merged['recv_rate'],
            "input_len_p99": df_merged['recv_rate'],
            "output_len_p50": df_merged['sent_rate'], 
            "output_len_p70": df_merged['sent_rate'], 
            "output_len_p90": df_merged['sent_rate'], 
            "output_len_p99": df_merged['sent_rate'], 
            "qps_success":df_merged['Rate'], 
        })
    return merged_df

def read_distribution_stats(df: pd.DataFrame) -> Tuple[List[Dict], List[Dict], List[Dict]]:
    time_diffs = df['timestamp'].diff().dt.total_seconds()
    section_in_seconds = int(time_diffs.mean())  # Use average time difference
    input_len_configs = []
    output_len_configs = []
    rps_configs = []
    for _, row in df.iterrows():
        input_len_configs.append({
            "p50": float(row['input_len_p50']),
            "p70": float(row['input_len_p70']),
            "p90": float(row['input_len_p90']),
            "p99": float(row['input_len_p99']),
            "period": section_in_seconds,
            "total_seconds": section_in_seconds
        })
        output_len_configs.append({
            "p50": float(row['output_len_p50']),
            "p70": float(row['output_len_p70']),
            "p90": float(row['output_len_p90']),
            "p99": float(row['output_len_p99']),
            "period": section_in_seconds,
            "total_seconds": section_in_seconds
        })
        rps_configs.append({
            "mean_rps": float(row['qps_success']),
            "amplitude": float(row['qps_success']) * 0.2,  # 20% variation
            "period": section_in_seconds,
            "total_seconds": section_in_seconds
        })
    return input_len_configs, output_len_configs, rps_configs

def generate_poisson_dist(target: int, sample_size: int, smooth_window_size: int = 1) -> List[int]:
    rps_list = np.random.poisson(lam=target, size=sample_size).tolist()
    # Apply moving average smoothing
    smoothed = []
    for i in range(len(rps_list)):
        start = max(0, i - smooth_window_size + 1)
        window = rps_list[start:i + 1]
        smoothed.append(max(1, round(sum(window) / len(window))))
    return smoothed

def generate_token_len_from_percentiles(
    p50: int,
    p70: int,
    p90: int,
    p99: int,
    period: int,
    total_seconds: int,
    scale: float,
    amplitude_factor: float = 0.2  # Controls the amplitude of sinusoidal variation
) -> List[int]:
    if not (p50 < p70 < p90 < p99):
        raise ValueError("Percentiles must be strictly increasing: p50 < p70 < p90 < p99")
    if p50 <= 0:
        raise ValueError("Token lengths must be positive")
    percentiles = [0.50, 0.70, 0.90, 0.99]
    token_lengths = [p50, p70, p90, p99]
    token_lengths = [x / scale for x in token_lengths]
    log_lengths = np.log(token_lengths)
    def objective(params, percs, lengths):
        mu, sigma = params
        expected = stats.norm.ppf(percs, mu, sigma)
        return np.sum((expected - lengths) ** 2)
    result = minimize(
        objective,
        x0=[np.mean(log_lengths), np.std(log_lengths)],
        args=(percentiles, log_lengths),
        method='Nelder-Mead'
    )
    mu, sigma = result.x
    t = np.arange(total_seconds)
    amplitude = p50 * amplitude_factor
    sinusoidal_variation = amplitude * np.sin(2 * np.pi * t / period)
    base_samples = np.random.lognormal(mu, sigma, size=total_seconds)
    scale_factor = p50 / np.median(base_samples)
    token_len_list = base_samples * scale_factor + sinusoidal_variation
    token_len_list = [int(max(1, x)) for x in token_len_list]
    return token_len_list

def to_fluctuate_pattern_config(config_type: str,
                                mean: float,
                            ) -> Dict[str, Any]:
    if config_type == 'quick_rising':
        return {'A': 0.5 * mean, 
                'B': mean, 
                'sigma': 0.1,
                'period': 5,
                'omega': None,
                'only_rise': True}
    elif config_type == 'slow_rising':
        return {'A': 0.5 * mean, 
                'B': mean, 
                'sigma': 0.1,
                'period': 0.25,
                'omega': None,
                'only_rise': True}
    elif config_type == 'slight_fluctuation':
        return {'A': 0.1 * mean, 
                'B': mean, 
                'sigma': 0.1,
                'period': 1, 
                'omega': None,
                'only_rise': False}
    elif config_type == 'severe_fluctuation':
        return {'A': 0.5 * mean, 
                'B': mean,
                'sigma': 0.1,
                'period': 12, 
                'omega': None,
                'only_rise': False}
    else:
        raise ValueError(f"Unknown config type: {config_type}")


def get_sample_interval_ms(file_path):
    # Initialize variables
    timestamps = []

    # Read the file and extract the first two timestamps
    with open(file_path, 'r') as file:
        reader = csv.DictReader(file)
        for row in reader:
            if 'Time' in row and row['Time']:
                # Parse the timestamp
                timestamps.append(datetime.strptime(row['Time'], "%Y-%m-%d %H:%M:%S"))
            # Stop after reading the first two timestamps
            if len(timestamps) == 2:
                break

    # Calculate the interval in milliseconds
    interval = None
    if len(timestamps) == 2:
        interval = int((timestamps[1] - timestamps[0]).total_seconds() * 1000)
        logging.info(f"Sampling interval: {interval} milliseconds")
    else:
        logging.error("Insufficient data to calculate the sampling interval.")
    return interval


def make_serializable(data):
    """Recursively convert data into JSON serializable types."""
    if isinstance(data, list):
        return [make_serializable(item) for item in data]
    elif isinstance(data, tuple):
        return tuple(make_serializable(item) for item in data)
    elif isinstance(data, dict):
        return {key: make_serializable(value) for key, value in data.items()}
    elif isinstance(data, (np.integer, np.int64)):  # Convert NumPy int types to int
        return int(data)
    elif isinstance(data, (np.floating, np.float64)):  # Convert NumPy float types to float
        return float(data)
    else:
        return data


def get_tokenizer(
        pretrained_model_name_or_path: str, trust_remote_code: bool
) -> Union[PreTrainedTokenizer, PreTrainedTokenizerFast]:
    return AutoTokenizer.from_pretrained(pretrained_model_name_or_path,
                                         trust_remote_code=trust_remote_code)


def plot_workload(workload_name: str, 
                  workload: str,
                  bin_size_sec: int = 1,
                  output_dir: str = None):
    """
    Plots workload statistics: total requests, prompt token count, and output token count binned by time.

    Args:
        workload_name (str): Name of the workload.
        workload (list of dict): Workload entries with timestamps and request details.
        bin_size_sec (int): Size of each bin in seconds for aggregation.
        output_file (str, optional): File path to save the plot.
    """
    print(f"plot_workload in directory {output_dir}")
    # Convert workload data to a DataFrame
    data = []
    for entry in workload:
        timestamp_sec = entry["timestamp"] / 1000  # Convert ms to sec
        num_requests = len(entry["requests"])
        total_prompt_tokens = np.mean([req["prompt_length"] for req in entry["requests"]]) if entry["requests"] else 0
        total_output_tokens = np.mean([req["output_length"] for req in entry["requests"]]) if entry["requests"] else 0
        data.append((timestamp_sec, num_requests, total_prompt_tokens, total_output_tokens))

    df = pd.DataFrame(data, columns=["timestamp", "num_requests", "total_prompt_tokens", "total_output_tokens"])

    # Define bins based on min/max timestamp
    min_time, max_time = df["timestamp"].min(), df["timestamp"].max()
    bins = np.arange(min_time, max_time + bin_size_sec, bin_size_sec)
    
    # Bin the data
    df["time_bin"] = pd.cut(df["timestamp"], bins, labels=bins[:-1])

    # Aggregate within each bin
    binned_df = df.groupby("time_bin").sum()

    # Convert index back to numeric
    binned_df.index = binned_df.index.astype(float)

    # Plotting
    fig, (ax_qps, ax_input, ax_output) = plt.subplots(3, 1, figsize=(15, 12))

    ax_qps.plot(binned_df.index, binned_df["num_requests"], label="Total Requests")
    ax_input.plot(binned_df.index, binned_df["total_prompt_tokens"], label="Total Prompt Tokens")
    ax_output.plot(binned_df.index, binned_df["total_output_tokens"], label="Total Output Tokens")

    # Formatting plots
    for ax, ylabel, title in zip([ax_qps, ax_input, ax_output],
                                  ["Requests per Second", "Prompt Token Count", "Output Token Count"],
                                  ["Total Requests Sent per Second", "Total Prompt Tokens per Second", "Total Output Tokens per Second"]):
        ax.set_xlabel("Time (seconds)")
        ax.set_ylabel(ylabel)
        ax.set_title(title)
        ax.legend()
    
    plt.tight_layout()

    # Save or show the plot
    if output_dir:
        os.makedirs(os.path.dirname(output_dir), exist_ok=True)
        plt.savefig(f"{output_dir}/{workload_name}.pdf")
        logging.info(f'Saved workload plot to {output_dir}/{workload_name}.pdf')
    else:
        plt.show()


def save_workload(load_struct: List[Any],
                  output_path: str,
                  use_jsonl: Optional[bool] = False):
    # create the path if it doesn't exist
    os.makedirs(os.path.dirname(output_path), exist_ok=True)

    if use_jsonl:
        with open(output_path + ".jsonl", "w") as file:
            for row in load_struct:
                json_line = json.dumps(row)  # Convert list to JSON string
                file.write(json_line + "\n")
            logging.warn(f'Saved workload file to {output_path + ".jsonl"}')
    else:
        with open(output_path + ".json", 'w') as file:
            json.dump(load_struct, file, indent=4)
        logging.warn(f'Saved workload file to {output_path + ".json"}')


def load_workload(input_path: str) -> List[Any]:
    load_struct = None
    if input_path.endswith(".jsonl"):
        with open(input_path, "r") as file:
            load_struct = [json.loads(line) for line in file]
    else:
        with open(input_path, "r") as file:
            load_struct = json.load(file)
    return load_struct

def load_config(config_path: str) -> Dict[str, Any]:
    with open(config_path, "r") as file:
        config = json.load(file)
    return config

# Function to wrap the prompt into OpenAI's chat completion message format.
def wrap_prompt_as_chat_message(prompt: str):
    """
    Wrap the prompt into OpenAI's chat completion message format.

    :param prompt: The user prompt to be converted.
    :return: A list containing chat completion messages.
    """
    user_message = {"role": "user", "content": prompt}
    return [user_message]

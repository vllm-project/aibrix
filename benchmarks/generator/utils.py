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


def plot_workload(workload_dict, interval_ms, output_file: str = None):
    """
    Plots the concurrency (item length) of the generated workload.

    Args:
        workload_dict (dict): A dictionary where the keys are workload names (labels) and the values are lists of lists representing the workload.
        interval_ms (int): Interval in milliseconds. 
    """
    fig, ax = plt.subplots()
    for workload_name, workload in workload_dict.items():
        concurrency_values = [len(item["requests"]) for item in workload]
        ax.plot(np.arange(len(concurrency_values)) * interval_ms, concurrency_values, label=workload_name)

    ax.set_ylim(0, )
    plt.xlabel('Time (ms)')
    plt.ylabel('Concurrency')
    plt.title('Workload Concurrency')
    plt.legend()
    if output_file is None:
        plt.show()
    else:
        os.makedirs(os.path.dirname(output_file), exist_ok=True)
        plt.savefig(f"{output_file}-traffic.pdf")
        logging.info(f'Saved traffic plot to {output_file}-traffic.pdf')
        
        
    fig, ax = plt.subplots()
    for workload_name, workload in workload_dict.items():
        input_lengths = [item["requests"][0]['prompt_length'] for item in workload]
        output_lengths = [item["requests"][0]['output_length'] for item in workload]
        ax.plot(np.arange(len(concurrency_values)) * interval_ms, input_lengths, label=f"{workload_name} prompt_length")
        ax.plot(np.arange(len(concurrency_values)) * interval_ms, output_lengths, label=f"{workload_name} output_length")

    ax.set_ylim(0, )
    plt.xlabel('Time (ms)')
    plt.ylabel('Lengths')
    plt.title('Request Sizes')
    plt.legend()
    if output_file is None:
        plt.show()
    else:
        os.makedirs(os.path.dirname(output_file), exist_ok=True)
        plt.savefig(f"{output_file}-requests.pdf")
        logging.info(f'Saved traffic plot to {output_file}-requests.pdf')


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


# Function to wrap the prompt into OpenAI's chat completion message format.
def wrap_prompt_as_chat_message(prompt: str):
    """
    Wrap the prompt into OpenAI's chat completion message format.

    :param prompt: The user prompt to be converted.
    :return: A list containing chat completion messages.
    """
    user_message = {"role": "user", "content": prompt}
    return [user_message]

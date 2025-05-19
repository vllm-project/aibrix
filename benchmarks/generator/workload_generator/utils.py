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
from collections import Counter

def if_sessioned_dataset(df: pd.DataFrame):
    return "session_id" in df.columns
    
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
            "median": float(row['input_len_p50']),
            "percentiles": [0.5, 0.7, 0.9, 0.99],
            "token_lengths": [float(row['input_len_p50']), float(row['input_len_p70']), float(row['input_len_p90']), float(row['input_len_p99'])],
            "period": section_in_seconds,
            "total_seconds": section_in_seconds
        })
        output_len_configs.append({
            "median": float(row['output_len_p50']),
            "percentiles": [0.5, 0.7, 0.9, 0.99],
            "token_lengths": [float(row['output_len_p50']), float(row['output_len_p70']), float(row['output_len_p90']), float(row['output_len_p99'])],
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

def plot_workload(workload: list,
                  bin_size_sec: int = 1,
                  output_dir: str = None):
    """
    Plots workload statistics: total requests, prompt token count, and output token count binned by time.
    Also plots a session timeline as a scatter plot.

    Args:
        workload (list of dict): Workload entries with timestamps and request details.
        bin_size_sec (int): Size of each bin in seconds for aggregation.
        output_dir (str, optional): Directory path to save the plot.
    """
    print(f"plot_workload in directory {output_dir}")
    
    # Convert workload data to a DataFrame
    data = []
    scatter_data = []

    for entry in workload:
        timestamp_sec = int(entry["timestamp"] / 1000)  # Convert ms to sec
        num_requests = len(entry["requests"])
        total_prompt_tokens = np.mean([
            req["prompt_length"] if req["prompt_length"] else 0 for req in entry["requests"]
        ]) if entry["requests"] else 0
        total_output_tokens = np.mean([
            req["output_length"] if req["output_length"] else 0 for req in entry["requests"]
        ]) if entry["requests"] else 0

        data.append((timestamp_sec, num_requests, total_prompt_tokens, total_output_tokens))

        # Add each request's timestamp and session ID for scatter plot
        for req in entry["requests"]:
            session_id = req.get("session_id")
            if session_id:
                scatter_data.append((timestamp_sec, session_id))

    df = pd.DataFrame(data, columns=["timestamp", "num_requests", "total_prompt_tokens", "total_output_tokens"])
    scatter_df = pd.DataFrame(scatter_data, columns=["timestamp", "session_id"])

    # Bin the main stats
    if not df.empty:
        min_time, max_time = df["timestamp"].min(), df["timestamp"].max()
        bins = np.arange(min_time, max_time + bin_size_sec, bin_size_sec)

        df["time_bin"] = pd.cut(df["timestamp"], bins, labels=bins[:-1])
        binned_df = df.groupby("time_bin").agg({
            "num_requests": "sum", 
            "total_prompt_tokens": "mean", 
            "total_output_tokens": "mean"
        })
        binned_df.index = binned_df.index.astype(float)
        print(binned_df)

    # Map session IDs to numeric values for y-axis
    if not scatter_df.empty:
        unique_sessions = sorted(scatter_df["session_id"].unique())
        session_to_y = {sid: i for i, sid in enumerate(unique_sessions)}
        scatter_df["y"] = scatter_df["session_id"].map(session_to_y)

    # Plotting
    fig, axes = plt.subplots(4, 1, figsize=(12, 10))  # 4 vertically stacked plots
    
    def calculate_statistics(values):
        values = [value for value in values if pd.notna(value) ]
        if len(values) == 0:
            return 0.0, 0.0, 0.0
        values = sorted(values)
        avg = sum(values) / len(values)
        median = np.median(values)
        percentile_99 = np.percentile(values, 99)
        return avg, median, percentile_99
    
    logging.warning(f"num_requests statistics (mean, median, 99p): {calculate_statistics(binned_df['num_requests'])}")
    logging.warning(f"total_prompt_tokens statistics (mean, median, 99p): {calculate_statistics(binned_df['total_prompt_tokens'])}")
    logging.warning(f"total_output_tokens statistics (mean, median, 99p): {calculate_statistics(binned_df['total_output_tokens'])}")

    # Top 3 plots: workload stats
    if not df.empty:
        axes[0].plot(binned_df.index, binned_df["num_requests"], label="Total Requests")
        axes[1].plot(binned_df.index, binned_df["total_prompt_tokens"], label="Total Prompt Tokens")
        axes[2].plot(binned_df.index, binned_df["total_output_tokens"], label="Total Output Tokens")

        for ax, ylabel, title in zip(axes[:3],
                                     ["Requests per Second", "Prompt Token Count", "Output Token Count"],
                                     ["Total Requests Sent per Second", "Total Prompt Tokens per Second", "Total Output Tokens per Second"]):
            ax.set_xlabel("Time (seconds)")
            ax.set_ylabel(ylabel)
            ax.set_title(title)
            ax.legend()

    # Bottom plot: session timeline scatter plot
    if not scatter_df.empty:
        axes[3].scatter(scatter_df["timestamp"], scatter_df["y"], alpha=0.6, s=10)
        axes[3].set_yticks([])  # Hide session ID labels to reduce clutter
        axes[3].set_title("Session Timeline (Each Dot = One Request)")
        axes[3].set_xlabel("Time (seconds)")
        axes[3].set_ylabel("Sessions (hidden)")

    plt.tight_layout()

    # Save or show the plot
    if output_dir:
        os.makedirs(output_dir, exist_ok=True)
        plt.savefig(f"{output_dir}/workload.pdf")
        logging.info(f'Saved workload plot to {output_dir}/workload.pdf')
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

def load_config(config_path: str) -> Dict[str, Any]:
    with open(config_path, "r") as file:
        config = json.load(file)
    return config


import logging
import json
import argparse
import os
import re
import matplotlib.pyplot as plt
import pandas as pd
import numpy as np

def parse_goodput_target(goodput_target):
    pattern = r'^(e2e|tpot|ttft):(-?\d+(\.\d+)?)$'
    match = re.match(pattern, goodput_target)
    
    if match:
        metric = match.group(1)
        threshold = float(match.group(2))  # Convert to float
    else:
        raise ValueError(f"Invalid goodput spec: {goodput_target}")
    return metric, threshold
    
def main(args):
    input_file = args.trace
    output_path = args.output
    data = []
    with open(input_file, "r") as f:
        for line in f:
            data.append(json.loads(line))
    # Extract metrics
    prompt_tokens = []
    output_tokens = []
    total_tokens = []
    latencies = []
    throughputs = []
    tokens_per_second = []
    ttft = []
    tpot = []
    total_errors = []
    timestamps = []
    end_times = []
    for i, item in enumerate(data):
        total_errors.append(1 if item["status"] == "error" else 0)
        timestamps.append(item.get("start_time", f"Entry {i}"))
        end_times.append(item.get("end_time", f"Entry {i}"))
        prompt_tokens.append(item["prompt_tokens"]) # Prompt tokens
        output_tokens.append(item["output_tokens"]) 
        total_tokens.append(item["total_tokens"]) 
        latencies.append(item["latency"])
        throughputs.append(item["throughput"])
        tokens_per_second.append(item["total_tokens"] / item["latency"])
        ttft.append(item["ttft"] if "ttft" in item else 0.0) # Time to First Token
        tpot.append(item["tpot"] if "tpot" in item else 0.0) # Time per Output Token
    goodput = 0.0
    if args.goodput_target is not None:
        metric, threshold = parse_goodput_target(args.goodput_target)
        if metric == "e2e":
            if len(latencies) > 0:
                goodput = len([item for item in latencies if (item is not None and item <= threshold)]) / float(len(latencies))
        elif metric == "ttft":
            if len(ttft) > 0:
                goodput = len([item for item in ttft if (item is not None  and item <= threshold)]) / float(len(ttft))
        elif metric == "tpot":
            if len(tpot) > 0:
                goodput = len([item for item in tpot if (item is not None and item <= threshold)]) / float(len(tpot))
        else:
            raise ValueError(f"Invalid goodput target: {args.goodput_target}")

    # Sort data by start_time
    sorted_indices = np.argsort(timestamps)
    timestamps = [timestamps[i] for i in sorted_indices]
    prompt_tokens = [prompt_tokens[i] for i in sorted_indices]
    output_tokens = [output_tokens[i] for i in sorted_indices]
    total_tokens = [total_tokens[i] for i in sorted_indices]
    latencies = [latencies[i] for i in sorted_indices]
    throughputs = [throughputs[i] for i in sorted_indices]
    tokens_per_second = [tokens_per_second[i] for i in sorted_indices]
    ttft = [ttft[i] for i in sorted_indices]
    tpot = [tpot[i] for i in sorted_indices]
    start_times = timestamps
    

    # Convert timestamps to pandas datetime (if timestamps are actual time values)
    try:
        timestamps = pd.to_datetime(timestamps, unit='s')
    except Exception:
        timestamps = pd.Series(timestamps)

    # Helper function to calculate statistics
    def calculate_statistics(values):
        values = [value for value in values if value is not None]
        if len(values) == 0:
            return 0.0, 0.0, 0.0
        values = sorted(values)
        avg = sum(values) / len(values)
        median = np.median(values)
        percentile_99 = np.percentile(values, 99)
        return avg, median, percentile_99

    # Calculate statistics for each metric
    stats = {
        "End-to-End Latency (s)": calculate_statistics(latencies),
        "Throughput (per request, toks/s)": calculate_statistics(throughputs),
        "Tokens per Second": calculate_statistics(tokens_per_second),
        "Request Prompt Tokens": calculate_statistics(prompt_tokens),
        "Request Output Tokens": calculate_statistics(output_tokens),
        "Request Total Tokens": calculate_statistics(total_tokens),
        "Time to First Token (TTFT)": calculate_statistics(ttft),
        "Time per Output Token (TPOT)": calculate_statistics(tpot),
        "Errors": calculate_statistics(total_errors),
    }

    # Print statistics
    for metric, (avg, median, p99) in stats.items():
        logging.warning(f"{metric} Statistics: Average = {avg:.4f}, Median = {median:.4f}, 99th Percentile = {p99:.4f}")
    if goodput != None:
        logging.warning(f"Goodput (reqs/s) {goodput:.4f}")
    logging.warning(f"Total Duration (s): {np.max(end_times) - np.min(start_times)}")
    logging.warning(f"Total tokens generated (toks): {np.sum(total_tokens)}")
    logging.warning(f"Throughput (end-to-end, toks/s): {np.sum(total_tokens)/(np.max(end_times) - np.min(start_times))}")
    # logging.warning(f"Failure Rate (%) {(total_errors / len(data)) * 100 if len(data) > 0 else 0}")

    # Create a DataFrame for plotting
    df = pd.DataFrame({
        "Timestamp": timestamps,
        "Prompt Tokens": prompt_tokens,
        "Output Tokens": output_tokens,
        "Total Tokens": total_tokens,
        "End-to-End Latency (s)": latencies,
        "Throughput": throughputs,
        "Tokens per Second": tokens_per_second,
        "Time to First Token (TTFT)": ttft,
        "Time per Output Token (TPOT)": tpot,
        "Errors": total_errors,
    }).set_index("Timestamp")

    # Plot each metric in a separate subplot
    num_metrics = len(df.columns)
    fig, axes = plt.subplots(num_metrics, 1, figsize=(12, 4 * num_metrics), sharex=True)

    for ax, (column, values) in zip(axes, df.items()):
        ax.plot(df.index, values, marker='o', linestyle='-', label=column)
        ax.set_ylabel(column)
        ax.legend()
        ax.grid()

    axes[-1].set_xlabel("Time")  # Only set x-axis label for the last subplot
    plt.suptitle("Time Series Analysis of LLM Performance Metrics")
    plt.xticks(rotation=45)
    plt.tight_layout(rect=[0, 0, 1, 0.96])  # Adjust layout to fit the title
    os.makedirs(output_path, exist_ok=True)
    plt.savefig(f"{output_path}/performance_metrics_time_series.pdf")


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description='extract and plot performance metrics from a JSONL file')
    parser.add_argument('--trace', type=str, required=True, help='Input trace containing collected metrics.')
    parser.add_argument('--output', type=str, required=True, default="output", help='Output path.')
    parser.add_argument('--goodput-target', type=str, required=False, default=None, help='Goodput target should be in the format of latency_metrics:threshold_in_seconds, choose latency metrics from one of the e2e, ttft, tpot.')
    
    args = parser.parse_args()
    main(args)
    
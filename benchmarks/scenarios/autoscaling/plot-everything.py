#!/usr/bin/env python3
import os
import json
import pandas as pd
import matplotlib.pyplot as plt
import numpy as np
import sys
from datetime import datetime
from matplotlib.gridspec import GridSpec

def read_experiment_file(file_path):
    try:
        with open(file_path, 'r', encoding='utf-8') as f:
            return f.readlines()
    except FileNotFoundError:
        raise FileNotFoundError(f"Could not find the file: {file_path}")
    except Exception as e:
        raise Exception(f"Error reading file: {e}")

def parse_experiment_output(lines):
    results = []
    for line in lines:
        if not line.strip():
            continue
        try:
            data = json.loads(line.strip())
            required_fields = ['status_code', 'start_time', 'end_time', 'latency', 'throughput', 
                             'prompt_tokens', 'output_tokens', 'total_tokens', 'input', 'output']
            if any(field not in data for field in required_fields):
                continue
            results.append(data)
        except json.JSONDecodeError:
            continue
    
    if not results:
        raise ValueError("No valid data entries found")
    
    df = pd.DataFrame(results)
    base_time = df['start_time'].iloc[0]
    df['start_time'] = df['start_time'] - base_time
    df['end_time'] = df['end_time'] - base_time
    df['second_bucket'] = df['start_time'].astype(int)
    rps_series = df.groupby('second_bucket').size()
    df['rps'] = df['second_bucket'].map(rps_series)
    
    success_rps = df[df['status_code'] == 200].groupby('second_bucket').size()
    failed_rps = df[df['status_code'] != 200].groupby('second_bucket').size()
    df['success_rps'] = df['second_bucket'].map(success_rps).fillna(0)
    df['failed_rps'] = df['second_bucket'].map(failed_rps).fillna(0)
    
    return df, base_time

def get_autoscaler_name(output_dir):
    autoscaling = None
    with open(f"{output_dir}/output.txt", 'r', encoding='utf-8') as f_:
        lines = f_.readlines()
        for line in lines:
            if "autoscaler" in line:
                autoscaling = line.split(":")[-1].strip()
                break
    if autoscaling == None:
        print(f"Invalid parsed autoscaling name: {autoscaling}")
        assert False
    return autoscaling.upper()

def parse_performance_stats(file_content):
    stats = {}
    for line in file_content.split('\n'):
        if ':' in line:
            key, value = line.split(':', 1)
            try:
                stats[key.strip()] = float(value.strip())
            except ValueError:
                continue
    return stats

def read_stats_file(filename):
    try:
        with open(filename, 'r') as f:
            return f.read()
    except FileNotFoundError:
        print(f"Error: File {filename} not found")
        return None
    except Exception as e:
        print(f"Error reading {filename}: {str(e)}")
        return None



def save_statistics(stats, output_dir, autoscaler, total_cost, num_request_to_be_sent):
    try:
        output_path = f'{output_dir}/performance_stats.txt'
        print(f"** Saving statistics to: {output_path}")
        with open(output_path, 'w') as f:
            f.write(f"Performance Statistics, autoscaler: {autoscaler}\n")
            for metric, value in stats.items():
                if 'Count' in metric:
                    f.write(f"{metric}: {value:.0f}\n")
                elif 'Tokens' in metric:
                    f.write(f"{metric}: {value:.1f}\n")
                else:
                    f.write(f"{metric}: {value:.3f}\n")
            f.write(f"Total Cost: {total_cost:.2f}\n")
            f.write(f"Total Number of Requests: {num_request_to_be_sent:.0f}\n")
        return output_path
    except Exception as e:
        raise Exception(f"Error saving statistics: {e}")

def calc_cost(pod_count_df, autoscaler, output_dir):
    cost_per_pod_per_hour = 1
    total_cost = 0
    for idx, row in pod_count_df.iterrows():
        if idx == 0:
            prev_time = row['asyncio_time']
            pod_count_df.at[idx, 'accumulated_cost'] = 0
            continue
        cur_time = row['asyncio_time']
        duration = (cur_time - prev_time) / 3600
        total_cost += (row['Running'] + row['Pending'] + row['Init']) * duration * cost_per_pod_per_hour
        pod_count_df.at[idx, 'accumulated_cost'] = total_cost
        prev_time = cur_time
    return total_cost

def analyze_performance(df):
    try:
        stats = {
            'Sample Count': len(df),
            'Average Latency': df['latency'].mean(),
            'Latency P50': df['latency'].quantile(0.50),
            'Latency P90': df['latency'].quantile(0.90),
            'Latency P95': df['latency'].quantile(0.95),
            'Latency P99': df['latency'].quantile(0.99),
            'Latency Std Dev': df['latency'].std(),
            'Min Latency': df['latency'].min(),
            'Max Latency': df['latency'].max(),
            'Average RPS': df['rps'].mean(),
            'Success RPS': df['success_rps'].mean(),
            'Failed RPS': df['failed_rps'].mean(),
            'Total Tokens (avg)': df['total_tokens'].mean(),
            'Prompt Tokens (avg)': df['prompt_tokens'].mean(),
            'Output Tokens (avg)': df['output_tokens'].mean(),
            'Total Tokens (min)': df['total_tokens'].min(),
            'Prompt Tokens (min)': df['prompt_tokens'].min(),
            'Output Tokens (min)': df['output_tokens'].min(),
            'Total Tokens (max)': df['total_tokens'].max(),
            'Prompt Tokens (max)': df['prompt_tokens'].max(),
            'Output Tokens (max)': df['output_tokens'].max(),
            'Total Tokens (p99)': df['total_tokens'].quantile(0.99),
            'Prompt Tokens (p99)': df['prompt_tokens'].quantile(0.99),
            'Output Tokens (p99)': df['output_tokens'].quantile(0.99),
            'Total Tokens (std)': df['total_tokens'].std(),
            'Prompt Tokens (std)': df['prompt_tokens'].std(),
            'Output Tokens (std)': df['output_tokens'].std(),
            # 'Total Num Requests': len(df),
            'Successful Num Requests': len(df[df['status_code'] == 200]),
            'Failed Num Requests': len(df[df['status_code'] != 200]),
        }
        return stats
    except Exception as e:
        raise Exception(f"Error analyzing performance metrics: {e}")
    

def plot_combined_visualization(experiment_home_dir):
    # Create figure
    fig = plt.figure(figsize=(12, 12))
    
    # Create GridSpec for time series plots
    gs_ts = GridSpec(2, 2, figure=fig)
    gs_ts.update(top=0.95, bottom=0.45, hspace=0.3, wspace=0.25)
    
    # Create subplots for time series
    ax_cdf = fig.add_subplot(gs_ts[0, 0])
    
    ax_rps = fig.add_subplot(gs_ts[0, 1])


    ax_pods = fig.add_subplot(gs_ts[1, 0])
    # ax_pods_twin = ax_pods.twinx()
    # ax_pods_twin.set_ylabel('RPS')

    ax_cost = fig.add_subplot(gs_ts[1, 1])
    
    # Create 1x4 grid for bar plots
    gs_bars = GridSpec(1, 4, figure=fig)
    gs_bars.update(top=0.35, bottom=0.1, left=0.1, right=0.9, wspace=0.4)
    
    # Create subplots for bar plots in a single row
    ax_bar1 = fig.add_subplot(gs_bars[0, 0])
    ax_bar2 = fig.add_subplot(gs_bars[0, 1])
    ax_bar3 = fig.add_subplot(gs_bars[0, 2])
    ax_bar4 = fig.add_subplot(gs_bars[0, 3])
    
    # Create subplots for bar plots
    # ax_bar1 = fig.add_subplot(gs_bars[0, 0])
    # ax_bar2 = fig.add_subplot(gs_bars[0, 1])
    # ax_bar3 = fig.add_subplot(gs_bars[0, 2])
    # ax_bar4 = fig.add_subplot(gs_bars[0, 3])
    tab_colors = plt.get_cmap('tab20').colors
    colors = {"APA": tab_colors[0], \
              "KPA": tab_colors[1], \
              "HPA": tab_colors[2], \
              "OPTIMIZER-KPA": tab_colors[3], \
            #   "NONE":"cyan", \
              
              "random": tab_colors[4], \
              "least-request": tab_colors[5], \
              "least-kv-cache": tab_colors[6], \
              "least-busy-time": tab_colors[7], \
              "least-latency": tab_colors[8], \
              "throughput": tab_colors[9], \
                }
    
    markers = {\
               "random":"o", \
               "least-request":"x", \
               "least-kv-cache":"^", \
               "least-busy-time":"s", \
               "least-latency":"D", \
               "throughput":"P", \
               }
    
    # Get all subdirectories
    all_dir = [d for d in os.listdir(experiment_home_dir) 
               if d != "fail" and os.path.isdir(os.path.join(experiment_home_dir, d))]
    
    # Process each directory for time series plots
    for idx, subdir in enumerate(all_dir):
        output_dir = os.path.join(experiment_home_dir, subdir)
        if "pod_logs" in output_dir:
            continue
        autoscaler = get_autoscaler_name(output_dir)
        color = colors[autoscaler]
        marker = '.'
        label_name = f'{autoscaler}'

        # Read and parse data
        experiment_output_file = os.path.join(output_dir, "output.jsonl")
        parsed_lines = read_experiment_file(experiment_output_file)
        print("*" * 50)
        print(f"experiment_output_file: {experiment_output_file}")
        df, asyncio_base_time = parse_experiment_output(parsed_lines)
        df = df.sort_values('start_time')


        with open(f"{output_dir}/pod_count.csv", 'r', encoding='utf-8') as f_:
                pod_count_df = pd.read_csv(f_)
                pod_count_df['asyncio_time'] = pod_count_df['asyncio_time'] - asyncio_base_time
        total_cost = calc_cost(pod_count_df, autoscaler, output_dir)
        num_request_to_be_sent = 0
        all_files = os.listdir(output_dir)
        for fn in all_files:
            if fn.endswith(".jsonl") and fn != "output.jsonl":
                with open(f"{output_dir}/{fn}", 'r', encoding='utf-8') as f_:
                    lines = read_experiment_file(f"{output_dir}/{fn}")
                    num_request_to_be_sent = len(lines)
        if num_request_to_be_sent == 0:
            print("plot-output.py, ERROR: No valid data entries found")
        stats = analyze_performance(df)
        stats_file = save_statistics(stats, output_dir, autoscaler, total_cost, num_request_to_be_sent)

        # Read pod count data
        pod_count_df = pd.read_csv(os.path.join(output_dir, "pod_count.csv"))
        pod_count_df['asyncio_time'] = pod_count_df['asyncio_time'] - asyncio_base_time
        
        # Calculate accumulated cost
        if 'accumulated_cost' not in pod_count_df.columns:
            cost_per_pod_per_hour = 1
            total_cost = 0.0
            pod_count_df['accumulated_cost'] = 0.0
            for i in range(1, len(pod_count_df)):
                duration = (pod_count_df.iloc[i]['asyncio_time'] - 
                          pod_count_df.iloc[i-1]['asyncio_time']) / 3600
                total_cost += (pod_count_df.iloc[i]['Running'] + 
                             pod_count_df.iloc[i]['Pending'] + 
                             pod_count_df.iloc[i]['Init']) * duration * cost_per_pod_per_hour
                pod_count_df.iloc[i, pod_count_df.columns.get_loc('accumulated_cost')] = total_cost
        
        # Plot time series data
        # 1. Latency CDF
        print(f"* dir: {subdir}")
        print(f"* autoscaler: {autoscaler}")
        try:
            latencies_sorted = np.sort(df['latency'].values)
        except Exception as e:
            print(f"Error sorting latencies: {e}")
            df.to_csv(f"{output_dir}/error_df.csv", index=False)
            print(f"Saved error data to: {output_dir}/error_df.csv")
            assert False
        p = np.arange(1, len(latencies_sorted) + 1) / len(latencies_sorted)
        ax_cdf.plot(latencies_sorted, p, label=label_name, color=color)
        
        # 2. Latency over time
        # ax_latency.scatter(df['end_time'], df['latency'], label=f'{autoscaler}', color=color, marker='.', s=5, alpha=0.3)
        ax_rps.plot(df['start_time'], df['rps'], label=label_name, color=color, linewidth=0.5, alpha=0.7)
        print(f"Max time: {label_name}, {df['start_time'].max()}")
        

        # 3. Running pods over time
        ax_pods.plot(pod_count_df['asyncio_time'], pod_count_df['Running'],
                    label=label_name, color=color,
                    marker=marker, markersize=4, markevery=0.1)
        # ax_pods_twin.plot(df['start_time'], df['rps'], label=f"RPS-({autoscaler})", color=color, linewidth=0.3, alpha=0.5)
        # ax_pods_twin.scatter(df['start_time'], df['success_rps'], label="Goodput", color=color, s=2, alpha=0.3)
        # ax_pods_twin.scatter(df['start_time'], df['failed_rps'], label="Failed RPS", color=color, s=5, alpha=0.3)
        
        # ax_pods_twin = ax_pods.twinx()
        # ax_pods_twin.scatter(df['end_time'], df['latency'], 
        #                  label=f'{autoscaler}', color=color, 
        #                  marker='.', s=5, alpha=0.3)
        
        # 4. Accumulated cost over time
        ax_cost.plot(pod_count_df['asyncio_time'], pod_count_df['accumulated_cost'],
                    label=label_name, color=color,
                    marker=marker, markersize=4, markevery=0.1)
    
    # Configure time series plots
    ax_cdf.set_title('Latency CDF', fontsize=12)
    ax_cdf.set_xlabel('Latency (seconds)')
    ax_cdf.set_ylabel('Probability')
    ax_cdf.set_xlim(left=0)
    ax_cdf.set_ylim(bottom=0, top=1.01)
    ax_cdf.grid(True)
    ax_cdf.legend()
    
    ax_rps.set_title('Load', fontsize=12)
    ax_rps.set_xlabel('Time (seconds)')
    ax_rps.set_ylabel('RPS')
    ax_rps.set_xlim(left=0)
    ax_rps.set_ylim(bottom=0)
    ax_rps.grid(True)
    ax_rps.legend()
    
    ax_pods.set_title('Running Pods', fontsize=12)
    ax_pods.set_xlabel('Time (seconds)')
    ax_pods.set_ylabel('# Running Pods')
    ax_pods.set_xlim(left=0)
    ax_pods.set_ylim(bottom=0)
    ax_pods.grid(True)
    lines_pods, labels_pods = ax_pods.get_legend_handles_labels()
    # lines_rps, labels_rps = ax_pods_twin.get_legend_handles_labels()
    # ax_pods.legend(lines_pods + lines_rps, labels_pods + labels_rps, loc='upper right')
    ax_pods.legend(lines_pods, labels_pods, loc='lower right')
    # ax_pods.legend()
    
    ax_cost.set_title('Accumulated Cost', fontsize=12)
    ax_cost.set_xlabel('Time (seconds)')
    ax_cost.set_ylabel('Accumulated Cost')
    ax_cost.set_xlim(left=0)
    ax_cost.set_ylim(bottom=0)
    ax_cost.grid(True)
    ax_cost.legend()
    
    # Process performance statistics
    stats_list = []
    title_list = []
    color_list = []
    for subdir in all_dir:
        output_dir = os.path.join(experiment_home_dir, subdir)
        if "pod_logs" in subdir:
            continue
        stat_fn = os.path.join(experiment_home_dir, subdir, "performance_stats.txt")
        content = read_stats_file(stat_fn)
        if content:
            stats = parse_performance_stats(content)
            autoscaler = get_autoscaler_name(output_dir)
            title = f"{autoscaler}"
            if autoscaler is None or autoscaler == "none" or autoscaler == "NONE":
                color_list.append(colors[autoscaler])
            else:
                color_list.append(colors[autoscaler])
            title_list.append(title)
            stats_list.append(stats)
    
    # Add performance comparison plots if stats are available
    if stats_list:
        # Calculate failed requests
        for stats in stats_list:
            stats['Failed Request'] = stats['Total Number of Requests'] - stats['Successful Num Requests']
        
        # Metrics to compare
        metrics = ['Average Latency', 'Latency P99', 'Total Cost', 'Failed Request']
        bar_axes = [ax_bar1, ax_bar2, ax_bar3, ax_bar4]
        for ax, metric in zip(bar_axes, metrics):
            values = [stats[metric] for stats in stats_list]
            x = np.arange(len(title_list))
            bars = ax.bar(x, values, color=color_list[:len(values)])
            ax.set_title(metric, fontsize=12)

            ax.set_xticks(x)
            # Adjust the rotation and alignment of tick labels
            ax.set_xticklabels(title_list, rotation=45, ha='right')  # Add horizontal alignment
            
            # Adjust the bottom margin to ensure labels don't get cut off
            plt.setp(ax.get_xticklabels(), rotation_mode="anchor")  # This ensures rotation happens around the right edge
            
            ax.grid(True, linestyle='--', alpha=0.7)
            for bar in bars:
                height = bar.get_height()
                ax.text(bar.get_x() + bar.get_width()/2., height, f'{height:.2f}', ha='center', va='bottom')
    plt.tight_layout()
    
    # Save the combined figure
    output_path = os.path.join(experiment_home_dir, 'combined_visualization.pdf')
    plt.savefig(output_path, bbox_inches='tight')
    print(f"** Saved combined visualization to: {output_path}")
    plt.close()

def main():
    if len(sys.argv) != 2:
        print("Usage: python script.py <experiment_home_dir>")
        return 1
    
    experiment_home_dir = sys.argv[1]
    plot_combined_visualization(experiment_home_dir)
    return 0

if __name__ == "__main__":
    sys.exit(main())

import sys
import os
import re
import pandas as pd
import matplotlib.pyplot as plt
from datetime import datetime
import matplotlib

def parse_log_file(filepath):
    pod_name = os.path.basename(filepath).replace('.log.txt', '')
    metrics_data = []
    with open(filepath, 'r') as f:
        content = f.read()
    pattern = r'INFO (\d{2}-\d{2} \d{2}:\d{2}:\d{2}).*?Avg prompt throughput: ([\d.]+).*?Avg generation throughput: ([\d.]+).*?Running: (\d+).*?Swapped: (\d+).*?Pending: (\d+).*?GPU KV cache usage: ([\d.]+).*?CPU KV cache usage: ([\d.]+)'
    matches = re.finditer(pattern, content)
    for match in matches:
        timestamp = datetime.strptime(match.group(1), '%m-%d %H:%M:%S')
        metrics_data.append({
            'pod': pod_name,
            'timestamp': timestamp,
            'prompt_throughput': float(match.group(2)),
            'generation_throughput': float(match.group(3)),
            'running_reqs': int(match.group(4)),
            'swapped_reqs': int(match.group(5)),
            'pending_reqs': int(match.group(6)),
            'gpu_cache': float(match.group(7)),
            'cpu_cache': float(match.group(8))
        })
    return pd.DataFrame(metrics_data)

def plot_metrics(logs_dir):
    all_data = pd.DataFrame()
    all_pod_logs_files = os.listdir(logs_dir)
    # for fn in all_pod_logs_files:
    #         print(f"plot_metrics, Found log file: {fn}")
    for fn in all_pod_logs_files:
        if fn.endswith('.log.txt') and "aibrix-controller-manager" not in fn and "aibrix-gateway-plugins" not in fn:
            temp = parse_log_file(os.path.join(logs_dir, fn))
            all_data = pd.concat([all_data, temp])
    if all_data.empty or all_data['pod'].nunique() == 0:
        print("No pod log files found")
        assert False
    metrics = {
        'Prompt Throughput': ['prompt_throughput'],
        'Generation Throughput': ['generation_throughput'],
        # 'Requests': ['running_reqs', 'swapped_reqs', 'pending_reqs'], # These are available metrics but just not plotting them for now.
        'Pending requests': ['pending_reqs'], # These are available metrics but just not plotting them for now.
        'Running requests': ['running_reqs'],
        # 'Cache Usage': ['gpu_cache', 'cpu_cache'] # Currently cpu cache is not being used. Commenting it to make the plot cleaner
        'Cache Usage': ['gpu_cache']
    }
    plt.style.use('bmh')
    pod_hash_ids = {}
    pod_idx = 0
    colors = matplotlib.colormaps.get_cmap('tab20').colors
    for title, metric_list in metrics.items():
        fig, ax = plt.subplots(figsize=(15, 8))
        for i, metric in enumerate(metric_list):
            for j, pod in enumerate(all_data['pod'].unique()):
                pod_data = all_data[all_data['pod'] == pod]
                # hash_id = pod.split('-')[-1].split('.')[0]
                if pod not in pod_hash_ids:
                    pod_hash_ids[pod] = f"Pod {pod_idx}"
                    pod_idx += 1
                # print(f"Pod: {pod_hash_ids[pod]}, metric: {metric}, data: {pod_data.shape}")
                ax.plot(pod_data['timestamp'], pod_data[metric], 
                       label=f'{pod_hash_ids[pod]} - {metric}', 
                       linestyle=['-', '--', '-.'][i % 3],
                       color=colors[j],
                       linewidth=2, marker='o', markersize=6)
        ax.set_title(f'{title} Over Time', fontsize=16, pad=20)
        ax.set_xlabel('Time', fontsize=14)
        ax.set_ylabel(f'{title}', fontsize=14)
        ax.tick_params(axis='both', labelsize=12)
        ax.grid(True, linestyle='--', alpha=0.7)
        plt.xticks(rotation=45)
        ax.legend(bbox_to_anchor=(1.05, 1), loc='upper left', fontsize=12, borderaxespad=0.)
        plt.tight_layout()
        fn = f'{logs_dir}/{title.lower().replace(" ", "_")}.pdf'
        plt.savefig(fn, bbox_inches='tight')
        print(f'** Saved plot: {fn}')
        plt.close()

if __name__ == "__main__":
    if len(sys.argv) < 2:
        print("Usage: python plot_per_pod.py <logs_dir>")
        sys.exit(1)
    logs_dir = sys.argv[1]
    plot_metrics(logs_dir)
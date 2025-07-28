import os
import pandas as pd
import matplotlib.pyplot as plt

QPS_RANGE = [0.1, 0.5, 0.9, 1.3, 1.7, 2.1, 2.5, 2.9, 3.3, 3.7, 4.1]
METRICS = ["ttft", "generation_time"]

experiments = [
    {"prefix": "aibrix_naive_7_output", "label": "aibrix w/o cache v0.7.0", "color": "blue", "marker": "x"},
    {"prefix": "naive_output", "label": "Native K8S", "color": "orange", "marker": "v"},
    {"prefix": "ps_naive_output", "label": "PS naive", "color": "black", "marker": "d"},
    # this is still experimental features
    # {"prefix": "aibrix_high_low_output", "label": "AIBrix w/ cache v0.6.1", "color": "red", "marker": "D"},
    {"prefix": "ps_stack_high_low_output", "label": "PS High-Low", "color": "green", "marker": "<"},
    {"prefix": "ps_stack_low_high_output", "label": "PS Low-High", "color": "brown", "marker": ">"},
]


# ready metrics results from dumped csv file
def load_metric_results(prefix, metric):
    qpses = []
    metric_values = []
    for qps in QPS_RANGE:
        file = f"{prefix}_{round(qps, 1)}.csv"
        if not os.path.exists(file):
            continue
        df = pd.read_csv(file)
        if metric not in df.columns:
            continue
        vals = df[metric].dropna().tolist()
        if vals:
            qpses.append(round(qps, 1))
            metric_values.append(sum(vals) / len(vals))
    return qpses, metric_values


# create two subplots. left and right
fig, axs = plt.subplots(1, 2, figsize=(14, 6), sharex=True)

for exp in experiments:
    for i, metric in enumerate(METRICS):
        qpses, values = load_metric_results(exp["prefix"], metric)
        print(f"{exp['label']} {metric}:", values)
        axs[i].plot(
            qpses,
            values,
            label=exp["label"],
            marker=exp["marker"],
            color=exp["color"],
            linewidth=2,
            markersize=6,
        )

# TTFT setting
axs[0].set_title("Time to First Token (TTFT)")
axs[0].set_xlabel("QPS")
axs[0].set_ylabel("Latency (s)")
axs[0].grid(True, linestyle="--", alpha=0.5)
axs[0].legend()

# generation setting
axs[1].set_title("Generation Time")
axs[1].set_xlabel("QPS")
axs[1].grid(True, linestyle="--", alpha=0.5)
axs[1].legend()

plt.suptitle("LLM Performance under Different QPS")
plt.tight_layout(rect=[0, 0, 1, 0.96])
plt.savefig("figure_ttft_generation_time.png")
plt.show()

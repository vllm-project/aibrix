#!/usr/bin/env python3
"""Generate vllm-bench grouped bar charts from a scenario summary file."""

from __future__ import annotations

import argparse
import json
import math
import os
import tempfile
from pathlib import Path
from typing import Iterable

os.environ.setdefault(
    "MPLCONFIGDIR",
    str(Path(tempfile.gettempdir()) / f"matplotlib-{os.getuid()}"),
)

try:
    import matplotlib.pyplot as plt
except ImportError as exc:  # pragma: no cover - runtime dependency check
    raise SystemExit(
        "matplotlib is required to generate figures. "
        "Install it with `pip install matplotlib`."
    ) from exc


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description=(
            "Generate vllm-bench summary figures from a scenario log directory, "
            "summary.json, or summary.csv."
        )
    )
    parser.add_argument(
        "input_path",
        help="Path to a scenario log directory, summary.json, or summary.csv",
    )
    return parser.parse_args()


def resolve_summary_json(input_path: str) -> Path:
    candidate = Path(input_path).expanduser().resolve()

    if candidate.is_dir():
        summary_path = candidate / "summary.json"
    elif candidate.name == "summary.json":
        summary_path = candidate
    elif candidate.name == "summary.csv":
        summary_path = candidate.with_name("summary.json")
    else:
        raise SystemExit(
            "input_path must be a scenario log directory, summary.json, or summary.csv"
        )

    if not summary_path.exists():
        raise SystemExit(f"summary.json not found: {summary_path}")

    return summary_path


def load_summary(summary_path: Path) -> dict:
    with summary_path.open("r", encoding="utf-8") as file:
        return json.load(file)


def metric_value(metrics: dict, key: str) -> float:
    value = metrics.get(key)
    if value is None:
        return math.nan
    try:
        return float(value)
    except (TypeError, ValueError):
        return math.nan


def case_label(result: dict) -> str:
    testcase = result.get("testcase", "unknown")
    version = result.get("version", "unknown")
    status = result.get("status", "unknown")
    suffix = "" if status == "passed" else f"\n[{status}]"
    return f"{testcase}\n{version}{suffix}"


def collect_rows(summary: dict) -> list[dict]:
    rows = []
    for result in summary.get("results", []):
        metrics = result.get("metrics") or {}
        rows.append(
            {
                "label": case_label(result),
                "request_throughput": metric_value(metrics, "request_throughput"),
                "output_throughput": metric_value(metrics, "output_throughput"),
                "mean_ttft_ms": metric_value(metrics, "mean_ttft_ms"),
                "p95_ttft_ms": metric_value(metrics, "p95_ttft_ms"),
                "p99_ttft_ms": metric_value(metrics, "p99_ttft_ms"),
                "mean_tpot_ms": metric_value(metrics, "mean_tpot_ms"),
                "p95_tpot_ms": metric_value(metrics, "p95_tpot_ms"),
                "p99_tpot_ms": metric_value(metrics, "p99_tpot_ms"),
            }
        )
    if not rows:
        raise SystemExit("summary.json does not contain any results")
    return rows


def autolabel(ax, bars: Iterable, decimals: int = 2) -> None:
    for bar in bars:
        height = bar.get_height()
        if math.isnan(height):
            continue
        ax.annotate(
            f"{height:.{decimals}f}",
            xy=(bar.get_x() + bar.get_width() / 2, height),
            xytext=(0, 4),
            textcoords="offset points",
            ha="center",
            va="bottom",
            fontsize=8,
            rotation=90,
        )


def plot_throughput_and_means(rows: list[dict], output_path: Path) -> None:
    metric_groups = [
        ("request_throughput", "Request Throughput", "left"),
        ("output_throughput", "Output Throughput", "left"),
        ("mean_ttft_ms", "Mean TTFT", "right"),
        ("mean_tpot_ms", "Mean TPOT", "right"),
    ]
    x = list(range(len(metric_groups)))
    width = 0.8 / max(len(rows), 1)

    fig, ax_left = plt.subplots(figsize=(max(10, len(rows) * 3.2), 6.5))
    ax_right = ax_left.twinx()
    colors = plt.get_cmap("tab10").colors
    legend_handles = []

    for row_index, row in enumerate(rows):
        offset = (row_index - (len(rows) - 1) / 2) * width
        color = colors[row_index % len(colors)]
        test_label = row["label"].replace("\n", " | ")

        left_positions = []
        left_values = []
        right_positions = []
        right_values = []
        for metric_index, (key, _name, axis_name) in enumerate(metric_groups):
            value = row[key]
            position = x[metric_index] + offset
            if axis_name == "left":
                left_positions.append(position)
                left_values.append(value)
            else:
                right_positions.append(position)
                right_values.append(value)

        left_bars = ax_left.bar(left_positions, left_values, width, color=color)
        right_bars = ax_right.bar(right_positions, right_values, width, color=color)
        autolabel(ax_left, left_bars, decimals=2)
        autolabel(ax_right, right_bars, decimals=1)
        legend_handles.append(plt.Rectangle((0, 0), 1, 1, color=color, label=test_label))

    ax_left.set_title("Throughput and Mean Latency Comparison")
    ax_left.set_ylabel("Throughput")
    ax_right.set_ylabel("Latency (ms)")
    ax_left.set_xticks(x)
    ax_left.set_xticklabels([name for _, name, _ in metric_groups])
    ax_left.grid(axis="y", linestyle="--", alpha=0.3)
    ax_left.legend(handles=legend_handles, loc="upper left", title="Testcase")

    fig.tight_layout()
    fig.savefig(output_path, dpi=200, bbox_inches="tight")
    plt.close(fig)


def plot_latency_group(
    rows: list[dict],
    output_path: Path,
    title: str,
    metric_specs: list[tuple[str, str, str]],
) -> None:
    labels = [row["label"] for row in rows]
    x = list(range(len(rows)))
    width = 0.22

    fig, ax = plt.subplots(figsize=(max(10, len(rows) * 2.4), 6.5))

    for index, (key, name, color) in enumerate(metric_specs):
        values = [row[key] for row in rows]
        offset = (index - 1) * width
        bars = ax.bar([pos + offset for pos in x], values, width, label=name, color=color)
        autolabel(ax, bars, decimals=1)

    ax.set_title(title)
    ax.set_ylabel("Latency (ms)")
    ax.set_xticks(x)
    ax.set_xticklabels(labels)
    ax.grid(axis="y", linestyle="--", alpha=0.3)
    ax.legend(loc="upper left")

    fig.tight_layout()
    fig.savefig(output_path, dpi=200, bbox_inches="tight")
    plt.close(fig)


def main() -> None:
    args = parse_args()
    summary_path = resolve_summary_json(args.input_path)
    summary = load_summary(summary_path)
    rows = collect_rows(summary)

    figures_dir = summary_path.parent / "figures"
    figures_dir.mkdir(parents=True, exist_ok=True)

    plot_throughput_and_means(rows, figures_dir / "01-throughput-and-mean-latency.png")
    plot_latency_group(
        rows,
        figures_dir / "02-ttft-latency.png",
        "TTFT Comparison",
        [
            ("mean_ttft_ms", "Mean TTFT", "#4C72B0"),
            ("p95_ttft_ms", "P95 TTFT", "#DD8452"),
            ("p99_ttft_ms", "P99 TTFT", "#C44E52"),
        ],
    )
    plot_latency_group(
        rows,
        figures_dir / "03-tpot-latency.png",
        "TPOT Comparison",
        [
            ("mean_tpot_ms", "Mean TPOT", "#55A868"),
            ("p95_tpot_ms", "P95 TPOT", "#937860"),
            ("p99_tpot_ms", "P99 TPOT", "#8172B2"),
        ],
    )

    print(f"Generated figures in {figures_dir}")


if __name__ == "__main__":
    main()

import argparse
import json
import pandas as pd
import numpy as np
import os

def main(args):
    # Init dataframe and load benchmark results
    benchmark_results = []
    with open(args.benchmark, "r") as f:
        for line in f:
            if line == "\n":
                continue
            benchmark_results.append(json.loads(line))
    benchmark_df = pd.DataFrame(benchmark_results, columns=['input_tokens', 'output_tokens', "request_rate", "seed", "model", "samples", "metric", "mean", "P50", "P90", "P99"])

    # Construct matrix indexes based on unique input and output tokens
    input_tokens = benchmark_df['input_tokens'].unique()
    output_tokens = benchmark_df['output_tokens'].unique()
    input_tokens.sort()
    output_tokens.sort()
    slo_tputs = np.zeros((len(output_tokens), len(input_tokens)), dtype=float)

    # Decide the percentile to use for SLO calculation
    percentile_field = "mean"
    if args.percentile > 0:
        percentile_field = f"P{args.percentile}"
    
    # Iterate slo_tputs and fill in the matrix with the throughput values that matches the SLO
    for i in range(len(output_tokens)):
        for j in range(len(input_tokens)):
            filtered_df = benchmark_df.loc[(benchmark_df['input_tokens'] == input_tokens[j]) & (benchmark_df['output_tokens'] == output_tokens[i])]

            # Filter the bencmarks by throughput SLO
            tput_df = filtered_df.loc[(filtered_df['metric'] == "TPUT") & (filtered_df['mean'] >= args.tput)]
            if len(tput_df) == 0:
                continue
            filtered_df = filtered_df.loc[filtered_df["request_rate"].isin(tput_df["request_rate"])]

            # Filter the bencmarks by token throughput SLO
            tt_df = filtered_df.loc[(filtered_df['metric'] == "TT") & (filtered_df['mean'] >= args.tt)]
            if len(tt_df) == 0:
                continue
            filtered_df = filtered_df.loc[filtered_df["request_rate"].isin(tt_df["request_rate"])]

            

            # Filter the bencmarks by E2E latency SLO
            e2e_df = filtered_df.loc[(filtered_df['metric'] == "E2E") & (filtered_df[percentile_field] <= args.e2e)]
            if len(e2e_df) == 0:
                continue
            filtered_df = filtered_df.loc[filtered_df["request_rate"].isin(e2e_df["request_rate"])]

            # Filter the bencmarks by TTFT SLO
            ttft_df = filtered_df.loc[(filtered_df['metric'] == "TTFT") & (filtered_df[percentile_field] <= args.ttft)]
            if len(ttft_df) == 0:
                continue
            filtered_df = filtered_df.loc[filtered_df["request_rate"].isin(ttft_df["request_rate"])]

            # Filter the bencmarks by TPOT SLO
            tpot_df = filtered_df.loc[(filtered_df['metric'] == "TPOT") & (filtered_df[percentile_field] <= args.TPOT)]
            if len(tpot_df) == 0:
                continue
            filtered_df = filtered_df.loc[filtered_df["request_rate"].isin(tpot_df["request_rate"])]

            # Conclude
            slo_tputs[i, j] = np.max(filtered_df.loc[filtered_df["metric"] == "TPUT", "mean"])

    # Print the matrix
    filename = os.path.splitext(os.path.basename(args.benchmark))[0]
    result = {
        "gpu": filename,
        "cost": args.cost,
        "tputs": slo_tputs.tolist(),
        "indexes": [output_tokens.tolist(), input_tokens.tolist()]
    }
    if args.o is not None:
        with open(args.o, "w") as f:
            json.dump(result, f)
    else:
        json.dump(result)

if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description="Benchmark the online serving throughput.")
    parser.add_argument("--benchmark", type=str, default="result.jsonl",
                        help="Benchmark result file.")
    parser.add_argument("--tput", type=int, default=0,
                        help="Throughput SLO target as RPS.")
    parser.add_argument("--tt", type=int, default=0,
                        help="Token Throughput SLO target.")
    parser.add_argument("--e2e", type=float, default=60,
                        help="E2E latency SLO target.")
    parser.add_argument("--ttft", type=float, default=60,
                        help="Time To First Token SLO target.")
    parser.add_argument("--TPOT", type=float, default=1,
                        help="Time Per Output Token SLO target.")
    parser.add_argument("--percentile", type=int, default=0,
                        help="Percentile to use for SLO calculation. Default to ignore percentile and use mean.",
                        choices=[0, 50, 90, 99])
    parser.add_argument("--cost", type=float, default=1.0,
                        help="Cost of the GPU.")
    parser.add_argument("-o", type=str, default=None,
                        help="Output file name.")
    args = parser.parse_args()
    main(args)
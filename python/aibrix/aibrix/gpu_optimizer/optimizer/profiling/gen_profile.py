# Copyright 2024 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# 	http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import argparse
import json
import os
from datetime import datetime

import numpy as np
import pandas as pd

REDIS_PROFILE_KEY = "aibrix:profile_%s_%s"


def main(args):
    # Init dataframe and load benchmark results
    benchmark = os.path.dirname(__file__) + f"/result/{args.deployment}.jsonl"
    if args.benchmark is not None:
        benchmark = args.benchmark

    benchmark_results = []
    with open(benchmark, "r") as f:
        for line in f:
            if line == "\n":
                continue
            benchmark_results.append(json.loads(line))
    benchmark_df = pd.DataFrame(
        benchmark_results,
        columns=[
            "input_tokens",
            "output_tokens",
            "request_rate",
            "seed",
            "model",
            "samples",
            "metric",
            "mean",
            "P50",
            "P90",
            "P99",
        ],
    )

    # Construct matrix indexes based on unique input and output tokens
    input_tokens = benchmark_df["input_tokens"].unique()
    output_tokens = benchmark_df["output_tokens"].unique()
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
            filtered_df = benchmark_df.loc[
                (benchmark_df["input_tokens"] == input_tokens[j])
                & (benchmark_df["output_tokens"] == output_tokens[i])
            ]

            # Filter the bencmarks by throughput SLO
            tput_df = filtered_df.loc[
                (filtered_df["metric"] == "TPUT") & (filtered_df["mean"] >= args.tput)
            ]
            if len(tput_df) == 0:
                continue
            filtered_df = filtered_df.loc[
                filtered_df["request_rate"].isin(tput_df["request_rate"])
            ]

            # Filter the bencmarks by token throughput SLO
            tt_df = filtered_df.loc[
                (filtered_df["metric"] == "TT") & (filtered_df["mean"] >= args.tt)
            ]
            if len(tt_df) == 0:
                continue
            filtered_df = filtered_df.loc[
                filtered_df["request_rate"].isin(tt_df["request_rate"])
            ]

            # Filter the bencmarks by E2E latency SLO
            e2e_df = filtered_df.loc[
                (filtered_df["metric"] == "E2E")
                & (filtered_df[percentile_field] <= args.e2e)
            ]
            if len(e2e_df) == 0:
                continue
            filtered_df = filtered_df.loc[
                filtered_df["request_rate"].isin(e2e_df["request_rate"])
            ]

            # Filter the bencmarks by TTFT SLO
            ttft_df = filtered_df.loc[
                (filtered_df["metric"] == "TTFT")
                & (filtered_df[percentile_field] <= args.ttft)
            ]
            if len(ttft_df) == 0:
                continue
            filtered_df = filtered_df.loc[
                filtered_df["request_rate"].isin(ttft_df["request_rate"])
            ]

            # Filter the bencmarks by TPOT SLO
            tpot_df = filtered_df.loc[
                (filtered_df["metric"] == "TPOT")
                & (filtered_df[percentile_field] <= args.TPOT)
            ]
            if len(tpot_df) == 0:
                continue
            filtered_df = filtered_df.loc[
                filtered_df["request_rate"].isin(tpot_df["request_rate"])
            ]

            # Conclude
            slo_tputs[i, j] = np.max(
                filtered_df.loc[filtered_df["metric"] == "TPUT", "mean"]
            )

    # Print the matrix
    filename = os.path.splitext(os.path.basename(benchmark))[0]
    result = {
        "gpu": filename,
        "cost": args.cost,
        "tputs": slo_tputs.tolist(),
        "indexes": [output_tokens.tolist(), input_tokens.tolist()],
        "created": datetime.now().timestamp(),
    }
    if args.o is not None:
        if _try_store_redis(args, result):
            return

        with open(args.o, "w") as f:
            json.dump(result, f)
    else:
        print(json.dumps(result))


def _try_store_redis(args, result) -> bool:
    import json
    import sys
    from urllib.parse import parse_qs, urlparse

    import redis

    # Parse the Redis URL
    url = urlparse(args.o)

    # Connect to the Redis server
    if url.scheme != "redis":
        return False

    # Connect to the Redis server
    db_name = str(url.path).strip("/")
    if db_name == "":
        db_name = "0"
    redis_client = redis.Redis(
        host=str(url.hostname),
        port=6379 if url.port is None else int(url.port),
        db=int(db_name),
        username=url.username,
        password=url.password,
    )

    # Store the result in Redis
    query_params = parse_qs(url.query)
    model_name = query_params.get("model", [""])[0]
    if model_name == "":
        print('"model" in Redic connection arguments is not provided.', file=sys.stderr)
        return True

    redis_key = REDIS_PROFILE_KEY % (model_name, args.deployment)
    redis_client.set(redis_key, json.dumps(result))
    print(f"Result stored in Redis: {redis_key}.")
    return True


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description="Benchmark the online serving throughput."
    )
    parser.add_argument(
        "deployment", type=str, help="Target deployment", default="result"
    )
    parser.add_argument(
        "--benchmark", type=str, default=None, help="Benchmark result file."
    )
    parser.add_argument(
        "--tput", type=float, default=0, help="Throughput SLO target as RPS."
    )
    parser.add_argument(
        "--tt", type=float, default=0, help="Token Throughput SLO target."
    )
    parser.add_argument(
        "--e2e", type=float, default=300, help="E2E latency SLO target."
    )
    parser.add_argument(
        "--ttft", type=float, default=60, help="Time To First Token SLO target."
    )
    parser.add_argument(
        "--TPOT", type=float, default=1, help="Time Per Output Token SLO target."
    )
    parser.add_argument(
        "--percentile",
        type=int,
        default=0,
        help="Percentile to use for SLO calculation. Default to ignore percentile and use mean.",
        choices=[0, 50, 90, 99],
    )
    parser.add_argument("--cost", type=float, default=1.0, help="Cost of the GPU.")
    parser.add_argument(
        "-o",
        type=str,
        default=None,
        help="Output file name. support redis as: redis://[username:password@]hostname:port[/db_name]?model=[model_name]",
    )
    args = parser.parse_args()
    main(args)

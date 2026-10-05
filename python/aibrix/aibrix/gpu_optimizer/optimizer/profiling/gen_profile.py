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
import logging
import math
import os
from dataclasses import dataclass
from datetime import datetime

import numpy as np
import pandas as pd

REDIS_PROFILE_KEY = "aibrix:profile_%s_%s"
TPUT_TOLERANCE = 0.9

DEFAULT_THROUGHPUT_SLO = 0.0
DEFAULT_TIME_SLO = math.inf

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("aibrix.gpu_optimizer.profiling")


@dataclass
class SLO:
    field: str = ""
    default: float = DEFAULT_THROUGHPUT_SLO
    arg_help: str = ""
    value: float = 0.0

    def is_set(self):
        return self.value != self.default

    def arg_name(self):
        return self.field.lower()

    def fill_args(self, args: dict):
        self.value = args[self.arg_name()]


slos = {
    "TPUT": SLO(
        **{  # type: ignore
            "default": DEFAULT_THROUGHPUT_SLO,
            "arg_help": "Throughput SLO target as RPS.",
        }
    ),
    "TT": SLO(
        **{  # type: ignore
            "default": DEFAULT_THROUGHPUT_SLO,
            "arg_help": "Token Throughput SLO target.",
        }
    ),
    "E2E": SLO(
        **{"default": DEFAULT_TIME_SLO, "arg_help": "E2E latency SLO target."}  # type: ignore
    ),
    "TTFT": SLO(
        **{"default": DEFAULT_TIME_SLO, "arg_help": "Time To First Token SLO target."}  # type: ignore
    ),
    "TPAT": SLO(
        **{"default": DEFAULT_TIME_SLO, "arg_help": "Time Per All Token SLO target."}  # type: ignore
    ),
    "TPOT": SLO(
        **{"default": DEFAULT_TIME_SLO, "arg_help": "Time Per Output Token SLO target."}  # type: ignore
    ),
}


def gen(args):
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
    e2es = np.zeros((len(output_tokens), len(input_tokens)), dtype=float)
    ttfts = np.zeros((len(output_tokens), len(input_tokens)), dtype=float)

    # Create TPAT metric
    tpat_df = benchmark_df[benchmark_df["metric"] == "E2E"].copy()
    tpat_df["metric"] = ["TPAT"] * len(tpat_df)
    for _, percentile in enumerate(["mean", "P50", "P90", "P99"]):
        tpat_df[percentile] = tpat_df[percentile] / (
            tpat_df["input_tokens"] + tpat_df["output_tokens"]
        )
    benchmark_df = pd.concat([benchmark_df, tpat_df], ignore_index=True)

    # Decide the percentile to use for SLO calculation
    percentile_field = "mean"
    if args.percentile > 0:
        percentile_field = f"P{args.percentile}"

    # Iterate slo_tputs and fill in the matrix with the throughput values that matches the SLO
    def setMetrics(conclusion_df, i, j):
        e2es[i, j] = conclusion_df.loc[conclusion_df["metric"] == "E2E", "mean"].iloc[0]
        if slos["TTFT"].is_set():
            ttfts[i, j] = conclusion_df.loc[
                conclusion_df["metric"] == "TTFT", "mean"
            ].iloc[0]

    for i in range(len(output_tokens)):
        for j in range(len(input_tokens)):
            feature_df = benchmark_df.loc[
                (benchmark_df["input_tokens"] == input_tokens[j])
                & (benchmark_df["output_tokens"] == output_tokens[i])
            ]

            # Filter the bencmarks by slos:
            filtered_df = feature_df
            for slo in slos.values():
                if slo.default == DEFAULT_TIME_SLO:
                    df = filtered_df.loc[
                        (filtered_df["metric"] == slo.field)
                        & (filtered_df[percentile_field] < slo.value)
                    ]
                else:
                    df = filtered_df.loc[
                        (filtered_df["metric"] == slo.field)
                        & (filtered_df["mean"] >= slo.value)
                    ]
                # Nothing match
                if len(df) == 0:
                    filtered_df = df
                    break
                # Filter by request rate
                filtered_df = filtered_df.loc[
                    filtered_df["request_rate"].isin(df["request_rate"])
                ]

            if len(filtered_df) == 0:
                setMetrics(feature_df, i, j)
                continue

            # Concluding

            # Since all SLO target passed, minimum request_rate is qualified.
            # We keep the smallest request_rate to maximize profile completeness.
            max_request_rate = np.min(filtered_df["request_rate"])
            logger.debug(
                f"Candidate request rates for {input_tokens[j]}:{output_tokens[i]}:\n{filtered_df['request_rate'].unique()}, {filtered_df.loc[(filtered_df['metric'] == 'TPUT'), 'mean'].tolist()}"
            )
            # Filter out request_rates that can cause pending requests accumulate: TPUT < request_rate * TPUT_TOLERANCE
            shortlist = filtered_df.loc[
                (filtered_df["metric"] == "TPUT")
                & (filtered_df["mean"] >= filtered_df["request_rate"] * TPUT_TOLERANCE)
            ]
            logger.debug(
                f"Candidate request rates for {input_tokens[j]}:{output_tokens[i]}:\n{shortlist['request_rate'].unique()}"
            )
            # Test max request rate
            if len(shortlist) > 0:
                max_request_rate = np.max(shortlist["request_rate"])

            conclusion_df = filtered_df.loc[
                filtered_df["request_rate"] == max_request_rate
            ]
            slo_tputs[i, j] = conclusion_df.loc[
                conclusion_df["metric"] == "TPUT", "mean"
            ].iloc[0]
            setMetrics(conclusion_df, i, j)

            msg = conclusion_df.loc[
                conclusion_df["metric"] == "TPUT", ["request_rate", "mean"]
            ]
            logger.debug(
                f"Candidate for {input_tokens[j]}:{output_tokens[i]} tputs:\n{msg}"
            )

    # Prepare the profile
    result = {
        "gpu": args.deployment,
        "cost": args.cost,
        "tputs": slo_tputs.tolist(),
        "indexes": [output_tokens.tolist(), input_tokens.tolist()],
        "created": datetime.now().timestamp(),
        "e2e": e2es.tolist(),
    }
    if slos["TTFT"].is_set():
        result["ttft"] = ttfts.tolist()
    result["slos"] = {"percentile": args.percentile}
    for field, slo in slos.items():
        if slo.is_set():
            result["slos"][slo.arg_name()] = slo.value

    # Output
    if args.o is not None:
        if _try_store_redis(args, result):
            return

        with open(args.o, "w") as f:
            json.dump(result, f)
    elif args.verbose:
        print(json.dumps(result, indent=2))
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


def main():
    parser = argparse.ArgumentParser(
        description="Benchmark the online serving throughput."
    )
    parser.add_argument(
        "deployment", type=str, help="Target deployment", default="result"
    )
    parser.add_argument(
        "--benchmark", type=str, default=None, help="Benchmark result file."
    )
    for field, slo in slos.items():
        slo.field = field
        parser.add_argument(
            f"--{slo.arg_name()}", type=float, default=slo.default, help=slo.arg_help
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
    parser.add_argument(
        "--verbose",
        "-v",
        action="store_true",
        help="Print more information for understanding the generated profile",
    )
    args = parser.parse_args()
    if args.verbose:
        logger.setLevel(level=logging.DEBUG)

    args_dict = vars(args)
    for slo in slos.values():
        slo.fill_args(args_dict)
    gen(args)


if __name__ == "__main__":
    main()

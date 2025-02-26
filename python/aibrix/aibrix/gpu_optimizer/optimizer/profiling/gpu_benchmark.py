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

"""Benchmark online serving throughput.

Adapted from https://github.com/tyler-griggs/melange-release/blob/main/melange/profiling/gpu-benchmark.py, which is adapted from
https://github.com/vllm-project/vllm/blob/main/benchmarks/benchmark_serving.py

"""

import argparse
import asyncio
import json
import random
import sys
import time
from datetime import datetime, timezone
from typing import AsyncGenerator, List, Literal, Optional, Tuple

import aiohttp
import numpy as np

# (prompt len, output len, request latency)
REQUEST_LATENCY: List[Tuple[int, int, float]] = []
# (prompt len, output len, [per-token latencies])
TOKEN_LATENCY: List[Tuple[int, int, List[float]]] = []
TIME_TO_FIRST_TOKEN: List[float] = []
TEMPERATURE = 0.0


def print_err(
    *values: object,
    sep: str | None = " ",
    end: str | None = "\n",
    flush: Literal[False] = False,
) -> None:
    print(*values, sep=sep, end=end, file=sys.stderr, flush=flush)


def sample_requests(
    num_requests: int,
    config_input_len: int,
    config_output_len: int,
    workload_dataset_file: Optional[str] = None,
) -> List[Tuple[str, int, int, float]]:
    """Sample requests from prompt dataset or generate synthetic ones."""
    if workload_dataset_file:
        try:
            with open(workload_dataset_file) as f:
                data = json.load(f)
                # Return timestamp and request tuples
                requests = []
                for i, entry in enumerate(data):
                    # Limit the number of requests read from workload

                    # print(f"Request {i}: {entry}")
                    cur_timestamp = entry["Timestamp"]
                    next_timestamp = (
                        data[i + 1]["Timestamp"] if i < len(data) - 1 else cur_timestamp
                    )
                    interval = (next_timestamp - cur_timestamp) / 1000.0
                    for i, req in enumerate(entry["Requests"]):
                        requests.append(
                            (
                                req["Prompt"],
                                req["Prompt Length"],
                                req["Output Length"],
                                interval if i == len(entry["Requests"]) - 1 else 0,
                            )
                        )
                        if num_requests > 0 and len(requests) >= num_requests:
                            return requests
                # print('total requests: ', len(requests))
                # print('the least requests: ', requests[len(requests) - 1])
                return requests
        except Exception as e:
            print_err(f"Warning: Failed to load prompt dataset ({e})")
            return []
    else:
        # Original synthetic prompt generation
        requests = []
        for _ in range(num_requests):
            synthetic_prompt = "hi " * config_input_len
            # assign timestamp to -1 for all requests
            requests.append((synthetic_prompt, config_input_len, config_output_len, -1))
        return requests


async def get_request(
    input_requests: List[Tuple[str, int, int, float]],
    request_rate: float,
    num_requests: int,
    verbose: bool,
    use_workload_interval: bool = False,
) -> AsyncGenerator[Tuple[str, int, int, float], None]:
    requests = iter(input_requests)
    start_time = time.perf_counter()

    batch = 0
    for i, (prompt, prompt_len, output_len, interval) in enumerate(requests):
        current_time = time.perf_counter() - start_time
        if use_workload_interval:
            if verbose:
                print(
                    f"Batch {batch}, Request {i}: Sending at {current_time:.3f}s with interval {interval:.3f}s"
                )
            yield (prompt, prompt_len, output_len, interval)
            if interval > 0:
                batch += 1
                await asyncio.sleep(interval)
            continue
        else:
            interval = 0.0
            if i < num_requests - 1 and request_rate != float("inf"):
                # Sample the request interval from the exponential distribution.
                interval = np.random.exponential(1.0 / request_rate)
                if verbose:
                    print(
                        f"Request {i}: Generated exponential interval of {interval:.3f}s"
                    )

            request_with_next = (prompt, prompt_len, output_len, interval)
            if verbose:
                print(f"Request {i}: Sending at {current_time:.3f}s")
            yield request_with_next

            if request_rate == float("inf"):
                # If the request rate is infinity, then we don't need to wait.
                continue

            # The next request will be sent after the interval.
            await asyncio.sleep(interval)


def load_response(resp: str):
    return json.loads(resp)


async def send_request(
    idx: int,
    backend: str,
    api_url: str,
    api_key: Optional[str],
    model: str,
    prompt: str,
    prompt_len: int,
    output_len: int,
    next_in: float,
    best_of: int,
    use_beam_search: bool,
    stream: bool,
    verbose: bool,
    trace: bool,
) -> None:
    headers = {
        "User-Agent": "Benchmark Client",
    }
    if api_key is not None or api_key != "":
        headers["Authorization"] = f"Bearer {api_key}"

    streaming = stream
    if backend == "vllm":
        pload = {
            "model": model,
            "prompt": prompt,
            # "n": 1,
            # "best_of": best_of,
            # "use_beam_search": use_beam_search,
            "temperature": 0.0 if use_beam_search else TEMPERATURE,
            # "top_p": 1.0,
            "max_tokens": output_len,
            # "ignore_eos": True,
        }
        if stream:
            pload["stream"] = 1
        # Only apply "next_in" for simulator which requires no api_key.
        if next_in > 0.0 and (api_key is None or api_key == ""):
            pload["next_in"] = next_in
    else:
        raise ValueError(f"Unknown backend: {backend}")

    request_start_time = time.perf_counter()
    ts = datetime.now(timezone.utc)
    timeout = aiohttp.ClientTimeout(total=3 * 3600)
    status_code = None
    async with aiohttp.ClientSession(timeout=timeout) as session:
        while True:
            # print(f"Sending request: {api_url}:{pload}")
            async with session.post(api_url, headers=headers, json=pload) as response:
                status_code = response.status
                chunks = []
                token_latencies = []
                previous_token_time = time.perf_counter()
                first = True
                try:
                    if streaming:
                        async for chunk, _ in response.content.iter_chunks():
                            # Stream on: Each chunk in the response is the full response so far
                            chunks = [chunk]

                            now_time = time.perf_counter()
                            if first:
                                time_to_first = now_time - previous_token_time
                                first = False
                            else:
                                token_latencies.append(now_time - previous_token_time)
                            previous_token_time = now_time

                            # Stream off: Chunks are full response.
                            # chunks.append(chunk)

                        output = b"".join(chunks).decode("utf-8")
                        santicized = output.rstrip(
                            "\n\t "
                        )  # Remove trailing whitespace characters including EOF, and "[DONE]"
                    else:
                        time_to_first = time.perf_counter() - previous_token_time
                        output = await response.text()
                        santicized = output
                except Exception as e:
                    print_err(f"Failed to read response for request {idx}: {e}")
                    break
            try:
                ret = load_response(santicized)

                # Re-send the request if it failed.
                if "error" not in ret:
                    break
            except Exception as e:
                # It's ok to parse failure, santicized output could be jsonl, other format, or internal error.
                print_err(f"Invalid response for request {idx}: {santicized}: {e}")
                break

    request_end_time = time.perf_counter()
    request_latency = request_end_time - request_start_time

    if trace:
        request_trace = {
            "request_id": idx,
            "input_tokens": prompt_len,
            "output_tokens": output_len
            if len(token_latencies) == 0
            else len(token_latencies) + 1,
            "timestamp": ts.strftime("%Y-%m-%d %H:%M:%S %Z%z"),
            "E2E": request_latency,
            "status_code": status_code,  # Add status code to trace
            "success": status_code == 200 if status_code else False,  # Add success flag
        }
        if len(token_latencies) > 0:
            request_trace["TTFT"] = time_to_first
            request_trace["TPOT_mean"] = np.mean(token_latencies)  # type: ignore
            request_trace["TPOT_P50"] = np.percentile(token_latencies, 50)  # type: ignore
            request_trace["TPOT_P90"] = np.percentile(token_latencies, 90)  # type: ignore
            request_trace["TPOT_P99"] = np.percentile(token_latencies, 99)  # type: ignore
        print(json.dumps(request_trace))
    REQUEST_LATENCY.append((prompt_len, output_len, request_latency))
    if len(token_latencies) > 0:
        TOKEN_LATENCY.append((prompt_len, output_len, token_latencies))
    TIME_TO_FIRST_TOKEN.append(time_to_first)


async def benchmark(
    backend: str,
    api_url: str,
    api_key: Optional[str],
    model: str,
    input_requests: List[Tuple[str, int, int, float]],
    best_of: int,
    use_beam_search: bool,
    request_rate: float,
    num_requests: int,
    stream: bool,
    verbose: bool,
    trace: bool,
    use_workload_interval: bool = False,
) -> None:
    tasks: List[asyncio.Task] = []

    async for request in get_request(
        input_requests, request_rate, num_requests, verbose, use_workload_interval
    ):
        prompt, prompt_len, output_len, next_in = request
        task = asyncio.create_task(
            send_request(
                len(tasks),
                backend,
                api_url,
                api_key,
                model,
                prompt,
                prompt_len,
                output_len,
                next_in,
                best_of,
                use_beam_search,
                stream,
                verbose,
                trace,
            )
        )
        tasks.append(task)

    await asyncio.gather(*tasks)


def main(args: argparse.Namespace):
    # Set global temperature from args
    global TEMPERATURE
    TEMPERATURE = args.temperature

    result = {}
    if args.verbose:
        print(args)
    else:
        result["input_tokens"] = args.input_len
        result["output_tokens"] = args.output_len
        result["request_rate"] = args.request_rate
        result["seed"] = args.seed
        result["model"] = args.model
        result["temperature"] = args.temperature
        result["samples"] = args.num_prompts

    random.seed(args.seed)
    np.random.seed(args.seed)

    api_url = f"http://{args.host}:{args.port}/v1/completions"
    input_requests = sample_requests(
        args.num_prompts, args.input_len, args.output_len, args.workload_dataset_file
    )
    result["samples"] = len(input_requests)  # Update number of samples

    benchmark_start_time = time.perf_counter()
    try:
        asyncio.run(
            benchmark(
                args.backend,
                api_url,
                args.api_key,
                args.model,
                input_requests,
                args.best_of,
                args.use_beam_search,
                args.request_rate,
                len(input_requests),
                args.stream,
                args.verbose,
                args.trace,
                args.use_workload_interval,
            )
        )
    except Exception:
        import traceback

        traceback.print_exc()
    benchmark_end_time = time.perf_counter()
    benchmark_time = benchmark_end_time - benchmark_start_time

    if args.verbose:
        print()
        print("RESULT SUMMARY")
        print(f"Request rate: {args.request_rate} req/s")
        print(f"Prompt count: {len(REQUEST_LATENCY)}")
        print(f"Total time: {benchmark_time:.2f} s")
        print(
            f"Request Throughput: {len(REQUEST_LATENCY) / benchmark_time:.2f} requests/s"
        )
        print(
            f"Output Token Throughput: {sum([output for _, output, _ in REQUEST_LATENCY]) / benchmark_time:.2f} tokens/s"
        )
        print()
    elif not args.trace:
        result["metric"] = "TPUT"  # Throughput
        result["mean"] = len(REQUEST_LATENCY) / benchmark_time
        print(json.dumps(result))
        result["metric"] = "TT"  # Token throughput
        result["mean"] = (
            sum([output for _, output, _ in REQUEST_LATENCY]) / benchmark_time
        )
        print(json.dumps(result))

    # Compute the latency statistics.
    avg_latency = np.mean([latency for _, _, latency in REQUEST_LATENCY])
    if args.verbose:
        print("REQUEST LATENCIES")
        print(f"Avg: {avg_latency:.2f} s")
        print(
            f"50p: {np.percentile([latency for _, _, latency in REQUEST_LATENCY], 50)} s"
        )
        print(
            f"90p: {np.percentile([latency for _, _, latency in REQUEST_LATENCY], 90)} s"
        )
        print(
            f"99p: {np.percentile([latency for _, _, latency in REQUEST_LATENCY], 99)} s"
        )
        print()
    elif not args.trace:
        result["metric"] = "E2E"  # Request latency
        result["mean"] = avg_latency
        result["P50"] = np.percentile(
            [latency for _, _, latency in REQUEST_LATENCY], 50
        )
        result["P90"] = np.percentile(
            [latency for _, _, latency in REQUEST_LATENCY], 90
        )
        result["P99"] = np.percentile(
            [latency for _, _, latency in REQUEST_LATENCY], 99
        )
        print(json.dumps(result))

    if len(TOKEN_LATENCY) == 0:
        all_token_latencies = np.array([0.0])
    else:
        all_token_latencies = np.array(
            [
                latency
                for _, _, token_latencies in TOKEN_LATENCY
                for latency in token_latencies
            ]
        )
    if args.verbose:
        print("TOKEN LATENCIES")
        print("TTFT")
        print(f"Avg: {np.mean(TIME_TO_FIRST_TOKEN)}")
        print(f"50p: {np.percentile(TIME_TO_FIRST_TOKEN, 50)}")
        print(f"90p: {np.percentile(TIME_TO_FIRST_TOKEN, 90)}")
        print(f"99p: {np.percentile(TIME_TO_FIRST_TOKEN, 99)}")
        print("TPOT")
        print(f"Avg: {np.mean(all_token_latencies)}")
        print(f"50p: {np.percentile(all_token_latencies, 50)}")
        print(f"90p: {np.percentile(all_token_latencies, 90)}")
        print(f"99p: {np.percentile(all_token_latencies, 99)}")
        print()
    elif not args.trace:
        result["metric"] = "TTFT"  # Time to first token
        result["mean"] = np.mean(TIME_TO_FIRST_TOKEN)
        result["P50"] = np.percentile(TIME_TO_FIRST_TOKEN, 50)
        result["P90"] = np.percentile(TIME_TO_FIRST_TOKEN, 90)
        result["P99"] = np.percentile(TIME_TO_FIRST_TOKEN, 99)
        print(json.dumps(result))
        result["metric"] = "TPOT"  # Token latency
        result["mean"] = np.mean(all_token_latencies)
        result["P50"] = np.percentile(all_token_latencies, 50)
        result["P90"] = np.percentile(all_token_latencies, 90)
        result["P99"] = np.percentile(all_token_latencies, 99)
        print(json.dumps(result))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description="Benchmark the online serving throughput."
    )
    parser.add_argument("--backend", type=str, default="vllm", choices=["vllm"])
    parser.add_argument("--host", type=str, default="localhost")
    parser.add_argument("--port", type=int, default=8000)
    parser.add_argument("--model", type=str, default="llama2-7b")
    parser.add_argument(
        "--best-of",
        type=int,
        default=1,
        help="Generates `best_of` sequences per prompt and " "returns the best one.",
    )
    parser.add_argument("--use-beam-search", action="store_true")
    parser.add_argument(
        "--num-prompts", type=int, default=0, help="Number of prompts to process."
    )
    parser.add_argument(
        "--request-rate",
        type=float,
        default=float("inf"),
        help="Number of requests per second. If this is inf, "
        "then all the requests are sent at time 0. "
        "Otherwise, we use Poisson process to synthesize "
        "the request arrival times.",
    )
    parser.add_argument("--seed", type=int, default=0)
    parser.add_argument(
        "--trust-remote-code",
        action="store_true",
        help="trust remote code from huggingface",
    )
    parser.add_argument("--input-len", type=int, default=0)
    parser.add_argument("--output-len", type=int, default=0)
    parser.add_argument("--api-key", type=str, default=None)
    parser.add_argument(
        "--verbose", action="store_true", help="Print human readable info to stdout"
    )
    parser.add_argument(
        "--trace",
        action="store_true",
        help="Print request trace to stdout instead of statistics",
    )
    parser.add_argument("--stream", action="store_true", help="Enable stream request.")
    parser.add_argument(
        "--workload_dataset_file",
        type=str,
        default=None,
        help="Path to a JSON file containing prompts",
    )
    parser.add_argument("--use-workload-interval", action="store_true")
    parser.add_argument(
        "--temperature",
        type=float,
        default=0.0,
        help="Temperature for text generation (default: 0.0)",
    )
    args = parser.parse_args()
    main(args)

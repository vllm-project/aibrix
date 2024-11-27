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
import time
from typing import AsyncGenerator, List, Tuple

import aiohttp
import numpy as np

# (prompt len, output len, request latency)
REQUEST_LATENCY: List[Tuple[int, int, float]] = []
# (prompt len, output len, [per-token latencies])
TOKEN_LATENCY: List[Tuple[int, int, List[float]]] = []
TIME_TO_FIRST_TOKEN: List[float] = []
TEMPERATURE = 0.0


def sample_requests(
    num_requests: int,
    config_input_len: int,
    config_output_len: int,
) -> List[Tuple[str, int, int]]:
    return [
        ("hi " * config_input_len, config_input_len, config_output_len)
        for _ in range(num_requests)
    ]


async def get_request(
    input_requests: List[Tuple[str, int, int]],
    request_rate: float,
    num_requests: int,
) -> AsyncGenerator[Tuple[str, int, int, float], None]:
    requests = iter(input_requests)
    for i, request in enumerate(requests):
        interval = 0.0
        if i < num_requests - 1 and request_rate != float("inf"):
            # Sample the request interval from the exponential distribution.
            interval = np.random.exponential(1.0 / request_rate)

        request_with_next = (request[0], request[1], request[2], interval)
        yield request_with_next

        if request_rate == float("inf"):
            # If the request rate is infinity, then we don't need to wait.
            continue

        # The next request will be sent after the interval.
        await asyncio.sleep(interval)


async def send_request(
    idx: int,
    backend: str,
    api_url: str,
    model: str,
    prompt: str,
    prompt_len: int,
    output_len: int,
    next_in: float,
    best_of: int,
    use_beam_search: bool,
    log_error: bool,
) -> None:
    headers = {
        "User-Agent": "Benchmark Client",
        "user": "your-user-name",
        "model": model,
    }
    streaming = True
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
            # "stream": stream,
        }
        if next_in > 0.0:
            pload["next_in"] = next_in
    else:
        raise ValueError(f"Unknown backend: {backend}")

    request_start_time = time.perf_counter()
    timeout = aiohttp.ClientTimeout(total=3 * 3600)
    async with aiohttp.ClientSession(timeout=timeout) as session:
        while True:
            # print(f"Sending request: {api_url}:{pload}")
            async with session.post(api_url, headers=headers, json=pload) as response:
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
                        santicized = output[:-1]  # Get rid of EOF
                    else:
                        time_to_first = time.perf_counter() - previous_token_time
                        output = await response.text()
                        santicized = output
                except Exception as e:
                    if log_error:
                        print(f"Failed to read response for request {idx}: {e}")
                    break
            try:
                ret = json.loads(santicized)

                # Re-send the request if it failed.
                if "error" not in ret:
                    break
            except Exception:
                # Will retry
                if log_error:
                    print(f"Invalid response for request {idx}: {output}")
                break

    request_end_time = time.perf_counter()
    request_latency = request_end_time - request_start_time
    if len(token_latencies) == 0:
        token_latencies = [0]
    REQUEST_LATENCY.append((prompt_len, output_len, request_latency))
    TOKEN_LATENCY.append((prompt_len, output_len, token_latencies))
    TIME_TO_FIRST_TOKEN.append(time_to_first)


async def benchmark(
    backend: str,
    api_url: str,
    model: str,
    input_requests: List[Tuple[str, int, int]],
    best_of: int,
    use_beam_search: bool,
    request_rate: float,
    num_requests: int,
    log_error: bool,
) -> None:
    tasks: List[asyncio.Task] = []

    async for request in get_request(input_requests, request_rate, num_requests):
        prompt, prompt_len, output_len, next_in = request
        task = asyncio.create_task(
            send_request(
                len(tasks),
                backend,
                api_url,
                model,
                prompt,
                prompt_len,
                output_len,
                next_in,
                best_of,
                use_beam_search,
                log_error,
            )
        )
        tasks.append(task)

    await asyncio.gather(*tasks)


def main(args: argparse.Namespace):
    result = {}
    if args.verbose:
        print(args)
    else:
        result["input_tokens"] = args.input_len
        result["output_tokens"] = args.output_len
        result["request_rate"] = args.request_rate
        result["seed"] = args.seed
        result["model"] = args.model
        result["samples"] = args.num_prompts

    random.seed(args.seed)
    np.random.seed(args.seed)

    api_url = f"http://{args.host}:{args.port}/v1/completions"
    input_requests = sample_requests(args.num_prompts, args.input_len, args.output_len)

    benchmark_start_time = time.perf_counter()
    asyncio.run(
        benchmark(
            args.backend,
            api_url,
            args.model,
            input_requests,
            args.best_of,
            args.use_beam_search,
            args.request_rate,
            args.num_prompts,
            args.verbose,
        )
    )
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
    else:
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
    else:
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

    all_token_latencies = np.array(
        [token_latencies for _, _, token_latencies in TOKEN_LATENCY]
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
    else:
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
        "--num-prompts", type=int, default=1000, help="Number of prompts to process."
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
    parser.add_argument("--input_len", type=int, default=0)
    parser.add_argument("--output_len", type=int, default=0)
    parser.add_argument("--verbose", action="store_true")
    args = parser.parse_args()
    main(args)

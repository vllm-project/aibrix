# SPDX-License-Identifier: Apache-2.0
# SPDX-FileCopyrightText: Copyright contributors to the vLLM project
# The source file is from https://github.com/vllm-project/vllm/blob/main/examples/others/lmcache/disagg_prefill_lmcache_v1/disagg_proxy_server.py
# AIBrix forks this file

# example:
# python3 disagg_proxy_server.py \
#     --host 0.0.0.0 \
#     --port 8000 \
#     --prefiller-hosts prefill1,prefill2 \
#     --decoder-hosts decode1,decode2 \
#     --prefiller-port 8000 \
#     --decoder-port 8000

import argparse
import os
import random
import time
import uuid
from contextlib import asynccontextmanager

import httpx
import numpy as np
from fastapi import FastAPI, Request
from fastapi.responses import StreamingResponse


@asynccontextmanager
async def lifespan(app: FastAPI):
    """
    Lifespan context manager to handle startup and shutdown events.
    """
    # Initialize client pool for all prefill/decode hosts
    app.state.prefill_clients = []
    app.state.decode_clients = []

    for host in global_args.prefiller_hosts:
        base_url = f"http://{host}:{global_args.prefiller_port}/v1"
        app.state.prefill_clients.append(
            httpx.AsyncClient(timeout=None, base_url=base_url)
        )

    for host in global_args.decoder_hosts:
        base_url = f"http://{host}:{global_args.decoder_port}/v1"
        app.state.decode_clients.append(
            httpx.AsyncClient(timeout=None, base_url=base_url)
        )

    yield

    for client in app.state.prefill_clients:
        await client.aclose()
    for client in app.state.decode_clients:
        await client.aclose()


app = FastAPI(lifespan=lifespan)


class StatsCalculator:
    def __init__(self):
        self._stats = []
        self._last_log_time = time.time()

    def add(self, value):
        self._stats.append(value)
        if time.time() - self._last_log_time > 5:
            self._log_stats()
            self._last_log_time = time.time()

    def _log_stats(self):
        np_arr = np.array(self._stats)
        output_str = (
                f"\nNum requests: {len(self._stats)}"
                + "\nPrefill node TTFT stats:"
                + f"\n - Average (ms): {np.mean(np_arr)}"
                + f"\n - Median (ms): {np.median(np_arr)}"
                + f"\n - 99th Percentile (ms): {np.percentile(np_arr, 99)}\n"
        )
        print("===============================", output_str, "===============================")


stats_calculator = StatsCalculator()
counter = 0


def parse_args():
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, default=8000)
    parser.add_argument("--host", type=str, default="localhost")
    parser.add_argument("--prefiller-hosts", type=str, default="localhost")
    parser.add_argument("--prefiller-port", type=int, default=3000)
    parser.add_argument("--decoder-hosts", type=str, default="localhost")
    parser.add_argument("--decoder-port", type=int, default=8200)
    args = parser.parse_args()

    # Parse comma-separated lists
    args.prefiller_hosts = [x.strip() for x in args.prefiller_hosts.split(",")]
    args.decoder_hosts = [x.strip() for x in args.decoder_hosts.split(",")]
    return args


async def send_request_to_service(client: httpx.AsyncClient, endpoint: str, req_id: str, req_data: dict):
    req_data_copy = req_data.copy()
    # nixl-specific kv_transfer_params for prefillers
    req_data_copy['kv_transfer_params'] = {
        "do_remote_decode": True,
        "do_remote_prefill": False,
        "remote_engine_id": None,
        "remote_block_ids": None,
        "remote_host": None,
        "remote_port": None
    }
    # disable streaming for prefillers
    req_data_copy["stream"] = False
    if "stream_options" in req_data_copy:
        del req_data_copy["stream_options"]
    req_data_copy["max_tokens"] = 1
    if "max_completion_tokens" in req_data_copy:
        req_data_copy["max_completion_tokens"] = 1

    headers = {
        "Authorization": f"Bearer {os.environ.get('OPENAI_API_KEY')}",
        "X-Request-Id": req_id
    }
    response = await client.post(endpoint, json=req_data_copy, headers=headers)
    response.raise_for_status()
    # extract nixl-specific kv_transfer_params returned from prefillers and
    # attach to the req_data for decode clients
    response_json = response.json()
    kv_transfer_params = response_json.get('kv_transfer_params', {})
    if kv_transfer_params:
        req_data["kv_transfer_params"] = kv_transfer_params
        req_data["kv_transfer_params"]["remote_host"] = client.base_url.host
    return response


async def stream_service_response(client: httpx.AsyncClient, endpoint: str, req_id: str, req_data: dict):
    headers = {
        "Authorization": f"Bearer {os.environ.get('OPENAI_API_KEY')}",
        "X-Request-Id": req_id
    }
    async with client.stream("POST", endpoint, json=req_data, headers=headers) as response:
        response.raise_for_status()
        async for chunk in response.aiter_bytes():
            yield chunk


def select_random_clients():
    prefill_idx = random.randint(0, len(app.state.prefill_clients) - 1)
    decode_idx = random.randint(0, len(app.state.decode_clients) - 1)
    prefill_client = app.state.prefill_clients[prefill_idx]
    decode_client = app.state.decode_clients[decode_idx]

    # Log selection result
    print(
        f"Selected prefill node: {global_args.prefiller_hosts[prefill_idx]} | "
        f"decode node: {global_args.decoder_hosts[decode_idx]}"
    )

    return prefill_client, decode_client


@app.post("/v1/completions")
async def handle_completions(request: Request):
    global counter, stats_calculator
    counter += 1
    st = time.time()

    try:
        req_id = str(uuid.uuid4())
        req_data = await request.json()
        prefill_client, decode_client = select_random_clients()

        await send_request_to_service(prefill_client, "/completions", req_id, req_data)
        et = time.time()
        stats_calculator.add(et - st)

        async def generate_stream():
            async for chunk in stream_service_response(decode_client, "/completions", req_id, req_data):
                yield chunk

        return StreamingResponse(generate_stream(), media_type="text/event-stream")

    except Exception as e:
        import sys, traceback
        print("Error occurred in disagg prefill proxy server - completions endpoint")
        print(e)
        print("".join(traceback.format_exception(*sys.exc_info())))
        raise


@app.post("/v1/chat/completions")
async def handle_chat_completions(request: Request):
    global counter, stats_calculator
    counter += 1
    st = time.time()

    try:
        req_id = str(uuid.uuid4())
        req_data = await request.json()
        prefill_client, decode_client = select_random_clients()

        await send_request_to_service(prefill_client, "/chat/completions", req_id, req_data)
        et = time.time()
        stats_calculator.add(et - st)

        async def generate_stream():
            async for chunk in stream_service_response(decode_client, "/chat/completions", req_id, req_data):
                yield chunk

        return StreamingResponse(generate_stream(), media_type="text/event-stream")

    except Exception as e:
        import sys, traceback
        print("Error occurred in disagg prefill proxy server - chat completions endpoint")
        print(e)
        print("".join(traceback.format_exception(*sys.exc_info())))
        raise


if __name__ == "__main__":
    global global_args
    global_args = parse_args()

    import uvicorn
    uvicorn.run(app, host=global_args.host, port=global_args.port)

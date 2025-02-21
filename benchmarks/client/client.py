import argparse
import logging
import time
import asyncio
import openai
import json
import io
import traceback


from typing import List
from utils import (load_workload, wrap_prompt_as_chat_message)

logging.basicConfig(level=logging.INFO)

async def send_request_streaming(client: openai.AsyncOpenAI, 
                                 model: str, 
                                 endpoint: str, 
                                 prompt: str, 
                                 output_file: str,
                                 ):
    start_time = asyncio.get_event_loop().time()
    first_response_time = None
    try:
        stream = await client.chat.completions.create(
            model=model,
            messages=prompt,
            temperature=0,
            max_tokens=2048,
            stream=True,
            stream_options={"include_usage": True},
        )
        text_chunks = []
        prompt_tokens = 0
        output_tokens = 0
        total_tokens = 0   
        
        async for chunk in stream:
            if chunk.choices:
                if chunk.choices[0].delta.content is not None:
                    if not first_response_time:
                        first_response_time = asyncio.get_event_loop().time()
                    output_text = chunk.choices[0].delta.content
                    text_chunks.append(output_text)
                    prompt_tokens = chunk.usage.prompt_tokens
                    output_tokens = chunk.usage.completion_tokens
                    total_tokens = chunk.usage.total_tokens
        response = "".join(text_chunks)
        response_time = asyncio.get_event_loop().time()
        latency = response_time - start_time
        throughput = output_tokens / latency
        ttft = first_response_time - start_time
        tpot = (response_time - first_response_time) / output_tokens
        result = {
            "input": prompt,
            "output": response,
            "prompt_tokens": prompt_tokens,
            "output_tokens": output_tokens,
            "total_tokens": total_tokens,
            "latency": latency,
            "throughput": throughput,
            "start_time": start_time,
            "current_time": asyncio.get_event_loop().time(),
            "ttft": ttft,
            "tpot": tpot, 
        }
        logging.info(result)
        # Write result to JSONL file
        output_file.write(json.dumps(result) + "\n")
        output_file.flush()  # Ensure data is written immediately to the file
        return result
    except Exception as e:
        logging.error(f"Error sending request to at {endpoint}: {str(e)}")
        traceback.print_exc() 
        return None

async def benchmark_streaming(client: openai.AsyncOpenAI,
                              endpoint: str,  
                              model: str, 
                              load_struct: List,
                              output_file: io.TextIOWrapper):

    batch_tasks = []
    base_time = time.time()
    num_requests = 0
    for requests_dict in load_struct:
        ts = int(requests_dict["timestamp"])
        requests = requests_dict["requests"]
        cur_time = time.time()
        target_time = base_time + ts / 1000.0
        logging.warning(f"Prepare to launch {len(requests)} streaming tasks after {target_time - cur_time}")
        if target_time > cur_time:
            await asyncio.sleep(target_time - cur_time)
        formatted_prompts = [wrap_prompt_as_chat_message(request["prompt"]) for request in requests]
        for formatted_prompt in formatted_prompts:
            task = asyncio.create_task(
                send_request_streaming(client = client, 
                                       model = model, 
                                       endpoint = endpoint, 
                                       prompt = formatted_prompt, 
                                       output_file = output_file)
            )
            batch_tasks.append(task)
        num_requests += len(requests)
    await asyncio.gather(*batch_tasks)
    logging.warning(f"All {num_requests} requests completed for deployment.")
    
# Asynchronous request handler
async def send_request_batch(client, model, endpoint, prompt, output_file):
    start_time = asyncio.get_event_loop().time()
    try:
        response = await client.chat.completions.create(
            model=model,
            messages=prompt,
            temperature=0,
            max_tokens=2048
        )

        latency = asyncio.get_event_loop().time() - start_time
        prompt_tokens = response.usage.prompt_tokens
        output_tokens = response.usage.completion_tokens
        total_tokens = response.usage.total_tokens
        throughput = output_tokens / latency
        output_text = response.choices[0].message.content

        result = {
            "input": prompt,
            "output": output_text,
            "prompt_tokens": prompt_tokens,
            "output_tokens": output_tokens,
            "total_tokens": total_tokens,
            "start_time": start_time,
            "current_time": asyncio.get_event_loop().time(),
            "latency": latency,
            "throughput": throughput
        }
        logging.info(result)
        # Write result to JSONL file
        output_file.write(json.dumps(result) + "\n")
        output_file.flush()  # Ensure data is written immediately to the file

        return result
    except Exception as e:
        logging.error(f"Error sending request to at {endpoint}: {str(e)}")
        return None


async def benchmark_batch(client: openai.AsyncOpenAI,
                          endpoint: str, 
                          model: str, 
                          load_struct: List, 
                          output_file: io.TextIOWrapper):
    batch_tasks = []
    base_time = time.time()
    num_requests = 0
    for requests_dict in load_struct:
        ts = int(requests_dict["timestamp"])
        requests = requests_dict["requests"]
        cur_time = time.time()
        target_time = base_time + ts / 1000.0
        logging.warning(f"Prepare to launch {len(requests)} batched tasks after {target_time - cur_time}")
        if target_time > cur_time:
            await asyncio.sleep(target_time - cur_time)
        formatted_prompts = [wrap_prompt_as_chat_message(request["prompt"]) for request in requests]
        for formatted_prompt in formatted_prompts:
            task = asyncio.create_task(
                send_request_batch(client, model, endpoint, formatted_prompt, output_file)
            )
            batch_tasks.append(task)
        num_requests += len(requests)
    await asyncio.gather(*batch_tasks)
    logging.warning(f"All {num_requests} requests completed for deployment.")


def main(args):
    logging.info(f"Starting benchmark on endpoint {args.endpoint}")
    with open(args.output_file_path, 'w', encoding='utf-8') as output_file:
        load_struct = load_workload(args.workload_path)
        client = openai.AsyncOpenAI(
            api_key=args.api_key,
            base_url=args.endpoint + "/v1",
        )
        if args.routing_strategy is not None:
            client.default_headers["routing-strategy"] = args.routing_strategy
        if not args.streaming:
            logging.info("Using batch client")
            start_time = time.time()
            asyncio.run(benchmark_batch(
                client = client,
                endpoint=args.endpoint, 
                model=args.model, 
                load_struct=load_struct, 
                output_file=output_file, 
            ))
            end_time = time.time()
            logging.info(f"Benchmark completed in {end_time - start_time:.2f} seconds")
        else:
            logging.info("Using streaming client")
            start_time = time.time()
            asyncio.run(benchmark_streaming(
                client = client,
                endpoint=args.endpoint, 
                model=args.model, 
                load_struct=load_struct, 
                output_file=output_file,
            ))
            end_time = time.time()
            logging.info(f"Benchmark completed in {end_time - start_time:.2f} seconds")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description='Workload Generator')
    parser.add_argument("--workload-path", type=str, default=None, help="File path to the workload file.")
    parser.add_argument('--endpoint', type=str, required=True)
    parser.add_argument("--model", type=str, required=True, help="Name of the model.")
    parser.add_argument("--api-key", type=str, required=True, help="API key to the service. ")
    parser.add_argument('--output-file-path', type=str, default="output.jsonl")
    parser.add_argument("--streaming", action="store_true", help="Use streaming client.")
    parser.add_argument("--routing-strategy", type=str, required=False, default=None, help="Routing strategy to use.")

    args = parser.parse_args()
    main(args)

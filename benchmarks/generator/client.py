import argparse
import logging
import time
import asyncio
import openai
import json
import io
import aiohttp


from typing import List
from utils import (load_workload, wrap_prompt_as_chat_message)

async def send_request_streaming(session, model, endpoint, headers, formatted_prompt, output_file):
    payload = {
        "model": model,
        "prompt": formatted_prompt,
        "temperature": 0,
        "max_tokens": 2048,
        "stream": 1,
    }
    with session.post(endpoint, headers=headers, json=payload) as response:
        chunks = []
        token_latencies = []
        previous_token_time = time.perf_counter()
        first = True
        output = None
        request_start_time = time.perf_counter()
        try: 
            async for chunk, _ in response.content.iter_chunks():
                chunks = [chunk]
                now_time = time.perf_counter()
                if first:
                    time_to_first = now_time - previous_token_time
                    first = False
                else:
                    token_latencies.append(now_time - previous_token_time)
                previous_token_time = now_time
            output = b"".join(chunks).decode("utf-8")
            output = output.rstrip(
                "\n\t "
            )  # Remove trailing whitespace characters including EOF, and "[DONE]"
        except Exception as e:
            logging.error(f"Failed to read response for request: {e}")
        
        try:
            response = json.loads(output)
        except Exception as e:
            logging.error(f"Invalid response for request {output}: {e}")
            
        request_end_time = time.perf_counter()
        request_latency = request_end_time - request_start_time
        
        logging.warning(
            f"Request completed in {request_latency:.2f} seconds with throughput 0.0 tokens/s, request {formatted_prompt} response {response}")
        return response
        

    
    
    # async with session.post(api_url, headers=headers, json=pload) as response:
    #             chunks = []
    #             token_latencies = []
    #             previous_token_time = time.perf_counter()
    #             first = True
    #             try:
    #                 if streaming:
    #                     async for chunk, _ in response.content.iter_chunks():
    #                         # Stream on: Each chunk in the response is the full response so far
    #                         chunks = [chunk]

    #                         now_time = time.perf_counter()
    #                         if first:
    #                             time_to_first = now_time - previous_token_time
    #                             first = False
    #                         else:
    #                             token_latencies.append(now_time - previous_token_time)
    #                         previous_token_time = now_time

    #                         # Stream off: Chunks are full response.
    #                         # chunks.append(chunk)

    #                     output = b"".join(chunks).decode("utf-8")
    #                     santicized = output.rstrip(
    #                         "\n\t "
    #                     )  # Remove trailing whitespace characters including EOF, and "[DONE]"
    #                 else:
    #                     time_to_first = time.perf_counter() - previous_token_time
    #                     output = await response.text()
    #                     santicized = output
    #             except Exception as e:
    #                 print_err(f"Failed to read response for request {idx}: {e}")
    #                 break
    #         try:
    #             ret = load_response(santicized)

    #             # Re-send the request if it failed.
    #             if "error" not in ret:
    #                 break
    #         except Exception as e:
    #             # It's ok to parse failure, santicized output could be jsonl, other format, or internal error.
    #             print_err(f"Invalid response for request {idx}: {santicized}: {e}")
    #             break

async def benchmark_streaming(endpoint: str, 
                          model: str, 
                          api_key: str, 
                          load_struct: List, 
                          output_file: io.TextIOWrapper):
    
    headers = {
        "User-Agent": "Benchmark Client",
    }
    if api_key is not None or api_key != "":
        headers["Authorization"] = f"Bearer {api_key}"
    
    with aiohttp.ClientSession() as session:
        base_time = time.time()
        all_tasks = []
        num_requests = 0
        for requests_dict in load_struct:
            ts = int(requests_dict["timestamp"])
            requests = requests_dict["requests"]
            cur_time = time.time()
            target_time = base_time + ts / 1000.0
            logging.warning(f"Prepare to launch {len(requests)} tasks after {target_time - cur_time}")
            formatted_prompts = [wrap_prompt_as_chat_message(request["prompt"]) for request in requests]
            if target_time > cur_time:
                await asyncio.sleep(target_time - cur_time)
            for formatted_prompt in formatted_prompts:
                task = asyncio.create_task(
                    send_request_streaming(session, model, endpoint, headers, formatted_prompt, output_file)
                )
                all_tasks.append(task)
            num_requests += len(formatted_prompts)
        await asyncio.gather(*all_tasks)
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
            "latency": latency,
            "throughput": throughput
        }

        # Write result to JSONL file
        output_file.write(json.dumps(result) + "\n")
        output_file.flush()  # Ensure data is written immediately to the file

        logging.warning(
            f"Request completed in {latency:.2f} seconds with throughput {throughput:.2f} tokens/s, request {prompt} response {response}")
        return result
    except Exception as e:
        logging.error(f"Error sending request to at {endpoint}: {str(e)}")
        return None


async def benchmark_batch(endpoint: str, 
                          model: str, 
                          api_key: str, 
                          load_struct: List, 
                          output_file: io.TextIOWrapper):
    client = openai.AsyncOpenAI(
        api_key=api_key,
        base_url=endpoint + "/v1",
    )

    batch_tasks = []
    base_time = time.time()
    num_requests = 0
    for requests_dict in load_struct:
        ts = int(requests_dict["timestamp"])
        requests = requests_dict["requests"]
        cur_time = time.time()
        target_time = base_time + ts / 1000.0
        logging.warning(f"Prepare to launch {len(requests)} tasks after {target_time - cur_time}")
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
    logging.info(f"Starting benchmark on endpoint {args.endpoint} client type {args.client_type}")
    with open(args.output_file_path, 'a', encoding='utf-8') as output_file:
        load_struct = load_workload(args.workload_path)
        if args.client_type == 'batch':
            logging.info("Using batch client")
            start_time = time.time()
            asyncio.run(benchmark_batch(endpoint=args.endpoint, model=args.model, api_key=args.api_key, load_struct=load_struct, output_file_path=output_file))
            end_time = time.time()
            logging.info(f"Benchmark completed in {end_time - start_time:.2f} seconds")
        else:
            logging.info("Using batch client")
            start_time = time.time()
            asyncio.run(benchmark_streaming(endpoint=args.endpoint, model=args.model, api_key=args.api_key, load_struct=load_struct, output_file=output_file))
            end_time = time.time()
            logging.info(f"Benchmark completed in {end_time - start_time:.2f} seconds")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description='Workload Generator')
    parser.add_argument("--workload-path", type=str, default=None, help="File path to the workload file.")
    parser.add_argument('--endpoint', type=str, required=True)
    parser.add_argument("--model", type=str, required=True, help="Name of the model.")
    parser.add_argument("--api-key", type=str, required=True, help="API key to the service. ")
    parser.add_argument('--output-file-path', type=str, default="output.jsonl")
    parser.add_argument('--client-type', type=str, choices=['batch', 'streaming'], default='batch',
                        help='Type of client to use (batch or streaming).')

    args = parser.parse_args()
    main(args)

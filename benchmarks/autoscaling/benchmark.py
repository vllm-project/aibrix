import dataclasses
import json
import os
import random
import time
from datetime import datetime
from typing import List, Optional, Tuple, Dict

import openai
import asyncio
import logging

from transformers import PreTrainedTokenizerBase

from vllm.engine.arg_utils import EngineArgs
from vllm.utils import FlexibleArgumentParser

from bench_workload_generator import generate_workload, plot_workload

try:
    from vllm.transformers_utils.tokenizer import get_tokenizer
except ImportError:
    from backend_request_func import get_tokenizer


def setup_logging(log_filename, level=logging.INFO):
    """
    Set the global log configuration. The logs will be written into the specified file and output to the console.

    :param log_filename: logging output file
    """

    logging.basicConfig(
        level=level,
        format="%(asctime)s - %(levelname)s - %(message)s",
        datefmt="%Y-%m-%d %H:%M:%S",
    )
    if not os.path.exists((log_dir := os.path.dirname(log_filename))):
        os.makedirs(log_dir, exist_ok=True)

    logger = logging.getLogger()
    logger.setLevel(level)

    # create a handler to file
    file_handler = logging.FileHandler(log_filename)
    file_handler.setLevel(level)

    # create a handler to console
    console_handler = logging.StreamHandler()
    console_handler.setLevel(level)

    formatter = logging.Formatter(
        "%(asctime)s - %(name)s - %(levelname)s - %(message)s"
    )
    file_handler.setFormatter(formatter)
    console_handler.setFormatter(formatter)
    logger.addHandler(file_handler)

    logging.info(f"save log to {log_filename}")


# Function to wrap the prompt into OpenAI's chat completion message format.
def wrap_prompt_as_chat_message(prompt: str):
    """
    Wrap the prompt into OpenAI's chat completion message format.

    :param prompt: The user prompt to be converted.
    :return: A list containing chat completion messages.
    """
    # Define a system message if needed (optional)
    # system_message = {"role": "system", "content": "You are a helpful assistant."}

    # Wrap the user prompt as a user message
    user_message = {"role": "user", "content": prompt}

    # Combine the system and user message into the expected message format
    return [user_message]


# Function to build OpenAI clients for each model endpoint.
def build_openai_clients(model_endpoints: Dict[str, str]):
    clients = {}
    for model, endpoint in model_endpoints.items():
        # Create an OpenAI client with custom base URL if necessary
        client = openai.AsyncOpenAI(
            api_key="sk-kFJ12nKsFVfVmGpj3QzX65s4RbN2xJqWzPYCjYu7wT3BlbLi",
            base_url=endpoint,
        )

        clients[model] = client
    return clients


# Build the OpenAI endpoint dictionary from the deployment plans
def build_openai_endpoints(deployment_plan):
    endpoints = {
        "deployment0": {
            "deepseek-coder-7b-instruct": "http://0.0.0.0:8000/v1",
        },
        "deployment1": {
            "model-1": "http://0.0.0.0:8071/v1",
            "model-2": "http://0.0.0.0:8072/v1",
            "model-3": "http://0.0.0.0:8073/v1",
            "model-4": "http://0.0.0.0:8074/v1"
        },
        "deployment2": {
            "model-1": "http://0.0.0.0:8071/v1",
            "model-2": "http://0.0.0.0:8072/v1",
            "model-3": "http://0.0.0.0:8073/v1",
            "model-4": "http://0.0.0.0:8074/v1"
        },
        "deployment3": {
            "model-1": "http://0.0.0.0:8070/v1",
            "model-2": "http://0.0.0.0:8070/v1",
            "model-3": "http://0.0.0.0:8070/v1",
            "model-4": "http://0.0.0.0:8070/v1"
        }
    }
    return endpoints[deployment_plan]


PROMPT = "You are a helpful assistant in recognizes the content of tables in markdown format. Here is a table as fellows. You need to answer my question about the table.\n# Table\n|Opening|Opening|Sl. No.|Film|Cast|Director|Music Director|Notes|\n|----|----|----|----|----|----|----|----|\n|J A N|9|1|Agni Pushpam|Jayabharathi, Kamalahasan|Jeassy|M. K. Arjunan||\n|J A N|16|2|Priyamvada|Mohan Sharma, Lakshmi, KPAC Lalitha|K. S. Sethumadhavan|V. Dakshinamoorthy||\n|J A N|23|3|Yakshagaanam|Madhu, Sheela|Sheela|M. S. Viswanathan||\n|J A N|30|4|Paalkkadal|Sheela, Sharada|T. K. Prasad|A. T. Ummer||\n|F E B|5|5|Amma|Madhu, Srividya|M. Krishnan Nair|M. K. Arjunan||\n|F E B|13|6|Appooppan|Thikkurissi Sukumaran Nair, Kamal Haasan|P. Bhaskaran|M. S. Baburaj||\n|F E B|20|7|Srishti|Chowalloor Krishnankutty, Ravi Alummoodu|K. T. Muhammad|M. S. Baburaj||\n|F E B|20|8|Vanadevatha|Prem Nazir, Madhubala|Yusufali Kechery|G. Devarajan||\n|F E B|27|9|Samasya|Madhu, Kamalahaasan|K. Thankappan|Shyam||\n|F E B|27|10|Yudhabhoomi|K. P. Ummer, Vidhubala|Crossbelt Mani|R. K. Shekhar||\n|M A R|5|11|Seemantha Puthran|Prem Nazir, Jayabharathi|A. B. Raj|M. K. Arjunan||\n|M A R|12|12|Swapnadanam|Rani Chandra, Dr. Mohandas|K. G. George|Bhaskar Chandavarkar||\n|M A R|19|13|Thulavarsham|Prem Nazir, sreedevi, Sudheer|N. Sankaran Nair|V. Dakshinamoorthy||\n|M A R|20|14|Aruthu|Kaviyoor Ponnamma, Kamalahasan|Ravi|G. Devarajan||\n|M A R|26|15|Swimming Pool|Kamal Haasan, M. G. Soman|J. Sasikumar|M. K. Arjunan||\n\n# Question\nWhat' s the content in the (1,1) cells\n"  # noqa: E501


# Asynchronous request handler
async def send_request(client, model, endpoint, prompt, output_file):
    start_time = asyncio.get_event_loop().time()
    start_ts = time.time()
    try:
        response = await client.chat.completions.create(
            model=model,
            messages=prompt,
            temperature=0,
            max_tokens=128
        )
        latency = asyncio.get_event_loop().time() - start_time
        prompt_tokens = response.usage.prompt_tokens
        output_tokens = response.usage.completion_tokens
        total_tokens = response.usage.total_tokens
        throughput = output_tokens / latency
        output_text = response.choices[0].message.content

        result = {
            "model": model,
            "endpoint": endpoint,
            "start_timestamp": start_ts,
            "latency": latency,
            "output": output_text,
            "prompt_tokens": prompt_tokens,
            "output_tokens": output_tokens,
            "total_tokens": total_tokens,
            "throughput": throughput,
        }

        logging.info(
            f"Request for {model} completed in {latency:.2f} seconds with throughput {throughput:.2f} tokens/s, answer: {output_text[:30]}...")
        return result
    except Exception as e:
        logging.error(f"Error sending request to {model} at {endpoint}: {str(e)}")
        result = {
            "model": model,
            "endpoint": endpoint,
            "start_timestamp": start_ts,
            "latency": asyncio.get_event_loop().time() - start_time,
            "start_time": start_time,
            "error": str(e),
        }
        return None
    finally:
        # Write result to JSONL file
        output_file.write(json.dumps(result) + "\n")
        output_file.flush()  # Ensure data is written immediately to the file



# Benchmark requests and log results into the specified file
async def benchmark_requests(clients, model_endpoints, prompts, num_requests, concurrency, output_file_path):
    models = list(model_endpoints.keys())
    total_models = len(models)
    requests_per_model = num_requests // total_models

    with open(output_file_path, 'a', encoding='utf-8') as output_file:
        for start_idx in range(0, requests_per_model, concurrency):
            batch_tasks = []
            end_idx = min(start_idx + concurrency, requests_per_model)

            for model_index, model in enumerate(models):
                client = clients[model]
                endpoint = model_endpoints[model]

                for request_idx in range(start_idx, end_idx):
                    prompt_idx = (model_index * requests_per_model + request_idx) % len(prompts)
                    prompt, _, _, _ = prompts[prompt_idx]
                    prompt = wrap_prompt_as_chat_message(prompt)

                    task = asyncio.create_task(send_request(client, model, endpoint, prompt, output_file))
                    batch_tasks.append(task)

            logging.info(f"submit a batch with {concurrency * total_models} requests")
            await asyncio.gather(*batch_tasks)
            logging.info(f"Completed requests {start_idx} to {end_idx - 1} for each model.")

        logging.info(f"All {num_requests} requests completed for deployment.")
        
async def benchmark_workloads(clients, model_endpoints, workload, interval, output_file_path):
    models = list(model_endpoints.keys())
    batch_tasks = []

    with open(output_file_path, 'a', encoding='utf-8') as output_file:
        start_time = time.time()   
        next_start = start_time + interval  
        for idx, prompts in enumerate(workload):
            if len(prompts) == 0:
                logging.info(f"===== Sending Batch[{idx}], concurrency={len(prompts)}: not sending in the batch")
            else:
                wrapped_prompts = [wrap_prompt_as_chat_message(prompt) for (prompt, _, _, _,) in prompts]
                for wrapper_prompt in wrapped_prompts:
                    model = random.choice(models)
                    client = clients[model]
                    endpoint = model_endpoints[model]
                    task = asyncio.create_task(send_request(client, model, endpoint, wrapper_prompt, output_file))
                    batch_tasks.append(task)
                wait_time = next_start - time.time()
                logging.info(f"===== Sending Batch[{idx}], concurrency={len(prompts)}: E.g. question: {prompts[0][:30]}...")
                if wait_time > 0:
                    time.sleep(wait_time)
                next_start += interval
        await asyncio.gather(*batch_tasks)


def sample_sharegpt_requests(
        dataset_path: str,
        num_requests: int,
        tokenizer: PreTrainedTokenizerBase,
        fixed_output_len: Optional[int] = None,
) -> List[Tuple[str, int, int, None]]:
    # Load the dataset
    with open(dataset_path, encoding='utf-8') as f:
        dataset = json.load(f)
    dataset = [data for data in dataset if len(data["conversations"]) >= 2]
    dataset = [(data["conversations"][0]["value"], data["conversations"][1]["value"]) for data in dataset]

    filtered_dataset: List[Tuple[str, int, int]] = []
    for i in range(len(dataset)):
        if len(filtered_dataset) == num_requests:
            break
        prompt = dataset[i][0]
        prompt_token_ids = tokenizer(prompt).input_ids
        completion = dataset[i][1]
        completion_token_ids = tokenizer(completion).input_ids
        prompt_len = len(prompt_token_ids)
        output_len = len(completion_token_ids) if fixed_output_len is None else fixed_output_len
        if prompt_len < 4 or (fixed_output_len is None and output_len < 4):
            continue
        if prompt_len > 1024 or prompt_len + output_len > 2048:
            continue
        filtered_dataset.append((prompt, prompt_len, output_len, None))

    return filtered_dataset


def main(args):
    FROM_GENERATOR = args.workload is not None
    WORKLOAD_MODE = args.concurrency is not None
    tokenizer = get_tokenizer(args.model, trust_remote_code=True)
    openai_endpoints = build_openai_endpoints(args.deployment_endpoints)
    openai_clients = build_openai_clients(openai_endpoints)
    if not FROM_GENERATOR:
        if args.dataset_path is not None:
            logging.info(f"Start to sample {args.num_prompts} prompts from {args.dataset_path}")
            num_prompts = args.num_prompts
            if WORKLOAD_MODE:
                # length * avearge bar
                num_prompts = int(args.w_B * args.w_duration_sec / args.w_interval_sec)
            input_requests = sample_sharegpt_requests(
                dataset_path=args.dataset_path,
                num_requests=num_prompts,
                tokenizer=tokenizer,
                fixed_output_len=args.sharegpt_output_len,
            )
        else:
            prompt_len = len(tokenizer(PROMPT).input_ids)
            input_requests = [(PROMPT, prompt_len, args.output_len, None)] * args.num_prompts

        logging.info(f"Samples results: {input_requests[0]}")

        start_time = time.time()       
        if WORKLOAD_MODE:
            logging.info(f"Starting benchmark for {args.num_prompts} prompts with deployment {args.deployment_endpoints}")
            asyncio.run(benchmark_requests(openai_clients, openai_endpoints, input_requests, args.num_prompts, args.concurrency,
                                        args.output_file_path))
        else:
            interval_sec = args.w_interval_sec
            logging.info(f"args.concurrency is None, generate workload: "
                        f"A={args.w_A}, B={args.w_B}, sigma={args.w_sigma}, period={args.w_period}, "
                        f"duration_sec={args.w_duration_sec}, interval_sec={interval_sec}")
            workloads = generate_workload(
                input_requests,
                A=args.w_A, B=args.w_B, sigma=args.w_sigma, only_rise=args.w_only_rise,
                period=args.w_period, duration_sec=args.w_duration_sec, interval_sec=interval_sec,
            )
            # if you want to see the workload traffic trend
            plot_workload({"llm": workloads}, interval_sec=interval_sec,
                        output_path=f"workload_plot/{identifier}.png")
            next_start = start_time + interval_sec
            for idx, each_input_requests in enumerate(workloads):
                if len(each_input_requests) == 0:
                    logging.info(f"===== Sending Batch[{idx}], concurrency={len(each_input_requests)}: not sending in the batch")
                else:
                    logging.info(f"===== Sending Batch[{idx}], concurrency={len(each_input_requests)}: E.g. question: {each_input_requests[0][:30]}...")
                    asyncio.run(benchmark_requests(openai_clients, openai_endpoints, each_input_requests, len(each_input_requests),
                                                len(each_input_requests), args.output_file_path))
                # wait until passing args.w_interval_sec
                wait_time = next_start - time.time()
                if wait_time > 0:
                    time.sleep(wait_time)
                next_start += interval_sec
        end_time = time.time()
        logging.info(f"Benchmark completed in {end_time - start_time:.2f} seconds")
    else:
        try:
            interval_sec = args.w_interval_sec
            with open(args.workload, 'r') as file:
                generated_workloads = json.load(file)
                start_time = time.time()    
                asyncio.run(benchmark_workloads(openai_clients, openai_endpoints, generated_workloads, interval_sec, args.output_file_path))
                end_time = time.time()
                logging.info(f"Benchmark completed in {end_time - start_time:.2f} seconds")
        except FileNotFoundError:
            print("The file was not found")
        except json.JSONDecodeError:
            print("The file contains invalid JSON")
        except Exception as e:
            print(f"An error occurred: {e}")
    


if __name__ == "__main__":
    parser = FlexibleArgumentParser(description='Benchmark the performance of multi-loras.')
    parser.add_argument("--dataset-path", type=str, default='/Users/kangrong.cn/data/AIBrix/ShareGPT_V3_unfiltered_cleaned_split.json', help="Path to the dataset.")
    parser.add_argument('--workload', type=str, default=None, help="Path to generated workload trace.")
    parser.add_argument("--sharegpt-output-len", type=int, default=None,
                        help="Output length for each request. Overrides the output length from the ShareGPT dataset.")
    parser.add_argument('--num-prompts', type=int, default=200, help="Number of the prompts sampled from dataset")
    parser.add_argument('--concurrency', type=int, default=None, help="Number of the prompts concurrency, if None, use workload param to generate")
    parser.add_argument('--output-len', type=int, default=10)
    parser.add_argument('--output-file-path', type=str, default="output.jsonl")
    parser.add_argument('--deployment-endpoints', type=str, default="deployment0")
    """
    A=1, B=1, sigma=0.1, only_rise: bool = False,
    period=0.25, duration_sec: int = None, interval_sec: int = None,
    """
    parser.add_argument('--w-A', type=float, default=5)
    parser.add_argument('--w-B', type=float, default=5)
    parser.add_argument('--w-sigma', type=float, default=0.1)
    parser.add_argument('--w-only-rise', action='store_true')
    parser.add_argument('--w-period', type=float, default=12)
    parser.add_argument('--w-duration-sec', type=int, default=600)
    parser.add_argument('--w-interval-sec', type=int, default=10)

    parser = EngineArgs.add_cli_args(parser)
    args = parser.parse_args()

    if args.concurrency is not None:
        identifier = f"onestep_np{args.num_prompts}_c{args.concurrency}"
    else:
        identifier = f"workload_A{args.w_A}_B{args.w_B}_P{args.w_period}_D{args.w_duration_sec}s_I{args.w_interval_sec}s"
    identifier += "_" + datetime.now().strftime("%Y%m%d_%H%M%S")
    args.output_file_path = f"output_stats/output_{identifier}.jsonl"
    os.makedirs(os.path.dirname(args.output_file_path), exist_ok=True)

    setup_logging(f'logs/bench_{identifier}.log')
    main(args)

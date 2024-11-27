import dataclasses
import json
import random
import time
from typing import List, Optional, Tuple, Dict

import openai
import asyncio
import logging

from transformers import PreTrainedTokenizerBase

from vllm import LLM, SamplingParams
from vllm.engine.arg_utils import EngineArgs
from vllm.utils import FlexibleArgumentParser

try:
    from vllm.transformers_utils.tokenizer import get_tokenizer
except ImportError:
    from backend_request_func import get_tokenizer

logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(name)s - %(levelname)s - %(message)s',
    handlers=[
        logging.StreamHandler()
    ]
)

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
            api_key="API_KEY",
            base_url=endpoint,
        )

        clients[model] = client
    return clients


# Build the OpenAI endpoint dictionary from the deployment plans
def build_openai_endpoints(deployment_plan):
    endpoints = {
        "deployment1": {
            "model-1": "http://0.0.0.0:8071/v1",
        },
        "deployment2": {
            "model-1": "http://0.0.0.0:8071/v1",
        },
        "deployment3": {
            "model-1": "http://0.0.0.0:8070/v1",
            "model-2": "http://0.0.0.0:8070/v1",
            "model-3": "http://0.0.0.0:8070/v1",
            "model-4": "http://0.0.0.0:8070/v1",
            "model-5": "http://0.0.0.0:8070/v1",
            "model-6": "http://0.0.0.0:8070/v1",
            "model-7": "http://0.0.0.0:8070/v1",
            "model-8": "http://0.0.0.0:8070/v1",
            "model-9": "http://0.0.0.0:8070/v1",
            "model-10": "http://0.0.0.0:8070/v1",
            "model-11": "http://0.0.0.0:8070/v1",
            "model-12": "http://0.0.0.0:8070/v1",
            "model-13": "http://0.0.0.0:8070/v1",
            "model-14": "http://0.0.0.0:8070/v1",
            "model-15": "http://0.0.0.0:8070/v1",
            "model-16": "http://0.0.0.0:8070/v1",
            "model-17": "http://0.0.0.0:8070/v1",
            "model-18": "http://0.0.0.0:8070/v1",
            "model-19": "http://0.0.0.0:8070/v1",
            "model-20": "http://0.0.0.0:8070/v1",
            "model-21": "http://0.0.0.0:8070/v1",
            "model-22": "http://0.0.0.0:8070/v1",
            "model-23": "http://0.0.0.0:8070/v1",
            "model-24": "http://0.0.0.0:8070/v1",
            "model-25": "http://0.0.0.0:8070/v1",
            "model-26": "http://0.0.0.0:8070/v1",
            "model-27": "http://0.0.0.0:8070/v1",
            "model-28": "http://0.0.0.0:8070/v1",
            "model-29": "http://0.0.0.0:8070/v1",
            "model-30": "http://0.0.0.0:8070/v1",
            "model-31": "http://0.0.0.0:8070/v1",
            "model-32": "http://0.0.0.0:8070/v1"
        }
    }
    return endpoints[deployment_plan]

PROMPT = "You are a helpful assistant in recognizes the content of tables in markdown format. Here is a table as fellows. You need to answer my question about the table.\n# Table\n|Opening|Opening|Sl. No.|Film|Cast|Director|Music Director|Notes|\n|----|----|----|----|----|----|----|----|\n|J A N|9|1|Agni Pushpam|Jayabharathi, Kamalahasan|Jeassy|M. K. Arjunan||\n|J A N|16|2|Priyamvada|Mohan Sharma, Lakshmi, KPAC Lalitha|K. S. Sethumadhavan|V. Dakshinamoorthy||\n|J A N|23|3|Yakshagaanam|Madhu, Sheela|Sheela|M. S. Viswanathan||\n|J A N|30|4|Paalkkadal|Sheela, Sharada|T. K. Prasad|A. T. Ummer||\n|F E B|5|5|Amma|Madhu, Srividya|M. Krishnan Nair|M. K. Arjunan||\n|F E B|13|6|Appooppan|Thikkurissi Sukumaran Nair, Kamal Haasan|P. Bhaskaran|M. S. Baburaj||\n|F E B|20|7|Srishti|Chowalloor Krishnankutty, Ravi Alummoodu|K. T. Muhammad|M. S. Baburaj||\n|F E B|20|8|Vanadevatha|Prem Nazir, Madhubala|Yusufali Kechery|G. Devarajan||\n|F E B|27|9|Samasya|Madhu, Kamalahaasan|K. Thankappan|Shyam||\n|F E B|27|10|Yudhabhoomi|K. P. Ummer, Vidhubala|Crossbelt Mani|R. K. Shekhar||\n|M A R|5|11|Seemantha Puthran|Prem Nazir, Jayabharathi|A. B. Raj|M. K. Arjunan||\n|M A R|12|12|Swapnadanam|Rani Chandra, Dr. Mohandas|K. G. George|Bhaskar Chandavarkar||\n|M A R|19|13|Thulavarsham|Prem Nazir, sreedevi, Sudheer|N. Sankaran Nair|V. Dakshinamoorthy||\n|M A R|20|14|Aruthu|Kaviyoor Ponnamma, Kamalahasan|Ravi|G. Devarajan||\n|M A R|26|15|Swimming Pool|Kamal Haasan, M. G. Soman|J. Sasikumar|M. K. Arjunan||\n\n# Question\nWhat' s the content in the (1,1) cells\n"  # noqa: E501

# Asynchronous request handler
async def send_request(client, model, endpoint, prompt, output_file, concurrency):
    start_time = asyncio.get_event_loop().time()
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
            "output": output_text,
            "prompt_tokens": prompt_tokens,
            "output_tokens": output_tokens,
            "total_tokens": total_tokens,
            "latency": latency,
            "throughput": throughput,
            "concurrency": concurrency,
        }

        # Write result to JSONL file
        output_file.write(json.dumps(result) + "\n")
        output_file.flush()  # Ensure data is written immediately to the file

        logging.info(f"Request for {model} completed in {latency:.2f} seconds with throughput {throughput:.2f} tokens/s")
        return result
    except Exception as e:
        logging.error(f"Error sending request to {model} at {endpoint}: {str(e)}")
        return None

# Benchmark requests and log results into the specified file
async def benchmark_requests(clients, model_endpoints, prompts, num_requests, concurrency, output_file_path):
    models = list(model_endpoints.keys())
    total_models = len(models)
    batch_size = concurrency * total_models
    rounds = num_requests // batch_size

    with open(output_file_path, 'a', encoding='utf-8') as output_file:
        for round_idx in range(rounds):
            batch_tasks = []

            for model_index, model in enumerate(models):
                client = clients[model]
                endpoint = model_endpoints[model]

                for request_idx in range(concurrency):
                    prompt_idx = (model_index * concurrency + request_idx + round_idx * batch_size) % len(prompts)
                    prompt, _, _, _ = prompts[prompt_idx]
                    prompt = wrap_prompt_as_chat_message(prompt)
                    task = asyncio.create_task(
                        send_request(client, model, endpoint, prompt, output_file, concurrency)
                    )
                    batch_tasks.append(task)

            logging.info(f"Submitting batch {round_idx + 1} with {batch_size} requests")
            await asyncio.gather(*batch_tasks)
            logging.info(f"Completed batch {round_idx + 1} with {len(batch_tasks)} for all models.")
            
        await asyncio.gather(*batch_tasks)
        logging.info(f"All {num_requests} requests completed for deployment.")


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
    tokenizer = get_tokenizer(args.model, trust_remote_code=True)
    openai_endpoints = build_openai_endpoints(args.deployment_endpoints)
    openai_clients = build_openai_clients(openai_endpoints)
    if args.models:
        openai_endpoints = dict(list(openai_endpoints.items())[:args.models])

    num_prompts = args.num_prompts
    if args.num_prompts < len(openai_endpoints) * args.concurrency:
        num_prompts = len(openai_endpoints) * args.concurrency

    if args.workload is not None:
        workload = None
        try:
            with open(args.workload, 'r') as file:
                workload = json.load(file)
        except FileNotFoundError:
            print("The file was not found")
        except json.JSONDecodeError:
            print("The file contains invalid JSON")
        except Exception as e:
            print(f"An error occurred: {e}")      
        logging.info(f"Starting benchmark for {num_prompts} prompts with deployment {args.deployment_endpoints}")
        start_time = time.time()
        next_start = time.time() + args.interval
        for prompts in workload:
            asyncio.run(benchmark_requests(openai_clients, openai_endpoints, prompts, len(prompts), args.concurrency, args.output_file_path))
            wait_time = next_start - time.time()
            if wait_time > 0:
                time.sleep(wait_time)
            next_start += args.interval
        end_time = time.time()
        logging.info(f"Benchmark completed in {end_time - start_time:.2f} seconds")
    else:
        if args.dataset_path is not None:
            logging.info(f"Start to sample {num_prompts} prompts from {args.dataset_path}")
            input_requests = sample_sharegpt_requests(
                dataset_path=args.dataset_path,
                num_requests=num_prompts,
                tokenizer=tokenizer,
                fixed_output_len=args.sharegpt_output_len,
            )
        else:
            prompt_len = len(tokenizer(PROMPT).input_ids)
            input_requests = [(PROMPT, prompt_len, args.output_len, None)] * num_prompts

        logging.info(f"Starting benchmark for {num_prompts} prompts with deployment {args.deployment_endpoints}")
        start_time = time.time()
        asyncio.run(benchmark_requests(openai_clients, openai_endpoints, input_requests, num_prompts, args.concurrency, args.output_file_path))
        end_time = time.time()
        logging.info(f"Benchmark completed in {end_time - start_time:.2f} seconds")



if __name__ == "__main__":
    parser = FlexibleArgumentParser(description='Benchmark the performance of multi-loras.')
    parser.add_argument("--dataset-path", type=str, default=None, help="Path to the dataset.")
    parser.add_argument("--workload", type=str, default=None, help="Path to generated workload trace.")
    parser.add_argument("--sharegpt-output-len", type=int, default=None, help="Output length for each request. Overrides the output length from the ShareGPT dataset.")
    parser.add_argument('--num-prompts', type=int, default=1, help="Number of the prompts sampled from dataset")
    parser.add_argument('--concurrency', type=int, default=1, help="Number of the prompts concurrency")
    parser.add_argument('--output-len', type=int, default=10)
    parser.add_argument('--output-file-path', type=str, default="output.jsonl")
    parser.add_argument('--deployment-endpoints', type=str, required=True)
    parser.add_argument('--models', type=int, default=None)
    parser.add_argument('--interval', type=float, default=0.2, help="Interval between requests in seconds.")
    
    parser = EngineArgs.add_cli_args(parser)
    args = parser.parse_args()
    main(args)

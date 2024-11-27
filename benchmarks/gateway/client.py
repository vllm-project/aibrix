import json
import time
from typing import List, Optional, Tuple
import openai
import asyncio
import logging

from transformers import PreTrainedTokenizerBase
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
    user_message = {"role": "user", "content": prompt}
    return [user_message]


PROMPT = "You are a helpful assistant in recognizes the content of tables in markdown format. Here is a table as fellows. You need to answer my question about the table.\n# Table\n|Opening|Opening|Sl. No.|Film|Cast|Director|Music Director|Notes|\n|----|----|----|----|----|----|----|----|\n|J A N|9|1|Agni Pushpam|Jayabharathi, Kamalahasan|Jeassy|M. K. Arjunan||\n|J A N|16|2|Priyamvada|Mohan Sharma, Lakshmi, KPAC Lalitha|K. S. Sethumadhavan|V. Dakshinamoorthy||\n|J A N|23|3|Yakshagaanam|Madhu, Sheela|Sheela|M. S. Viswanathan||\n|J A N|30|4|Paalkkadal|Sheela, Sharada|T. K. Prasad|A. T. Ummer||\n|F E B|5|5|Amma|Madhu, Srividya|M. Krishnan Nair|M. K. Arjunan||\n|F E B|13|6|Appooppan|Thikkurissi Sukumaran Nair, Kamal Haasan|P. Bhaskaran|M. S. Baburaj||\n|F E B|20|7|Srishti|Chowalloor Krishnankutty, Ravi Alummoodu|K. T. Muhammad|M. S. Baburaj||\n|F E B|20|8|Vanadevatha|Prem Nazir, Madhubala|Yusufali Kechery|G. Devarajan||\n|F E B|27|9|Samasya|Madhu, Kamalahaasan|K. Thankappan|Shyam||\n|F E B|27|10|Yudhabhoomi|K. P. Ummer, Vidhubala|Crossbelt Mani|R. K. Shekhar||\n|M A R|5|11|Seemantha Puthran|Prem Nazir, Jayabharathi|A. B. Raj|M. K. Arjunan||\n|M A R|12|12|Swapnadanam|Rani Chandra, Dr. Mohandas|K. G. George|Bhaskar Chandavarkar||\n|M A R|19|13|Thulavarsham|Prem Nazir, sreedevi, Sudheer|N. Sankaran Nair|V. Dakshinamoorthy||\n|M A R|20|14|Aruthu|Kaviyoor Ponnamma, Kamalahasan|Ravi|G. Devarajan||\n|M A R|26|15|Swimming Pool|Kamal Haasan, M. G. Soman|J. Sasikumar|M. K. Arjunan||\n\n# Question\nWhat' s the content in the (1,1) cells\n"  # noqa: E501


# Asynchronous request handler
async def send_request(client, endpoint, prompt, output_file):
    start_time = asyncio.get_event_loop().time()
    try:
        response = await client.chat.completions.create(
            model='deepseek-coder-7b-instruct',
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

        logging.info(
            f"Request completed in {latency:.2f} seconds with throughput {throughput:.2f} tokens/s, response {response}")
        return result
    except Exception as e:
        logging.error(f"Error sending request to at {endpoint}: {str(e)}")
        return None


# Benchmark requests and log results into the specified file
async def benchmark_requests(endpoint, prompts, num_requests, interval, output_file_path):
    client = openai.AsyncOpenAI(
        api_key="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",
        base_url=endpoint+"/v1",
    )

    with open(output_file_path, 'a', encoding='utf-8') as output_file:
        batch_tasks = []
        for request_idx in range(num_requests):
            prompt_idx = request_idx % len(prompts)
            prompt, _, _ = prompts[prompt_idx]
            prompt = wrap_prompt_as_chat_message(prompt)

            task = asyncio.create_task(
                send_request(client, endpoint, prompt, output_file)
            )
            batch_tasks.append(task)
            logging.info(f"Submitting request {request_idx + 1}/{num_requests}, sleeping for {interval} seconds")
            await asyncio.sleep(interval)

        await asyncio.gather(*batch_tasks)
        logging.info(f"All {num_requests} requests completed for deployment.")
        
async def benchmark_workload(endpoint, workload, interval, output_file_path):
    client = openai.AsyncOpenAI(
        api_key="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",
        base_url=endpoint+"/v1",
    )

    with open(output_file_path, 'a', encoding='utf-8') as output_file:
        batch_tasks = []
        next_start = time.time() + interval
        for request_idx, prompts in enumerate(workload):
            wrapped_prompts = [wrap_prompt_as_chat_message(prompt) for (prompt, _, _, _,) in prompts]
            for wrapper_prompt in wrapped_prompts:
                task = asyncio.create_task(
                    send_request(client, endpoint, wrapper_prompt, output_file)
                )
                batch_tasks.append(task)
            wait_time = next_start - time.time()
            logging.info(f"Submitting request batch {request_idx}, sleeping for {wait_time} seconds")
            if wait_time > 0:
                time.sleep(wait_time)
            next_start += interval
            await asyncio.sleep(interval)

        await asyncio.gather(*batch_tasks)
        logging.info(f"All requests completed for deployment.")


def sample_sharegpt_requests(
        dataset_path: str,
        num_requests: int,
        tokenizer: PreTrainedTokenizerBase,
        fixed_output_len: Optional[int] = None,
) -> list[tuple[str, int, int]]:
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
        filtered_dataset.append((prompt, prompt_len, output_len))

    return filtered_dataset


def main(args):
    tokenizer = get_tokenizer(args.model, trust_remote_code=True)
    num_prompts = args.num_prompts
    if args.workload is not None:
        try:
            with open(args.workload, 'r') as file:
                workload = json.load(file)
        except FileNotFoundError:
            print("The file was not found")
        except json.JSONDecodeError:
            print("The file contains invalid JSON")
        except Exception as e:
            print(f"An error occurred: {e}")      
        logging.info(f"Starting benchmark for {num_prompts} prompts on endpoint {args.endpoint}")
        start_time = time.time()
        asyncio.run(benchmark_workload(args.endpoint, workload, num_prompts, args.interval, args.output_file_path))
        end_time = time.time()
        logging.info(f"Benchmark completed in {end_time - start_time:.2f} seconds")
  
    else:
        if args.dataset_path is not None:
            logging.info(f"Sampling {num_prompts} prompts from {args.dataset_path}")
            input_requests = sample_sharegpt_requests(
                dataset_path=args.dataset_path,
                num_requests=num_prompts,
                tokenizer=tokenizer,
                fixed_output_len=args.sharegpt_output_len,
            )
        else:
            prompt_len = len(tokenizer(PROMPT).input_ids)
            input_requests = [(PROMPT, prompt_len, args.output_len, None)] * num_prompts

        logging.info(f"Starting benchmark for {num_prompts} prompts on endpoint {args.endpoint}")
        start_time = time.time()
        asyncio.run(benchmark_requests(args.endpoint, input_requests, num_prompts, args.interval, args.output_file_path))
        end_time = time.time()
        logging.info(f"Benchmark completed in {end_time - start_time:.2f} seconds")


if __name__ == "__main__":
    parser = FlexibleArgumentParser(description='Benchmark the performance of multi-loras.')
    parser.add_argument("--dataset-path", type=str, default=None, help="Path to the dataset.")
    parser.add_argument("--workload", type=str, default=None, help="Path to generated workload trace.")
    parser.add_argument("--sharegpt-output-len", type=int, default=512, help="Output length for each request.")
    parser.add_argument('--num-prompts', type=int, default=1000, help="Number of the prompts sampled from dataset")
    parser.add_argument('--interval', type=float, default=0.2, help="Interval between requests in seconds.")
    parser.add_argument('--output-len', type=int, default=128)
    parser.add_argument('--output-file-path', type=str, default="output.jsonl")
    parser.add_argument('--endpoint', type=str, required=True)

    parser = EngineArgs.add_cli_args(parser)
    args = parser.parse_args()
    main(args)

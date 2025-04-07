from threading import Lock
from typing import List, Optional, Tuple
import threading
import os
import json
import time

from locust import HttpUser, task, between
from transformers import PreTrainedTokenizerBase

try:
    from vllm.transformers_utils.tokenizer import get_tokenizer
except ImportError:
    from backend_request_func import get_tokenizer


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


class BenchmarkUser(HttpUser):
    prompts: list[str]
    model: str
    routing_strategy: str
    num_requests: int
    index_lock: Lock
    request_index: int
    output_file: str

    wait_time = between(1, 3)  # Adjust as needed

    def on_start(self):
        self.model = os.getenv("MODEL", "llama2-70b")
        self.routing_strategy = os.getenv("ROUTING_STRATEGY", "")
        self.output_file = os.getenv("OUTPUT_FILE", "result.jsonl")
        self.num_requests = int(os.getenv("NUM_REQUESTS", 20))
        self.request_index = 0  # Initialize request index
        self.index_lock = threading.Lock()  # Initialize a lock for thread safety
        tokenizer = get_tokenizer('deepseek-ai/deepseek-coder-7b-instruct', trust_remote_code=True)
        input_requests = sample_sharegpt_requests(
            dataset_path='/tmp/ShareGPT_V3_unfiltered_cleaned_split.json',
            num_requests=self.num_requests,
            tokenizer=tokenizer,
            fixed_output_len=256,  # filter out too large output query which may overflow the model
        )
        self.prompts = list(map(lambda x: x[0], input_requests))

    @task
    def send_request(self):
        try:
            # Lock access to request_index to make it thread-safe
            with self.index_lock:
                prompt = self.prompts[self.request_index % len(self.prompts)]
                self.request_index += 1  # Increment request index safely

            start_time = time.time()
            headers={
                "Content-Type": "application/json",
                "Authorization": "Bearer sk-1234",
            }

            if self.routing_strategy:
                headers["routing-strategy"] = self.routing_strategy

            response = self.client.post("/v1/chat/completions", json={
                "model": self.model,
                "messages": [{"role": "user", "content": prompt}],
                "temperature": 0.0
            }, headers=headers)

            # Convert response text to JSON
            response_json = response.json()

            # Calculate latency and metrics
            latency = time.time() - start_time
            prompt_tokens = response_json["usage"]["prompt_tokens"]
            output_tokens = response_json["usage"]["completion_tokens"]
            total_tokens = response_json["usage"]["total_tokens"]
            throughput = output_tokens / latency
            output_text = response_json["choices"][0]["message"]["content"]

            # Prepare result to write to file
            result = {
                "model": self.model,
                "prompt": prompt,
                "output": output_text,
                "prompt_tokens": prompt_tokens,
                "output_tokens": output_tokens,
                "total_tokens": total_tokens,
                "latency": latency,
                "throughput": throughput,
            }

            # Write result to a file after each request
            with open(self.output_file, "a") as outfile:
                json.dump(result, outfile)
                outfile.write("\n")  # Newline for JSONL format
            print(f"Response: {output_text}\n")

            # # Optional: Stop after reaching the desired number of requests
            # if self.request_index >= self.num_requests:
            #     self.environment.runner.quit()  # Stop Locust when limit is reached

        except Exception as e:
            print(f"Error: {str(e)}")

import time
import click
from openai import OpenAI
from tqdm import tqdm
from tabulate import tabulate
from termcolor import colored
import os
import json
import asyncio
from typing import AsyncGenerator, List, Optional, Tuple, Dict


# Define the list of models to benchmark
# select any LLM listed here: https://docs.litellm.ai/docs/providers
models = ["deepseek-coder-33b-instruct"]

# Enter LLM API keys
# https://docs.litellm.ai/docs/providers
os.environ["OPENAI_API_KEY"] = "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"

# List of questions to benchmark (replace with your questions)
questions = ["When will BerriAI IPO?", "When will LiteLLM hit $100M ARR?"]

# Enter your system prompt here
system_prompt = """
You are LiteLLMs helpful assistant
"""

async def send_request(
    question: str
) -> float:
    start_time = time.time()
                
    openai_api_key = "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"
    openai_api_base = "http://180.184.47.126:80/v1"

    client = OpenAI(
        api_key=openai_api_key,
        base_url=openai_api_base,
    )

    response = client.chat.completions.create(
        model="deepseek-coder-33b-instruct",
        messages=[
            {"role": "system", "content": system_prompt},
            {"role": "user", "content": question},
        ],
        temperature=0.7,
    )
    
    end = time.time()
    total_time = end - start_time
    
    return total_time
    

@click.command()
@click.option(
    "--system-prompt",
    default="You are a helpful assistant that can answer questions.",
    help="System prompt for the conversation.",
)
def main(system_prompt):
    # download from here: https://huggingface.co/datasets/anon8231489123/ShareGPT_Vicuna_unfiltered/blob/main/ShareGPT_V3_unfiltered_cleaned_split.json
    dataset_path = "/benchmark/ShareGPT_V3_unfiltered_cleaned_split.json"
    # Load the dataset.
    with open(dataset_path) as f:
        dataset = json.load(f)
    # Filter out the conversations with less than 2 turns.
    dataset = [data for data in dataset if len(data["conversations"]) >= 2]
    # Only keep the first two turns of each conversation.
    dataset = [
        (data["conversations"][0]["value"], data["conversations"][1]["value"])
        for data in dataset
    ]
    
    prompts = [prompt for prompt, _ in dataset]
    
    tasks: List[asyncio.Task] = []
    for prompt in prompts:
        task = asyncio.create_task(
            send_request(
                prompt
            )
        )
        tasks.append(task)
        
    results = await asyncio.gather(*tasks)
    combined_latencies = []        
    for latency in results:
        combined_latencies.append(latency)

    print(combined_latencies)
    
if __name__ == "__main__":
    main()

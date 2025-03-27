import argparse
import numpy as np
from transformers import AutoTokenizer
from typing import Dict, Any
from synthetic_prompt_generator import generate_synthetic_prompt
from util import save_dataset_jsonl

mean = 10   # Example mean
std_dev = 2  # Example standard deviation

def sample_normal(mean: int, std: int):
    sample = np.random.normal(mean, std)
    return sample
    
def generate_synthetic(args):
    num_sessions = sample_normal(args.num_sessions_mean, args.num_sessions_std)
    tokenizer = AutoTokenizer.from_pretrained(
        args.tokenizer,
        legacy=True,
        model_max_length=4096,  # Increased to handle longer prefixes
        padding_side="right",
        truncation_side="right",
        use_fast=True
    )
    flat_prompts_data = []
    for session_id in range(0, num_sessions):
        num_turns = sample_normal(args.num_turns_mean, args.num_turns_std)
        for _ in range(0, num_turns):
            prompt_length = sample_normal(args.prompt_length_mean, args.prompt_length_std)
            prompt, token_count = generate_synthetic_prompt(tokenizer, prompt_length)
            # Process the prompt as needed
            flat_prompts_data.append({
                "prompt": prompt,
                "token_count": token_count,
                "prefix_group": session_id,
                "config_id": 0
            })
    save_dataset_jsonl(flat_prompts_data, f"multiturn-sessions{args.num_sessions_mean}-turns{args.num_turns_mean}-len{args.prompt_length_mean}-dataset.jsonl")
        



if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Configure workload parameters.")
    parser.add_argument("--tokenizer", type=str, default="deepseek-ai/deepseek-llm-7b-chat", help="Name of the tokenizer.")
    parser.add_argument("--prompt-length-mean", type=int, default=100, help="Length of the prompt (mean).")
    parser.add_argument("--prompt-length-std", type=int, default=10, help="Length of the prompt (std).")
    parser.add_argument("--num-turns-mean", type=int, default=10, help="Number of turns (mean).")
    parser.add_argument("--num-turns-std", type=int, default=1, help="Number of turns (std).")
    parser.add_argument("--num-sessions-mean", type=int, default=10, help="Number of sessions (mean).")
    parser.add_argument("--num-sessions-std", type=int, default=10, help="Number of sessions (std).")
    parser.add_argument('--type', type=str, required=True, choices=['synthetic'], help='Type of data.')
    
    args = parser.parse_args()
    generate_synthetic(args)
    
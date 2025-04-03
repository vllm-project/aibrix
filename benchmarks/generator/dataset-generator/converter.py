import logging
import pandas as pd
import json
import argparse
import logging
from transformers import PreTrainedTokenizerBase
from transformers import AutoTokenizer
from util import save_dataset_jsonl



def process_dataset_trace(
        dataset_path: str,
        tokenizer: PreTrainedTokenizerBase,
        output: str
) -> pd.DataFrame:
    # Load the dataset into a DataFrame
    logging.warn(f"...Start dataframe transformation")
    with open(dataset_path, encoding='utf-8') as f:
        dataset = [json.loads(line) for line in f]
    prompts = []
    for entry in dataset:
        prompts.append({
            "prompt": entry['input'][0]['content'],
            "completion": entry['output'],
        })
    output_filename = f"output/trace-{output}"
    save_dataset_jsonl(prompts, output_filename)
    logging.warn(f"...Finished saving dataset to {output_filename}")
    
    
def process_dataset_sharegpt(
        dataset_path: str,
        tokenizer: PreTrainedTokenizerBase,
        output: str
) -> pd.DataFrame:
    # Load the dataset into a DataFrame
    logging.warn(f"...Start dataframe transformation")
    with open(dataset_path, encoding='utf-8') as f:
        dataset = json.load(f)
    sessioned_prompts = []
    for session_dict in dataset:
        session_id = session_dict["id"]
        flat_prompts_data = [conv_dict["value"] for conv_dict in session_dict["conversations"] if conv_dict["from"] == "human"]
        sessioned_prompts.append({
            "session_id": session_id,
            "prompts": flat_prompts_data,
        })
    output_filename = f"output/sharegpt-{output}"
    save_dataset_jsonl(sessioned_prompts, output_filename)
    logging.warn(f"...Finished saving dataset to {output_filename}")
    
    
if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Configure workload parameters.")
    parser.add_argument("--path", type=str, default=".", help="Dataset Path.")
    parser.add_argument("--tokenizer", type=str, default="deepseek-ai/deepseek-llm-7b-chat", help="Name of the tokenizer.")
    parser.add_argument("--output", type=str, default="prompts.jsonl", help="Output file name.")
    parser.add_argument('--type', type=str, required=True, choices=['trace','sharegpt'], help='Type of dataset consumed. Choose among: trace, sharegpt.')
    args = parser.parse_args()
    tokenizer = AutoTokenizer.from_pretrained(
        args.tokenizer, 
        legacy=True,
        model_max_length=4096,  
        padding_side="right",
        truncation_side="right",
        use_fast=True
    )
    if args.type == 'trace':
        process_dataset_trace(args.path, tokenizer, args.output)
    elif args.type == 'sharegpt':
        process_dataset_sharegpt(args.path, tokenizer, args.output)
    

        
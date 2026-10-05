import logging
import pandas as pd
import json
import logging
from generator.dataset_generator.util import save_dataset_jsonl

    
def process_dataset_trace(dataset_path: str, output: str) -> pd.DataFrame:
    logging.warning("...Start dataframe transformation")
    with open(dataset_path, encoding='utf-8') as f:
        dataset = [json.loads(line) for line in f]
    prompts = [{
        "prompt": entry['input'][0]['content'],
        "completion": entry['output'],
    } for entry in dataset]
    save_dataset_jsonl(prompts, output)
    logging.warning(f"...Finished saving dataset to {output}")
    
    
def process_dataset_sharegpt(dataset_path: str, output: str) -> pd.DataFrame:
    logging.warning("...Start dataframe transformation")
    with open(dataset_path, encoding='utf-8') as f:
        dataset = json.load(f)
    sessioned_prompts = [{
        "session_id": session["id"],
        "prompts": [conv["value"] for conv in session["conversations"] if conv["from"] == "human"]
    } for session in dataset]
    save_dataset_jsonl(sessioned_prompts, output)
    logging.warning(f"...Finished saving dataset to {output}")
    
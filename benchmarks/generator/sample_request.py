import logging
import json
import sys
import random 

import pandas as pd

from typing import Tuple, Optional, List
from transformers import PreTrainedTokenizerBase


def load_requests(
        dataset_path: str,
        tokenizer: PreTrainedTokenizerBase,
) -> pd.DataFrame:
    if "ShareGPT" in dataset_path:
        return load_sharegpt_requests(dataset_path, tokenizer)
    else:
        return load_generated_dataset(dataset_path, tokenizer)
    
def load_sharegpt_requests(
        dataset_path: str,
        tokenizer: PreTrainedTokenizerBase,
) -> pd.DataFrame:
    # Load the dataset into a DataFrame
    logging.warn(f"...Start dataframe transformation")
    with open(dataset_path, encoding='utf-8') as f:
        dataset = json.load(f)
    dataset = [
        (data["conversations"][0]["value"], data["conversations"][1]["value"])
        for data in dataset if len(data["conversations"]) >= 2
    ]
    df = pd.DataFrame(dataset, columns=["prompt", "completion"])
    # Tokenize and calculate lengths
    df["prompt_len"] = df["prompt"].apply(lambda x: len(tokenizer(x).input_ids))
    df["completion_len"] = df["completion"].apply(lambda x: len(tokenizer(x).input_ids))
    logging.warn(f"...Complete dataframe transformation")
    return df

def load_generated_dataset(
        dataset_path: str,
        tokenizer: PreTrainedTokenizerBase,
) -> pd.DataFrame:
    # Load the dataset into a DataFrame
    with open(dataset_path, encoding='utf-8') as f:
        dataset = [json.loads(line) for line in f]
    # Create a DataFrame with the desired columns
    logging.warn(f"...Start dataframe transformation")
    df = pd.DataFrame({
        'prompt': [entry['input'][0]['content'] for entry in dataset],
        'completion': [entry['output'] for entry in dataset],
        'prompt_len': [entry['prompt_tokens'] for entry in dataset],
        'completion_len': [entry['output_tokens'] for entry in dataset]
    })
    logging.warn(f"...Complete dataframe transformation")
    return df

def sample_requests_len_range(
        df: pd.DataFrame,
        num_requests: int,
        input_lens: List[int],
        output_lens: List[int],
        initial_err_perc: Optional[float] = 0.5,
        err_step: float = 0.05,
        model: str = None,
) -> List[Tuple[str, int, int, None]]:
    filtered_results = []

    # Relaxation mechanism
    for i in range(num_requests):
        input_len = input_lens[i]
        output_len = output_lens[i]
        err_perc = initial_err_perc

        while err_perc <= 1:
            input_range = range(0, sys.maxsize)
            output_range = range(0, sys.maxsize)
            if input_len is not None:
                input_range = (int(input_len * (1 - err_perc)), int(input_len * (1 + err_perc)))
            else:
                input_range = (0, sys.maxsize)
            if output_len is not None:
                output_range = (int(output_len * (1 - err_perc)), int(output_len * (1 + err_perc))) 
            else:
                output_range = (0, sys.maxsize)
            filtered = df[
                (df["prompt_len"] >= input_range[0]) &
                (df["prompt_len"] <= input_range[1]) &
                (df["completion_len"] >= output_range[0]) &
                (df["completion_len"] <= output_range[1])
                ]
            if not filtered.empty:
                # Select the first match or random sample
                total_rows = len(filtered)
                sample = filtered.iloc[random.randint(0, total_rows - 1)] 
                filtered_results.append({"prompt": sample["prompt"],
                                         "model": model,
                                         "prompt_length": sample["prompt_len"],
                                         "output_length": sample["completion_len"]})
                break  # Stop relaxing for this request once a match is found

            # Reduce err_perc for next iteration
            logging.debug(f"Relax err_perc {err_perc} by {err_step} new err_perc {err_perc + err_step} input_range {input_range} output_range {output_range}")
            err_perc += err_step

        if err_perc >= 1:
            logging.warn(f"No match found for request {i + 1} even after relaxing err_perc to {err_perc} fallback to random")
            total_rows = len(df)
            sample = df.iloc[random.randint(0, total_rows - 1)] 
            filtered_results.append({"prompt": sample["prompt"],
                                     "model": model,
                                     "prompt_length": sample["prompt_len"],
                                     "output_length": sample["completion_len"]})

    return filtered_results


def sample_requests_all(
        df: pd.DataFrame,
        start_idx: int,
        qps: int,
        model: None,
) -> List[Tuple[str, int, int, None]]:
    results = []

    # Relaxation mechanism
    end_idx = min(start_idx + qps, len(df))
    for i in  range(start_idx, end_idx):
        print(f"start_idx {start_idx} end_idx {end_idx} i {i} len {len(df)} ")
        row = df.iloc[i]
        results.append({"prompt": row["prompt"],
                        "model": model,
                        "prompt_length": row["prompt_len"],
                        "output_length": row["completion_len"]})

    return results
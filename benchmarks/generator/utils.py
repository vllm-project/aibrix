import logging
import json
import os

import numpy as np
import pandas as pd
import matplotlib.pyplot as plt

from typing import Tuple, Optional, List, Union
from transformers import (AutoTokenizer, PreTrainedTokenizer, PreTrainedTokenizerBase,
                          PreTrainedTokenizerFast)

def make_serializable(data):
    """Recursively convert data into JSON serializable types."""
    if isinstance(data, list):
        return [make_serializable(item) for item in data]
    elif isinstance(data, tuple):
        return tuple(make_serializable(item) for item in data)
    elif isinstance(data, dict):
        return {key: make_serializable(value) for key, value in data.items()}
    elif isinstance(data, (np.integer, np.int64)):  # Convert NumPy int types to int
        return int(data)
    elif isinstance(data, (np.floating, np.float64)):  # Convert NumPy float types to float
        return float(data)
    else:
        return data
    
def sample_sharegpt_requests(
        dataset_path: str,
        num_requests: int,
        tokenizer: Optional[PreTrainedTokenizerBase] = None,
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
        if tokenizer is not None:
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
        else:
            filtered_dataset.append((prompt, -1, -1, None))

    return filtered_dataset

def load_sharegpt_requests(
    dataset_path: str,
    tokenizer: PreTrainedTokenizerBase,
    ) -> pd.DataFrame:
    # Load the dataset into a DataFrame
    with open(dataset_path, encoding='utf-8') as f:
        dataset = json.load(f)
    dataset = [
        (data["conversations"][0]["value"], data["conversations"][1]["value"])
        for data in dataset if len(data["conversations"]) >= 2
    ]
    df = pd.DataFrame(dataset, columns=["prompt", "completion"])
    logging.INFO(f"...Start dataframe transformation")
    # Tokenize and calculate lengths
    df["prompt_len"] = df["prompt"].apply(lambda x: len(tokenizer(x).input_ids))
    df["completion_len"] = df["completion"].apply(lambda x: len(tokenizer(x).input_ids))
    logging.INFO(f"...Complete dataframe transformation")
    return df
    

def sample_sharegpt_requests_len_range(
    df: pd.DataFrame,
    num_requests: int,
    input_lens: List[int],
    output_lens: List[int],
    initial_err_perc: Optional[float] = 0.5,
    err_step: float = 0.05
) -> List[Tuple[str, int, int, None]]:
    filtered_results = []

    # Relaxation mechanism
    for i in range(num_requests):
        input_len = input_lens[i]
        output_len = output_lens[i]
        err_perc = initial_err_perc

        while err_perc >= 0:
            input_range = (int(input_len * err_perc), int(input_len * (1 + err_perc)))
            output_range = (int(output_len * err_perc), int(output_len * (1 + err_perc)))

            filtered = df[
                (df["prompt_len"] >= input_range[0]) & 
                (df["prompt_len"] <= input_range[1]) &
                (df["completion_len"] >= output_range[0]) & 
                (df["completion_len"] <= output_range[1])
            ]

            if not filtered.empty:
                # Select the first match or random sample
                sample = filtered.iloc[0]  # Or filtered.sample(1) for random
                filtered_results.append((sample["prompt"], sample["prompt_len"], sample["completion_len"], None))
                break  # Stop relaxing for this request once a match is found

            # Reduce err_perc for next iteration
            logging.warn(f"Relax err_perc {err_perc} by {err_step}")
            err_perc -= err_step

        if err_perc < 0:
            raise Exception(f"No match found for request {i + 1} even after relaxing err_perc to 0")

    logging.info(f"Successfully found {len(filtered_results)} requests")
    return filtered_results


def get_tokenizer(
    pretrained_model_name_or_path: str, trust_remote_code: bool
) -> Union[PreTrainedTokenizer, PreTrainedTokenizerFast]:
    return AutoTokenizer.from_pretrained(pretrained_model_name_or_path,
                                         trust_remote_code=trust_remote_code)
    
def plot_workload(workload_dict, interval_sec, output_file: str = None):
    """
    Plots the concurrency (item length) of the generated workload.

    Args:
        workload_dict (dict): A dictionary where the keys are workload names (labels) and the values are lists of lists representing the workload.
        interval_sec (int):
    """
    fig, ax = plt.subplots()
    for workload_name, workload in workload_dict.items():
        concurrency_values = [len(item) for item in workload]
        ax.plot(np.arange(len(concurrency_values)) * interval_sec, concurrency_values, label=workload_name)

    ax.set_ylim(0,)
    plt.xlabel('Time (Sec)')
    plt.ylabel('Concurrency')
    plt.title('Workload Concurrency')
    plt.legend()
    if output_file is None:
        plt.show()
    else:
        os.makedirs(os.path.dirname(output_file), exist_ok=True)
        plt.savefig(output_file)
        logging.info(f'Saved workload plot to {output_file}')
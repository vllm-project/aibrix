import logging
import json
import os

import numpy as np
import matplotlib.pyplot as plt

from typing import List, Union, Any, Optional
from transformers import (AutoTokenizer, PreTrainedTokenizer, 
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
    
def get_tokenizer(
    pretrained_model_name_or_path: str, trust_remote_code: bool
) -> Union[PreTrainedTokenizer, PreTrainedTokenizerFast]:
    return AutoTokenizer.from_pretrained(pretrained_model_name_or_path,
                                         trust_remote_code=trust_remote_code)
    
def plot_workload(workload_dict, interval_ms, output_file: str = None):
    """
    Plots the concurrency (item length) of the generated workload.

    Args:
        workload_dict (dict): A dictionary where the keys are workload names (labels) and the values are lists of lists representing the workload.
        interval_ms (int): Interval in milliseconds. 
    """
    fig, ax = plt.subplots()
    for workload_name, workload in workload_dict.items():
        concurrency_values = [len(item) for (_, item) in workload]
        ax.plot(np.arange(len(concurrency_values)) * interval_ms, concurrency_values, label=workload_name)

    ax.set_ylim(0,)
    plt.xlabel('Time (ms)')
    plt.ylabel('Concurrency')
    plt.title('Workload Concurrency')
    plt.legend()
    if output_file is None:
        plt.show()
    else:
        os.makedirs(os.path.dirname(output_file), exist_ok=True)
        plt.savefig(output_file)
        logging.warn(f'Saved workload plot to {output_file}')
        
def save_workload(load_struct: List[Any], 
                  output_path: str, 
                  use_jsonl: Optional[bool] = False):
    if use_jsonl:
        with open(output_path + ".jsonl", "w") as file:
            for row in load_struct:
                json_line = json.dumps(row)  # Convert list to JSON string
                file.write(json_line + "\n")
                logging.warn(f'Saved workload file to {output_path + ".jsonl"}')
    else:
        with open(output_path + ".json", 'w') as file:
            json.dump(load_struct, file, indent=4)
            logging.warn(f'Saved workload file to {output_path + ".json"}')
            
def load_workload(load_struct: List[Any], 
                  input_path: str, 
                  use_jsonl: Optional[bool] = False) -> List[Any]:
    load_struct = None
    if use_jsonl:
        with open(input_path, "r") as file:
            load_struct = [json.loads(line) for line in file]
    else:
        with open(input_path, "r") as file:
            load_struct = json.load(file)
    return load_struct
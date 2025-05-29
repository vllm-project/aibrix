import logging
import json
import sys
import random 

import pandas as pd
import numpy as np

from typing import Tuple, Optional, List, Dict, Any
from transformers import PreTrainedTokenizerBase


def load_requests(
        dataset_path: str,
        tokenizer: PreTrainedTokenizerBase,
) -> pd.DataFrame:
    with open(dataset_path, encoding='utf-8') as f:
        dataset = [json.loads(line) for line in f]
        if "session_id" in dataset[0]:
            return _load_sessioned_dataset(dataset, tokenizer)
        else:
            return _load_plain_dataset(dataset, tokenizer)
    
    
def _load_sessioned_dataset(
    dataset: List[Dict[str, Any]],
    tokenizer: PreTrainedTokenizerBase,
) -> pd.DataFrame:
    session_dict = {}

    for entry in dataset:
        if "prompts" not in entry or len(entry["prompts"]) == 0:
            continue
        session_id = entry.get("session_id")
        if session_id not in session_dict:
            session_dict[session_id] = {
                "session_id": session_id,
                "prompts": [],
                "completions": [],
                "prompt_lens": [],
                "completion_lens": [],
            }

        completions = entry.get("completions", [])

        for i, prompt in enumerate(entry.get("prompts", [])):
            completion = completions[i] if i < len(completions) else None
            
            session_dict[session_id]["prompts"].append(prompt)
            session_dict[session_id]["completions"].append(completion)
            session_dict[session_id]["prompt_lens"].append(len(tokenizer(prompt).input_ids))
            session_dict[session_id]["completion_lens"].append(len(completion) if completion else None)

    df = pd.DataFrame(session_dict.values())  # Convert structured dict to DataFrame
    logging.warning("...Complete structured sessioned dataframe transformation")
    return df

def _load_plain_dataset(
    dataset: List[Dict[str, Any]],
    tokenizer: PreTrainedTokenizerBase,
) -> pd.DataFrame:
    df = pd.DataFrame(
        {
            'prompt': entry['prompt'] ,
            'completion': entry['completion'] if 'completion' in entry else None,
            'prompt_len': len(tokenizer(entry['prompt']).input_ids),
            'completion_len': len(tokenizer(entry['completion']).input_ids) if 'completion' in entry else None,
        }
        for entry in dataset if entry['prompt'] is not None
    )
    logging.warn(f"...Complete sessioned dataframe transformation")
    return df

class RequestFinder:
    def __init__(self, df: pd.DataFrame):
        self.df = df
        self.sampled_index = list(range(0, len(self.df)))
        random.shuffle(self.sampled_index)
        self.cur_sampled = 0
     
    def find_requests_max_session(
            self,
            num_requests: int,
            max_concurrent_session: int,
    ):
        if "session_id" not in self.df.columns:
            raise NotImplementedError(f"find_requests_max_session only supports sessioned dataset")
        filtered_results = []
        for _ in range(num_requests):
            if self.df.empty:
                return filtered_results
            total_rows = len(self.df)
            range_search = min(total_rows, max_concurrent_session)
            logging.debug(f"search range {range_search} total_rows {total_rows}")
            sample_session = self.df.iloc[random.randint(0, range_search - 1)] 
            sample_idx = sample_session.name
            filtered_results.append({"prompt": sample_session["prompts"].pop(0),
                                    "prompt_length": sample_session["prompt_lens"].pop(0),
                                    "output_length": sample_session["completion_lens"].pop(0),
                                    "session_id": sample_session["session_id"]})
            if len(sample_session["prompts"]) == 0:
                self.df.drop(index=sample_idx, inplace=True)  # Remove the selected row
                self.df.reset_index(drop=True, inplace=True)  # Reset index to avoid issues
            break    
        return filtered_results    
    
    def find_requests_len_range(
        self, 
        num_requests: int,
        input_lens: List[int],
        output_lens: List[int],
        initial_err_perc: Optional[float] = 0.5,
        err_step: float = 0.05,
        repeating: bool = True
    ):
        if "prompt" in self.df.columns:
            return self._find_requests_plain_len_range(num_requests, input_lens, output_lens, initial_err_perc, err_step, repeating)
        else:
            return self._find_requests_sessioned_len_range(num_requests, input_lens, output_lens, initial_err_perc, err_step)
    

    def _find_requests_plain_len_range(
            self,
            num_requests: int,
            input_lens: List[int],
            output_lens: List[int],
            initial_err_perc: Optional[float] = 0.5,
            err_step: float = 0.05,
            repeating: bool = True,
    ) -> List[Tuple[str, int, int, None]]:
        filtered_results = []
        # Relaxation mechanism
        for i in range(num_requests):
            if self.df.empty:
                return filtered_results
            input_len = input_lens[i]
            output_len = output_lens[i]
            err_perc = initial_err_perc
            if input_len is None and output_len is None:
                next_idx = 0
                if repeating:
                    next_idx = self.sampled_index[self.cur_sampled]
                    self.cur_sampled += 1
                    if self.cur_sampled >= len(self.sampled_index):
                        self.cur_sampled = 0
                        random.shuffle(self.sampled_index)
                    sample = self.df.iloc[next_idx] 
                else:
                    sample = self.df.iloc[random.randint(0, len(self.df) - 1)]
                sample_idx = sample.name
                filtered_results.append({"prompt": sample["prompt"],
                                        "prompt_length": sample["prompt_len"],
                                        "output_length": sample["completion_len"]})
                if not repeating:
                    self.df.drop(index=sample_idx, inplace=True)  # Remove the selected row
                    self.df.reset_index(drop=True, inplace=True)  # Reset index to avoid issues
            else:
                while err_perc <= 1:
                    input_range = (int(input_len * (1 - err_perc)), int(input_len * (1 + err_perc))) if input_len else (0, sys.maxsize)
                    output_range = (int(output_len * (1 - err_perc)), int(output_len * (1 + err_perc))) if output_len else (0, sys.maxsize)
                    filtered = self.df[
                        (self.df["prompt_len"] >= input_range[0]) &
                        (self.df["prompt_len"] <= input_range[1]) &
                        ((pd.isna(self.df["completion_len"])) |
                        ((self.df["completion_len"] >= output_range[0]) &
                        (self.df["completion_len"] <= output_range[1])))
                        ]
                    if not filtered.empty:
                        # Select the first match or random sample
                        total_rows = len(filtered)
                        sample = filtered.iloc[random.randint(0, total_rows - 1)] 
                        sample_idx = sample.name
                        filtered_results.append({"prompt": sample["prompt"],
                                                "prompt_length": sample["prompt_len"],
                                                "output_length": sample["completion_len"]})
                        if not repeating:
                            self.df.drop(index=sample_idx, inplace=True)  # Remove the selected row
                            self.df.reset_index(drop=True, inplace=True)  # Reset index to avoid issues
                        break  # Stop relaxing for this request once a match is found

                    # Reduce err_perc for next iteration
                    logging.debug(f"Relax err_perc {err_perc} by {err_step} new err_perc {err_perc + err_step} input_range {input_range} output_range {output_range}")
                    err_perc += err_step

                if err_perc >= 1: 
                    # Convert None to NaN using the recommended approach
                    completion_len = self.df["completion_len"].replace({None: np.nan}).infer_objects(copy=False)
                    completion_diff_square = np.power((completion_len - output_len), 2)
                    completion_diff_square = np.nan_to_num(completion_diff_square, nan=0.0)
                    self.df["distance"] = np.sqrt(np.power((self.df["prompt_len"] - input_len), 2) + completion_diff_square)
                    closest_sample = self.df.nsmallest(1, "distance").iloc[0]
                    closest_input, closest_output = closest_sample["prompt_len"], closest_sample["completion_len"]
                    sample_idx = closest_sample.name
                    filtered_results.append({"prompt": closest_sample["prompt"],
                                            "prompt_length": closest_sample["prompt_len"],
                                            "output_length": closest_sample["completion_len"]})
                    if not repeating:
                        self.df.drop(index=sample_idx, inplace=True)  # Remove the selected row
                        self.df.reset_index(drop=True, inplace=True)  # Reset index to avoid issues
                    logging.warn(f"No exact match found for request {i + 1}, target input/output lengths {input_len}/{output_len}, use closest QA pair input {closest_input} output {closest_output}.")

        return filtered_results

    def _find_requests_sessioned_len_range(
        self,
        num_requests: int,
        input_lens: List[int],
        output_lens: List[int],
        initial_err_perc: Optional[float] = 0.5,
        err_step: float = 0.05,
    ):
        filtered_results = []
        # Relaxation mechanism
        for i in range(num_requests):
            if self.df.empty:
                return filtered_results
            input_len = input_lens[i]
            output_len = output_lens[i]
            err_perc = initial_err_perc
            while err_perc <= 1:
                input_range = (int(input_len * (1 - err_perc)), int(input_len * (1 + err_perc))) if input_len else (0, sys.maxsize)
                output_range = (int(output_len * (1 - err_perc)), int(output_len * (1 + err_perc))) if output_len else (0, sys.maxsize)

                # Print them for inspection
                filtered_sessions = self.df[
                    (self.df["prompt_lens"].apply(lambda x: x[0]) >= input_range[0]) &
                    (self.df["prompt_lens"].apply(lambda x: x[0]) <= input_range[1]) &
                    (
                        self.df["completion_lens"].apply(lambda x: pd.isna(x[0])) |
                        (
                            self.df["completion_lens"].apply(lambda x: x[0]) >= output_range[0]
                        ) & (
                            self.df["completion_lens"].apply(lambda x: x[0]) <= output_range[1]
                        )
                    )
                ]
                if not filtered_sessions.empty:
                    # Select the first match or random sample
                    total_sessions = len(filtered_sessions)
                    sample_session = filtered_sessions.iloc[random.randint(0, total_sessions - 1)]
                    sample_idx = sample_session.name
                    filtered_results.append({"prompt": sample_session["prompts"].pop(0),
                                            "prompt_length": sample_session["prompt_lens"].pop(0),
                                            "output_length": sample_session["completion_lens"].pop(0),
                                            "session_id": sample_session["session_id"]})
                    if len(sample_session["prompts"]) == 0:
                        self.df.drop(index=sample_idx, inplace=True)  # Remove the selected row
                        self.df.reset_index(drop=True, inplace=True)  # Reset index to avoid issues
                    break
                logging.debug(f"Relax err_perc {err_perc} by {err_step} new err_perc {err_perc + err_step} input_range {input_range} output_range {output_range}")
                err_perc += err_step
            
            if err_perc >= 1:
                self.df["distance"] = np.sqrt(
                    (self.df["prompt_lens"].apply(lambda x: x[0]) - input_len) ** 2 + 
                    (self.df["completion_lens"].apply(lambda x: output_len if pd.isna(x[0]) else x[0]) - output_len) ** 2
                )
                closest_session = self.df.nsmallest(1, "distance").iloc[0]
                closest_input, closest_output = closest_session["prompt_len"][0], closest_session["completion_lens"][0]
                sample_idx = closest_session.name
                filtered_results.append({"prompt": sample_session["prompts"].pop(0),
                                        "prompt_length": sample_session["prompt_lens"].pop(0),
                                        "output_length": sample_session["completion_lens"].pop(0),
                                        "session_id": sample_session["session_id"]})
                if len(sample_session["prompts"] == 0):
                    self.df.drop(index=sample_idx, inplace=True)  # Remove the selected row
                    self.df.reset_index(drop=True, inplace=True)  # Reset index to avoid issues
                logging.warn(f"No exact match found for request {i + 1}, target input/output lengths {input_len}/{output_len}, use closest QA pair input {closest_input} output {closest_output}.")
        return filtered_results
  
    def sample_requests_all(
            self,
            start_idx: int,
            qps: int,
    ) -> List[Tuple[str, int, int, None]]:
        results = []

        # Relaxation mechanism
        end_idx = min(start_idx + qps, len(self.df))
        for i in  range(start_idx, end_idx):
            row = self.df.iloc[i]
            results.append({"prompt": row["prompt"],
                            "prompt_length": row["prompt_len"],
                            "output_length": row["completion_len"]})

        return results
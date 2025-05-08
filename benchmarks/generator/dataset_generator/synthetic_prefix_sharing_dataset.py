#!/usr/bin/env python3
import random
import json
import logging
import numpy as np
import argparse

from tqdm import tqdm
from transformers import AutoTokenizer
from scipy.stats import truncnorm
from generator.dataset_generator.synthetic_prompt import generate_synthetic_prompt, adjust_prompt_length
from generator.dataset_generator.util import save_workload_jsonl, save_dataset_jsonl


def generate_unique_prefix(base_text, index):
    return str(index) + base_text[len(str(index)):]
    

def prepare_prompts(tokenizer, config):
    """
    Prepare prompts based on the provided configuration
    
    Args:
        tokenizer: The tokenizer to use
        config: Dictionary with prefix_length, suffix_length, num_samples_per_prefix, num_prefix
        
    Returns:
        Tuple of (all_prompts, tot_input_len, prompts_token_counts)
    """
    
    prompt_length_mean = config["prompt_length"]
    prompt_length_std = config["prompt_length_std"]
    shared_proportion_mean = config["shared_proportion"]
    shared_proportion_std = config["shared_proportion_std"]
    num_samples_per_prefix = config["num_samples_per_prefix"]
    num_prefix = config["num_prefix"]
    
    # Generate a base prefix using realistic content
    tot_input_len = 0
    all_prompts = []
    prompts_token_counts = []  # Store token counts for each prompt
    
    for i in range(num_prefix):
        shared_length_mean = int(prompt_length_mean * shared_proportion_mean)
        base_prefix, token_count = generate_synthetic_prompt(tokenizer, shared_length_mean)
        unique_prefix = generate_unique_prefix(base_prefix, i)
        prompt_list = []
        token_count_list = []
        
        for _ in tqdm(range(num_samples_per_prefix), desc=f"Preparing prompts for config {config['id']} Prefix {i}"):
            # Generate a realistic suffix
            
            # Function to sample L from a normal distribution with truncation at 1 (to ensure L > 0)
            def sample_L(mu_L, sigma_L):
                L = int(np.round(truncnorm.rvs((1 - mu_L) / sigma_L, np.inf, loc=mu_L, scale=sigma_L)))
                return max(1, L)  # Ensure L is at least 1

            # Function to sample P from a truncated normal distribution ensuring 0 <= P <= L
            def sample_P(mu_P, sigma_P, L):
                lower, upper = 0, L
                P = int(np.round(truncnorm.rvs((lower - mu_P) / sigma_P, (upper - mu_P) / sigma_P, loc=mu_P, scale=sigma_P)))
                return max(0, min(P, L))  # Ensure P is within [0, L]
            
            sampled_prompt_length = int(sample_L(prompt_length_mean, prompt_length_std))
            sampled_shared_length = int(sample_P(
                shared_proportion_mean  * sampled_prompt_length, 
                shared_proportion_std * sampled_prompt_length,
                sampled_prompt_length))
            
            target_prefix_length = sampled_shared_length
            target_suffix_length = sampled_prompt_length - target_prefix_length
            
            adjusted_prefix = adjust_prompt_length(tokenizer, unique_prefix, target_prefix_length)
            suffix, suffix_len = generate_synthetic_prompt(tokenizer, target_suffix_length)
            adjusted_suffix = adjust_prompt_length(tokenizer, suffix, target_suffix_length)
            
            prompt = adjusted_prefix + " " + adjusted_suffix
            
            # Count tokens
            token_count = len(tokenizer.encode(prompt))
            logging.debug(f"generate_synthetic_prompt num_prefix {num_prefix} config {config['id']} sampled_prompt_length {sampled_prompt_length} sampled_shared_length {sampled_shared_length} target_prefix_length {target_prefix_length} suffix_length {target_suffix_length} num_samples_per_prefix {num_samples_per_prefix} total token count {token_count}")
            
            tot_input_len += token_count
            
            prompt_list.append(prompt)
            token_count_list.append(token_count)
        
        all_prompts.append(prompt_list)
        prompts_token_counts.append(token_count_list)
    
    return all_prompts, tot_input_len, prompts_token_counts

def calculate_prefix_proportion(prefix_length, suffix_length):
    """
    Calculate the proportion of the prompt that is prefix.
    
    Prefix proportion = prefix_length / (prefix_length + suffix_length)
    
    Args:
        prefix_length: Length of the prefix in tokens
        suffix_length: Length of the suffix in tokens
        
    Returns:
        Prefix proportion (float)
    """
    return prefix_length / (prefix_length + suffix_length)

def calculate_average_prefix_sharing_ratio(tokenizer, all_prompts):
    """
    Calculate the average prefix sharing ratio across all prompt lists.
    
    For each list of prompts:
        sharing_ratio = (token length of common prefix) / (average token length of prompts in that list)

    The function returns the average of these ratios across all prompt lists.
    
    Args:
        tokenizer: The tokenizer to use
        all_prompts: List of prompt lists (e.g., List[List[str]])

    Returns:
        Average prefix sharing ratio (float)
    """
    def get_common_prefix(strs):
        if not strs:
            return ""
        shortest = min(strs, key=len)
        for i in range(len(shortest)):
            char = shortest[i]
            for other in strs:
                if other[i] != char:
                    return shortest[:i]
        return shortest

    sharing_ratios = []

    for prompt_list in all_prompts:
        if not prompt_list:
            continue
        
        common_prefix = get_common_prefix(prompt_list)
        common_prefix_token_len = len(tokenizer.encode(common_prefix))

        prompt_token_lens = [len(tokenizer.encode(p)) for p in prompt_list]
        average_prompt_token_len = sum(prompt_token_lens) / len(prompt_token_lens)

        if average_prompt_token_len == 0:
            continue  # Avoid division by zero

        sharing_ratio = common_prefix_token_len / average_prompt_token_len
        sharing_ratios.append(sharing_ratio)

    if not sharing_ratios:
        return 0.0

    return sum(sharing_ratios) / len(sharing_ratios)

def calculate_prefix_sharing_ratio(tokenizer, all_prompts, prompts_token_counts, prefix_length):
    """
    Calculate the prefix sharing ratio in the entire workload based on token counts
    
    Prefix sharing ratio = (total tokens in shared prefixes) / (total tokens in all prompts)
    
    Args:
        tokenizer: The tokenizer to use
        all_prompts: List of prompt lists
        prompts_token_counts: List of list of token counts corresponding to all_prompts
        prefix_length: Length of the prefix in tokens
        
    Returns:
        Prefix sharing ratio (float)
    """
    # Flatten the token counts
    flat_token_counts = [
        token_count 
        for token_count_list in prompts_token_counts 
        for token_count in token_count_list
    ]
    total_prompt_tokens = sum(flat_token_counts)
    
    # Count the unique prefixes
    unique_prefixes = []
    for prompt_list in all_prompts:
        if prompt_list and len(prompt_list) > 0:
            # Take first prompt from each list to get the unique prefix
            first_prompt = prompt_list[0]
            logging.warning(f"len(unique_prefixes) {len(unique_prefixes)} len(str(len(unique_prefixes))) {len(str(len(unique_prefixes)))} (len(str(len(unique_prefixes))) + prefix_length) {(len(str(len(unique_prefixes))) + prefix_length)}")
            prefix = first_prompt[:(len(str(len(unique_prefixes))) + prefix_length)]
            unique_prefixes.append(prefix)
    
    # Calculate token counts for each unique prefix
    unique_prefix_token_counts = [len(tokenizer.encode(prefix)) for prefix in unique_prefixes]
    total_shared_prefix_tokens = sum(unique_prefix_token_counts)
    
    # Calculate how many tokens would be needed if each prompt had its own prefix
    total_prefix_tokens_if_not_shared = 0
    for i, prompt_list in enumerate(all_prompts):
        prefix_token_count = unique_prefix_token_counts[i] if i < len(unique_prefix_token_counts) else 0
        total_prefix_tokens_if_not_shared += prefix_token_count * len(prompt_list)
    
    # Calculate tokens saved by sharing
    tokens_saved_by_sharing = total_prefix_tokens_if_not_shared - total_shared_prefix_tokens
    
    # Calculate sharing ratio
    sharing_ratio = tokens_saved_by_sharing / total_prompt_tokens
    
    return sharing_ratio

def generate_poisson_arrival_times(num_requests, rps, start_time=0):
    """
    Generate arrival times based on Poisson distribution
    
    Args:
        num_requests: Total number of requests
        rps: Requests per second (lambda parameter for Poisson)
        start_time: Starting timestamp (in milliseconds)
        
    Returns:
        List of timestamps in milliseconds
    """
    # For Poisson process, inter-arrival times follow exponential distribution
    # with mean = 1/lambda, where lambda = rps
    inter_arrival_times = np.random.exponential(scale=1.0/rps, size=num_requests)
    
    # Convert to cumulative times (in seconds)
    arrival_times = np.cumsum(inter_arrival_times)
    
    # Convert to millisecond timestamps and add start_time
    timestamps = [int(start_time + t * 1000) for t in arrival_times]
    
    return timestamps

def generate_workload_from_config(tokenizer, config):
    """
    Process multiple workload configurations and combine them
    
    Args:
        tokenizer: The tokenizer to use
        configs: List of workload configuration dictionaries
        
    Returns:
        Dictionary with combined workload data
    """
    all_prompts_combined = []
    all_timestamps_combined = []
    total_tokens = 0
    
    # Variables to track overall prefix sharing
    total_prompts_count = 0
    total_unique_prefixes = 0
    total_prefix_tokens = 0
    
    # Variables for overall prefix sharing calculation
    all_prompts_for_sharing = []
    all_prompts_token_counts = []
    all_prefix_lengths = []
    
    current_time = 0  # Track current time for sequential workloads
    
    # Process each configuration
    # for i, config in enumerate(configs):
        # Add an ID to the config for reference
        # config["id"] = i+1
    prefix_length = int(config["prompt_length"] * config["shared_proportion"])
    suffix_length = int(config["prompt_length"] - prefix_length)
    # Generate prompts for this config
    prompts, tokens, token_counts = prepare_prompts(tokenizer, config)
    total_tokens += tokens
    
    # Calculate prefix sharing ratio for this config
    sharing_ratio = calculate_prefix_sharing_ratio(
        tokenizer, prompts, token_counts, prefix_length
    )
    
    # Calculate prefix proportion
    prefix_proportion = calculate_prefix_proportion(
        prefix_length, suffix_length
    )
    
    # Create flattened prompt data with prefix group information
    flat_prompts_data = []
    for prefix_idx, prompt_list in enumerate(prompts):
        for j, prompt in enumerate(prompt_list):
            flat_prompts_data.append({
                "prompt": prompt,
                "token_count": token_counts[prefix_idx][j],
                "prefix_group": prefix_idx,
                "config_id": config["id"]
            })
    
    # Determine if we should randomize the order
    randomize_order = config.get("randomize_order", False)
    
    # If randomize_order is True, shuffle the prompts across different prefix groups
    if randomize_order:
        random.shuffle(flat_prompts_data)
    
    # Generate timestamps for this config
    rps = config.get("rps", 1)
    timestamps = generate_poisson_arrival_times(
        num_requests=len(flat_prompts_data),
        rps=rps,
        start_time=current_time
    )
    
    # Update current_time for next config
    if timestamps:
        current_time = max(timestamps) + 1000  # Add a 1-second gap between configs
    
    # Add timestamps to prompt data
    for j, prompt_data in enumerate(flat_prompts_data):
        prompt_data["timestamp"] = timestamps[j]
        all_prompts_combined.append(prompt_data)
    
    # Update overall prefix sharing tracking
    total_prompts_count += len(flat_prompts_data)
    
    # Store config data for overall prefix calculation
    all_prompts_for_sharing.extend(prompts)
    all_prompts_token_counts.extend(token_counts)
    all_prefix_lengths.extend([prefix_length] * len(prompts))
    
    # Store stats for this config
    total_num_req = config["num_prefix"] * config["num_samples_per_prefix"]
    total_duration = total_num_req / rps
    
    config_stat = {
        "config_id": config["id"],
        "prefix_length": prefix_length,
        "suffix_length": suffix_length,
        "num_samples_per_prefix": config["num_samples_per_prefix"],
        "num_prefix": config["num_prefix"],
        "rps": rps,
        "randomize_order": randomize_order,
        "num_requests": len(flat_prompts_data),
        "total_tokens": tokens,
        "total_duration": total_duration,
        "prefix_sharing_ratio": sharing_ratio,
        "prefix_proportion": prefix_proportion,
        "start_time": min(timestamps) if timestamps else 0,
        "end_time": max(timestamps) if timestamps else 0
    }
    
    # Calculate overall prefix sharing ratio using the same token-based method
    overall_sharing_ratio = 0
    overall_sharing_ratio = config_stat["prefix_sharing_ratio"]
    overall_prefix_proportion = config_stat["prefix_proportion"]
    
    # Sort combined data by timestamp
    all_prompts_combined.sort(key=lambda x: x["timestamp"])
    
    return {
        "prompts": all_prompts_combined,
        "stats": config_stat,
        "total_tokens": total_tokens,
        "overall_sharing_ratio": overall_sharing_ratio,
        "overall_prefix_proportion": overall_prefix_proportion
    }


def generate_dataset_from_config(tokenizer, config, num_configs):
    """
    Process multiple workload configurations and combine them
    
    Args:
        tokenizer: The tokenizer to use
        configs: List of workload configuration dictionaries
        
    Returns:
        Dictionary with combined workload data
    """
    all_prompts_combined = []
    total_tokens = 0
    # config_stats = []
    
    # Variables to track overall prefix sharing
    total_prompts_count = 0
    
    # Variables for overall prefix sharing calculation
    all_prompts_for_sharing = []
    all_prompts_token_counts = []
    all_prefix_lengths = []
    
    # Process each configuration

    # Add an ID to the config for reference
    prefix_length = int(config["prompt_length"] * config["shared_proportion"])
    suffix_length = int(config["prompt_length"] - prefix_length)
    # Generate prompts for this config
    prompts, tokens, token_counts = prepare_prompts(tokenizer, config)
    total_tokens += tokens
    
    # Calculate prefix sharing ratio for this config
    sharing_ratio = calculate_average_prefix_sharing_ratio(
        tokenizer, prompts
    )
    
    # Calculate prefix proportion
    prefix_proportion = calculate_prefix_proportion(
        prefix_length, suffix_length
    )
    
    # Create flattened prompt data with prefix group information
    sessioned_prompts = []
    total_prompts_count = 0
    for prefix_idx, prompt_list in enumerate(prompts):
        per_session_prompts = []
        for j, prompt in enumerate(prompt_list):
            all_prompts_combined.append({
                "prompt": prompt,
                "token_count": token_counts[prefix_idx][j],
                "prefix_group": prefix_idx,
                "config_id": config["id"]
            })
            per_session_prompts.append(prompt)
            total_prompts_count += 1
        session_id = num_configs * config["id"] + prefix_idx
        sessioned_prompts.append({
            "session_id": session_id,
            "prompts": per_session_prompts,
        })

    
    # Store config data for overall prefix calculation
    all_prompts_for_sharing.extend(prompts)
    all_prompts_token_counts.extend(token_counts)
    all_prefix_lengths.extend([prefix_length] * len(prompts))
    

    config_stat = {
        "config_id": config["id"],
        "prefix_length": prefix_length,
        "suffix_length": suffix_length,
        "num_samples_per_prefix": config["num_samples_per_prefix"],
        "num_prefix": config["num_prefix"],
        "rps": config['rps'],
        "num_requests": total_prompts_count,
        "total_tokens": tokens,
        "prefix_sharing_ratio": sharing_ratio,
        "prefix_proportion": prefix_proportion,
    }
    
    # Calculate overall prefix sharing ratio using the same token-based method
    overall_sharing_ratio = 0
    overall_sharing_ratio = config_stat["prefix_sharing_ratio"]
    overall_prefix_proportion = config_stat["prefix_proportion"]
    
    return {
        "prompts": all_prompts_combined,
        "stats": config_stat,
        "total_tokens": total_tokens,
        "overall_sharing_ratio": overall_sharing_ratio,
        "overall_prefix_proportion": overall_prefix_proportion
    }

def save_stats(workload_data, stats_file):
    """
    Save workload statistics to a JSON file
    
    Args:
        workload_data: Dictionary with prompts and stats
        stats_file: Output file path for stats
    """
    with open(stats_file, 'w') as f:
        json.dump({
            "config_stats": workload_data["stats"],
            "num_tokens": workload_data["total_tokens"],
            "num_requests": len(workload_data["prompts"]),
            "overall_sharing_ratio": workload_data["overall_sharing_ratio"],
            "overall_prefix_proportion": workload_data["overall_prefix_proportion"],
        }, f, indent=2)
    
    total_duration = 0
    total_num_requests = 0
    logging.info("\nConfiguration details:")
    for cfg in workload_data["stats"]:
        num_req = cfg['num_prefix'] * cfg['num_samples_per_prefix']
        duration = num_req / cfg['rps']
        total_duration += duration
        total_num_requests += num_req
        logging.info(f"Config {cfg['config_id']}:")
        logging.info(f"  - Prefix length: {cfg['prefix_length']}")
        logging.info(f"  - Suffix length: {cfg['suffix_length']}")
        logging.info(f"  - Number of requests per prefix: {cfg['num_samples_per_prefix']}")
        logging.info(f"  - Number of different prefixes: {cfg['num_prefix']}")
        logging.info(f"  - RPS: {cfg['rps']}")
        logging.info(f"  - Randomized order: {cfg['randomize_order']}")
        logging.info(f"  - Duration: {duration:.0f} seconds")
        logging.info(f"  - Number of requests {cfg['num_requests']}")
        logging.info(f"  - Prefix proportion: {cfg['prefix_proportion']*100:.2f}% (portion of each prompt that is shared)")
        logging.info(f"  - Efficiency gain: {cfg['prefix_sharing_ratio']*100:.2f}% (computational savings from prefix sharing)")
        logging.info(f"  - Time range: {int(cfg['start_time']/1000)}s to {int(cfg['end_time']/1000)}s")

    logging.info("\nWorkload Summary:")
    logging.info(f"Total number of requests: {total_num_requests}")
    logging.info(f"Total duration: {total_duration:.0f} seconds")
    logging.info(f"Total prompts: {len(workload_data['prompts'])}")
    logging.info(f"Total tokens: {workload_data['total_tokens']}")
    logging.info(f"Overall prefix proportion: {workload_data['overall_prefix_proportion']*100:.2f}% (portion of each prompt that is shared)")
    logging.info(f"Overall efficiency gain: {workload_data['overall_sharing_ratio']*100:.2f}% (computational savings from prefix sharing)")


def get_configurations(args: argparse.Namespace):
    """
    Generate configurations based on command line arguments
    
    Args:
        args: Parsed command line arguments
        
    Returns:
        List of configuration dictionaries
    """
    
    prompt_length = args.prompt_length
    prompt_length_std = args.prompt_length_std
    shared_proportion = args.shared_proportion
    shared_proportion_std = args.shared_proportion_std
    num_samples_per_prefix = args.num_samples_per_prefix
    num_prefix = args.num_prefix
    rps = args.rps
    randomize_order = args.randomize_order
    
    # Split the comma-separated values into lists
    prompt_length_list = [int(x) for x in str(prompt_length).split(",")]
    prompt_length_std_list = [int(x) for x in str(prompt_length_std).split(",")]
    shared_proportion_list = [float(x) for x in str(shared_proportion).split(",")]
    shared_proportion_std_list = [float(x) for x in str(shared_proportion_std).split(",")]
    num_samples_per_prefix_list = [int(x) for x in str(num_samples_per_prefix).split(",")]
    num_prefix_list = [int(x) for x in str(num_prefix).split(",")]
    
    if len(prompt_length_list) != args.num_configs:
        raise ValueError(f"Length of prompt_length {len(prompt_length_list)} and num_configs {args.num_configs} must be equal.")
    if len(prompt_length_std_list) != args.num_configs:
        raise ValueError(f"Length of prompt_length_std {len(prompt_length_std_list)} and num_configs {args.num_configs} must be equal.")
    if len(shared_proportion_list) != args.num_configs:
        raise ValueError(f"Length of shared_proportion {len(shared_proportion_list)} and num_configs {args.num_configs} must be equal.")
    if len(shared_proportion_std_list) != args.num_configs:
        raise ValueError(f"Length of shared_proportion_std {len(shared_proportion_std_list)} and num_configs {args.num_configs} must be equal.")
    if len(num_samples_per_prefix_list) != args.num_configs:
        raise ValueError(f"Length of num_samples_per_prefix {len(num_samples_per_prefix_list)} and num_configs {args.num_configs} must be equal.")
    if len(num_prefix_list) != args.num_configs:
        raise ValueError(f"Length of num_prefix {len(num_prefix_list)} and num_configs {args.num_configs} must be equal.")
    
    # Generate configurations based on the provided parameters
    prefix_workload_configs = []
    
    for i in range(args.num_configs):
        prefix_workload_configs.append({
            "prompt_length": prompt_length_list[i],
            "prompt_length_std": prompt_length_std_list[i],
            "shared_proportion": shared_proportion_list[i],
            "shared_proportion_std": shared_proportion_std_list[i],
            "num_samples_per_prefix": num_samples_per_prefix_list[i],
            "num_prefix": num_prefix_list[i],
            "rps": rps,
            "randomize_order": randomize_order,
        })
    logging.info(f"Generated prefix_workload_configs {prefix_workload_configs}")
    
    return prefix_workload_configs
    
def main(args):
    to_workload = args.to_workload
    app_name = args.app_name
    prefix_workload_configs = get_configurations(args)
    
    # Initialize tokenizer
    tokenizer = AutoTokenizer.from_pretrained(
        args.tokenizer, 
        legacy=True,
        model_max_length=4096,  # Increased to handle longer prefixes
        padding_side="right",
        truncation_side="right",
        use_fast=True
    )
    
    
    if to_workload:
        for i, prefix_workload_config in enumerate(prefix_workload_configs):
            rand_str = "-randomized" if prefix_workload_config["randomize_order"] else ""
            prefix_estimate = int(prefix_workload_config["prompt_length"] * prefix_workload_configs[0]["shared_proportion"])
            suffix_estimate = int(prefix_workload_config["prompt_length"] * (1 - prefix_workload_configs[0]["shared_proportion"]))
            rps = prefix_workload_configs[0]["rps"]
            base_filename = f"{app_name}-prefix-share-workload-p{prefix_estimate}-s{suffix_estimate}-rps{rps}{rand_str}"
            # Add randomization info to filename
            prefix_workload_config["id"] = i
            workload_data = generate_workload_from_config(tokenizer, prefix_workload_config)
            # Save results
            stats_file = f"{base_filename}-stats.json"
            save_workload_jsonl(workload_data, args.output)
            save_stats(workload_data, stats_file)
            logging.warning(f"Saving workload statistics to {stats_file}")
            logging.warning(f"Saving workload traces to {args.output}")
    else: # To dataset
        workload_data = []
        for i, prefix_workload_config in enumerate(prefix_workload_configs):
            rand_str = "-randomized" if prefix_workload_config["randomize_order"] else ""
            prefix_estimate = int(prefix_workload_config["prompt_length"] * prefix_workload_configs[0]["shared_proportion"])
            suffix_estimate = int(prefix_workload_config["prompt_length"] * (1 - prefix_workload_configs[0]["shared_proportion"])) 
            base_filename = f"{app_name}-prefix-share-dataset-p{prefix_estimate}-{suffix_estimate}.jsonl"
            prefix_workload_config["id"] = i
            dataset_dict = generate_dataset_from_config(tokenizer, prefix_workload_config, args.num_configs)
            logging.warning(f"Dataset statistics: {dataset_dict['stats']} total_tokens: {dataset_dict['total_tokens']} overall_sharing_ratio: {dataset_dict['overall_sharing_ratio']} overall_prefix_proportion: {dataset_dict['overall_prefix_proportion']}")
            logging.warning(f"Saving dataset to {args.output}")
            length = len(dataset_dict["prompts"][0])
            workload_data.extend(dataset_dict["prompts"])
        save_dataset_jsonl(workload_data, args.output)
        
        
    
if __name__ == "__main__":
    random.seed(0)
    np.random.seed(0)
    parser = argparse.ArgumentParser(description="Configure workload parameters.")
    parser.add_argument("--tokenizer", type=str, default="deepseek-ai/deepseek-llm-7b-chat", help="Name of the tokenizer.")
    parser.add_argument("--app-name", type=str, default="app", help="Name of the application.")
    parser.add_argument("--num-configs", type=int, default=1, help="Number of configurations.")
    parser.add_argument("--prompt-length", type=str, default="1000", help="Lengths of the prompt. Use \',\' to separate multiple configurations.")
    parser.add_argument("--prompt-length-std", type=str, default="100", help="Standard deviations of the prompt length. Use \',\' to separate multiple configurations.")
    parser.add_argument("--shared-proportion", type=str, default="0.9", help="Proportions of shared content. Use \',\' to separate multiple configurations.")
    parser.add_argument("--shared-proportion-std", type=str, default="0.01", help="Standard deviations of shared proportion. Use \',\' to separate multiple configurations.")
    parser.add_argument("--num-samples-per-prefix", type=str, default="200", help="Number of samples per prefix. Use \',\' to separate multiple configurations.")
    parser.add_argument("--num-prefix", type=str, default="10", help="Number of prefixes. Use \',\' to separate multiple configurations.")
    parser.add_argument("--rps", type=int, default=0, help="Requests per second.")
    parser.add_argument("--randomize-order", action="store_true", help="Randomize order if flag is set.")
    parser.add_argument("--to-workload", action="store_true", help="Generate workload if flag is set (needs rps to be set).")
    parser.add_argument("--output", type=str, default="output.jsonl", help="Output file name.")
    

    args = parser.parse_args()
    main(args)

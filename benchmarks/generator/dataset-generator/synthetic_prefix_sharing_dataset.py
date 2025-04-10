#!/usr/bin/env python3
import random
import json
import logging
import numpy as np
import argparse

from tqdm import tqdm
from transformers import AutoTokenizer
from scipy.stats import truncnorm
from synthetic_prompt import generate_synthetic_prompt, adjust_prompt_length
from util import save_workload_jsonl, save_dataset_jsonl


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
    
    for i in tqdm(range(num_prefix), desc=f"Preparing prompts for config {config['id']}"):
        shared_length_mean = int(prompt_length_mean * shared_proportion_mean)
        base_prefix, token_count = generate_synthetic_prompt(tokenizer, shared_length_mean)
        unique_prefix = generate_unique_prefix(base_prefix, i)
        prompt_list = []
        token_count_list = []
        
        for _ in range(num_samples_per_prefix):
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
            logging.info(f"generate_synthetic_prompt num_prefix {num_prefix} config {config['id']} sampled_prompt_length {sampled_prompt_length} sampled_shared_length {sampled_shared_length} target_prefix_length {target_prefix_length} suffix_length {target_suffix_length} num_samples_per_prefix {num_samples_per_prefix} total token count {token_count}")
            
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
            logging.debug(f"len(unique_prefixes) {len(unique_prefixes)} len(str(len(unique_prefixes))) {len(str(len(unique_prefixes)))} (len(str(len(unique_prefixes))) + prefix_length) {(len(str(len(unique_prefixes))) + prefix_length)}")
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
    # if len(configs) == 1:
        # If there's only one config, use its sharing ratio
    overall_sharing_ratio = config_stat["prefix_sharing_ratio"]
    overall_prefix_proportion = config_stat["prefix_proportion"]
    # else:
    #     # For multiple configs, calculate an overall ratio based on all prompts
    #     # This is more complex and would need special handling for different prefix lengths
    #     # For now, we'll use a weighted average based on token counts
    #     total_config_tokens = sum(cfg["total_tokens"] for cfg in config_stats)
    #     overall_sharing_ratio = sum(
    #         cfg["prefix_sharing_ratio"] * cfg["total_tokens"] / total_config_tokens
    #         for cfg in config_stats
    #     ) if total_config_tokens > 0 else 0
        
    #     # Calculate weighted average of prefix proportions
    #     overall_prefix_proportion = sum(
    #         cfg["prefix_proportion"] * cfg["total_tokens"] / total_config_tokens
    #         for cfg in config_stats
    #     ) if total_config_tokens > 0 else 0
    
    # Sort combined data by timestamp
    all_prompts_combined.sort(key=lambda x: x["timestamp"])
    
    return {
        "prompts": all_prompts_combined,
        "stats": config_stat,
        "total_tokens": total_tokens,
        "overall_sharing_ratio": overall_sharing_ratio,
        "overall_prefix_proportion": overall_prefix_proportion
    }


def generate_dataset_from_config(tokenizer, config):
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
    sharing_ratio = calculate_prefix_sharing_ratio(
        tokenizer, prompts, token_counts, prefix_length
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
        sessioned_prompts.append({
            "session_id": prefix_idx,
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

    # Sort combined data by timestamp
    return {
        "prompts": sessioned_prompts,
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
    print("\nConfiguration details:")
    for cfg in workload_data["stats"]:
        num_req = cfg['num_prefix'] * cfg['num_samples_per_prefix']
        duration = num_req / cfg['rps']
        total_duration += duration
        total_num_requests += num_req
        print(f"Config {cfg['config_id']}:")
        print(f"  - Prefix length: {cfg['prefix_length']}")
        print(f"  - Suffix length: {cfg['suffix_length']}")
        print(f"  - Number of requests per prefix: {cfg['num_samples_per_prefix']}")
        print(f"  - Number of different prefixes: {cfg['num_prefix']}")
        print(f"  - RPS: {cfg['rps']}")
        print(f"  - Randomized order: {cfg['randomize_order']}")
        print(f"  - Duration: {duration:.0f} seconds")
        print(f"  - Number of requests {cfg['num_requests']}")
        print(f"  - Prefix proportion: {cfg['prefix_proportion']*100:.2f}% (portion of each prompt that is shared)")
        print(f"  - Efficiency gain: {cfg['prefix_sharing_ratio']*100:.2f}% (computational savings from prefix sharing)")
        print(f"  - Time range: {int(cfg['start_time']/1000)}s to {int(cfg['end_time']/1000)}s")

    print("\nWorkload Summary:")
    print(f"Total number of requests: {total_num_requests}")
    print(f"Total duration: {total_duration:.0f} seconds")
    print(f"Total prompts: {len(workload_data['prompts'])}")
    print(f"Total tokens: {workload_data['total_tokens']}")
    print(f"Overall prefix proportion: {workload_data['overall_prefix_proportion']*100:.2f}% (portion of each prompt that is shared)")
    print(f"Overall efficiency gain: {workload_data['overall_sharing_ratio']*100:.2f}% (computational savings from prefix sharing)")

if __name__ == "__main__":
    random.seed(0)
    np.random.seed(0)
    parser = argparse.ArgumentParser(description="Configure workload parameters.")
    parser.add_argument("--tokenizer", type=str, default="deepseek-ai/deepseek-llm-7b-chat", help="Name of the tokenizer.")
    parser.add_argument("--app-name", type=str, default="app", help="Name of the application.")
    parser.add_argument("--prompt-length", type=int, default=3871, help="Length of the prompt.")
    parser.add_argument("--prompt-length-std", type=int, default=1656, help="Standard deviation of the prompt length.")
    parser.add_argument("--shared-proportion", type=float, default=0.97, help="Proportion of shared content.")
    parser.add_argument("--shared-proportion-std", type=float, default=0.074, help="Standard deviation of shared proportion.")
    parser.add_argument("--num-samples-per-prefix", type=int, default=200, help="Number of samples per prefix.")
    parser.add_argument("--num-prefix", type=int, default=10, help="Number of prefixes.")
    parser.add_argument("--rps", type=int, default=0, help="Requests per second.")
    parser.add_argument("--randomize-order", action="store_true", help="Randomize order if flag is set.")
    parser.add_argument("--to-workload", action="store_true", help="Generate workload if flag is set (needs rps to be set).")
    
    args = parser.parse_args()
    
    to_workload = args.to_workload
    app_name = args.app_name
    prefix_workload_configs = [
        {
            "prompt_length": args.prompt_length,
            "prompt_length_std": args.prompt_length_std,
            "shared_proportion": args.shared_proportion,
            "shared_proportion_std": args.shared_proportion_std,
            "num_samples_per_prefix": args.num_samples_per_prefix,
            "num_prefix": args.num_prefix,
            "rps": args.rps,
            "randomize_order": args.randomize_order
        },
    ]
    
    # ToolBench
    # app_name = "toolbench"
    # prefix_workload_configs = [
    #     {
    #         "prompt_length": 1835,
    #         "prompt_length_std" : 742,
    #         "shared_proportion": 0.85,
    #         "shared_proportion_std": 0.13,
    #         "num_samples_per_prefix": 200,
    #         "num_prefix": 10,
    #         "rps": 0,
    #         "randomize_order": True  # Add the randomization parameter
    #     },
    # ]
    
    ## Agent
    # app_name = "agent"
    # prefix_workload_configs = [
    #     {
    #         "prompt_length": 2285,
    #         "prompt_length_std" : 471,
    #         "shared_proportion": 0.97,
    #         "shared_proportion_std": 0.14,
    #         "num_samples_per_prefix": 200,
    #         "num_prefix": 10,
    #         "rps": 0,
    #         "randomize_order": True  # Add the randomization parameter
    #     },
    # ]
        
    ## Programming
    # app_name = "programming"
    # prefix_workload_configs = [
    #     {
    #         "prompt_length": 3871,
    #         "prompt_length_std" : 1656,
    #         "shared_proportion": 0.97,
    #         "shared_proportion_std": 0.074,
    #         "num_samples_per_prefix": 200,
    #         "num_prefix": 10,
    #         "rps": 0,
    #         "randomize_order": True  # Add the randomization parameter
    #     },
    # ]
    
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
            print("Generating multi-configuration workload...")
            workload_data = generate_workload_from_config(tokenizer, prefix_workload_config)
            # Save results
            output_file = f"{base_filename}.jsonl"
            stats_file = f"{base_filename}-stats.json"
            save_workload_jsonl(workload_data, output_file)
            save_stats(workload_data, stats_file)
            print(f"Saving workload statistics to {stats_file}")
            print(f"Saving workload traces to {output_file}")
    else: # To dataset
        for i, prefix_workload_config in enumerate(prefix_workload_configs):
            rand_str = "-randomized" if prefix_workload_config["randomize_order"] else ""
            prefix_estimate = int(prefix_workload_config["prompt_length"] * prefix_workload_configs[0]["shared_proportion"])
            suffix_estimate = int(prefix_workload_config["prompt_length"] * (1 - prefix_workload_configs[0]["shared_proportion"])) 
            base_filename = f"{app_name}-prefix-share-dataset-p{prefix_estimate}-{suffix_estimate}.jsonl"
            prefix_workload_config["id"] = i
            dataset_dict = generate_dataset_from_config(tokenizer, prefix_workload_config)
            save_dataset_jsonl(dataset_dict["prompts"], f"{base_filename}-dataset.jsonl")
            print(f"Saving dataset to {base_filename}-dataset.jsonl")
            print(f"Dataset statistics: {dataset_dict['stats']} total_tokens: {dataset_dict['total_tokens']} overall_sharing_ratio: {dataset_dict['overall_sharing_ratio']} overall_prefix_proportion: {dataset_dict['overall_prefix_proportion']}")
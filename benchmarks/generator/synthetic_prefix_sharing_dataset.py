#!/usr/bin/env python3
import random
import json
import logging
import numpy as np
from tqdm import tqdm
from transformers import AutoTokenizer
from scipy.stats import truncnorm

# A collection of realistic text templates for generating prompts
REALISTIC_TEMPLATES = [
    # Question-answering templates
    "Can you explain how {topic} works in simple terms?",
    "What are the main differences between {topic_a} and {topic_b}?",
    "I need to understand {topic} for my {purpose}. Can you help?",
    "Could you provide a step-by-step guide on how to {action}?",
    "What are the best practices for {activity} in {field}?",
    
    # Creative writing templates
    "Write a short story about {character} who discovers {object} in {location}.",
    "Create a poem about {theme} using the style of {author}.",
    "Describe a scene where {character_a} meets {character_b} at {location}.",
    "Write a dialogue between {character_a} and {character_b} discussing {topic}.",
    "Develop a plot outline for a story about {theme} set in {setting}.",
    
    # Professional content templates
    "Draft an email to {recipient} regarding {subject}.",
    "Write a product description for {product} highlighting its {feature}.",
    "Create a marketing copy for {service} targeting {audience}.",
    "Compose a social media post announcing {event} for {platform}.",
    "Draft a professional bio for {person} who specializes in {expertise}.",
    
    # Information retrieval templates
    "Summarize the key points about {topic} in bullet points.",
    "What are the latest developments in {field} as of 2024?",
    "Provide a comparison table of {item_a}, {item_b}, and {item_c}.",
    "What are the pros and cons of {subject}?",
    "Give me 5 tips for improving {skill}."
]

# Domain-specific vocabulary to make the prompts more realistic
TOPICS = [
    "machine learning", "artificial intelligence", "neural networks", "deep learning", 
    "natural language processing", "computer vision", "reinforcement learning",
    "blockchain", "cryptocurrency", "smart contracts", "decentralized finance",
    "cloud computing", "serverless architecture", "microservices", "containerization",
    "cybersecurity", "ethical hacking", "network security", "encryption",
    "data science", "big data", "data visualization", "statistical analysis",
    "software development", "agile methodology", "DevOps", "continuous integration"
]

ACTIONS = [
    "deploy a machine learning model", "optimize database queries", "secure a web application",
    "build a responsive website", "create a mobile app", "implement an API",
    "analyze data using Python", "set up a cloud infrastructure", "configure a firewall",
    "develop a recommendation system", "train a neural network", "perform sentiment analysis"
]

CHARACTERS = [
    "a software engineer", "a data scientist", "a startup founder", "a cybersecurity expert",
    "an AI researcher", "a product manager", "a UX designer", "a digital nomad",
    "a tech entrepreneur", "a blockchain developer", "a virtual reality designer"
]

LOCATIONS = [
    "Silicon Valley", "a tech conference", "a coworking space", "a virtual reality world",
    "a futuristic city", "a remote island with high-speed internet", "a hackathon",
    "an innovation lab", "a digital marketplace", "an AI research center"
]

def generate_realistic_prompt(tokenizer, target_token_length):
    """
    Generate a realistic prompt using templates and domain-specific vocabulary
    
    Args:
        tokenizer: The tokenizer to use
        target_token_length: Desired length in tokens
        
    Returns:
        A realistic prompt string
    """
    # Start with a random template
    template = random.choice(REALISTIC_TEMPLATES)
    
    # Fill in the template with random relevant content
    filled_template = template.format(
        topic=random.choice(TOPICS),
        topic_a=random.choice(TOPICS),
        topic_b=random.choice(TOPICS),
        purpose=random.choice(["project", "research", "presentation", "startup idea", "blog post"]),
        action=random.choice(ACTIONS),
        activity=random.choice(["coding", "designing", "analyzing", "implementing", "testing"]),
        field=random.choice(["tech", "finance", "healthcare", "education", "e-commerce"]),
        character=random.choice(CHARACTERS),
        character_a=random.choice(CHARACTERS),
        character_b=random.choice(CHARACTERS),
        object=random.choice(["a quantum computer", "an AI assistant", "a time machine", "a virtual reality device"]),
        location=random.choice(LOCATIONS),
        theme=random.choice(["innovation", "digital transformation", "future of work", "technological singularity"]),
        author=random.choice(["a tech visionary", "a sci-fi writer", "a futurist", "a digital artist"]),
        setting=random.choice(["a smart city", "a space colony", "a digital universe", "a post-AI world"]),
        recipient=random.choice(["a potential client", "a team member", "a project stakeholder", "a tech investor"]),
        subject=random.choice(["project proposal", "software update", "partnership opportunity", "technical issue"]),
        product=random.choice(["AI software", "smart device", "cloud service", "tech gadget"]),
        feature=random.choice(["innovative features", "user-friendly interface", "cutting-edge technology", "performance"]),
        service=random.choice(["consulting service", "tech solution", "software as a service", "digital platform"]),
        audience=random.choice(["tech enthusiasts", "business professionals", "developers", "startups"]),
        event=random.choice(["product launch", "tech conference", "software release", "hackathon"]),
        platform=random.choice(["LinkedIn", "Twitter", "Facebook", "Instagram"]),
        person=random.choice(CHARACTERS),
        expertise=random.choice(TOPICS),
        item_a=random.choice(TOPICS),
        item_b=random.choice(TOPICS),
        item_c=random.choice(TOPICS),
        skill=random.choice(["programming", "data analysis", "system design", "technical writing", "debugging"])
    )
    
    # Check token length
    token_count = len(tokenizer.encode(filled_template))
    
    # If the template is too short, extend it with additional relevant content
    while token_count < target_token_length:
        # Add more content to the prompt
        additional_content = [
            f" Additionally, I'm interested in learning about {random.choice(TOPICS)}.",
            f" Could you also explain how this relates to {random.choice(TOPICS)}?",
            f" I'm asking because I need to {random.choice(ACTIONS)} for {random.choice(['my work', 'a client', 'a project', 'my research'])}.",
            f" For context, I have experience with {random.choice(TOPICS)} but I'm new to this specific area.",
            f" I've been trying to understand this concept for {random.choice(['days', 'weeks', 'months'])} and would appreciate a clear explanation."
        ]
        
        filled_template += random.choice(additional_content)
        token_count = len(tokenizer.encode(filled_template))
    
    # If the prompt is too long, truncate it to the desired length
    if token_count > target_token_length:
        tokenized = tokenizer.encode(filled_template)[:target_token_length]
        filled_template = tokenizer.decode(tokenized, skip_special_tokens=True)
    
    return filled_template

def generate_unique_prefix(base_text, index):
    return str(index) + base_text[len(str(index)):]

def adjust_prompt_length(tokenizer, prompt, target_token_length):
    token_count = len(tokenizer.encode(prompt))
    adjusted_prompt = prompt
    if token_count < target_token_length:
        while token_count < target_token_length:
            additional_content = [
                f" Additionally, I'm interested in learning about {random.choice(TOPICS)}.",
                f" Could you also explain how this relates to {random.choice(TOPICS)}?",
                f" I'm asking because I need to {random.choice(ACTIONS)} for {random.choice(['my work', 'a client', 'a project', 'my research'])}.",
                f" For context, I have experience with {random.choice(TOPICS)} but I'm new to this specific area.",
                f" I've been trying to understand this concept for {random.choice(['days', 'weeks', 'months'])} and would appreciate a clear explanation."
            ]
            adjusted_prompt += random.choice(additional_content)
            token_count = len(tokenizer.encode(adjusted_prompt))
    elif token_count > target_token_length:
        adjusted_prompt_tokenized = tokenizer.encode(prompt)[:target_token_length]
        adjusted_prompt = tokenizer.decode(adjusted_prompt_tokenized, skip_special_tokens=True)
    return adjusted_prompt
    
    

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
        base_prefix = generate_realistic_prompt(tokenizer, shared_length_mean)
        unique_prefix = generate_unique_prefix(base_prefix, i)
        prompt_list = []
        token_count_list = []
        
        for j in range(num_samples_per_prefix):
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
            suffix = generate_realistic_prompt(tokenizer, target_suffix_length)
            adjusted_suffix = adjust_prompt_length(tokenizer, suffix, target_suffix_length)
            
            prompt = adjusted_prefix + " " + adjusted_suffix
            
            # Count tokens
            token_count = len(tokenizer.encode(prompt))
            logging.info(f"generate_realistic_prompt num_prefix {num_prefix} config {config['id']} sampled_prompt_length {sampled_prompt_length} sampled_shared_length {sampled_shared_length} target_prefix_length {target_prefix_length} suffix_length {target_suffix_length} num_samples_per_prefix {num_samples_per_prefix} total token count {token_count}")
            
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

def generate_workload_from_config(tokenizer, configs):
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
    config_stats = []
    
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
    for i, config in enumerate(configs):
        # Add an ID to the config for reference
        config["id"] = i+1
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
        
        config_stats.append({
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
        })
    
    # Calculate overall prefix sharing ratio using the same token-based method
    overall_sharing_ratio = 0
    if len(configs) == 1:
        # If there's only one config, use its sharing ratio
        overall_sharing_ratio = config_stats[0]["prefix_sharing_ratio"]
        overall_prefix_proportion = config_stats[0]["prefix_proportion"]
    else:
        # For multiple configs, calculate an overall ratio based on all prompts
        # This is more complex and would need special handling for different prefix lengths
        # For now, we'll use a weighted average based on token counts
        total_config_tokens = sum(cfg["total_tokens"] for cfg in config_stats)
        overall_sharing_ratio = sum(
            cfg["prefix_sharing_ratio"] * cfg["total_tokens"] / total_config_tokens
            for cfg in config_stats
        ) if total_config_tokens > 0 else 0
        
        # Calculate weighted average of prefix proportions
        overall_prefix_proportion = sum(
            cfg["prefix_proportion"] * cfg["total_tokens"] / total_config_tokens
            for cfg in config_stats
        ) if total_config_tokens > 0 else 0
    
    # Sort combined data by timestamp
    all_prompts_combined.sort(key=lambda x: x["timestamp"])
    
    return {
        "prompts": all_prompts_combined,
        "stats": config_stats,
        "total_tokens": total_tokens,
        "overall_sharing_ratio": overall_sharing_ratio,
        "overall_prefix_proportion": overall_prefix_proportion
    }


def generate_dataset_from_config(tokenizer, configs):
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
    config_stats = []
    
    # Variables to track overall prefix sharing
    total_prompts_count = 0
    
    # Variables for overall prefix sharing calculation
    all_prompts_for_sharing = []
    all_prompts_token_counts = []
    all_prefix_lengths = []
    
    # Process each configuration
    for i, config in enumerate(configs):
        # Add an ID to the config for reference
        config["id"] = i+1
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

        for j, prompt_data in enumerate(flat_prompts_data):
            all_prompts_combined.append(prompt_data)
        
        # Update overall prefix sharing tracking
        total_prompts_count += len(flat_prompts_data)
        
        # Store config data for overall prefix calculation
        all_prompts_for_sharing.extend(prompts)
        all_prompts_token_counts.extend(token_counts)
        all_prefix_lengths.extend([prefix_length] * len(prompts))
        

        config_stats.append({
            "config_id": config["id"],
            "prefix_length": prefix_length,
            "suffix_length": suffix_length,
            "num_samples_per_prefix": config["num_samples_per_prefix"],
            "num_prefix": config["num_prefix"],
            "rps": config['rps'],
            "randomize_order": randomize_order,
            "num_requests": len(flat_prompts_data),
            "total_tokens": tokens,
            "prefix_sharing_ratio": sharing_ratio,
            "prefix_proportion": prefix_proportion,
        })
    
    # Calculate overall prefix sharing ratio using the same token-based method
    overall_sharing_ratio = 0
    if len(configs) == 1:
        # If there's only one config, use its sharing ratio
        overall_sharing_ratio = config_stats[0]["prefix_sharing_ratio"]
        overall_prefix_proportion = config_stats[0]["prefix_proportion"]
    else:
        # For multiple configs, calculate an overall ratio based on all prompts
        # This is more complex and would need special handling for different prefix lengths
        # For now, we'll use a weighted average based on token counts
        total_config_tokens = sum(cfg["total_tokens"] for cfg in config_stats)
        overall_sharing_ratio = sum(
            cfg["prefix_sharing_ratio"] * cfg["total_tokens"] / total_config_tokens
            for cfg in config_stats
        ) if total_config_tokens > 0 else 0
        
        # Calculate weighted average of prefix proportions
        overall_prefix_proportion = sum(
            cfg["prefix_proportion"] * cfg["total_tokens"] / total_config_tokens
            for cfg in config_stats
        ) if total_config_tokens > 0 else 0
    
    # Sort combined data by timestamp

    return {
        "prompts": all_prompts_combined,
        "stats": config_stats,
        "total_tokens": total_tokens,
        "overall_sharing_ratio": overall_sharing_ratio,
        "overall_prefix_proportion": overall_prefix_proportion
    }



def save_workload_jsonl(workload_data, output_file):
    """
    Save the combined workload to a JSONL file
    
    Args:
        workload_data: Dictionary with prompts and stats
        output_file: Output file path
    """
    with open(output_file, 'w') as f:
        for item in workload_data["prompts"]:
            entry = {
                "timestamp": item["timestamp"],
                "requests": [
                    {
                        "Prompt Length": item["token_count"],  # Use token count instead of character length
                        "Output Length": 8,  # Fixed value as per example
                        "prompt": item["prompt"],
                        "prefix_group": item["prefix_group"],  # Add prefix group info for analysis
                        "config_id": item["config_id"]
                    }
                ]
            }
            f.write(json.dumps(entry) + '\n')
            
def save_dataset_jsonl(workload_data, output_file):
    with open(output_file, 'w') as f:
        for prompt in workload_data:
            f.write(json.dumps(prompt) + '\n')

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
    to_workload = False
    app_name = ""
    
    # prompt_length = 2048
    # prompt_length_std = 512
    # shared_proportion = 0.6
    # shared_proportion_std = 0.1
    # num_samples_per_prefix = 2
    # num_prefix = 2
    # rps = 5
    # randomize_order = True

    # prefix_workload_configs = [
    #     {
    #         "prompt_length": prompt_length,
    #         "prompt_length_std" : prompt_length_std,
    #         "shared_proportion": shared_proportion,
    #         "shared_proportion_std": shared_proportion_std,
    #         "num_samples_per_prefix": num_samples_per_prefix,
    #         "num_prefix": num_prefix,
    #         "rps": rps,
    #         "randomize_order": randomize_order  # Add the randomization parameter
    #     },
    # ]
    

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
    app_name = "programming"
    prefix_workload_configs = [
        {
            "prompt_length": 3871,
            "prompt_length_std" : 1656,
            "shared_proportion": 0.97,
            "shared_proportion_std": 0.074,
            "num_samples_per_prefix": 200,
            "num_prefix": 10,
            "rps": 0,
            "randomize_order": True  # Add the randomization parameter
        },
    ]
    
    # Initialize tokenizer
    tokenizer = AutoTokenizer.from_pretrained(
        "deepseek-ai/deepseek-llm-7b-chat", 
        legacy=True,
        model_max_length=4096,  # Increased to handle longer prefixes
        padding_side="right",
        truncation_side="right",
        use_fast=True
    )
    
    rand_str = "-randomized" if prefix_workload_configs[0]["randomize_order"] else ""
    prefix_estimate = int(prefix_workload_configs[0]["prompt_length"] * prefix_workload_configs[0]["shared_proportion"])
    suffix_estimate = int(prefix_workload_configs[0]["prompt_length"] * (1 - prefix_workload_configs[0]["shared_proportion"]))
    if to_workload:
        rps = prefix_workload_configs[0]["rps"]
        base_filename = f"{app_name}-prefix-share-workload-p{prefix_estimate}-s{suffix_estimate}-rps{rps}{rand_str}"
        # Add randomization info to filename
        print("Generating multi-configuration workload...")
        workload_data = generate_workload_from_config(tokenizer, prefix_workload_configs)
        # Save results
        output_file = f"{base_filename}.jsonl"
        stats_file = f"{base_filename}-stats.json"
        save_workload_jsonl(workload_data, output_file)
        save_stats(workload_data, stats_file)
        print(f"Saving workload statistics to {stats_file}")
        print(f"Saving workload traces to {output_file}")
    else: # To dataset
        base_filename = f"{app_name}-prefix-share-dataset-p{prefix_estimate}-{suffix_estimate}.jsonl"
        dataset_dict = generate_dataset_from_config(tokenizer, prefix_workload_configs)
        save_dataset_jsonl(dataset_dict["prompts"], f"{base_filename}-dataset.jsonl")
        print(f"Saving dataset to {base_filename}-dataset.jsonl")
        print(f"Dataset statistics: {dataset_dict['stats']} total_tokens: {dataset_dict['total_tokens']} overall_sharing_ratio: {dataset_dict['overall_sharing_ratio']} overall_prefix_proportion: {dataset_dict['overall_prefix_proportion']}")
        
        
        # df = pd.DataFrame({
        #     'prompt': [entry['input'][0]['content'] for entry in dataset],
        #     'completion': [entry['output'] for entry in dataset],
        #     'prompt_len': [entry['prompt_tokens'] for entry in dataset],
        #     'completion_len': [entry['output_tokens'] for entry in dataset]
        # })
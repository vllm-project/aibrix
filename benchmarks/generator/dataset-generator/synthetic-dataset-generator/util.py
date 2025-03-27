import json

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
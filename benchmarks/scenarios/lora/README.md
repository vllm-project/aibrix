# Lora benchmark


## Prerequisite

### 1. Install dependencies

```
apt update && apt install -y vim wget zip jq
pip3 install huggingface_hub transformers openai peft vllm
```

### 1. Download Models

Public Cloud

```
import os
from huggingface_hub import snapshot_download

os.environ['HF_TOKEN']="hf_xxxxxxxx"

base_model_path = snapshot_download(repo_id="meta-llama/Llama-2-7b-hf", ignore_patterns=[ "*.bin"])
sql_lora_path = snapshot_download(repo_id="Fredh99/llama-2-7b-sft-lora", local_dir="/models/Fredh99/llama-2-7b-sft-lora")
```

Volcano Engine

```
TODO: Add Toscli tutorials
```

### 2. Merge Models

```
import torch
from transformers import AutoModelForCausalLM, AutoTokenizer
from peft import PeftModel, PeftConfig

def merge_base_and_lora(base_model_name_or_path, lora_model_name_or_path, output_path):
    # Load the base model and tokenizer
    print("Loading base model...")
    base_model = AutoModelForCausalLM.from_pretrained(base_model_name_or_path)
    tokenizer = AutoTokenizer.from_pretrained(base_model_name_or_path)
    
    # Load the LoRA weights
    print("Loading LoRA model...")
    peft_config = PeftConfig.from_pretrained(lora_model_name_or_path)
    lora_model = PeftModel.from_pretrained(base_model, lora_model_name_or_path)
    
    # Merge LoRA weights into the base model
    print("Merging LoRA weights into the base model...")
    lora_model = lora_model.merge_and_unload()
    
    # Save the merged model
    print(f"Saving the merged model to {output_path}...")
    lora_model.save_pretrained(output_path)
    tokenizer.save_pretrained(output_path)

    print(f"Merged model saved successfully at {output_path}!")

merge_base_and_lora('meta-llama/Llama-2-7b-hf', '/models/Fredh99/llama-2-7b-sft-lora', '/models/merge-model-weights')
```

### 3. Download datasets

```
wget https://huggingface.co/datasets/anon8231489123/ShareGPT_Vicuna_unfiltered/resolve/main/ShareGPT_V3_unfiltered_cleaned_split.json
```

### 4. Download chat template

```
wget https://raw.githubusercontent.com/chujiezheng/chat_templates/refs/heads/main/chat_templates/llama-2-chat.jinja
```

## Deployments

### Deployment 1

Merged Lora weights

```
CUDA_VISIBLE_DEVICES=0 vllm serve /models/merge-model-weights --served-model-name model-1 --host "0.0.0.0" --port "8071" --chat-template llama-2-chat.jinja
```

### Deployment 2

Unmerged Lora weights but deployed separately.

```
CUDA_VISIBLE_DEVICES=0 vllm serve meta-llama/Llama-2-7b-hf --host "0.0.0.0" --port "8071" --enable-lora --lora-modules model-1=/models/Fredh99/llama-2-7b-sft-lora --chat-template llama-2-chat.jinja --max-lora-rank=64
```

### Deployment 3

Multi-Lora deployments

> Note: we just deploy 32 models for easy management. add one more argument `--model` to control it.

```
CUDA_VISIBLE_DEVICES=0 vllm serve meta-llama/Llama-2-7b-hf --host "0.0.0.0" --port "8070" --enable-lora --lora-modules \
model-1=/models/Fredh99/llama-2-7b-sft-lora \
model-2=/models/Fredh99/llama-2-7b-sft-lora \
model-3=/models/Fredh99/llama-2-7b-sft-lora \
model-4=/models/Fredh99/llama-2-7b-sft-lora \
model-5=/models/Fredh99/llama-2-7b-sft-lora \
model-6=/models/Fredh99/llama-2-7b-sft-lora \
model-7=/models/Fredh99/llama-2-7b-sft-lora \
model-8=/models/Fredh99/llama-2-7b-sft-lora \
model-9=/models/Fredh99/llama-2-7b-sft-lora \
model-10=/models/Fredh99/llama-2-7b-sft-lora \
model-11=/models/Fredh99/llama-2-7b-sft-lora \
model-12=/models/Fredh99/llama-2-7b-sft-lora \
model-13=/models/Fredh99/llama-2-7b-sft-lora \
model-14=/models/Fredh99/llama-2-7b-sft-lora \
model-15=/models/Fredh99/llama-2-7b-sft-lora \
model-16=/models/Fredh99/llama-2-7b-sft-lora \
model-17=/models/Fredh99/llama-2-7b-sft-lora \
model-18=/models/Fredh99/llama-2-7b-sft-lora \
model-19=/models/Fredh99/llama-2-7b-sft-lora \
model-20=/models/Fredh99/llama-2-7b-sft-lora \
model-21=/models/Fredh99/llama-2-7b-sft-lora \
model-22=/models/Fredh99/llama-2-7b-sft-lora \
model-23=/models/Fredh99/llama-2-7b-sft-lora \
model-24=/models/Fredh99/llama-2-7b-sft-lora \
model-25=/models/Fredh99/llama-2-7b-sft-lora \
model-26=/models/Fredh99/llama-2-7b-sft-lora \
model-27=/models/Fredh99/llama-2-7b-sft-lora \
model-28=/models/Fredh99/llama-2-7b-sft-lora \
model-29=/models/Fredh99/llama-2-7b-sft-lora \
model-30=/models/Fredh99/llama-2-7b-sft-lora \
model-31=/models/Fredh99/llama-2-7b-sft-lora \
model-32=/models/Fredh99/llama-2-7b-sft-lora \
--chat-template llama-2-chat.jinja --max-lora-rank=64 --max-loras 32
```

> Note: by default, `--max-loras` is equal to number of loras, feel free to change it for different experiments. 

## Experiment 1 - Pin lora size 4, run difference concurrency.

deployment1
```
#!/bin/bash
for concurrency in {1..32}; do
    output_file="benchmark_merged.jsonl"
    echo "Running benchmark with concurrency ${concurrency} and output file ${output_file}"

    python3 benchmark.py \
        --dataset-path "/workspace/ShareGPT_V3_unfiltered_cleaned_split.json" \
        --concurrency ${concurrency} \
        --output-file-path "${output_file}" \
        --deployment-endpoints "deployment1"
done
```

deployment2
```
#!/bin/bash
for concurrency in {1..32}; do
    output_file="benchmark_unmerged_single_lora.jsonl"
    echo "Running benchmark with concurrency ${concurrency} and output file ${output_file}"

    python3 benchmark.py \
        --dataset-path "/workspace/ShareGPT_V3_unfiltered_cleaned_split.json" \
        --concurrency ${concurrency} \
        --output-file-path "${output_file}" \
        --deployment-endpoints "deployment2"
done
```

deployment3
```
#!/bin/bash
for concurrency in {1..32}; do
    output_file="benchmark_unmerged_multi_lora_4.jsonl"
    echo "Running benchmark with concurrency ${concurrency} and output file ${output_file}"

    python3 benchmark.py \
        --dataset-path "/workspace/ShareGPT_V3_unfiltered_cleaned_split.json" \
        --concurrency ${concurrency} \
        --output-file-path "${output_file}" \
        --deployment-endpoints "deployment3"
done
```

## Experiment 2 - Pin lora size 8, run difference concurrency.

There's no difference between deployment1 and deployment2, we can skip the tests.
> Note: we can play with server side `max-loras` and do different experiments here.

```
#!/bin/bash
for concurrency in {1..32}; do
    output_file="benchmark_unmerged_multi_lora_8_max_loras_8.jsonl"
    echo "Running benchmark with concurrency ${concurrency} and output file ${output_file}"

    python3 benchmark.py \
        --dataset-path "/workspace/ShareGPT_V3_unfiltered_cleaned_split.json" \
        --concurrency ${concurrency} \
        --output-file-path "${output_file}" \
        --deployment-endpoints "deployment3" \
        --models 8
done
```

## Experiment 3 - Pin concurrency size 1, run difference loras

```
#!/bin/bash
for model_number in {1..32}; do
    output_file="benchmark_unmerged_multi_lora_${model_number}_concurrency_1.jsonl"
    echo "Running benchmark with model number ${model_number} and output file ${output_file}"

    python3 benchmark.py \
        --dataset-path "/workspace/ShareGPT_V3_unfiltered_cleaned_split.json" \
        --concurrency 1 \
        --output-file-path "${output_file}" \
        --deployment-endpoints "deployment3" \
        --models ${model_number}
done
```

> Note: Find results in deployment1 and deployment2 concurrency=1 cases.

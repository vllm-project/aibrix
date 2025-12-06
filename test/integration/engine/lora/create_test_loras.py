"""Create dummy LoRA adapters for testing.

This script generates dummy LoRA adapters compatible with a specified base model.
It automatically reads model configuration from HuggingFace to ensure compatibility.

Usage:
    python create_test_loras.py --base-model Qwen/Qwen3-8B --num-loras 10 --ranks 8,16,32
    python create_test_loras.py --base-model meta-llama/Llama-2-7b-hf --num-loras 5
"""

import os
import json
import shutil
import argparse
from pathlib import Path

import torch
from safetensors.torch import save_file

try:
    from transformers import AutoConfig
    HAS_TRANSFORMERS = True
except ImportError:
    HAS_TRANSFORMERS = False
    print("Warning: transformers not installed. Using manual config specification.")


# Default configurations for common models (fallback if HuggingFace unavailable)
DEFAULT_MODEL_CONFIGS = {
    "meta-llama/Llama-2-7b-hf": {
        "hidden_size": 4096,
        "intermediate_size": 11008,
        "num_hidden_layers": 32,
        "num_attention_heads": 32,
        "num_key_value_heads": 32,
    },
    "meta-llama/Llama-2-13b-hf": {
        "hidden_size": 5120,
        "intermediate_size": 13824,
        "num_hidden_layers": 40,
        "num_attention_heads": 40,
        "num_key_value_heads": 40,
    },
    "meta-llama/Llama-2-70b-hf": {
        "hidden_size": 8192,
        "intermediate_size": 28672,
        "num_hidden_layers": 80,
        "num_attention_heads": 64,
        "num_key_value_heads": 8,
    },
    "Qwen/Qwen3-8B": {
        "hidden_size": 4096,
        "intermediate_size": 12288,
        "num_hidden_layers": 36,
        "num_attention_heads": 32,
        "num_key_value_heads": 8,
    },
    "mistralai/Mistral-7B-v0.1": {
        "hidden_size": 4096,
        "intermediate_size": 14336,
        "num_hidden_layers": 32,
        "num_attention_heads": 32,
        "num_key_value_heads": 8,
    },
}


def get_model_config(model_name: str) -> dict:
    """Get model configuration from HuggingFace or fallback to defaults.

    Args:
        model_name: HuggingFace model name (e.g., 'Qwen/Qwen3-8B')

    Returns:
        Dict with model configuration parameters
    """
    if HAS_TRANSFORMERS:
        try:
            print(f"Fetching config for {model_name} from HuggingFace...")
            config = AutoConfig.from_pretrained(model_name, trust_remote_code=True)

            model_config = {
                "hidden_size": config.hidden_size,
                "intermediate_size": getattr(config, "intermediate_size", config.hidden_size * 4),
                "num_hidden_layers": config.num_hidden_layers,
                "num_attention_heads": config.num_attention_heads,
                "num_key_value_heads": getattr(config, "num_key_value_heads", config.num_attention_heads),
            }
            print(f"  hidden_size: {model_config['hidden_size']}")
            print(f"  intermediate_size: {model_config['intermediate_size']}")
            print(f"  num_hidden_layers: {model_config['num_hidden_layers']}")
            print(f"  num_attention_heads: {model_config['num_attention_heads']}")
            print(f"  num_key_value_heads: {model_config['num_key_value_heads']}")
            return model_config
        except Exception as e:
            print(f"Warning: Failed to fetch config from HuggingFace: {e}")

    # Fallback to defaults
    if model_name in DEFAULT_MODEL_CONFIGS:
        print(f"Using default config for {model_name}")
        return DEFAULT_MODEL_CONFIGS[model_name]

    raise ValueError(
        f"Unknown model: {model_name}. Please install transformers or add to DEFAULT_MODEL_CONFIGS."
    )


def estimate_lora_size(
    model_config: dict,
    rank: int,
    target_modules: list[str],
    dtype_bytes: int = 2,
) -> dict:
    """Estimate LoRA adapter size in memory.

    Args:
        model_config: Model configuration dict
        rank: LoRA rank
        target_modules: List of target modules
        dtype_bytes: Bytes per element (2 for fp16)

    Returns:
        Dict with size estimates
    """
    hidden_size = model_config["hidden_size"]
    intermediate_size = model_config["intermediate_size"]
    num_layers = model_config["num_hidden_layers"]
    num_heads = model_config["num_attention_heads"]
    num_kv_heads = model_config["num_key_value_heads"]
    head_dim = hidden_size // num_heads

    total_params = 0
    module_sizes = {}

    for module in target_modules:
        if module == "q_proj":
            in_dim, out_dim = hidden_size, hidden_size
        elif module == "k_proj":
            in_dim, out_dim = hidden_size, num_kv_heads * head_dim
        elif module == "v_proj":
            in_dim, out_dim = hidden_size, num_kv_heads * head_dim
        elif module == "o_proj":
            in_dim, out_dim = hidden_size, hidden_size
        elif module in ("gate_proj", "up_proj"):
            in_dim, out_dim = hidden_size, intermediate_size
        elif module == "down_proj":
            in_dim, out_dim = intermediate_size, hidden_size
        else:
            continue

        # LoRA A: [rank, in_dim], LoRA B: [out_dim, rank]
        params_per_layer = rank * in_dim + out_dim * rank
        total_module_params = params_per_layer * num_layers
        module_sizes[module] = total_module_params
        total_params += total_module_params

    return {
        "total_params": total_params,
        "total_bytes": total_params * dtype_bytes,
        "total_mb": (total_params * dtype_bytes) / (1024 * 1024),
        "module_sizes": module_sizes,
    }


def create_lora_adapter(
    output_dir: str,
    model_config: dict,
    rank: int = 16,
    lora_alpha: int = 16,
    target_modules: list[str] | None = None,
    dtype: torch.dtype = torch.float16,
    base_model_name: str | None = None,
) -> dict:
    """Create a dummy LoRA adapter for testing.

    Args:
        output_dir: Directory to save the LoRA adapter
        model_config: Model configuration dict from get_model_config()
        rank: LoRA rank
        lora_alpha: LoRA alpha scaling factor
        target_modules: List of target modules (default: attention projections)
        dtype: Weight dtype
        base_model_name: Base model name for adapter_config.json

    Returns:
        Dict with adapter info
    """
    os.makedirs(output_dir, exist_ok=True)

    if target_modules is None:
        target_modules = ["q_proj", "k_proj", "v_proj", "o_proj"]

    hidden_size = model_config["hidden_size"]
    intermediate_size = model_config["intermediate_size"]
    num_layers = model_config["num_hidden_layers"]
    num_heads = model_config["num_attention_heads"]
    num_kv_heads = model_config["num_key_value_heads"]
    head_dim = hidden_size // num_heads

    # Create adapter_config.json (PEFT format)
    config = {
        "r": rank,
        "lora_alpha": lora_alpha,
        "target_modules": target_modules,
        "bias": "none",
        "task_type": "CAUSAL_LM",
        "peft_type": "LORA",
        "base_model_name_or_path": base_model_name,
    }

    with open(os.path.join(output_dir, "adapter_config.json"), "w") as f:
        json.dump(config, f, indent=2)

    # Create dummy weights
    tensors = {}
    total_params = 0

    for module in target_modules:
        # Determine dimensions based on module type
        if module == "q_proj":
            in_dim, out_dim = hidden_size, hidden_size
        elif module == "k_proj":
            in_dim, out_dim = hidden_size, num_kv_heads * head_dim
        elif module == "v_proj":
            in_dim, out_dim = hidden_size, num_kv_heads * head_dim
        elif module == "o_proj":
            in_dim, out_dim = hidden_size, hidden_size
        elif module == "gate_proj":
            in_dim, out_dim = hidden_size, intermediate_size
        elif module == "up_proj":
            in_dim, out_dim = hidden_size, intermediate_size
        elif module == "down_proj":
            in_dim, out_dim = intermediate_size, hidden_size
        else:
            print(f"Warning: Unknown module {module}, skipping")
            continue

        for layer_idx in range(num_layers):
            # Determine the correct path (model-specific)
            # Most models use: base_model.model.model.layers.{layer}.self_attn.{module}
            if module in ("q_proj", "k_proj", "v_proj", "o_proj"):
                prefix = f"base_model.model.model.layers.{layer_idx}.self_attn.{module}"
            else:
                prefix = f"base_model.model.model.layers.{layer_idx}.mlp.{module}"

            # LoRA A: [rank, in_dim]
            lora_a = torch.randn(rank, in_dim, dtype=dtype) * 0.01
            tensors[f"{prefix}.lora_A.weight"] = lora_a
            total_params += rank * in_dim

            # LoRA B: [out_dim, rank] - initialized to zero for stable training start
            lora_b = torch.zeros(out_dim, rank, dtype=dtype)
            tensors[f"{prefix}.lora_B.weight"] = lora_b
            total_params += out_dim * rank

    # Save weights
    save_file(tensors, os.path.join(output_dir, "adapter_model.safetensors"))

    return {
        "path": output_dir,
        "rank": rank,
        "lora_alpha": lora_alpha,
        "target_modules": target_modules,
        "num_layers": num_layers,
        "total_params": total_params,
        "size_mb": (total_params * 2) / (1024 * 1024),  # Assuming fp16
    }


def create_test_loras(
    base_model: str,
    output_dir: str,
    num_loras: int = 10,
    ranks: list[int] | None = None,
    target_modules: list[str] | None = None,
) -> list[dict]:
    """Create multiple test LoRA adapters.

    Args:
        base_model: HuggingFace model name
        output_dir: Base directory for LoRA adapters
        num_loras: Number of LoRA adapters to create
        ranks: List of ranks to use (cycles through if len < num_loras)
        target_modules: Target modules for LoRA

    Returns:
        List of adapter info dicts
    """
    if ranks is None:
        ranks = [8]

    # Clean up existing
    if os.path.exists(output_dir):
        print(f"Cleaning up existing directory: {output_dir}")
        shutil.rmtree(output_dir)

    os.makedirs(output_dir, exist_ok=True)

    # Get model config
    model_config = get_model_config(base_model)

    # Create adapters
    adapters = []
    for i in range(num_loras):
        rank = ranks[i % len(ranks)]
        adapter_dir = os.path.join(output_dir, f"lora_{i}_rank{rank}")

        print(f"Creating LoRA {i} with rank {rank}...")
        adapter_info = create_lora_adapter(
            output_dir=adapter_dir,
            model_config=model_config,
            rank=rank,
            target_modules=target_modules,
            base_model_name=base_model,
        )
        adapters.append(adapter_info)

    # Print summary
    print(f"\n{'='*60}")
    print(f"CREATED {num_loras} LORA ADAPTERS")
    print(f"{'='*60}")
    print(f"Base model: {base_model}")
    print(f"Output directory: {output_dir}")
    print(f"Ranks used: {ranks}")
    print(f"\nAdapters:")
    for i, adapter in enumerate(adapters):
        print(f"  {i}: rank={adapter['rank']}, size={adapter['size_mb']:.2f} MB")
    print(f"{'='*60}")

    # Save manifest
    manifest = {
        "base_model": base_model,
        "model_config": model_config,
        "num_adapters": num_loras,
        "adapters": [
            {
                "index": i,
                "path": adapters[i]["path"],
                "rank": adapters[i]["rank"],
                "size_mb": adapters[i]["size_mb"],
            }
            for i in range(num_loras)
        ],
    }
    with open(os.path.join(output_dir, "manifest.json"), "w") as f:
        json.dump(manifest, f, indent=2)

    return adapters


def verify_adapters(output_dir: str) -> bool:
    """Verify all adapters in a directory are valid.

    Args:
        output_dir: Directory containing LoRA adapters

    Returns:
        True if all adapters are valid
    """
    manifest_path = os.path.join(output_dir, "manifest.json")
    if not os.path.exists(manifest_path):
        print(f"Error: No manifest.json found in {output_dir}")
        return False

    with open(manifest_path) as f:
        manifest = json.load(f)

    print(f"Verifying {manifest['num_adapters']} adapters for {manifest['base_model']}...")

    all_valid = True
    for adapter in manifest["adapters"]:
        path = adapter["path"]
        config_path = os.path.join(path, "adapter_config.json")
        weights_path = os.path.join(path, "adapter_model.safetensors")

        if not os.path.exists(config_path):
            print(f"  ERROR: Missing config: {config_path}")
            all_valid = False
        elif not os.path.exists(weights_path):
            print(f"  ERROR: Missing weights: {weights_path}")
            all_valid = False
        else:
            print(f"  OK: lora_{adapter['index']} (rank={adapter['rank']})")

    return all_valid


def main():
    parser = argparse.ArgumentParser(
        description="Create dummy LoRA adapters for testing",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Examples:
  # Create 10 LoRAs with default rank 8 for Qwen3-8B
  python create_test_loras.py --base-model Qwen/Qwen3-8B

  # Create 5 LoRAs with different ranks
  python create_test_loras.py --base-model meta-llama/Llama-2-7b-hf \\
      --num-loras 5 --ranks 8,16,32

  # Create LoRAs with MLP modules included
  python create_test_loras.py --base-model Qwen/Qwen3-8B \\
      --target-modules q_proj,k_proj,v_proj,o_proj,gate_proj,up_proj,down_proj

  # Verify existing adapters
  python create_test_loras.py --verify --output-dir /tmp/test_loras
        """,
    )
    parser.add_argument(
        "--base-model",
        type=str,
        default="Qwen/Qwen3-8B",
        help="HuggingFace model name (default: Qwen/Qwen3-8B)",
    )
    parser.add_argument(
        "--output-dir",
        type=str,
        default="/tmp/test_loras",
        help="Output directory for LoRA adapters (default: /tmp/test_loras)",
    )
    parser.add_argument(
        "--num-loras",
        type=int,
        default=10,
        help="Number of LoRA adapters to create (default: 10)",
    )
    parser.add_argument(
        "--ranks",
        type=str,
        default="8",
        help="Comma-separated list of ranks (default: 8). Cycles through if fewer than num-loras.",
    )
    parser.add_argument(
        "--target-modules",
        type=str,
        default="q_proj,k_proj,v_proj,o_proj",
        help="Comma-separated list of target modules (default: q_proj,k_proj,v_proj,o_proj)",
    )
    parser.add_argument(
        "--verify",
        action="store_true",
        help="Verify existing adapters instead of creating new ones",
    )
    parser.add_argument(
        "--estimate",
        action="store_true",
        help="Only estimate sizes without creating adapters",
    )

    args = parser.parse_args()

    if args.verify:
        success = verify_adapters(args.output_dir)
        return 0 if success else 1

    ranks = [int(r.strip()) for r in args.ranks.split(",")]
    target_modules = [m.strip() for m in args.target_modules.split(",")]

    if args.estimate:
        model_config = get_model_config(args.base_model)
        print(f"\n{'='*60}")
        print("SIZE ESTIMATES")
        print(f"{'='*60}")
        for rank in ranks:
            estimate = estimate_lora_size(model_config, rank, target_modules)
            print(f"Rank {rank}: {estimate['total_mb']:.2f} MB ({estimate['total_params']:,} params)")
        print(f"{'='*60}")
        return 0

    adapters = create_test_loras(
        base_model=args.base_model,
        output_dir=args.output_dir,
        num_loras=args.num_loras,
        ranks=ranks,
        target_modules=target_modules,
    )

    # Verify
    success = verify_adapters(args.output_dir)
    if success:
        print("\nAll LoRA adapters created and verified successfully!")
    else:
        print("\nWarning: Some adapters failed verification!")
        return 1

    return 0


if __name__ == "__main__":
    exit(main())

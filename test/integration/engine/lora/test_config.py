"""Common test configuration for LoRA memory management tests.

This module provides configuration and utilities for the LoRA test suite.
It supports both:
1. Pre-created test LoRAs (from create_test_loras.py)
2. Real LoRA adapters from HuggingFace
"""

import os
import json
from pathlib import Path

# =====================================================================
# Model Configuration
# =====================================================================

# Base model for testing
BASE_MODEL = "Qwen/Qwen3-8B"

# Directory for generated test LoRAs (from create_test_loras.py)
LORA_BASE_PATH = "/tmp/test_loras"

# Real LoRA for testing (fallback if generated LoRAs not available)
LORA_MODEL = "mtzig/qwen3-8b-tfdark-lora2"

# =====================================================================
# Test Parameters
# =====================================================================

TEST_PROMPT = "Hello, how are you today?"
MAX_TOKENS = 50
NUM_TEST_LORAS = 10

# =====================================================================
# LoRA Path Resolution
# =====================================================================


def get_lora_path(index: int) -> str:
    """Get path to a test LoRA adapter.

    Tries to use generated test LoRAs first (with varying ranks),
    falls back to real LoRA model if not available.

    Args:
        index: LoRA index (0-based)

    Returns:
        Path to LoRA adapter directory or HuggingFace model name
    """
    # Try to find generated test LoRAs with manifest
    manifest_path = os.path.join(LORA_BASE_PATH, "manifest.json")
    if os.path.exists(manifest_path):
        try:
            with open(manifest_path) as f:
                manifest = json.load(f)

            adapters = manifest.get("adapters", [])
            if adapters:
                # Cycle through available adapters
                adapter = adapters[index % len(adapters)]
                path = adapter["path"]
                if os.path.exists(path):
                    return path
        except Exception:
            pass

    # Try legacy path format (without manifest)
    legacy_path = os.path.join(LORA_BASE_PATH, f"lora_{index}")
    if os.path.exists(legacy_path):
        return legacy_path

    # Try rank-specific path format
    for rank in [8, 16, 32]:
        rank_path = os.path.join(LORA_BASE_PATH, f"lora_{index}_rank{rank}")
        if os.path.exists(rank_path):
            return rank_path

    # Fall back to real LoRA model
    return LORA_MODEL


def get_available_lora_count() -> int:
    """Get the number of available test LoRA adapters.

    Returns:
        Number of available LoRAs (from manifest or directory scan)
    """
    manifest_path = os.path.join(LORA_BASE_PATH, "manifest.json")
    if os.path.exists(manifest_path):
        try:
            with open(manifest_path) as f:
                manifest = json.load(f)
            return manifest.get("num_adapters", 0)
        except Exception:
            pass

    # Count directories
    if os.path.exists(LORA_BASE_PATH):
        count = 0
        for entry in os.listdir(LORA_BASE_PATH):
            if entry.startswith("lora_") and os.path.isdir(os.path.join(LORA_BASE_PATH, entry)):
                count += 1
        return count

    return 0


def get_test_lora_info() -> dict:
    """Get information about available test LoRAs.

    Returns:
        Dict with test LoRA information
    """
    manifest_path = os.path.join(LORA_BASE_PATH, "manifest.json")
    if os.path.exists(manifest_path):
        try:
            with open(manifest_path) as f:
                return json.load(f)
        except Exception:
            pass

    return {
        "base_model": BASE_MODEL,
        "source": "fallback",
        "lora_model": LORA_MODEL,
        "num_adapters": 0,
    }


def print_test_config():
    """Print current test configuration."""
    info = get_test_lora_info()
    lora_count = get_available_lora_count()

    print(f"{'='*60}")
    print("TEST CONFIGURATION")
    print(f"{'='*60}")
    print(f"Base Model: {BASE_MODEL}")
    print(f"LoRA Base Path: {LORA_BASE_PATH}")
    print(f"Available LoRAs: {lora_count}")

    if lora_count > 0 and info.get("adapters"):
        print("\nLoRA Adapters:")
        for adapter in info["adapters"][:5]:  # Show first 5
            print(f"  - lora_{adapter['index']}: rank={adapter['rank']}, size={adapter['size_mb']:.2f} MB")
        if len(info["adapters"]) > 5:
            print(f"  ... and {len(info['adapters']) - 5} more")
    else:
        print(f"\nUsing fallback LoRA: {LORA_MODEL}")

    print(f"{'='*60}")


if __name__ == "__main__":
    # Print configuration when run directly
    print_test_config()

    # Test path resolution
    print("\nLoRA Path Resolution:")
    for i in range(5):
        print(f"  get_lora_path({i}) = {get_lora_path(i)}")

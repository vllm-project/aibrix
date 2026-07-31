# vLLM LoRA Feature Test Suite

This test suite validates vLLM's LoRA adapter memory management, including LRU eviction, CPU-GPU swapping, pinning, and API behaviors.

## Prerequisites

- Python 3.10+
- vLLM installed with LoRA support
- GPU with sufficient memory (recommended: 24GB+ for Qwen3-8B)
- Required packages: `pytest`, `pytest-asyncio`
- Install vLLM.

```bash
# 1. create env
python3 -m venv ~/venvs/vllm
source ~/venvs/vllm/bin/activate

# 2. Upgrade pip
pip install --upgrade pip

# 3. Install vLLM (and whatever else you need)
pip install "vllm[torch]"==0.12.0

# 4. Install test packages
pip install pytest pytest-asyncio
```

## Quick Start

```bash
# 1. Generate test LoRA adapters
python create_test_loras.py --base-model Qwen/Qwen3-8B --num-loras 10 --ranks 8,16,32

# 2. Run all tests
python run_all_tests.py
```

## Test Coverage

The test suite validates:
- LRU eviction behavior
- CPU-GPU swap latency
- LoRA pinning functionality
- Memory isolation between LoRA and KV cache
- Memory budget and pre-allocation
- Batching constraints with max_loras
- Concurrent request handling
- API idempotency


## Artifact Preparation

### Option 1: Generate Dummy LoRAs (Recommended for Testing)

Generate dummy LoRA adapters that match your base model's architecture:

```bash
# Basic usage - creates 10 LoRAs with rank 8
python create_test_loras.py --base-model Qwen/Qwen3-8B

# Multiple ranks for swap latency testing
python create_test_loras.py --base-model Qwen/Qwen3-8B --num-loras 10 --ranks 8,16,32

# Include MLP modules for larger LoRAs
python create_test_loras.py --base-model Qwen/Qwen3-8B \
    --target-modules q_proj,k_proj,v_proj,o_proj,gate_proj,up_proj,down_proj

# Custom output directory
python create_test_loras.py --base-model Qwen/Qwen3-8B \
    --output-dir /path/to/loras
```

#### Estimate LoRA Sizes

```bash
python create_test_loras.py --base-model Qwen/Qwen3-8B --ranks 8,16,32,64 --estimate
```

#### Verify Generated Adapters

```bash
python create_test_loras.py --verify --output-dir /tmp/test_loras
```

### Option 2: Use Real LoRA from HuggingFace

Edit `test_config.py` to use a real LoRA adapter:

```python
# In test_config.py
LORA_MODEL = "your-org/your-lora-adapter"
```

The test suite automatically falls back to `LORA_MODEL` if generated LoRAs are not found.

## Running Tests

### Run All Tests

```bash
python run_all_tests.py
```

Options:
- `--timeout 600` - Timeout per test file in seconds (default: 600)
- `--quiet` - Reduce output verbosity

### Run Specific Tests with pytest

```bash
# All tests in a file
pytest test_lru_swap_pinning.py -v -s

# Specific test class
pytest test_memory.py::TestMemoryBudget -v -s

# Specific test method
pytest test_api.py::TestDynamicLoading::test_reload_same_lora -v -s

# Run tests matching a keyword
pytest -v -s -k "eviction"
```


## Configuration

Edit `test_config.py` to customize:

```python
# Base model
BASE_MODEL = "Qwen/Qwen3-8B"

# Generated LoRA directory
LORA_BASE_PATH = "/tmp/test_loras"

# Fallback LoRA (if generated not found)
LORA_MODEL = "mtzig/qwen3-8b-tfdark-lora2"

# Test parameters
TEST_PROMPT = "Hello, how are you today?"
MAX_TOKENS = 50
```

## Troubleshooting

### LoRA Loading Fails

1. Verify LoRA adapters exist: `python create_test_loras.py --verify`
2. Check base model compatibility
3. Ensure `max_lora_rank` >= actual LoRA rank

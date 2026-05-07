# AGENTS.md

This file captures repository-specific guidance for AI coding agents working in `python/aibrix`. Keep it focused on facts about this codebase rather than generic agent workflow rules.

## Setup And Common Commands

```bash
# Install development dependencies
poetry install --no-root --with dev

# Development install inside the Poetry environment
poetry run pip install -e .

# Full formatting, linting, and type checking pass inside Poetry
poetry run bash ./scripts/format.sh

# Individual checks
poetry run ruff check aibrix/
poetry run ruff format aibrix/
poetry run mypy aibrix/

# Run the full test suite inside Poetry
poetry run pytest

# Run focused suites while iterating
poetry run pytest tests/batch/
poetry run pytest tests/downloader/
poetry run pytest tests/metadata/
poetry run pytest tests/runtime/
```

Note: The Python tooling here is installed via Poetry. Unless you are already inside `poetry shell`, run validation commands with `poetry run ...`. In particular, run `poetry run bash ./scripts/format.sh` and `poetry run pytest` from the `python/aibrix` repository root after code changes in this subtree.

## CLI Entry Points

Defined in `pyproject.toml`:

- `aibrix_runtime` - main runtime server
- `aibrix_download` - model downloader CLI
- `aibrix_batch_worker` - batch worker process
- `aibrix_metadata` - metadata service
- `aibrix_benchmark` - GPU benchmarking tool
- `aibrix_gen_profile` - profile generation tool
- `aibrix_gen_secrets` - secret generation helper

## Architecture Map

### Core Runtime (`aibrix/app.py`)
- FastAPI server for model management, metrics, and health endpoints
- Integrates with inference engines, currently centered on vLLM
- Exposes model download, listing, and LoRA adapter operations

### Batch System (`aibrix/batch/`)
- Coordinates job lifecycle, scheduling, persistence, and worker execution
- Includes driver, manager, scheduler, proxy, and storage integration layers

### Downloader (`aibrix/downloader/`)
- Supports HuggingFace, S3, and TOS backends
- Handles caching, file locking, and download directory management

### Metadata Service (`aibrix/metadata/`)
- Separate FastAPI app for metadata and batch-related APIs
- Uses HTTPX-based clients for external service communication

### OpenAPI Layer (`aibrix/openapi/`)
- Defines request and response protocols for model management
- Contains engine abstractions and vLLM-specific implementations

### GPU Optimizer (`aibrix/gpu_optimizer/`)
- Contains load monitoring, profiling, clustering, benchmarking, and optimization logic

### Storage (`aibrix/storage/`)
- Provides storage abstractions and implementations used by runtime and batch features

## Configuration Pointers

- `aibrix/envs.py` defines environment-variable-backed settings
- `aibrix/config.py` contains configuration constants
- `aibrix/batch/constant.py` defines batch defaults such as pool size and timeouts

## Repo Conventions

- Use `aibrix.logger`; loggers are initialized through `init_logger` and support structlog
- Follow existing subsystem patterns before introducing new abstractions
- Prefer targeted tests for the subsystem you change, then run broader validation before finishing, using `poetry run pytest ...`

## Test Map

- `tests/batch/` covers batch APIs, driver behavior, persistence, RBAC, and worker integrations
- `tests/downloader/` covers backend-specific downloader behavior and related utilities
- `tests/metadata/` covers metadata APIs, secrets, and integration flows
- `tests/metrics/` covers metrics behavior across engine modes
- `tests/runtime/` covers runtime downloader and artifact-service behavior
- `tests/storage/` covers storage backends and shared storage utilities
- `tests/gpu_optimizer/` covers optimizer and benchmark-related logic
- `tests/e2e/` contains end-to-end API coverage

## Change Checklist

- After every code modification, run `poetry run bash ./scripts/format.sh` from the `python/aibrix` repository root (`/Users/bytedance/Studio/aibrix/python/aibrix`)
- Run the most relevant `poetry run pytest ...` targets for the touched subsystem
- Run full `poetry run pytest` when changes cross subsystem boundaries or affect shared infrastructure
- Update affected docs, config wiring, or CLI entry points when behavior changes

## Codebase Freshness Rule

Do not assume the codebase is unchanged between tasks, even within the same session.

Before making edits or reasoning about behavior:
- Re-open the relevant files to confirm their current contents and surrounding context.
- Re-verify key call sites, signatures, and invariants through code reading or repository history.
- Use Git tools (e.g., diff/blame/log/show) to confirm what changed since the last task and to validate any assumptions that depend on prior state.

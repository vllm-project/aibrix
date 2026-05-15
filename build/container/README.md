# AIBrix KVCache-Enabled vLLM & SGLang Images

This directory contains Dockerfiles that build **vLLM** and **SGLang** images
enhanced with AIBrix capabilities:

- **aibrix_kvcache** - Built from source for KV cache disaggregation
- **nixl + nixl-cu12** - UCX-based high-performance networking libraries
- **UCX tooling** - Pre-installed debugging and performance testing utilities

## Image Naming Convention

**Upstream vs. AIBrix Images:**

| Upstream Image | AIBrix Enhanced Image                                          | Description |
|----------------|----------------------------------------------------------------|-------------|
| `vllm/vllm-openai:v0.10.2` | `aibrix/vllm-openai:v0.10.2-aibrix-v0.5.0-nixl-0.7.1-20251123` | vLLM base + AIBrix KVCache + UCX/NIXL networking |
| `lmsysorg/sglang:v0.5.5.post3` | `aibrix/sglang:v0.5.5.post3-aibrix-v0.5.0-nixl-0.7.1-20251123` | SGLang base + AIBrix KVCache + UCX/NIXL networking |

**AIBrix images** extend upstream inference engines with:
- Distributed KV cache support via `aibrix_kvcache`
- RDMA-capable networking through NIXL/UCX for disaggregated inference
- Compatible torch versions automatically derived from base images

## Compatibility Matrix

Default build arguments produce the following component versions:

| Component | vLLM Image | SGLang Image |
|-----------|-----------|--------------|
| Engine version | v0.10.2 | v0.5.5.post3 |
| Torch version | 2.8 | 2.9  |
| aibrix_kvcache | v0.5.0 | v0.5.0 |
| NIXL / CUDA plugin | 0.7.1 | 0.7.1 |
| UCX | 1.19.0 | 1.19.0 |

**Version Compatibility:**
- Torch version is automatically extracted from the upstream base image to ensure compatibility
- AIBrix KVCache is built against the exact torch version from the base image
- NIXL and UCX versions are pinned for stable RDMA networking

## Building the Images

### vLLM Image

```bash
docker build \
  -f Dockerfile.vllm \
  --build-arg VLLM_VERSION=v0.10.2 \
  --build-arg AIBRIX_BRANCH=v0.5.0 \
  --build-arg NIXL_VERSION=0.7.1 \
  -t aibrix/vllm-openai:v0.10.2-aibrix-v0.5.0-nixl-0.7.1-$(date +'%Y%m%d') \
  .
```

### SGLang Image

```bash
docker build \
  -f Dockerfile.sglang \
  --build-arg SGLANG_VERSION=v0.5.5.post3 \
  --build-arg AIBRIX_BRANCH=v0.5.0 \
  --build-arg NIXL_VERSION=0.7.1 \
  -t aibrix/sglang:v0.5.5.post3-aibrix-v0.5.0-nixl-0.7.1-$(date +'%Y%m%d') \
  .
```

### Build Arguments

All build arguments are optional and have sensible defaults:

| Argument | Default | Description |
|----------|---------|-------------|
| `VLLM_VERSION` | `v0.10.2` | vLLM upstream version to use as base |
| `SGLANG_VERSION` | `v0.5.5.post3` | SGLang upstream version to use as base |
| `AIBRIX_BRANCH` | `v0.5.0` | AIBrix release tag or branch to build from |
| `NIXL_VERSION` | `0.7.1` | NIXL networking library version |
| `AIBRIX_REPO` | `https://github.com/vllm-project/aibrix` | AIBrix repository URL |

## Release History

AIBrix maintains stable image releases with tested component combinations:

### v0.5.0 (Current)

| Component      | vLLM    | SGLang       | Notes                           |
|----------------|---------|--------------|---------------------------------|
| Engine         | v0.10.2 | v0.5.5.post3 | Stable inference engines        |
| CUDA           | 12.8    | 12.9         | CUDA Version                    |
| Torch          | 2.8     | 2.9          | PyTorch Version                 |
| AIBrix KVCache | v0.5.0  | v0.5.0       | KV cache disaggregation support |
| NIXL           | 0.7.1   | 0.7.1        | UCX-based RDMA networking       |
| UCX            | 1.19.0  | 1.19.0       | Pre-installed for debugging     |

**Recommended Tags:**
- `aibrix/vllm-openai:v0.10.2-aibrix-v0.5.0-nixl-0.7.1-20251123`
- `aibrix/sglang:v0.5.5.post3-aibrix-v0.5.0-nixl-0.7.1-20251123`

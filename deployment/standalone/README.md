# AIBrix Docker Compose

Simplified single-node AIBrix deployment without Kubernetes complexity. Perfect for development, testing, and single-GPU/multi-GPU inference workloads.

## Features

- **OpenAI-Compatible API** - Drop-in replacement for OpenAI API
- **Envoy Proxy** - Production-grade L7 proxy with health checks, retries, and circuit breaking
- **vLLM Backend** - High-performance LLM inference engine
- **P/D Disaggregation** - Optional separate prefill/decode engines for multi-GPU setups
- **Metadata Service** - Model registry and file management
- **Redis** - Shared state storage

## Quick Start

### 1. Prerequisites

- Docker with Compose v2
- NVIDIA Container Toolkit (for GPU support)
- NVIDIA GPU with sufficient VRAM (16GB+ recommended)

### 2. Configure

```bash
cd docker-compose

# Copy and edit the configuration
cp .env.example .env

# Edit with your settings
# Required: MODEL_NAME, HF_TOKEN (for gated models like Llama)
vim .env
```

### 3. Start

```bash
# Simple mode (single vLLM engine)
./start.sh

# Or with P/D disaggregation (requires 2+ GPUs)
./start.sh --pd
```

### 4. Test

```bash
# Check health
curl http://localhost/health

# List models
curl http://localhost/v1/models

# Chat completion
curl http://localhost/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "meta-llama/Llama-3.1-8B-Instruct",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

## Architecture

### Simple Mode (Default)

```
                    ┌─────────────┐
                    │   Client    │
                    └──────┬──────┘
                           │
                           ▼
                    ┌─────────────┐
                    │ Envoy (:80) │  L7 Proxy
                    └──────┬──────┘
                           │
           ┌───────────────┼───────────────┐
           │               │               │
           ▼               ▼               ▼
    ┌─────────────┐ ┌─────────────┐ ┌─────────────┐
    │    vLLM     │ │  Metadata   │ │    Redis    │
    │   (:8000)   │ │   (:8090)   │ │   (:6379)   │
    └─────────────┘ └─────────────┘ └─────────────┘
        Inference    Model Registry  State Storage
```

### P/D Disaggregation Mode

```
                    ┌─────────────┐
                    │   Client    │
                    └──────┬──────┘
                           │
                           ▼
                    ┌─────────────┐
                    │ Envoy (:80) │
                    └──────┬──────┘
                           │
           ┌───────────────┼───────────────┐
           │               │               │
           ▼               ▼               ▼
    ┌─────────────┐ ┌─────────────┐ ┌─────────────┐
    │   Prefill   │ │   Decode    │ │  Metadata   │
    │   (:8001)   │ │   (:8002)   │ │   (:8090)   │
    └─────────────┘ └─────────────┘ └─────────────┘
       GPU 0 (KV)     GPU 1 (Gen)    Model Registry
```

## Configuration

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `MODEL_NAME` | `meta-llama/Llama-3.1-8B-Instruct` | HuggingFace model ID |
| `MODEL_DIR` | `~/.cache/huggingface` | Model cache directory |
| `HF_TOKEN` | - | HuggingFace token (required for gated models) |
| `VLLM_GPU` | `0` | GPU ID for simple mode |
| `PREFILL_GPU` | `0` | GPU ID for prefill engine (P/D mode) |
| `DECODE_GPU` | `1` | GPU ID for decode engine (P/D mode) |
| `MAX_MODEL_LEN` | `8192` | Maximum context length |
| `GPU_MEMORY_UTILIZATION` | `0.9` | GPU memory fraction to use |
| `HTTP_PORT` | `80` | HTTP entry point port |
| `VLLM_PORT` | `8000` | Direct vLLM access port |

See `.env.example` for all available options.

## Services

| Service | Port | Description |
|---------|------|-------------|
| `envoy` | 80, 9901 | HTTP proxy (80) and admin interface (9901) |
| `vllm` | 8000 | vLLM inference engine (simple mode) |
| `prefill-engine` | 8001 | Prefill engine (P/D mode) |
| `decode-engine` | 8002 | Decode engine (P/D mode) |
| `metadata-service` | 8090 | Model registry and file management |
| `redis` | 6379 | State storage |

## Endpoints

### Inference API (OpenAI-Compatible)

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/v1/chat/completions` | POST | Chat completion (streaming supported) |
| `/v1/completions` | POST | Text completion |
| `/v1/embeddings` | POST | Text embeddings |
| `/v1/models` | GET | List available models |

### Health & Monitoring

| Endpoint | Description |
|----------|-------------|
| `/health` | vLLM health check |
| `/healthz` | Envoy health check |
| `/v1/models` | Model availability |
| `:9901/` | Envoy admin dashboard |
| `:9901/stats` | Envoy statistics |
| `:9901/clusters` | Upstream cluster health |

## Commands

### Start/Stop

```bash
# Start (simple mode)
docker compose up -d

# Start (P/D mode)
docker compose --profile pd up -d

# Stop
docker compose down

# Stop and remove volumes
docker compose down -v
```

### Logs

```bash
# All services
docker compose logs -f

# Specific service
docker compose logs -f vllm

# Last 100 lines
docker compose logs --tail=100 vllm
```

### Status

```bash
# Service status
docker compose ps

# Resource usage
docker stats
```

### Scaling

```bash
# Note: Scaling requires additional configuration
# Each replica needs its own GPU
docker compose up -d --scale vllm=2
```

## Troubleshooting

### GPU Not Found

```bash
# Check NVIDIA driver
nvidia-smi

# Check Docker GPU access
docker run --rm --gpus all nvidia/cuda:12.0-base nvidia-smi

# Install NVIDIA Container Toolkit
# https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/install-guide.html
```

### Model Not Loading

```bash
# Check vLLM logs
docker compose logs vllm

# Verify model path
docker compose exec vllm ls -la /root/.cache/huggingface

# Check GPU memory
nvidia-smi

# Try smaller context length
# Edit .env: MAX_MODEL_LEN=4096
```

### Connection Errors

```bash
# Check Envoy health
curl http://localhost:9901/ready

# Check upstream clusters
curl http://localhost:9901/clusters

# View Envoy logs
docker compose logs envoy

# Test direct vLLM access
curl http://localhost:8000/health
```

### Out of Memory

```bash
# Reduce GPU memory utilization
# Edit .env: GPU_MEMORY_UTILIZATION=0.8

# Reduce context length
# Edit .env: MAX_MODEL_LEN=4096

# Use a smaller model
# Edit .env: MODEL_NAME=meta-llama/Llama-3.2-3B-Instruct
```

## Comparison: Docker Compose vs Kubernetes

| Feature | Docker Compose | Kubernetes |
|---------|----------------|------------|
| Setup Complexity | Simple | Complex |
| Multi-node | No | Yes |
| Auto-scaling | No | Yes |
| Service Discovery | Static | Dynamic |
| Load Balancing | Envoy | Gateway API |
| Best For | Development, Single-node | Production, Multi-node |

For production multi-node deployments, use the Helm chart:
```bash
helm install aibrix ./dist/chart
```

## File Structure

```
standalone/
├── docker-compose.yml     # Service definitions
├── .env.example           # Configuration template
├── start.sh               # Startup script
├── configs/
│   └── envoy.yaml         # Envoy config with ext_proc for gateway-plugin
└── README.md              # This file
```

## Upgrading

```bash
# Pull latest images
docker compose pull

# Restart with new images
docker compose up -d
```

## License

Apache 2.0 - See [LICENSE](../LICENSE) for details.

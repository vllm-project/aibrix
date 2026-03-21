# AIBrix - Local Mode

Run the AIBrix gateway (Envoy + gateway-plugin) as two bare processes without Docker or Kubernetes - just native binaries with static config to discover vLLM engine instances directly.

This is ideal for:
- Local development and debugging of routing algorithms
- Single-node testing without container overhead
- Quick validation of gateway behavior

> Note: AIBrix controller orchestration is not included in the local Mode, they can not run without Kubernetes.

## Architecture

```
                                 ┌────────────────────────┐
  curl :10080                    │    gateway-plugin      │
       │                         │    (gRPC :50052)       │
       ▼                         │                        │
┌─────────────┐  ext_proc gRPC   │  --standalone          │
│   Envoy     │ ───────────────► │  --endpoints-config    │
│  (:10080)   │                  │                        │
│             │ ◄─ target-pod ── │  selects best backend  │
│  ORIGINAL   │    header        └────────────────────────┘
│  _DST       │
│  cluster    │ ──── route to ──► vLLM engine(s)
└─────────────┘    selected IP    (e.g., 127.0.0.1:8000)
```

Envoy receives HTTP requests and forwards headers/body to the gateway-plugin via ext_proc. The plugin extracts the model name, looks up available backends from `endpoints.yaml`, selects the best one using the configured routing algorithm, and returns the target address via the `target-pod` header. Envoy then routes the request to that address using an `ORIGINAL_DST` cluster.

## Prerequisites

### 1. Install Go (1.22+)

**Linux:**
```bash
wget https://go.dev/dl/go1.22.5.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.22.5.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc
go version
```

**macOS:**
```bash
brew install go
```

### 2. Build the gateway-plugin binary

```bash
make build-gateway-plugins-nozmq
```

This produces `bin/gateway-plugins` - a pure Go binary (no ZMQ/CGO dependencies).

### 3. Install Envoy

**Linux (x86_64):**
```bash
ENVOY_VERSION=1.37.1
wget -O envoy https://github.com/envoyproxy/envoy/releases/download/v${ENVOY_VERSION}/envoy-${ENVOY_VERSION}-linux-x86_64
chmod +x envoy
sudo mv envoy /usr/local/bin/
envoy --version
```

**macOS:**
```bash
brew install envoy
```

### 4. Start your vLLM engine

```bash
# Example: run vLLM on port 8000
vllm serve Qwen/Qwen3.5-4B --port 8000
```

### 5. (Optional) Redis

Redis is **not required** in local mode. Without Redis, rate limiting is disabled but routing works normally.

If you want rate limiting:
```bash
redis-server
```

## Quick Start

The `run-local.sh` and `stop-local.sh` scripts are **Linux-only** (they use `setsid`, `ss`, etc.). On macOS, start the two processes manually:

```bash
# macOS: start manually
bin/gateway-plugins --standalone --endpoints-config=deployment/local/configs/endpoints.yaml &
envoy -c deployment/local/configs/envoy.yaml --use-dynamic-base-id --log-level warn &
```

**Linux:**

```bash
cd deployment/local

# Edit endpoints to match your vLLM setup
vim configs/endpoints.yaml

# Start
./run-local.sh

# Test
curl http://localhost:10080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "Qwen/Qwen3.5-4B",
    "messages": [{"role": "user", "content": "Hello"}]
  }'

# Stop
./stop-local.sh
```

## Configuration

### Endpoints (`configs/endpoints.yaml`)

Define your vLLM backend addresses:

```yaml
# Single backend
models:
  - name: "Qwen/Qwen3.5-4B"
    endpoints:
      - "127.0.0.1:8000"

# Multiple backends (gateway routes across them)
models:
  - name: "Qwen/Qwen2.5-1.5B-Instruct"
    endpoints:
      - "192.168.1.10:8000"
      - "192.168.1.11:8000"

# P/D disaggregated serving
models:
  - name: "Qwen/Qwen2.5-72B"
    rolesets:
      - name: default
        prefill:
          - "192.168.1.10:8000"
        decode:
          - "192.168.1.11:8000"
```

### Routing Algorithm

Set via environment variable before starting:

```bash
ROUTING_ALGORITHM=round_robin ./run-local.sh
```

Available algorithms: `random`, `round_robin`, `least_request`, `prefix_cache_aware`, etc.

### Custom configs

```bash
./run-local.sh -e /path/to/my-endpoints.yaml -c /path/to/my-envoy.yaml
```

## Endpoints

| Endpoint | Port | Description |
|----------|------|-------------|
| HTTP API | 10080 | Send inference requests here |
| Envoy Admin | 9901 | Envoy admin interface (stats, config dump) |
| Gateway Metrics | 8080 | Prometheus metrics from gateway-plugin |
| Health Check | 10080/healthz | Envoy health |

## Logs

```bash
tail -f deployment/local/logs/gateway-plugin.log
tail -f deployment/local/logs/envoy.log
```

## Troubleshooting

**"no healthy upstream"** - The vLLM backend is not reachable at the address in `endpoints.yaml`. Verify the engine is running and the address is correct.

**"ext_proc gRPC error"** - The gateway-plugin is not running or crashed. Check `logs/gateway-plugin.log`.

**Envoy won't start** - Check `logs/envoy.log`. Common issue: port conflict on 10080 or 9901.

**Routing not working** - Check that the model name in your request matches exactly what's in `endpoints.yaml`.

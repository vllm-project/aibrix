# vLLM P/D Disaggregation Examples

Note:
- The examples in this directory are for demonstration purposes only. Feel free to use your own model path, rdma network, image etc instead.
- Two routing strategies are supported:
  - Static routing using the upstream vLLM sample router (disagg_proxy_server.py) — suitable for quick testing.
  - Dynamic routing via AIBrix Envoy Gateway with `routing-strategy: pd`.
- In order to use this example, you need to build your own vLLM image with Nixl. Guidance is provided.
- AIBrix storm service support replica mode and pool mode, please refer to [AIBrix storm service]( mode, please refer to [AIBrix storm service](https://aibrix.readthedocs.io/latest/designs/aibrix-stormservice.html) for more details.
- `vke.volcengine.com/rdma`, `k8s.volcengine.com/pod-networks` and `NCCL_IB_GID_INDEX` are specific to Volcano Engine Cloud. Feel free to customize it for your own cluster.

## Configuration

### Build vLLM images with Nixl Support

```Dockerfile
# Build arguments to enable flexible base image and nixl version.
# Override at build time with --build-arg if needed.

# Default vLLM OpenAI-compatible image tag.
# Available tags: https://hub.docker.com/r/vllm/vllm-openai/tags
ARG VLLM_BASE_TAG="v0.9.2"

# Optional nixl version. If left empty, the latest version will be installed.
ARG NIXL_VER=""

# Base image from official vLLM project.
# Available tags can be found at:
#   https://hub.docker.com/r/vllm/vllm-openai/tags
# This image is also mirrored/extended under:
#   https://hub.docker.com/r/aibrix/vllm-openai/tags
FROM vllm/vllm-openai:${VLLM_BASE_TAG}

# Install nixl package:
# - If NIXL_VER is specified, install exact version: nixl==<version>
# - Otherwise, install the latest available version.
RUN if [ -n "${NIXL_VER}" ]; then \
        echo "Installing nixl==${NIXL_VER}..."; \
        pip install "nixl==${NIXL_VER}"; \
    else \
        echo "Installing latest nixl..."; \
        pip install nixl; \
    fi
```

```bash
# 1. Build with default vLLM tag and pinned nixl version
docker build \
  --build-arg NIXL_VER=0.4.1 \
  -t aibrix/vllm-openai:v0.9.2-nixl-v0.4.1 .

# 2. Build with custom vLLM tag and latest nixl
docker build \
  --build-arg VLLM_BASE_TAG=v0.10.2 \
  -t aibrix/vllm-openai:v0.10.0-nixl-latest .
```

> Note: sample router has been included in the image. We do not need additional steps to build it. 

## Option 1: Static Routing with disagg_proxy_server.py (Legacy)

### Start the Router

Currently, the router is very simple and it relies on user to pass the prefill and decode IPs.
Launch the router `kubectl apply -f router.yaml`, ssh to the pod and launch the process.

Copy `disagg_proxy_server.py` into the container.

```bash
python3 disagg_proxy_server.py \
    --host localhost \
    --port 8000 \
    --prefiller-host 192.168.0.125,192.168.0.127 \
    --prefiller-port 8000 \
    --decoder-host 192.168.0.129,192.168.0.149 \
    --decoder-port 8000
```

### Run Query Example inside the Router

```bash
curl http://localhost:8000/v1/chat/completions \
-H "Content-Type: application/json" \
-d '{
    "model": "qwen3-8B",
    "messages": [
        {"role": "system", "content": "You are a helpful assistant."},
        {"role": "user", "content": "help me write a random generator in python"}
    ]
}'
```

```bash
curl http://localhost:8000/v1/completions \
    -H "Content-Type: application/json" \
    -d '{
        "model": "qwen3-8B",
        "prompt": "San Francisco is a",
        "max_tokens": 128,
        "temperature": 0
    }'
```
---

## Option 2: Dynamic P/D Routing with AIBrix Gateway

AIBrix now supports **native Prefill/Decode disaggregation** through its Envoy Gateway integration. By setting the `routing-strategy: pd` header, requests are automatically split between prefill and decode pods for optimal performance.


### Get Gateway Endpoint

```bash
LB_IP=$(kubectl get svc -n envoy-gateway-system \
  envoy-aibrix-system-aibrix-eg-903790dc \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

ENDPOINT="${LB_IP}:80"
echo "Gateway endpoint: http://${ENDPOINT}"
```

> The service name may vary, use `kubectl get svc -n envoy-gateway-system` to confirm.

### Send a Disaggregated Request

```bash
curl -v "http://${ENDPOINT}/v1/chat/completions" \
  -H "routing-strategy: pd" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen3-8B",
    "messages": [{"role": "user", "content": "Say this is a test!"}],
    "temperature": 0.7
  }'
```

> **Requirements**:
> - The `model` field must exactly match the label `model.aibrix.ai/name` in your deployment.
> - The `routing-strategy: pd` header **must be present** to enable P/D splitting.


# SGLang P/D Disaggregation Examples

Note: 
- The examples in this directory are for demonstration purposes only. Feel free to use your own model path, rdma network, image etc instead.
- - Two routing strategies are supported:
  - Routing solution in the examples uses SGlang upstream router `aibrix/sglang-router:v0.1.6` images.
  - Dynamic routing via AIBrix Envoy Gateway with `routing-strategy: pd`.
- SGLang images are built with nixl, if you want to use mooncake-transfer-engine, please change configuration to `--disaggregation-transfer-backend=mooncake`. Note, mooncake requires RDMA to work.
- AIBrix storm service support replica mode and pool mode, please refer to [AIBrix storm service]( mode, please refer to [AIBrix storm service](https://aibrix.readthedocs.io/latest/designs/aibrix-stormservice.html) for more details.
- `vke.volcengine.com/rdma`, `k8s.volcengine.com/pod-networks` and `NCCL_IB_GID_INDEX` are specific to Volcano Engine Cloud. Feel free to customize it for your own cluster.


## Configuration

### Build SGLang images with Nixl Support

```Dockerfile
# Build arguments for flexible base image and nixl version.
# These can be overridden at build time with --build-arg.

# Default SGLang base image tag (matches tags from official repos)
ARG SGLANG_BASE_TAG="v0.4.9.post3-cu126"

# Optional nixl version. Leave empty to install the latest available version.
ARG NIXL_VER=""

# Base image from the official SGLang project.
# Available tags can be found at:
#   https://hub.docker.com/r/lmsysorg/sglang/tags
# This image is also mirrored/extended under:
#   https://hub.docker.com/r/aibrix/sglang/tags
FROM lmsysorg/sglang:${SGLANG_BASE_TAG}

# Install nixl package.
# - If NIXL_VER is provided (non-empty), install the exact version: nixl==<version>
# - If NIXL_VER is empty or unset, install the latest version: nixl
RUN if [ -n "${NIXL_VER}" ]; then \
        echo "Installing nixl==${NIXL_VER}..."; \
        pip install "nixl==${NIXL_VER}"; \
    else \
        echo "Installing latest nixl..."; \
        pip install nixl; \
    fi
```

```bash
# 1. Pin both SGLang and nixl versions
docker build \
  --build-arg SGLANG_BASE_TAG=v0.4.9.post3-cu126 \
  --build-arg NIXL_VER=0.4.1 \
  -t aibrix/sglang:v0.4.9.post3-cu126-nixl-v0.4.1 .

# 1. Use a newer SGLang version with latest nixl
docker build \
  --build-arg SGLANG_BASE_TAG=v0.5.0-cu126 \
  -t aibrix/sglang:v0.5.5.post3-nixl-latest .
```

### Build SGLang Router Images

```Dockerfile
FROM python:3.10.12

RUN pip install sglang-router==0.1.6
```

```bash
docker build -t aibrix/sglang-router:v0.1.6 -f Dockerfile.router .
```

### RBAC required in SGlang Router

Please apply the following RBAC rules in your cluster and make sure sglang-router uses `default` service account.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: pod-read
rules:
- apiGroups:
  - ""
  resources:
  - pods
  verbs:
  - get
  - watch
  - list
```

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: pod-read-binding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: pod-read
subjects:
- kind: ServiceAccount
  name: default
  namespace: default
```

### Run Query Example inside the Router 

```bash
curl http://localhost:30000/v1/chat/completions \
-H "Content-Type: application/json" \
-d '{
    "model": "/models/Qwen2.5-7B-Instruct",
    "messages": [
        {"role": "system", "content": "You are a helpful assistant."},
        {"role": "user", "content": "help me write a random generator in python"}
    ]
}'
```

### Dynamic P/D Routing with AIBrix Gateway

#### Get Gateway Endpoint

```bash
LB_IP=$(kubectl get svc -n envoy-gateway-system \
  envoy-aibrix-system-aibrix-eg-903790dc \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

ENDPOINT="${LB_IP}:80"
echo "Gateway endpoint: http://${ENDPOINT}"
```

> The service name may vary, use `kubectl get svc -n envoy-gateway-system` to confirm.

#### Send a Disaggregated Request

```bash
curl -v "http://${ENDPOINT}/v1/chat/completions" \
  -H "routing-strategy: pd" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "/models/Qwen2.5-7B-Instruct",
    "messages": [{"role": "user", "content": "Say this is a test!"}],
    "temperature": 0.7
  }'
```

> **Requirements**:
> - The `model` field must exactly match the label `model.aibrix.ai/name` in your deployment.
> - The `routing-strategy: pd` header **must be present** to enable P/D splitting.

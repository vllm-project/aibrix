# AIBrix v0.4.0 Regression Tests

Streamlined Helm templates for AIBrix regression testing with clean resource naming. All deployments use **StormService** consistently.

## Structure

```
v0.4.0/
├── Chart.yaml              # Helm chart metadata
├── values.yaml             # Default values with common settings
├── templates/              # Helm templates (4 engine-specific templates)
│   ├── _helpers.tpl              # Template helper functions
│   ├── sglang-disaggregated.yaml      # SGLang with prefill/decode roles + router
│   ├── sglang-non-disaggregated.yaml  # SGLang with single server role
│   ├── vllm-disaggregated.yaml        # VLLM with prefill/decode roles
│   └── vllm-non-disaggregated.yaml    # VLLM with single server role
└── configs/                # Base experiment configurations
    ├── sglang-disagg-base.yaml     # SGLang disaggregated defaults
    ├── sglang-non-disagg-base.yaml # SGLang non-disaggregated defaults
    ├── vllm-disagg-base.yaml       # VLLM disaggregated defaults
    └── vllm-non-disagg-base.yaml   # VLLM non-disaggregated defaults
```

## Usage

Deploy experiments using descriptive release names with base configs and `--set` overrides:

### SGLang Examples

```bash
# SGLang 1p1d disaggregated with sgl-router cache-aware routing on Qwen3-8B
helm install sglang-qwen3-8b-disagg-1p1d-sgl-router-cache . --values configs/sglang-disagg-base.yaml

# SGLang 2p1d with random routing on Qwen3-8B  
helm install sglang-qwen3-8b-disagg-2p1d-sgl-router-random . --values configs/sglang-disagg-base.yaml \
  --set workers.prefill.replicas=2 \
  --set router.policy=random

# SGLang 4p3d setup on Qwen3-8B, using aibrix router
helm install sglang-qwen3-8b-disagg-4p3d-aibrix-router-pd . --values configs/sglang-disagg-base.yaml \
  --set workers.prefill.replicas=4 \
  --set workers.decode.replicas=3

# SGLang non-disaggregated 3-replica setup on Qwen3-8B
helm install sglang-qwen3-8b-non-disagg-3-aibrix-least-request . --values configs/sglang-non-disagg-base.yaml \
  --set deployment.replicas=3

# SGLang on Qwen3-32B with tensor parallelism (2 GPUs per worker)
helm install sglang-qwen3-32b-disagg-1p1d-sgl-router-random . --values configs/sglang-disagg-base.yaml \
  --set model.name=qwen3-32b \
  --set model.path=/models/Qwen3-32B \
  --set model.tensorParallel=2 \
  --set resources.gpu=2
```

### VLLM Examples

```bash
# VLLM 1p1d disaggregated on Qwen3-8B (default)
helm install vllm-qwen3-8b-disagg-1p1d-vllm-router-random . --values configs/vllm-disagg-base.yaml

# VLLM 2p1d disaggregated on Qwen3-8B
helm install vllm-qwen3-8b-disagg-2p1d-vllm-router-random . --values configs/vllm-disagg-base.yaml \
  --set workers.prefill.replicas=2

# VLLM non-disaggregated high availability (5 replicas) 
helm install vllm-qwen3-8b-non-disagg-5-aibrix-httproute . --values configs/vllm-non-disagg-base.yaml \
  --set deployment.replicas=5

# VLLM on Qwen3-32B with tensor parallelism
helm install vllm-qwen3-32b-disagg-1p1d-aibrix-httproute . --values configs/vllm-disagg-base.yaml \
  --set model.name=qwen3-32b \
  --set model.path=/models/Qwen3-32B \
  --set model.tensorParallel=2 \
  --set resources.gpu=2
```

### Cleanup specific experiment

```bash
helm uninstall sglang-qwen3-8b-disagg-1p1d-cache-aware
```

## Base Configurations

Use **4 base configs** with `helm --set` overrides to avoid config duplication:

| Base Config | Engine | Architecture | Key Override Parameters |
|-------------|--------|--------------|------------------------|
| `sglang-disagg-base.yaml` | SGLang | Disaggregated | `workers.prefill.replicas`, `workers.decode.replicas`, `router.policy` |
| `sglang-non-disagg-base.yaml` | SGLang | Non-disaggregated | `deployment.replicas` |
| `vllm-disagg-base.yaml` | VLLM | Disaggregated | `workers.prefill.replicas`, `workers.decode.replicas` |
| `vllm-non-disagg-base.yaml` | VLLM | Non-disaggregated | `deployment.replicas` |

### All configs support common overrides:
- `model.name`, `model.path`, `model.tensorParallel`
- `resources.gpu`, `resources.rdma`
- `engine.image` (custom image override)

## Parameter Reference

### Model Configuration
| Parameter | Values | Description |
|-----------|--------|-------------|
| `model.name` | `qwen3-8b`, `qwen3-32b` | Model identifier |
| `model.path` | `/models/Qwen3-8B`, `/models/Qwen3-32B` | Path to model files |
| `model.tensorParallel` | 1, 2, 4 | Tensor parallelism degree (32B models need 2+) |

### Resource Configuration  
| Parameter | Values | Description |
|-----------|--------|-------------|
| `resources.gpu` | 1, 2, 4 | GPUs per worker (should match tensorParallel) |
| `resources.rdma` | 1, `""` | RDMA resources (empty string disables) |
| `resources.memory.sharedMemory` | `true`, `false` | Enable shared memory volume |

### Architecture Configuration
| Parameter | Values | Description |
|-----------|--------|-------------|
| `workers.prefill.replicas` | 1, 2, 4, 8 | Prefill workers (disaggregated only) |
| `workers.decode.replicas` | 1, 2, 3 | Decode workers (disaggregated only) |
| `deployment.replicas` | 1, 2, 3, 5 | Server replicas (non-disaggregated only) |
| `router.policy` | `cache_aware`, `random` | SGLang routing policy |

### Image Configuration
| Parameter | Values | Description |
|-----------|--------|-------------|
| `engine.image` | Full image path | Override engine image (auto-selected if empty) |
| `router.image` | Full image path | Override router image |

## Resource Naming

The release name becomes the Kubernetes resource name directly:

```bash
helm install sglang-qwen3-8b-disagg-1p1d-sgl-router-random . --values configs/sglang-disagg-base.yaml
# Creates StormService: sglang-qwen3-8b-disagg-1p1d-sgl-router-random

helm install my-experiment . --values configs/vllm-non-disagg-base.yaml
# Creates StormService: my-experiment
```

### Naming Convention Suggestions

For systematic testing, use descriptive names with this pattern:
`{engine}-{model}-{architecture}-{replicas}-{router}`

**Examples**:
- `sglang-qwen3-8b-disagg-1p1d-sgl-router-cache`
- `sglang-qwen3-32b-disagg-2p2d-sgl-router-random`  
- `vllm-qwen3-8b-non-disagg-5-aibrix-httproute`
- `vllm-qwen3-32b-disagg-1p1d-aibrix-pd`

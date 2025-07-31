# vLLM Remote Tokenizer Feature

This feature enables model-aware remote tokenizer support for vLLM inference engines in AIBrix gateway.

## Quick Start

Enable vLLM remote tokenizer with one command:

```bash
kubectl apply -k config/features/vllm-remote-tokenizer/
```

## Configuration

The following environment variables are configured:

| Variable | Default | Description |
|----------|---------|-------------|
| AIBRIX_ENABLE_VLLM_REMOTE_TOKENIZER | false | Enable remote tokenizer feature |
| AIBRIX_VLLM_TOKENIZER_ENDPOINT_TEMPLATE | http://%s:8000 | URL template for vLLM endpoints |
| AIBRIX_TOKENIZER_HEALTH_CHECK_PERIOD | 30s | Health check interval |
| AIBRIX_TOKENIZER_TTL | 5m | Tokenizer cache TTL |
| AIBRIX_MAX_TOKENIZERS_PER_POOL | 100 | Maximum tokenizers per pool |
| AIBRIX_TOKENIZER_REQUEST_TIMEOUT | 10s | Request timeout |

## Customization

To use custom values, copy this directory and modify `gateway-plugins-env-patch.yaml`:

```bash
cp -r config/features/vllm-remote-tokenizer/ config/features/my-vllm-config/
# Edit config/features/my-vllm-config/gateway-plugins-env-patch.yaml
kubectl apply -k config/features/my-vllm-config/
```

## Enable the Feature

To enable vLLM remote tokenizer after installation:

```bash
kubectl set env deployment/gateway-plugins -n aibrix-system AIBRIX_ENABLE_VLLM_REMOTE_TOKENIZER=true
```

Or use a custom Kustomization overlay with the environment variable set to `true`.

## Disable

To disable, set the environment variable to false:

```bash
kubectl set env deployment/gateway-plugins -n aibrix-system AIBRIX_ENABLE_VLLM_REMOTE_TOKENIZER=false
```

## Verification

Check if enabled:

```bash
kubectl get deployment gateway-plugins -n aibrix-system -o json | \
  jq '.spec.template.spec.containers[0].env[] | select(.name | startswith("AIBRIX_ENABLE_VLLM"))'
```

Check metrics:

```bash
kubectl port-forward -n aibrix-system svc/gateway-plugins 8080:8080
curl http://localhost:8080/metrics | grep aibrix_tokenizer_pool
```
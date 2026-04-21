# Semantic Router Sample

This sample deploys semantic routing with a single entry model (`MoM`) that routes requests to:

- `qwen3-8b` for math-oriented prompts (`use_reasoning: true`)
- `llama3-8b-instruct` for business-oriented prompts (`use_reasoning: false`)

## 1) Create Hugging Face token secret

Set your token first, then create the secret:

```bash
export HF_TOKEN="<your-huggingface-token>"
kubectl create secret generic hf-token-secret \
  --from-literal=token="${HF_TOKEN}" \
  -n vllm-semantic-router-system
```

## 2) Apply Gateway API / Envoy resources

From the repository root:

```bash
kubectl apply -f samples/semantic-router/gwapi-resources.yaml
```

## 3) Deploy backend model services

These two model deployments are referenced by the router config (`*.default.svc.cluster.local`):

```bash
kubectl apply -f samples/semantic-router/models/llama3-8b-instruct.yaml
kubectl apply -f samples/semantic-router/models/qwen3-8b.yaml
```

## 4) Apply semantic-router manifests

```bash
kubectl apply -f samples/semantic-router/semantic-router-configmap.yaml
kubectl apply -f samples/semantic-router/semantic-router.yaml
```

## 5) Port-forward the Envoy service

```bash
export ENVOY_SERVICE=$(kubectl get svc -n envoy-gateway-system \
  --selector=gateway.envoyproxy.io/owning-gateway-namespace=aibrix-system,gateway.envoyproxy.io/owning-gateway-name=aibrix-eg \
  -o jsonpath='{.items[0].metadata.name}')

kubectl port-forward -n envoy-gateway-system "svc/${ENVOY_SERVICE}" 8080:80
```

## 6) Test semantic routing

### Math prompt -> `qwen3-8b`

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "MoM",
    "messages": [
      {"role": "user", "content": "What is the derivative of x^3 + 2x?"}
    ],
    "max_tokens": 100
  }'
```

### Business prompt -> `llama3-8b-instruct`

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "MoM",
    "messages": [
      {"role": "user", "content": "What are the key factors to consider when entering a new market?"}
    ],
    "max_tokens": 100
  }'
```
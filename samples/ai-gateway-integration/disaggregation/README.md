# Aibrix Integration with Envoy AI Gateway — P/D Disaggregation Guide

This guide demonstrates how to deploy **Qwen2-7B** with **Prefill/Decode (PD) disaggregation** using **Envoy AI Gateway**, the **Gateway API Inference Extension**.


## Project Structure

```bash
samples/qwen2-7b-pd-disaggregation/
├── gateway.yaml                        # GatewayClass + Gateway
├── aigatewayroute.yaml                 # AIGatewayRoute: routes by `x-ai-eg-model`
├── llm-d-inference-scheduler-epp.yaml  # EPP deployment (standalone scheduler)
├── qwen2-7b-inferencepool.yaml         # InferencePool + EPP + RBAC + ConfigMap
└── vllm-sim-pd-stormservice.yaml       # StormService: deploys prefill & decode pods
```

---

## Prerequisites

- Kubernetes cluster (**v1.29+ recommended**)
- `kubectl` configured
- Helm v3.8+
- Internet access to pull images from `docker.io`, `ghcr.io`, and GitHub

---

## Installation Steps

### 1. (Optional) Install Aibrix Custom Application

If you use an internal Aibrix Helm chart:

```bash
helm install aibrix dist/chart \
  -n aibrix-system --create-namespace \
  --set gateway.enable=false
```

> **Critical**: Set `gateway.enable: false` to avoid conflicts with the standalone Envoy AI Gateway data plane installed in Step 5.

```yaml
gateway:
  enable: false  # ← Must be false
```

---

### 2. Install AI Gateway CRDs

```bash
helm upgrade -i aieg-crd oci://docker.io/envoyproxy/ai-gateway-crds-helm \
  --version v0.0.0-latest \
  --namespace envoy-ai-gateway-system \
  --create-namespace
```

> [Official CRD Installation Guide](https://aigateway.envoyproxy.io/docs/getting-started/installation#step-1-install-ai-gateway-crds)

---

### 3. Install AI Gateway Controller

```bash
helm upgrade -i aieg oci://docker.io/envoyproxy/ai-gateway-helm \
  --version v0.0.0-latest \
  --namespace envoy-ai-gateway-system \
  --create-namespace

kubectl wait --timeout=2m -n envoy-ai-gateway-system deployment/ai-gateway-controller --for=condition=Available
```

> [Controller Installation Guide](https://aigateway.envoyproxy.io/docs/getting-started/installation#step-2-install-ai-gateway-resources)

---

### 4. Install Gateway API Inference Extension (EPP Framework)

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/v1.0.1/manifests.yaml
```

This installs:
- `InferencePool`, `InferenceObjective` CRDs
- Core controllers, webhooks, and RBAC

> [Inference Extension Guide](https://aigateway.envoyproxy.io/docs/capabilities/inference/httproute-inferencepool#step-1-install-gateway-api-inference-extension)

---

### 5. Install Envoy Gateway (Data Plane) with InferencePool Support

```bash
helm upgrade -i eg oci://docker.io/envoyproxy/gateway-helm \
  --version v0.0.0-latest \
  --namespace envoy-gateway-system \
  --create-namespace \
  -f https://raw.githubusercontent.com/envoyproxy/ai-gateway/main/manifests/envoy-gateway-values.yaml \
  -f https://raw.githubusercontent.com/envoyproxy/ai-gateway/main/examples/inference-pool/envoy-gateway-values-addon.yaml
```

> [Envoy Gateway + Addons](https://aigateway.envoyproxy.io/docs/getting-started/prerequisites#additional-features-rate-limiting-inferencepool-etc)

Wait for Envoy Gateway to be ready:
```bash
kubectl wait --timeout=2m -n envoy-gateway-system deployment/envoy-gateway --for=condition=Available
```

---

### 6. Deploy Qwen2-7B PD Disaggregation Stack

```bash
cd samples/ai-gateway-integration/disaggregation

# Deploy prefill/decode pods via StormService
kubectl apply -f vllm-sim-pd-stormservice.yaml

# Deploy EPP scheduler, RBAC, ConfigMap, and InferencePool
kubectl apply -f qwen2-7b-inferencepool.yaml
kubectl apply -f llm-d-inference-scheduler-epp.yaml

# Deploy Gateway and AIGatewayRoute
kubectl apply -f gateway.yaml
kubectl apply -f aigatewayroute.yaml
```

---

## Verify Deployment

### Check Pods

```bash
# Prefill & Decode Pods
$ kubectl get pods -l app=vllm-sim-pd
NAME                                      READY   STATUS    RESTARTS   AGE
vllm-sim-pd-roleset-xwr7t-decode-fvn7r    2/2     Running   0          34m
vllm-sim-pd-roleset-xwr7t-prefill-svnnr   1/1     Running   0          34m

# EPP Scheduler
$ kubectl get pods -l app=qwen2-7b-epp
NAME                            READY   STATUS    RESTARTS   AGE
qwen2-7b-epp-65b76fc64d-7z5qz   1/1     Running   0          34m
```

### Check CRDs

```bash
$ kubectl get InferencePool
NAME       AGE
qwen2-7b   5m

$ kubectl get InferenceObjective
NAME       INFERENCE POOL   PRIORITY   AGE
qwen2-7b   qwen2-7b         10         5m
```

### Check Envoy Gateway

```bash
$ kubectl get svc -n envoy-gateway-system
NAME                                         TYPE           CLUSTER-IP      EXTERNAL-IP   PORT(S)
envoy-default-aibrix-ai-gateway-588291e8     LoadBalancer   10.96.xxx.xxx   <pending>     80:3xxxx/TCP
```

---

## Test the Setup

### Port-forward to Gateway (for local testing)

```bash
kubectl port-forward -n envoy-gateway-system svc/envoy-default-aibrix-ai-gateway-588291e8 8080:80
```

### Send Inference Request

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "x-ai-eg-model: qwen2-7b" \
  -d '{
    "model": "qwen2-7b",
    "messages": [{"role": "user", "content": "Hello! How are you?"}],
    "temperature": 0.7
  }'
```


> The header `x-ai-eg-model: qwen2-7b` is matched by your `AIGatewayRoute`, which routes to the `qwen2-7b` `InferencePool`.  
> The **EPP scheduler** then intelligently selects **prefill** or **decode** endpoints based on request context.

---

## Architecture Highlights

| Component | Role |
|--------|------|
| **StormService** | Deploys labeled prefill (`role=prefill`) and decode (`role=decode`) pods |
| **InferencePool** | Selects all `app: vllm-sim-pd` pods; targets port `8000` (routing-sidecar) |
| **EPP Scheduler** | Uses plugins (`by-label`, `prefix-cache-scorer`) to route to optimal endpoint |
| **AIGatewayRoute** | Routes by custom header `x-ai-eg-model` |
| **Routing Sidecar** | On decode pods, proxies requests from `8000` → `8200` (vLLM engine) |

---

Sure! Here's the concise English version:

---

## LLM-D Inference Scheduler Overview

The [LLM-D Inference Scheduler](https://github.com/llm-d/llm-d-inference-scheduler) is a specialized **Endpoint Picker Plugin (EPP)** designed for **Prefill/Decode (P/D) disaggregated** LLM serving. It runs within Envoy AI Gateway and intelligently routes inference requests to the optimal backend pod based on request phase (prefill vs. decode), KV cache state, and Kubernetes labels.

Built on top of the **Gateway API Inference Extension (GIE)**, it adds LLM-specific optimizations like prefix-cache awareness and automatic P/D classification.

---

## EPP Configuration Overview

Routing behavior is defined via a `ConfigMap`:

- **`by-label`**: Filters pods by `role=prefill` or `role=decode`
- **`prefix-cache-scorer`**: Prioritizes decode endpoints that already cache parts of the prompt
- **`pd-profile-handler`**: Automatically detects request type and selects the appropriate scheduling profile
- **`prefill-header-handler`**: Treats a request as prefill when the `prefill-header-handler` header is set (typically to an `<ip>:<port>` address)
- **Two profiles**: Separate strategies for prefill and decode traffic, enabling efficient, cache-aware routing

This setup enables seamless P/D disaggregation with maximal cache reuse and minimal latency.

## References

- [Envoy AI Gateway](https://github.com/envoyproxy/ai-gateway)
- [Gateway API Inference Extension](https://github.com/kubernetes-sigs/gateway-api-inference-extension)
- [LLM-D Scheduler & PD Disaggregation](https://github.com/llm-d)

---

# Aibrix Integration with Envoy AI Gateway Deployment Guide

This guide walks you through deploying a multi-model AI inference gateway using **Envoy AI Gateway**, **Gateway API Inference Extension**, and custom Aibrix-branded routing rules.

### Project Structure

```bash
samples/ai-gateway-integration
├── gateway.yaml                  # GatewayClass + Gateway
├── aigatewayroute.yaml           # Multi-model routing rules (llama2-7b, mistral-7b)
├── envoy-gateway-inferencepool-rbac.yaml # Envoy Gateway access to InferencePool
├── llama-7b-inferencepool.yaml   # InferencePool + EPP for Llama2-7B
├── mistral-7b-inferencepool.yaml # InferencePool + EPP for Mistral-7B
├── llama-7b.yaml                 # Mock model llama-7b deployments
└── mistral-7b.yaml               # Mock model mistral-7b deployments
```

### Prerequisites
- Kubernetes cluster (v1.32+ recommended for Envoy AI Gateway v1.0.0)
- kubectl configured
- helm v3.8+
- Internet access to pull images from docker.io and GitHub

### Installation Steps

1. Install Aibrix Custom Application (Optional)

If you have an internal Aibrix [Helm chart](../../dist/chart):
```bash
helm install aibrix dist/chart \
  -n aibrix-system --create-namespace \
  --set gateway.enable=false
```

> **Note**: If you are using an internal Aibrix Helm chart, **you must set `gateway.enable: false`** in `values.yaml`.  
> This is critical because **Steps 2–5 below will install the AI Gateway controller and Envoy data plane independently**. 
> Enabling the built-in gateway here would cause resource conflicts or duplicate deployments.

```yaml
...
gateway:
  enable: false  # ← Set this to false to skip internal gateway deployment
...
```

2. Install AI Gateway CRDs

```bash
helm upgrade -i aieg-crd oci://docker.io/envoyproxy/ai-gateway-crds-helm \
  --version v1.0.0 \
  --namespace envoy-ai-gateway-system \
  --create-namespace
```

> For more details, see the official [installation guide](https://aigateway.envoyproxy.io/docs/getting-started/installation#step-1-install-ai-gateway-crds) for AI Gateway CRDs.


3. Install AI Gateway Controller

```bash
helm upgrade -i aieg oci://docker.io/envoyproxy/ai-gateway-helm \
  --version v1.0.0 \
  --namespace envoy-ai-gateway-system \
  --create-namespace
```

> For more details, see the official [installation guide](https://aigateway.envoyproxy.io/docs/getting-started/installation#step-2-install-ai-gateway-resources) for AI Gateway Resources.

Wait for the controller to be ready:
```bash
kubectl wait --timeout=2m -n envoy-ai-gateway-system deployment/ai-gateway-controller --for=condition=Available
```

4. Install Gateway API Inference Extension (EPP Framework)

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/v1.5.0/manifests.yaml
```

> For more details, see the official [installation guide](https://aigateway.envoyproxy.io/docs/capabilities/inference/httproute-inferencepool#step-1-install-gateway-api-inference-extension) for Gateway API Inference Extension.


This deploys:
CRDs (InferencePool, InferenceObjective, InferenceModelRewrite)
RBAC, webhooks, and core controllers

> On arm64 local clusters such as Apple Silicon minikube, verify that the EPP image you load
> matches the node architecture. An architecture mismatch can leave the EPP pod running while
> its ext-proc gRPC port is not actually serving requests, which shows up as
> `HTTP/1.1 500` with `ext_proc_error_gRPC_error_14` in Envoy logs.

5. Install Envoy Gateway (Data Plane)

```bash
helm upgrade -i eg oci://docker.io/envoyproxy/gateway-helm \
  --version v1.8.2 \
  --namespace envoy-gateway-system \
  --create-namespace \
  -f https://raw.githubusercontent.com/envoyproxy/ai-gateway/v1.0.0/manifests/envoy-gateway-values.yaml \
  -f https://raw.githubusercontent.com/envoyproxy/ai-gateway/v1.0.0/examples/inference-pool/envoy-gateway-values-addon.yaml
```

> For more details, see the official [installation guide](https://aigateway.envoyproxy.io/docs/getting-started/prerequisites#additional-features-rate-limiting-inferencepool-etc) for Envoy Gateway.
> The InferencePool addon is required. Without it, `HTTPRoute` reports `ResolvedRefs=False`
> with `InvalidKind` for `inference.networking.k8s.io/InferencePool`.

The InferencePool addon does not grant Envoy Gateway access to `InferencePool` resources.
The sample applies that RBAC in Step 6.


6. Deploy Aibrix AI Gateway Resources

Apply your custom gateway and routing configuration:

```bash
cd samples/ai-gateway-integration

# Deploy for each model
kubectl apply -f llama-7b.yaml
kubectl apply -f mistral-7b.yaml

# Deploy GatewayClass, Gateway, and Envoy Gateway RBAC
kubectl apply -f gateway.yaml
kubectl apply -f envoy-gateway-inferencepool-rbac.yaml

# Deploy backend resources for each model
kubectl apply -f llama-7b-inferencepool.yaml
kubectl apply -f mistral-7b-inferencepool.yaml

# Deploy routing rules after the backend references exist
kubectl apply -f aigatewayroute.yaml
```

Wait for the model and EPP deployments to be ready:

```bash
kubectl wait --timeout=2m deployment/mock-llama2-7b --for=condition=Available
kubectl wait --timeout=2m deployment/mock-mistral-7b --for=condition=Available
kubectl wait --timeout=2m deployment/llama2-7b-epp --for=condition=Available
kubectl wait --timeout=2m deployment/mistral-7b-epp --for=condition=Available
kubectl wait --timeout=2m -n envoy-gateway-system deployment/envoy-gateway --for=condition=Available
```

### Verify Deployment Status

After installation, you can verify that all components are running correctly. Below is an example of expected output from a successful deployment:

-  Pods in `aibrix-system`
```bash
$ kubectl get pods -n aibrix-system
NAME                                         READY   STATUS    RESTARTS   AGE
aibrix-controller-manager-7dcf4b8d97-9mgw8   1/1     Running   0          3h35m
aibrix-gpu-optimizer-556d946fbb-gzh85        1/1     Running   0          3h35m
aibrix-metadata-service-bdfd4459d-678k5      1/1     Running   0          3h35m
aibrix-redis-master-74945dc65d-sr2sq         1/1     Running   0          3h35m
```

- Pods in `envoy-ai-gateway-system`
```bash
$ kubectl get pods -n envoy-ai-gateway-system
NAME                                     READY   STATUS    RESTARTS   AGE
ai-gateway-controller-5558c7cf7c-bzh65   1/1     Running   0          3h34m 
```

- Pods in `envoy-gateway-system`
```bash
$ kubectl get pods -n envoy-gateway-system
NAME                                                       READY   STATUS    RESTARTS   AGE
envoy-default-aibrix-ai-gateway-588291e8-54d5f9b6f-2psp6   3/3     Running   0          128m
envoy-gateway-6dd8f9b8f-kjngn                              1/1     Running   0          3h33m 
```

- AI Gateway CRDs
```bash
$ kubectl get InferencePool
NAME         AGE
llama2-7b    121m
mistral-7b   121m

$ kubectl get InferenceObjective
NAME         INFERENCE POOL   PRIORITY   AGE
llama2-7b    llama2-7b        10         121m
mistral-7b   mistral-7b       10         121m 
```

- Model and EPP Backend Pods (in default namespace)

```bash
$ kubectl get pods
NAME                               READY   STATUS    RESTARTS   AGE
llama2-7b-epp-6fb99fd7df-7xlxq     1/1     Running   0          121m
mistral-7b-epp-7c7f7fcb66-bw87d    1/1     Running   0          121m
mock-llama2-7b-6444f9b459-7gzmx    1/1     Running   0          131m
mock-llama2-7b-6444f9b459-92bsl    1/1     Running   0          131m
mock-llama2-7b-6444f9b459-krj8c    1/1     Running   0          131m
mock-mistral-7b-5fddcff595-5268f   1/1     Running   0          131m
mock-mistral-7b-5fddcff595-t65cp   1/1     Running   0          131m
```

### Test the Setup

Once all pods are ready, test routing via curl:

- Llama2-7B

```bash
curl -v http://<GATEWAY_IP>/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "x-ai-eg-model: llama2-7b" \
  -H "Authorization: Bearer test-key-1234567890" \
  -d '{
        "model": "llama2-7b",
        "messages": [{"role": "user", "content": "Say this is a test!"}],
        "temperature": 0.7
      }'
```

- Mistral-7B

```bash
curl -v http://<GATEWAY_IP>/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "x-ai-eg-model: mistral-7b" \
  -H "Authorization: Bearer test-key-0987654321" \
  -d '{
        "model": "mistral-7b",
        "messages": [{"role": "user", "content": "Say this is a test!"}],
        "temperature": 0.7
      }'
```

For local testing, discover the Envoy Gateway service and port-forward it:

```bash
GATEWAY_SERVICE=$(kubectl get svc -n envoy-gateway-system \
  -l gateway.envoyproxy.io/owning-gateway-name=aibrix-ai-gateway,gateway.envoyproxy.io/owning-gateway-namespace=default \
  -o jsonpath='{.items[0].metadata.name}')

kubectl port-forward -n envoy-gateway-system "svc/${GATEWAY_SERVICE}" 8080:80
```

Then replace `<GATEWAY_IP>` with `localhost:8080`. Or use the external IP of the Envoy Gateway service if exposed via LoadBalancer.

### References
- [Envoy AI Gateway](https://github.com/envoyproxy/ai-gateway)
- [Gateway API Inference Extension](https://github.com/kubernetes-sigs/gateway-api-inference-extension)

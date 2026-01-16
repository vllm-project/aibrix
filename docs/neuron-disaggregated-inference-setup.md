# Aibrix + Neuron Disaggregated Inference (P/D) Setup Guide

This guide documents the complete setup for running disaggregated inference (prefill/decode separation) with AWS Trainium2 using aibrix as the routing layer.

## Table of Contents
1. [Prerequisites](#prerequisites)
2. [EKS Nodegroup with EFA Setup](#eks-nodegroup-with-efa-setup)
3. [Prefill/Decode Deployment Architecture](#prefilldecode-deployment-architecture)
4. [Aibrix Cluster Setup](#aibrix-cluster-setup)
5. [Code Changes to Aibrix Fork](#code-changes-to-aibrix-fork)
6. [Compilation](#compilation)
7. [Gateway and Timeout Configuration](#gateway-and-timeout-configuration)
8. [ModelAdapter Configuration](#modeladapter-configuration)
9. [Testing](#testing)

---

## Prerequisites

- EKS cluster with Trainium2 node capacity
- kubectl and AWS CLI access to the cluster
- Helm installed
- vLLM with Neuron support and NIXL connector

---

## 1. EKS Nodegroup with EFA Setup

NIXL requires EFA (Elastic Fabric Adapter) for high-bandwidth KV cache transfer between prefill and decode pods. You must configure your EKS nodegroup to enable EFA before deploying workloads.

### Step 1: Create Launch Template with EFA NIC

Create a Launch Template that includes:
- EFA network interface (`efa-only` type)
- Neuron AMI (AL2023)
- UserData to install EFA userspace drivers

```bash
# Create Launch Template with EFA NIC configuration
aws ec2 create-launch-template-version \
  --launch-template-id <your-lt-id> \
  --launch-template-data "{
    \"BlockDeviceMappings\": [{
      \"DeviceName\": \"/dev/xvda\",
      \"Ebs\": {\"VolumeSize\": 512, \"VolumeType\": \"gp3\", \"DeleteOnTermination\": true}
    }],
    \"NetworkInterfaces\": [
      {\"DeviceIndex\": 0, \"DeleteOnTermination\": true},
      {\"DeviceIndex\": 1, \"InterfaceType\": \"efa-only\", \"DeleteOnTermination\": true}
    ],
    \"UserData\": \"<base64-encoded-userdata>\"
  }"
```

### Step 2: UserData for EFA Driver Installation

The UserData should include EFA userspace installation:

```bash
#!/bin/bash
set -euxo pipefail

# Install EFA userspace drivers
dnf -y install curl tar rdma-core pciutils libfabric libfabric-utils
cd /tmp
curl -fsSLO https://efa-installer.amazonaws.com/aws-efa-installer-latest.tar.gz
tar -xzf aws-efa-installer-latest.tar.gz
cd aws-efa-installer
./efa_installer.sh -y
```

### Step 3: Create Managed Node Group

Create the nodegroup using the Launch Template:

```bash
aws eks create-nodegroup \
  --cluster-name <cluster-name> \
  --nodegroup-name <nodegroup-name> \
  --subnets <subnet-id> \
  --node-role <node-role-arn> \
  --scaling-config minSize=1,maxSize=4,desiredSize=2 \
  --ami-type "AL2023_x86_64_NEURON" \
  --instance-types "trn2.48xlarge" \
  --launch-template "id=<lt-id>,version=<lt-version>"
```

### Step 4: Install AWS EFA Device Plugin

Install the EFA device plugin to expose EFA resources to Kubernetes:

```bash
helm repo add eks https://aws.github.io/eks-charts
helm repo update
helm upgrade --install aws-efa-k8s-device-plugin eks/aws-efa-k8s-device-plugin -n kube-system
```

### Step 5: Verify EFA Resources

Confirm nodes have EFA resources available:

```bash
kubectl describe node <node-name> | grep -A5 "Allocatable"
# Should show: vpc.amazonaws.com/efa: 1
```

Verify on-node EFA:
```bash
# SSH to node and run:
ls /sys/class/infiniband          # Should show rdmap* devices
/opt/amazon/efa/bin/fi_info -p efa  # Should list EFA providers
```

---

## 2. Prefill/Decode Deployment Architecture

Disaggregated inference separates the prefill (prompt processing) and decode (token generation) phases into different pod pools. This enables:

- **Independent scaling**: Scale prefill and decode pools separately based on workload (e.g., 2 prefill : 4 decode)
- **Resource optimization**: Prefill is compute-intensive, decode is memory-bound
- **Higher throughput**: Pipeline parallelism across the two phases

### Deployment Strategy

Deploy prefill and decode as **separate Kubernetes Deployments**:

```
┌─────────────────────────────────────────────────────────────┐
│                    Kubernetes Cluster                        │
│                                                              │
│  ┌─────────────────────┐    ┌─────────────────────────────┐ │
│  │ Prefill Deployment  │    │    Decode Deployment        │ │
│  │   (replicas: 2)     │    │      (replicas: 4)          │ │
│  │                     │    │                             │ │
│  │  ┌──────┐ ┌──────┐  │    │  ┌──────┐ ┌──────┐ ┌──────┐│ │
│  │  │Pod 1 │ │Pod 2 │  │    │  │Pod 1 │ │Pod 2 │ │Pod 3 ││ │
│  │  └──────┘ └──────┘  │    │  └──────┘ └──────┘ └──────┘│ │
│  │                     │    │           ┌──────┐         │ │
│  │                     │    │           │Pod 4 │         │ │
│  └─────────────────────┘    └───────────└──────┘─────────┘ │
│                                                              │
│            KV Cache Transfer via NIXL/EFA                    │
└─────────────────────────────────────────────────────────────┘
```

### Prefill Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: prefill
  namespace: default
spec:
  replicas: 2  # Scale independently
  selector:
    matchLabels:
      role-name: prefill
  template:
    metadata:
      labels:
        role-name: prefill
        model.aibrix.ai/name: llama31-8b
      annotations:
        model.aibrix.ai/port: "8000"
    spec:
      containers:
      - name: vllm
        image: your-vllm-neuron-image
        ports:
        - containerPort: 8000
        - containerPort: 5557  # NIXL port
        env:
        - name: VLLM_KV_ROLE
          value: "kv_producer"
        resources:
          limits:
            vpc.amazonaws.com/efa: 1  # Request EFA device
            aws.amazon.com/neuron: 16
          requests:
            vpc.amazonaws.com/efa: 1
            aws.amazon.com/neuron: 16
```

### Decode Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: decode
  namespace: default
spec:
  replicas: 4  # Scale independently from prefill
  selector:
    matchLabels:
      role-name: decode
  template:
    metadata:
      labels:
        role-name: decode
        model.aibrix.ai/name: llama31-8b
      annotations:
        model.aibrix.ai/port: "8000"
    spec:
      containers:
      - name: vllm
        image: your-vllm-neuron-image
        ports:
        - containerPort: 8000
        - containerPort: 5557  # NIXL port
        env:
        - name: VLLM_KV_ROLE
          value: "kv_consumer"
        resources:
          limits:
            vpc.amazonaws.com/efa: 1
            aws.amazon.com/neuron: 16
          requests:
            vpc.amazonaws.com/efa: 1
            aws.amazon.com/neuron: 16
```

### Required Labels and Annotations

| Field | Prefill | Decode | Purpose |
|-------|---------|--------|---------|
| `role-name` label | `prefill` | `decode` | Aibrix pd router identifies pod role |
| `model.aibrix.ai/name` label | `<model>` | `<model>` | Same model name for both |
| `model.aibrix.ai/port` annotation | `8000` | `8000` | vLLM serving port |

---

## 3. Aibrix Cluster Setup

### Step 1: Install Aibrix Dependencies (Envoy Gateway)

```bash
cd ~/aibrix
kubectl apply -k config/dependency --server-side
kubectl wait --for=condition=Available --timeout=3m deployment/envoy-gateway -n envoy-gateway-system
```

### Step 2: Install Aibrix Core

```bash
kubectl apply -k config/default
kubectl wait --for=condition=Available --timeout=3m deployment/aibrix-gateway-plugins -n aibrix-system
kubectl wait --for=condition=Available --timeout=3m deployment/aibrix-controller-manager -n aibrix-system
```

### Step 3: Configure Gateway Plugins Image

Use the custom image with Neuron/NIXL support:

```bash
kubectl set image deployment/aibrix-gateway-plugins -n aibrix-system \
  gateway-plugin=public.ecr.aws/e6d8z6l9/gateway-plugins:neuron-nixl
```

### Step 4: Set Environment Variables

```bash
# Set routing algorithm to 'pd' (prefill/decode disaggregation)
# NOTE: Use ROUTING_ALGORITHM, NOT AIBRIX_ROUTING_ALGORITHM
kubectl set env deployment/aibrix-gateway-plugins -n aibrix-system ROUTING_ALGORITHM=pd

# Set prefill request timeout (in seconds) - adjust based on model size
kubectl set env deployment/aibrix-gateway-plugins -n aibrix-system AIBRIX_PREFILL_REQUEST_TIMEOUT=600

# Reduce metrics scrape frequency (optional)
kubectl set env deployment/aibrix-gateway-plugins -n aibrix-system AIBRIX_POD_METRIC_REFRESH_INTERVAL_MS=10000

# Set gateway timeout for controller-manager - adjust based on model size
kubectl set env deployment/aibrix-controller-manager -n aibrix-system AIBRIX_GATEWAY_TIMEOUT_SECONDS=600
```

### Step 5: Wait for Rollout

```bash
kubectl rollout status deployment/aibrix-gateway-plugins -n aibrix-system
kubectl rollout status deployment/aibrix-controller-manager -n aibrix-system
```

---

## 4. Code Changes to Aibrix Fork

### File: `pkg/plugins/gateway/algorithms/pd_disaggregation.go`

Key changes:

1. **Pod Filtering** - Filter pods by `role-name` label:
```go
func (r *pdDisaggregationRouter) filterPrefillDecodePods(ctx *types.RoutingContext, pods types.PodList) ([]*v1.Pod, []*v1.Pod, error) {
    var prefillPods, decodePods []*v1.Pod
    for _, pod := range pods.All() {
        roleName := pod.Labels["role-name"]
        switch roleName {
        case "prefill":
            prefillPods = append(prefillPods, pod)
        case "decode":
            decodePods = append(decodePods, pod)
        }
    }
    return prefillPods, decodePods, nil
}
```

2. **Prefill Request Modification** - Set `max_tokens=1`:
```go
func (r *pdDisaggregationRouter) buildPrefillRequest(originalReq map[string]interface{}) map[string]interface{} {
    prefillReq := make(map[string]interface{})
    for k, v := range originalReq {
        prefillReq[k] = v
    }
    prefillReq["max_tokens"] = 1
    return prefillReq
}
```

3. **KV Transfer Params** - Extract and forward to decode:
```go
func (r *pdDisaggregationRouter) buildDecodeRequest(originalReq map[string]interface{}, kvParams map[string]interface{}) map[string]interface{} {
    decodeReq := make(map[string]interface{})
    for k, v := range originalReq {
        decodeReq[k] = v
    }
    decodeReq["kv_transfer_params"] = kvParams
    return decodeReq
}
```

### File: `pkg/plugins/gateway/algorithms/router.go`

Register the pd router:
```go
func init() {
    registerRouter(RouterPD, NewPDDisaggregationRouter)
}

const RouterPD = "pd"
```

### File: `pkg/plugins/gateway/gateway.go`

Skip HTTPRoute validation for pd algorithm:
```go
if !ok || routingCtx.Algorithm != routing.RouterPD {
    httprouteErr = s.validateHTTPRouteStatus(ctx, model)
}
```

---

## 5. Compilation

### Build Gateway Plugins

```bash
cd ~/aibrix

# Build the gateway-plugins binary
make build-gateway-plugins

# Build Docker image
make docker-build-gateway-plugins IMG=public.ecr.aws/e6d8z6l9/gateway-plugins:neuron-nixl

# Push to registry
make docker-push-gateway-plugins IMG=public.ecr.aws/e6d8z6l9/gateway-plugins:neuron-nixl
```

---

## 6. Gateway and Timeout Configuration

### Update EnvoyExtensionPolicy

```bash
kubectl patch envoyextensionpolicy aibrix-gateway-plugins-extension-policy -n aibrix-system \
  --type='json' -p='[{"op": "replace", "path": "/spec/extProc/0/messageTimeout", "value": "600s"}]'
```

### Update HTTPRoute Timeout

```bash
kubectl patch httproute llama31-8b-router -n aibrix-system \
  --type='json' -p='[{"op": "replace", "path": "/spec/rules/0/timeouts/request", "value": "600s"}]'
```

### Create BackendTrafficPolicy

```bash
kubectl apply -f - <<EOF
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: BackendTrafficPolicy
metadata:
  name: aibrix-backend-timeout
  namespace: aibrix-system
spec:
  targetRefs:
  - group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: aibrix-reserved-router
  timeout:
    http:
      connectionIdleTimeout: 600s
      maxConnectionDuration: 600s
      requestTimeout: 600s
EOF
```

---

## 7. ModelAdapter Configuration

```bash
kubectl apply -f - <<EOF
apiVersion: model.aibrix.ai/v1alpha1
kind: ModelAdapter
metadata:
  name: llama31-8b
  namespace: default
  labels:
    model.aibrix.ai/name: llama31-8b
spec:
  baseModel: llama31-8b
  artifactURL: "/var/mdl/llama31-8b"
  podSelector:
    matchLabels:
      role-name: prefill
EOF
```

---

## 8. Testing

### Test Request

```bash
kubectl run test --rm -it --image=curlimages/curl --restart=Never -- \
  curl -v -X POST \
  "http://<envoy-gateway-svc>.envoy-gateway-system:80/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d '{"model": "llama31-8b", "messages": [{"role": "user", "content": "Hello"}], "max_tokens": 10, "temperature": 0}'
```

### Expected Response

```json
{
  "model": "llama31-8b",
  "choices": [{
    "message": {"content": "Hello, how can I assist you today?"},
    "finish_reason": "length"
  }],
  "usage": {"prompt_tokens": 36, "completion_tokens": 10}
}
```

### Verify Logs

```bash
kubectl logs -l role-name=prefill --since=1m
kubectl logs -l role-name=decode --since=1m
kubectl logs deployment/aibrix-gateway-plugins -n aibrix-system --since=1m
```

---

## Architecture Summary

```
Client Request
      │
      ▼
┌─────────────────────┐
│  aibrix-gateway-    │
│     plugins         │
│  (pd routing)       │
└─────────────────────┘
      │
      ├──────────────────────┐
      ▼                      ▼
┌─────────────┐      ┌─────────────┐
│  Prefill    │ NIXL │   Decode    │
│ Deployment  │─────▶│ Deployment  │
│(max_tokens=1│ (KV) │(actual gen) │
└─────────────┘      └─────────────┘
      │                      │
      └──────────┬───────────┘
                 ▼
          Response to Client

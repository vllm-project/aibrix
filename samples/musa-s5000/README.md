# Deploying AIBrix on Moore Threads MTT S5000

This sample runs `Qwen/Qwen3-8B` on Moore Threads MTT S5000 GPUs with the
MUSA software stack. It provides vLLM and SGLang manifests for colocated and
Prefill/Decode (P/D) deployments.

The manifests use AIBrix's native pod discovery and routing contract. They do
not create an `InferencePool`, Endpoint Picker, `AIGatewayRoute`, or a separate
routing sidecar.

## Files and resource footprint

```text
samples/musa-s5000/
├── README.md
├── vllm-colocated.yaml
├── vllm-pd.yaml
├── sglang-colocated.yaml
└── sglang-pd.yaml
```

| Manifest | Kubernetes resource | GPU topology | Transfer path |
| --- | --- | --- | --- |
| `vllm-colocated.yaml` | `Deployment` | TP=1, 1 GPU | N/A |
| `vllm-pd.yaml` | `StormService` | 1P TP=1 + 1D TP=1, 2 GPUs total | Mooncake over TCP |
| `sglang-colocated.yaml` | `Deployment` | TP=1, 1 GPU | N/A |
| `sglang-pd.yaml` | `StormService` | 1P TP=1 + 1D TP=1, 2 GPUs total | Mooncake over TCP |

Deploy one profile at a time. All four use the served model name `qwen3-8b`,
so running them together would mix their pods in the same AIBrix model route.

## Current limitations

- The vLLM P/D manifest is a deployment sample, but native AIBrix P/D routing
  with `MooncakeConnector` is not end-to-end ready on this branch. The AIBrix
  Mooncake transfer agent recognizes the connector label but does not yet copy
  connector metadata from the Prefill response into the Decode request. Do not
  treat a successful pod deployment as proof that `routing-strategy: pd` works
  for this profile.
- The SGLang manifests follow the official SGLang and Mooncake MUSA build
  paths. The composed P/D image and Mooncake-over-TCP profile still require
  validation on the target MTT S5000 driver and container-runtime versions.
- These hardware profiles are not exercised by the default AIBrix CI jobs.

## Prerequisites

Prepare a Kubernetes cluster with:

1. AIBrix installed, including AIBrix Gateway and the StormService CRDs. See
   the [AIBrix quickstart](https://aibrix.readthedocs.io/latest/getting_started/quickstart.html).
2. A compatible MUSA driver, Moore Threads container runtime integration, and
   Kubernetes device plugin.
3. The device plugin advertising each MTT S5000 as `mthreads.com/gpu`.
4. One available GPU for a colocated profile or two for a P/D profile.
5. Network access or a local cache for `Qwen/Qwen3-8B`.
6. A RuntimeClass named `mthreads` for the SGLang profiles. If the cluster
   injects MUSA devices without a RuntimeClass, remove `runtimeClassName` from
   those manifests.

Verify the advertised devices:

```bash
kubectl get nodes \
  -o custom-columns='NAME:.metadata.name,S5000:.status.allocatable.mthreads\.com/gpu'
```

Check the RuntimeClass used by SGLang:

```bash
kubectl get runtimeclass mthreads
```

## Prepare the images

### vLLM

The vLLM manifests use this MUSA-enabled image:

```text
registry.mthreads.com/mcconline/inference/vllm:v0.28.0-ph1-5.2.0-torch2.11.0.post1-20260915
```

Make sure every target node can pull the image. Create an image pull Secret
and add `imagePullSecrets` to the pod specs if the registry requires
authentication.

### SGLang

SGLang provides an official MUSA Dockerfile and documents `Qwen/Qwen3-8B` as
its launch example. Build the current official source as `sglang:main-musa`:

```bash
git clone https://github.com/sgl-project/sglang.git
cd sglang
docker build -f docker/musa.Dockerfile -t sglang:main-musa .
```

See the
[official Moore Threads GPU guide](https://github.com/sgl-project/sglang/blob/main/docs/docs/hardware-platforms/mthreads_gpu.mdx)
for the driver, MUSA package, and build prerequisites.

The stock SGLang MUSA image does not include the Mooncake Python package used
by `sglang-pd.yaml`. Build a second image with Mooncake's official MUSA
Dockerfile, using `sglang:main-musa` as its base:

```bash
cd ..
git clone --recurse-submodules https://github.com/kvcache-ai/Mooncake.git
cd Mooncake
docker build -f docker/musa.Dockerfile \
  --build-arg BASE_IMAGE=sglang:main-musa \
  -t sglang:main-musa-mooncake .
```

The colocated manifest uses `sglang:main-musa`; the P/D manifest uses
`sglang:main-musa-mooncake`. The Kubernetes nodes must be able to resolve the
applicable image. Push it to a registry and update the manifest, or import the
local image into every target node. For repeatable deployments, retag both
images with the validated SGLang and Mooncake commits rather than rebuilding
moving `main` revisions.

### Hugging Face token

The manifests read `HF_TOKEN` from an optional Secret named
`musa-hf-token`. The public Qwen checkpoint normally does not require it. If
your environment does, create the Secret in the deployment namespace:

```bash
kubectl create secret generic musa-hf-token \
  --from-literal=HF_TOKEN="${HF_TOKEN}"
```

## AIBrix model contract

Every engine pod exposes port `8000` and carries these labels:

```yaml
model.aibrix.ai/name: qwen3-8b
model.aibrix.ai/port: "8000"
model.aibrix.ai/metric-port: "8000"
model.aibrix.ai/engine: vllm # sglang in the SGLang manifests
```

The following values must stay aligned:

| Setting | Value |
| --- | --- |
| Model source | `Qwen/Qwen3-8B` |
| Served model name | `qwen3-8b` |
| AIBrix model label | `qwen3-8b` |
| Request body `model` | `qwen3-8b` |

Each model-server container requests and limits exactly one
`mthreads.com/gpu`. The P/D manifests have two containers in separate pods,
so their total request is two GPUs.

## Deploy a colocated profile

Run commands from the repository root and choose one engine.

For vLLM:

```bash
kubectl apply -f samples/musa-s5000/vllm-colocated.yaml
kubectl rollout status deployment/musa-vllm-qwen3-8b --timeout=30m
```

For SGLang:

```bash
kubectl apply -f samples/musa-s5000/sglang-colocated.yaml
kubectl rollout status deployment/musa-sglang-qwen3-8b --timeout=30m
```

Both manifests run TP=1 on one MTT S5000. They use conservative sequence and
memory settings as a functional baseline, not as a performance profile.

## Deploy a P/D profile

The P/D manifests create one `StormService` containing one Prefill role and
one Decode role. Each role requests one MTT S5000.

For vLLM:

```bash
kubectl apply -f samples/musa-s5000/vllm-pd.yaml
kubectl get pods -l app=musa-vllm-qwen3-8b-pd -w
```

The workers use `MooncakeConnector`, TCP transport, and
`kv_load_failure_policy=fail`. Both roles carry
`model.aibrix.ai/kv-connector-type: mooncake`, but the Gateway limitation
described above must be resolved before native P/D requests can complete.

For SGLang:

```bash
kubectl apply -f samples/musa-s5000/sglang-pd.yaml
kubectl get pods -l app=musa-sglang-qwen3-8b-pd -w
```

The workers use SGLang's `prefill` and `decode` disaggregation modes and set
`MOONCAKE_PROTOCOL=tcp`. They run the derived
`sglang:main-musa-mooncake` image described above. The baseline does not
request an RDMA resource.

## Verify the deployment

### Check pods and GPU requests

Replace `<app-label>` with the label from the selected profile:

```bash
kubectl get pods -l app=<app-label> \
  -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,NODE:.spec.nodeName,GPU:.spec.containers[*].resources.requests.mthreads\.com/gpu'
```

Expected results:

- colocated: one Ready pod requesting one GPU;
- P/D: one Ready Prefill pod and one Ready Decode pod, each requesting one
  GPU.

### Check an engine directly

Select one engine pod and forward its serving port:

```bash
kubectl port-forward pod/<model-pod> 8000:8000
curl --fail http://127.0.0.1:8000/health
curl --fail http://127.0.0.1:8000/metrics
```

Send a request directly to confirm that the image, model, and MUSA runtime
work before testing Gateway routing:

```bash
curl --fail-with-body http://127.0.0.1:8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "qwen3-8b",
    "messages": [{"role": "user", "content": "Say this is a test."}],
    "max_tokens": 32,
    "temperature": 0
  }'
```

### Send a request through AIBrix Gateway

Set `ENDPOINT` to the reachable AIBrix Gateway address:

```bash
export ENDPOINT=<gateway-host-and-port>
```

For a colocated profile:

```bash
curl --fail-with-body "http://${ENDPOINT}/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -H 'routing-strategy: least-request' \
  -d '{
    "model": "qwen3-8b",
    "messages": [{"role": "user", "content": "Say this is a test."}],
    "max_tokens": 32,
    "temperature": 0
  }'
```

For SGLang P/D after its MUSA/TCP path has been validated:

```bash
curl --fail-with-body "http://${ENDPOINT}/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -H 'routing-strategy: pd' \
  -d '{
    "model": "qwen3-8b",
    "messages": [{"role": "user", "content": "Say this is a P/D test."}],
    "max_tokens": 32,
    "temperature": 0
  }'
```

Confirm from logs or transfer metrics that both roles handled the request and
that KV transfer completed. A successful HTTP response alone does not prove
that P/D transfer occurred.

## Remove a profile

Delete the selected profile before switching engines or modes:

```bash
kubectl delete -f samples/musa-s5000/vllm-colocated.yaml
# or
kubectl delete -f samples/musa-s5000/vllm-pd.yaml
# or
kubectl delete -f samples/musa-s5000/sglang-colocated.yaml
# or
kubectl delete -f samples/musa-s5000/sglang-pd.yaml
```

## Troubleshooting

### Pods remain Pending

Check that `mthreads.com/gpu` is allocatable and that each model container
requests one device. A same-node P/D placement requires two free GPUs on that
node; otherwise allow the roles to schedule on separate nodes.

### SGLang reports an image pull error

`sglang:main-musa` and `sglang:main-musa-mooncake` are locally built image
names, not public registry references. Import the applicable image into each
node or push it to an accessible registry and update `image` in the SGLang
manifests.

### The engine starts but the model does not load

Confirm that the selected image contains the MUSA runtime, that the model is
reachable, and that `/dev/shm` is large enough. Keep the served name
`qwen3-8b` even if the checkpoint is mounted from a local path.

### Gateway cannot discover a pod

Confirm that the pod carries the `model.aibrix.ai/name`,
`model.aibrix.ai/port`, and `model.aibrix.ai/engine` labels and that its
readiness probe succeeds. The request's `model` field must match the model
label exactly.

### P/D routing fails

Confirm that both roles are Ready, use the same served model name, and belong
to the same StormService replica. For SGLang, confirm that the MUSA build
supports Mooncake transfer with `MOONCAKE_PROTOCOL=tcp`. For vLLM, check the
current limitation above before debugging the worker manifests.

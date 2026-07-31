# AIBrix Demo on Volcano Engine

This guide walks you through deploying and testing AIBrix with various model serving scenarios on Volcano Engine,
including routing strategies, autoscaling, observability dashboards, and KV cache integration.

## Installation

Install:

```bash
kubectl apply -f https://github.com/vllm-project/aibrix/releases/download/v0.4.1/aibrix-dependency-v0.4.1.yaml --server-side
kubectl apply -f https://github.com/vllm-project/aibrix/releases/download/v0.4.1/aibrix-core-v0.4.1.yaml
```

Uninstall:

```bash
kubectl delete -f aibrix-core-v0.4.1.yaml
kubectl delete -f aibrix-dependency-v0.4.1.yaml
```

Expose the endpoint:

```bash
LB_IP=$(kubectl get svc/envoy-aibrix-system-aibrix-eg-903790dc -n envoy-gateway-system -o=jsonpath='{.status.loadBalancer.ingress[0].ip}')
ENDPOINT="${LB_IP}:80"
```

## Model deployment

Deploy a model:

```bash
kubectl apply -f deepseek-8b-naive.yaml
```

Once the model is ready:

```bash
curl http://${ENDPOINT}/v1/models | jq
```

## Demo 1: Routing

### Authentication issue - 401

This request missed the api-key header:

```bash
curl -v http://${ENDPOINT}/v1/completions \
    -H "Content-Type: application/json" \
    -d '{
        "model": "deepseek-r1-distill-llama-8b",
        "prompt": "San Francisco is a",
        "max_tokens": 128,
        "temperature": 0
    }' | jq
```

### Model issues - 400

This request uses a wrong model name `deepseek-r1-distill-llama-10b`, it should be `deepseek-r1-distill-llama-8b`.

```bash
curl -v http://${ENDPOINT}/v1/completions \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer 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" \
    -d '{
        "model": "deepseek-r1-distill-llama-10b",
        "prompt": "San Francisco is a",
        "max_tokens": 128,
        "temperature": 0
    }' | jq
```

### Different request strategy

Run three times to see different `target-pod` value from headers.

```bash
curl -v http://${ENDPOINT}/v1/completions \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer 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" \
    -H "routing-strategy: random" \
    -d '{
        "model": "deepseek-r1-distill-llama-8b",
        "prompt": "San Francisco is a",
        "max_tokens": 128,
        "temperature": 0
    }' | jq
```

### Prefix cache strategy demo

Run Jupyter scripts. Call three times. All `target-pod` should be same, the 2nd & 3rd TTFT should be less than 1st time.

## Demo 2: Autoscaling

Need a client to send request. With the vLLM client, check autoscaling experiment.

```bash
OPENAI_API_KEY=sk-*** python benchmark_serving.py \
    --backend vllm \
    --model deepseek-ai/DeepSeek-R1-Distill-Llama-8B \
    --trust-remote-code \
    --served-model-name deepseek-r1-distill-llama-8b \
    --base-url http://14.103.140.67:80 \
    --endpoint /v1/completions \
    --num-prompts 10000 \
    --request-rate 50 \
    --metric_percentiles '50,90,95,99' \
    --goodput ttft:1000 tpot:100 \
    --max-concurrency 200 \
    --random-input-len 4096 \
    --random-output-len 200 \
    --dataset-name sharegpt \
    --dataset-path /Users/bytedance/Downloads/ShareGPT_V3_unfiltered_cleaned_split.json \
    --ignore-eos
```

## Demo 3. Dashboard

```bash
kubectl port-forward svc/grafana-service 3000:3000 -n kube-system
```

> Note: Configure your prometheus using VMP basic auth.

Import all Grafana dashboards from [here](https://github.com/vllm-project/aibrix/tree/main/observability/grafana).

## Demo 4. Deepseek-R1 demo

SSH host and set up nvme first:

```bash
lsblk
sudo mkfs.ext4 /dev/nvme0n1
sudo mkdir -p /mnt/nvme0
sudo mount /dev/nvme0n1 /mnt/nvme0
mkdir /mnt/nvme0/models
```

Bring up the distributed inference:

```bash
kubectl apply -f deepseek-r1.yaml
```

Run vLLM benchmark against the server.

```bash
python benchmark_serving.py \
    --backend vllm \
    --model deepseek-ai/DeepSeek-R1 \
    --served-model-name deepseek-r1-671b \
    --base-url http://115.190.25.67:80 \
    --endpoint /v1/completions \
    --num-prompts 100 \
    --request-rate 1 \
    --metric_percentiles '50,90,95,99' \
    --goodput ttft:5000 tpot:50 \
    --max-concurrency 200 \
    --random-input-len 2000 \
    --random-output-len 200 \
    --dataset-name random \
    --ignore-eos \
    --seed 61
```

## Demo 5 - kv cache scenarios

Use base image:

```bash
aibrix-cn-beijing.cr.volces.com/aibrix/vllm-openai:aibrix-kvcache-v0.8.5-20250510
```

Edit Dockerfile, reinstall infinistore changes.

```dockerfile
FROM aibrix-cn-beijing.cr.volces.com/aibrix/vllm-openai:aibrix-kvcache-v0.8.5-20250510

# required
ENV PIP_PROGRESS_BAR=off
RUN wget https://test-files.pythonhosted.org/packages/f9/31/f9dbdfc77eadafaff9e882b501d0490625c113e3834891cd59d3223b747d/infinistore-0.2.42-cp312-cp312-manylinux_2_28_x86_64.whl

RUN pip3 install infinistore-0.2.42-cp312-cp312-manylinux_2_28_x86_64.whl --index-url=https://mirrors.ivolces.com/pypi/simple/
```

Consider to download from TOS:

```bash
./tosutil cp tos://aibrix-artifact-testing/artifacts/infinistore-0.2.42-cp312-cp312-manylinux_2_28_x86_64.whl .
```

Launch the kv cache service:

```bash
kubectl apply -f kvcache.yaml
```

Use `direct` or `cluster` way to bring up the inference engine and connect to the kv cache servers.

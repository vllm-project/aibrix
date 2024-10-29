## Gateway Routing benchmark

## Prerequisite

### Test Dataset

```bash
wget https://huggingface.co/datasets/anon8231489123/ShareGPT_Vicuna_unfiltered/blob/main/ShareGPT_V3_unfiltered_cleaned_split.json
```

### Client - Curl

```bash
curl -v http://localhost:8888/v1/completions \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer sk-any-key" \
    -d '{
        "model": "deepseek-coder-7b-instruct",
        "messages": [{"role": "user", "content": "Say this is a test!"}],
        "max_tokens": 128
    }'
```

### Client - Locust

```
locust -f benchmark.py --host http://localhost:8887
```

## Experiments

experiment 1 & 2 should use exact same client setting and they are comparable.

### Experiment 1: gateway overhead (httpRoute) vs k8s service (baseline)

```bash
kubectl -n envoy-gateway-system port-forward service/envoy-aibrix-system-aibrix-eg-903790dc 8888:80
kubectl port-forward svc/deepseek-coder-7b-instruct 8887:8000 -n aibrix-system
```

> Note: we can not use port-forward in > 1 pod testing, all the traffic will go into one pod.
> Change model service and gateway service to LoadBalancer for real testing.

### Experiment 2: Three Routing Strategies

wait until cache ready, manually send some request to activate model.

```bash
> Note: this is for local testing, feel free to change to Elastic IP later.

# service port-forwarding
OUTPUT_FILE=k8s-service.jsonl locust -f benchmark.py --host http://localhost:8887 --headless --users 30 --spawn-rate 0.08 --run-time 10m --csv benchmark_gateway_httproute.csv --csv-full-history --logfile benchmark_gateway_httproute.log

# gateway port-forwarding
OUTPUT_FILE=http-route.jsonl locust -f benchmark.py --host http://localhost:8888 --headless --users 30 --spawn-rate 0.08 --run-time 10m --csv benchmark_gateway_httproute.csv --csv-full-history --logfile benchmark_gateway_httproute.log

OUTPUT_FILE=random.jsonl ROUTING_STRATEGY=random locust -f benchmark.py --host http://localhost:8888 --headless --users 30 --spawn-rate 0.08 --run-time 10m --csv benchmark_gateway_random.csv --csv-full-history --logfile benchmark_gateway_random.log

OUTPUT_FILE=least-request.jsonl ROUTING_STRATEGY=least-request locust -f benchmark.py --host http://localhost:8888 --headless --users 30 --spawn-rate 0.08 --run-time 10m --csv benchmark_gateway_least_request.csv --csv-full-history --logfile benchmark_gateway_least_request.log

OUTPUT_FILE=throughput.jsonl ROUTING_STRATEGY=throughput locust -f benchmark.py --host http://localhost:8888 --headless --users 30 --spawn-rate 0.08 --run-time 10m --csv benchmark_gateway_throughput.csv --csv-full-history --logfile benchmark_gateway_throughput.log
```

## Local Testing

```bash
make docker-build-plugins
aibrix/plugins:9bd45a9915b71936ff0001a6fbfc32f10b65e480

k edit deployment aibrix-gateway-plugins

k delete pod aibrix-gateway-plugins-759b87dc65-j9qs8 # commit is exact same, we just need to update once
```

```bash
curl  http://localhost:8888/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any_key" \
  -d '{
     "model": "llama2-70b",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'
```

> Note: We do not need model or routing strategy in the header now. this is clean and sdk compatibile.


## New Client Testing

```bash
python client.py \
--dataset-path "/tmp/ShareGPT_V3_unfiltered_cleaned_split.json" \
--endpoint "http://101.126.24.162:8000" \
--num-prompts 2000 \
--interval 0.05 \
--output-file-path "k8s-v2.jsonl"
```

```bash
python client.py \
--dataset-path "/tmp/ShareGPT_V3_unfiltered_cleaned_split.json" \
--endpoint "http://101.126.81.102:80" \
--num-prompts 2000 \
--interval 0.05 \
--output-file-path "httproute-v2.jsonl"
```

update env
```bash
python client.py \
--dataset-path "/tmp/ShareGPT_V3_unfiltered_cleaned_split.json" \
--endpoint "http://101.126.81.102:80" \
--num-prompts 2000 \
--interval 0.05 \
--output-file-path "random-v2.jsonl"
```

```bash
python client.py \
--dataset-path "/tmp/ShareGPT_V3_unfiltered_cleaned_split.json" \
--endpoint "http://101.126.81.102:80" \
--num-prompts 2000 \
--interval 0.05 \
--output-file-path "least-request-v2.jsonl"
```

```bash
python client.py \
--dataset-path "/tmp/ShareGPT_V3_unfiltered_cleaned_split.json" \
--endpoint "http://101.126.81.102:80" \
--num-prompts 2000 \
--interval 0.05 \
--output-file-path "throughput-v2.jsonl"
```
## Mocked vLLM application

1. Builder mocked base model image
```dockerfile
docker build -t aibrix/vllm-mock:nightly -f Dockerfile .

# If you are using Docker-Desktop on Mac, Kubernetes shares the local image repository with Docker.
# Therefore, the following command is not necessary.
kind load docker-image aibrix/vllm-mock:nightly
```

2. Deploy mocked model image
```shell
kubectl apply -f docs/development/app/deployment.yaml
kubectl -n aibrix-system port-forward svc/llama2-70b 8000:8000 &
```

## Test python app separately

```shell
curl http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any_key" \
  -d '{
     "model": "llama2-70b",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'
```



## Test with envoy gateway

Install envoy gateway and setup HTTP Route
```shell
make docker-build && make docker-build-plugins

kind load docker-image aibrix/plugins:v0.1.0

make install && make deploy
```

Check status
```shell
helm status eg -n envoy-gateway-system

helm get all eg -n envoy-gateway-system
```

Port forward to the Envoy service:
```shell
kubectl -n aibrix-system port-forward service/envoy-aibrix-system-eg-95d1b654 8888:80 &
```

# Add rpm/tpm config 
```shell
kubectl -n aibrix-system exec -it aibrix-redis-master-<pod-name> -- redis-cli

set aibrix:your-user-name_TPM_LIMIT 100
set aibrix:your-user-name_RPM_LIMIT 10
```

Test request (ensure header model name matches with deployment's model name for routing)
```shell
curl -v http://localhost:8888/v1/chat/completions \
  -H "user: your-user-name" \
  -H "model: llama2-70b" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any_key" \
  -d '{
     "model": "llama2-70b",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'
```


Delete envoy gateway and corresponding objects
```shell
make undeploy && make uninstall
```


## Test Metrics

In order to facilitate the testing of Metrics, we make the Metrics value returned by
this mocked vllm deployment inversely proportional to the replica.

We scale the deployment to 1 replica firstly.
We can observe that the total value of Metrics is 100.

```shell
kubectl scale deployment llama2-70b --replicas=1
curl http://localhost:8000/metrics
```

```log
# HELP vllm:request_success_total Count of successfully processed requests.
# TYPE vllm:request_success_total counter
vllm:request_success_total{finished_reason="stop",model_name="llama2-70b"} 100.0
# HELP vllm:avg_prompt_throughput_toks_per_s Average prefill throughput in tokens/s.
# TYPE vllm:avg_prompt_throughput_toks_per_s gauge
vllm:avg_prompt_throughput_toks_per_s{model_name="llama2-70b"} 100.0
# HELP vllm:avg_generation_throughput_toks_per_s Average generation throughput in tokens/s.
# TYPE vllm:avg_generation_throughput_toks_per_s gauge
vllm:avg_generation_throughput_toks_per_s{model_name="llama2-70b"} 100.0
```

Then we scale the deployment to 5 replicas.
We can now see that the total value of Metrics becomes 100 / 5 = 20. 
This is beneficial for testing AutoScaling.

```shell
kubectl scale deployment llama2-70b --replicas=5
curl http://localhost:8000/metrics
```

```
# HELP vllm:request_success_total Count of successfully processed requests.
# TYPE vllm:request_success_total counter
vllm:request_success_total{finished_reason="stop",model_name="llama2-70b"} 20.0
# HELP vllm:avg_prompt_throughput_toks_per_s Average prefill throughput in tokens/s.
# TYPE vllm:avg_prompt_throughput_toks_per_s gauge
vllm:avg_prompt_throughput_toks_per_s{model_name="llama2-70b"} 20.0
# HELP vllm:avg_generation_throughput_toks_per_s Average generation throughput in tokens/s.
# TYPE vllm:avg_generation_throughput_toks_per_s gauge
vllm:avg_generation_throughput_toks_per_s{model_name="llama2-70b"} 20.0
```
# Mocked vLLM application

## Usage options

1. You should follow the README.md to deploy aibrix first. After that, you can deploy mocked app as a deployment
2. You can launch `app.py` directly on your dev environment without containers.

## Local Development

### Install dependencies

```bash
# Slim version (basic mock only)
pip install -r requirements.txt

# Full version (with simulator support)
pip install -r requirements-simulator.txt
```

### Running the server locally

Run the mocked app without Kubernetes:

```bash
STANDALONE_MODE=true python app.py
```

The server will start on `http://localhost:8000`.

### Testing endpoints

Two test files are provided for validating the endpoints:

```bash
# Start the server first
STANDALONE_MODE=true python app.py

# In another terminal, run the tests:

# Test OpenAI-compatible endpoints (uses OpenAI SDK)
python test_openai_endpoints.py

# Test vLLM-specific endpoints (uses HTTP client)
python test_vllm_endpoints.py
```

You can also specify a custom base URL:

```bash
python test_openai_endpoints.py --base-url http://localhost:8000/v1
python test_vllm_endpoints.py --base-url http://localhost:8000
```

## Mocked vLLM Basic Deployment

### Deploy the mocked app
1. Builder mocked base model image
```dockerfile
docker build -t aibrix/vllm-mock:nightly -f Dockerfile .
```

1.b (Optional) Load container image to docker context

> Note: If you are using Docker-Desktop on Mac, Kubernetes shares the local image repository with Docker.
> Therefore, the following command is not necessary. Only kind user need this step.

```shell
kind load docker-image aibrix/vllm-mock:nightly
```

2. Deploy mocked model image
```shell
kubectl create -k config/mock

# you can run following command to delete the deployment 
kubectl delete -k config/mock
```

### Deploy the simulator app
Alternatively, [vidur](https://github.com/microsoft/vidur) is integrated for high-fidality vLLM simulation:
0. Config HuggingFace token for model tokenizer by changing huggingface_token in config.json
```json
{
    "huggingface_token": "your huggingface token"
}
```

1. Builder simulator base model image
```dockerfile
docker build -t aibrix/vllm-simulator:nightly --build-arg SIMULATION=a100 -f Dockerfile .
```

1.b (Optional) Load container image to docker context

> Note: If you are using Docker-Desktop on Mac, Kubernetes shares the local image repository with Docker.
> Therefore, the following command is not necessary. Only kind user need this step.

```shell
kind load docker-image aibrix/vllm-simulator:nightly
```

2. Deploy simulator model image
```shell
kubectl create -k config/simulator

# you can run following command to delete the deployment 
kubectl delete -k config/simulator
```

### Test the metric invocation

1. Get the service endpoint

You have two options to expose the service:

```shell
# Option 1: Port forward the envoy service
kubectl -n envoy-gateway-system port-forward service/envoy-aibrix-system-aibrix-eg-903790dc 8000:80 &

# Option 2: Port forward the model service
kubectl -n default port-forward svc/llama2-7b 8000:8000 &
```

> The default Bearer Token is test-key-1234567890, defined in [api-key-patch.yaml](./config/mock/api-key-patch.yaml)

1. Test model invocation

```shell
curl http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer test-key-1234567890" \
  -d '{
     "model": "llama2-7b",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'
```

## Mocked vLLM Features

### OpenAI-Compatible Endpoints
- `/v1/models` - List available models
- `/v1/chat/completions` - Chat completions (streaming supported)
- `/v1/completions` - Text completions (streaming supported)
- `/v1/embeddings` - Text embeddings
- `/v1/images/generations` - Image generation
- `/v1/video/generations` - Video generation
- `/v1/rerank` - Document reranking
- `/v1/audio/speech` - Text-to-speech
- `/v1/audio/transcriptions` - Audio transcription
- `/v1/audio/translations` - Audio translation

### vLLM-Specific Endpoints
- `/v1/load_lora_adapter` - Load a LoRA adapter
- `/v1/unload_lora_adapter` - Unload a LoRA adapter
- `/tokenize` - Tokenize text
- `/detokenize` - Detokenize tokens
- `/load` - Server load metrics
- `/version` - Version info
- `/metrics` - Prometheus metrics

### Health/Utility Endpoints
- `/health` - Health check
- `/ready` - Ready check
- `/ping` - Ping check


## How to test AIBrix features

### Gateway rpm/tpm configs

```shell
# note: not mandatory to create user to access gateway API

kubectl -n aibrix-system port-forward svc/aibrix-metadata-service 8090:8090 &

curl http://localhost:8090/CreateUser \
  -H "Content-Type: application/json" \
  -d '{"name": "your-user-name","rpm": 100,"tpm": 1000}'
```

Test request
```shell
curl -v http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer test-key-1234567890" \
  -d '{
     "model": "llama2-7b",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'
```

### Routing Strategy

valid options: `random`, `least-latency`, `throughput`

```shell
curl -v http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer test-key-1234567890" \
  -H "routing-strategy: random" \
  -d '{
     "model": "llama2-7b",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'
```

### Metrics

In order to facilitate the testing of Metrics, we make the Metrics value returned by
this mocked vllm deployment inversely proportional to the replica.

We scale the deployment to 1 replica firstly.
We can observe that the total value of Metrics is 100.

```shell
kubectl scale deployment llama2-7b --replicas=1
curl http://localhost:8000/metrics
```

```log
# HELP vllm:request_success_total Count of successfully processed requests.
# TYPE vllm:request_success_total counter
vllm:request_success_total{model_name="llama2-7b"} 100.0
# HELP vllm:avg_prompt_throughput_toks_per_s Average prefill throughput in tokens/s.
# TYPE vllm:avg_prompt_throughput_toks_per_s gauge
vllm:avg_prompt_throughput_toks_per_s{model_name="llama2-7b"} 100.0
# HELP vllm:avg_generation_throughput_toks_per_s Average generation throughput in tokens/s.
# TYPE vllm:avg_generation_throughput_toks_per_s gauge
vllm:avg_generation_throughput_toks_per_s{model_name="llama2-7b"} 100.0
```

Then we scale the deployment to 5 replicas.
We can now see that the total value of Metrics becomes 100 / 5 = 20. 
This is beneficial for testing AutoScaling.

```shell
kubectl scale deployment llama2-7b --replicas=5
curl http://localhost:8000/metrics
```

```log
# HELP vllm:request_success_total Count of successfully processed requests.
# TYPE vllm:request_success_total counter
vllm:request_success_total{model_name="llama2-7b"} 20.0
# HELP vllm:avg_prompt_throughput_toks_per_s Average prefill throughput in tokens/s.
# TYPE vllm:avg_prompt_throughput_toks_per_s gauge
vllm:avg_prompt_throughput_toks_per_s{model_name="llama2-7b"} 20.0
# HELP vllm:avg_generation_throughput_toks_per_s Average generation throughput in tokens/s.
# TYPE vllm:avg_generation_throughput_toks_per_s gauge
vllm:avg_generation_throughput_toks_per_s{model_name="llama2-7b"} 20.0
```

#### Update Override Metrics

You can dynamically override specific metric values via the `/set_metrics` endpoint.

### Supported Override Keys

The following keys can be included in the JSON payload to override metrics:

- `total` – base total requests (used to derive other defaults)
- `success_total` → `vllm:request_success_total`
- `prompt_tokens_total` → `vllm:prompt_tokens_total`
- `generation_tokens_total` → `vllm:generation_tokens_total`
- `running` → `vllm:num_requests_running`
- `waiting` → `vllm:num_requests_waiting`
- `swapped` → `vllm:num_requests_swapped`
- `avg_prompt_throughput` → `vllm:avg_prompt_throughput_toks_per_s`
- `avg_generation_throughput` → `vllm:avg_generation_throughput_toks_per_s`
- `gpu_cache_usage_perc` → `vllm:gpu_cache_usage_perc`
- `cpu_cache_usage_perc` → `vllm:cpu_cache_usage_perc`
- `model_name` – sets the `model_name` label on all metrics

> Note: Histogram metrics (e.g., latency, token counts) are randomly generated and cannot be overridden.

### Examples

```bash
# Check current metrics
curl -X GET http://localhost:8000/metrics

# Override GPU cache usage and running requests
curl -X POST http://localhost:8000/set_metrics \
  -H "Content-Type: application/json" \
  -d '{
    "gpu_cache_usage_perc": 75.0,
    "running": 50,
    "waiting": 10,
    "success_total": 200
  }'

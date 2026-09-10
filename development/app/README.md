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

### Testing PD contracts locally

The PR2 mock supports explicit, connector-scoped PD contracts. These settings
are test-only and do not change the gateway implementation. When
`MOCK_PD_CONTRACT` is unset, the mock keeps its legacy behavior.

| Environment variable | Values | Purpose |
| --- | --- | --- |
| `MOCK_PD_CONTRACT` | `vllm-aibrix-shfs`, `vllm-aibrix-nixl`, `sglang-http`, `trtllm-openai` | Enables strict PD request validation |
| `MOCK_PD_ROLE` | `prefill`, `decode` | Identifies the current mock pod role |
| `POD_NAME` | pod name | Adds a stable pod name to responses and recorder entries |
| `LLM_ENGINE` | `vllm`, `sglang`, `trtllm` | Identifies the mock engine |

A standalone process represents one pod, so start separate processes if you
want to test both prefill and decode legs. Use `127.0.0.1` for local curl
requests when a proxy is configured for `localhost`.

Example strict SGLang prefill mock:

```bash
STANDALONE_MODE=true \
MOCK_PD_CONTRACT=sglang-http \
MOCK_PD_ROLE=prefill \
LLM_ENGINE=sglang \
POD_NAME=local-sglang-prefill \
python app.py
```

Send a request with the required bootstrap fields:

```bash
curl -sS -i -X POST http://127.0.0.1:8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'x-request-id: local-sglang-1' \
  -d '{
    "model": "llama2-7b-sglang",
    "messages": [{"role": "user", "content": "hello"}],
    "bootstrap_host": "10.0.0.1",
    "bootstrap_port": 8998,
    "bootstrap_room": 12345,
    "max_tokens": 8
  }'
```

The successful response remains a normal OpenAI-compatible completion. The
PD-specific request can be inspected through the recorder:

```bash
curl -sS \
  'http://127.0.0.1:8000/debug/requests?request_id=local-sglang-1'
```

Each record includes the request ID, sequence, path, engine, role, headers,
parsed JSON, status, outcome, response, and the exact raw body as base64.
`Authorization` values are redacted before they are recorded. The debug
endpoint is intended for internal Pod-proxy access and does not use the normal
OpenAI Bearer-token middleware. It has no reset/delete operation; use a unique
`x-request-id` for each test.

The four strict contract modes validate these internal prefill/decode shapes:

- `vllm-aibrix-shfs`: prefill receives `kv_transfer_params.do_remote_decode=true`;
  decode receives the merged transfer fields and opaque sentinel.
- `vllm-aibrix-nixl`: prefill must not receive the SHFS skeleton; gateway wraps
  the complete prefill response under `disagg_prefill_resp` for decode.
- `sglang-http`: both legs receive top-level `bootstrap_host`,
  `bootstrap_port`, and `bootstrap_room`.
- `trtllm-openai`: prefill uses `context_only`; decode uses `generation_only`
  with `disagg_request_id`, `first_gen_tokens`,
  `encoded_opaque_state`, and prompt token IDs.

Request-scoped fault injection is available for failure-path testing:

```bash
# Fail only when this process has MOCK_PD_ROLE=prefill.
curl -sS -i -X POST http://127.0.0.1:8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'x-request-id: local-failure-1' \
  -H 'x-aibrix-mock-fail: prefill' \
  -d '{"model":"llama2-7b","messages":[{"role":"user","content":"fail"}]}'

# Delay the current mock request by 200 ms.
curl -sS -i -X POST http://127.0.0.1:8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'x-request-id: local-delay-1' \
  -H 'x-aibrix-mock-delay-ms: 200' \
  -d '{"model":"llama2-7b","messages":[{"role":"user","content":"wait"}]}'
```

Matching `fail` requests return HTTP 500 and invalid delay values return HTTP
400. Both outcomes are recorded. The delay limit is 30 seconds.

### PD mock deployment in Kubernetes

`config/mock/kustomization.yaml` deploys separate mock topologies for SHFS,
NIXL, SGLang, and TRT-LLM. NIXL uses the independent model
`llama2-7b-vllm-nixl` and the label
`model.aibrix.ai/kv-connector-type: nixl`, so it cannot be paired with the
default vLLM SHFS pods.

```bash
kubectl apply -k config/mock
kubectl get stormservices -n default
kubectl delete -k config/mock
```

The Kubernetes PD manifests set `MOCK_PD_CONTRACT` and `MOCK_PD_ROLE` on every
prefill/decode container. The mock app does not require Redis for PR2. SGLang
cross-pod failure state is reserved for the later PD failure e2e work.

### Upstream KV connector source map

The mock contracts are compatibility contracts for the gateway behavior that
is declared in AIBrix. When an upstream engine or connector changes, check the
HTTP schema first, then the prefill request/response code, then the decode
consumer. Do not silently change an existing contract for a new upstream
version; add a version- or connector-specific contract when the wire shape
changes.

#### vLLM

vLLM implements KV transfer through the `kv_connector` hierarchy:

```text
vllm/distributed/kv_transfer/
└── kv_connector/v1/
    ├── base.py
    ├── nixl_connector.py
    ├── shared_storage_connector.py
    └── mooncake/
        └── mooncake_connector.py
```

The HTTP extension is defined in
[`vllm/entrypoints/openai/protocol.py`](https://github.com/vllm-project/vllm/blob/main/vllm/entrypoints/openai/protocol.py).
The connector implementations are under the
[`v1 kv_connector directory`](https://github.com/vllm-project/vllm/tree/main/vllm/distributed/kv_transfer/kv_connector/v1).

The corresponding AIBrix code is:

```text
pkg/plugins/gateway/algorithms/pd/engine/vllm.go
pkg/plugins/gateway/algorithms/pd/transfer/resolver.go
pkg/plugins/gateway/algorithms/pd/transfer/shfs.go
pkg/plugins/gateway/algorithms/pd/transfer/nixl.go
pkg/plugins/gateway/algorithms/pd/transfer/mooncake.go
```

For vLLM changes, update the SHFS or NIXL mock contract only after confirming
which AIBrix transfer agent is selected. Do not treat every `LLM_ENGINE=vllm`
request as having the same wire protocol.

#### SGLang

SGLang uses disaggregation and bootstrap fields rather than vLLM's
`kv_connector` hierarchy:

```text
python/sglang/srt/entrypoints/openai/protocol.py
python/sglang/srt/entrypoints/openai/serving_chat.py
python/sglang/srt/disaggregation/prefill.py
python/sglang/srt/disaggregation/decode.py
```

The relevant HTTP fields are top-level
`bootstrap_host`, `bootstrap_port`, and `bootstrap_room`:

- [`protocol.py`](https://github.com/sgl-project/sglang/blob/main/python/sglang/srt/entrypoints/openai/protocol.py)
- [`serving_chat.py`](https://github.com/sgl-project/sglang/blob/main/python/sglang/srt/entrypoints/openai/serving_chat.py)
- [`prefill.py`](https://github.com/sgl-project/sglang/blob/main/python/sglang/srt/disaggregation/prefill.py)
- [`decode.py`](https://github.com/sgl-project/sglang/blob/main/python/sglang/srt/disaggregation/decode.py)

AIBrix mutates these fields in
`pkg/plugins/gateway/algorithms/pd/engine/sglang.go`. If SGLang changes the
bootstrap schema, update `sglang-http` and its raw-body preservation tests.

#### TensorRT-LLM

TensorRT-LLM keeps the HTTP protocol and PD service in:

```text
tensorrt_llm/serve/openai_protocol.py
tensorrt_llm/serve/openai_disagg_service.py
tensorrt_llm/disaggregated_params.py
```

Useful upstream references:

- [`openai_protocol.py`](https://github.com/NVIDIA/TensorRT-LLM/blob/main/tensorrt_llm/serve/openai_protocol.py)
- [`openai_disagg_service.py`](https://github.com/NVIDIA/TensorRT-LLM/blob/main/tensorrt_llm/serve/openai_disagg_service.py)
- [`disaggregated_params.py`](https://github.com/NVIDIA/TensorRT-LLM/blob/main/tensorrt_llm/disaggregated_params.py)

The AIBrix implementation is
`pkg/plugins/gateway/algorithms/pd/engine/trtllm.go`. Track
`request_type`, `disagg_request_id`, `first_gen_tokens`,
`encoded_opaque_state`, and `prompt_token_ids`. The HTTP response shape uses
`choices[0].disaggregated_params`; the mock must not replace it with an
unrelated top-level shape.

When a field changes, use this maintenance path:

```text
upstream HTTP schema
  → prefill request builder
  → prefill response builder
  → AIBrix gateway merge
  → decode request validator
```

The detailed evidence and version notes are maintained in
[`PD_CONTRACT_TRACE.md`](./PD_CONTRACT_TRACE.md).

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
- `/v1/images/generations` - Image generation (OpenAI format)
- `/v1/video/generations` - Video generation (OpenAI/Sora format)
- `/v1/rerank` - Document reranking
- `/v1/audio/speech` - Text-to-speech (OpenAI + vLLM-Omni voices)
- `/v1/audio/transcriptions` - Audio transcription
- `/v1/audio/translations` - Audio translation

### vLLM-Omni Endpoints
- `/v1/chat/completions` - Image generation/editing (returns base64 image in content array when model name contains image keywords or diffusion params are present)
- `/v1/chat/completions` - Chat with `modalities: ["text", "audio"]` returns mock audio in response
- `/v1/audio/speech` - TTS with vLLM-Omni params (`language`, `instructions`, `task_type`, `ref_audio`, `ref_text`)
- `/v1/audio/voices` - List available TTS voices
- `/v1/videos` - Video generation (vLLM-Omni/Wan2.2 format, multipart/form-data, synchronous `b64_json` response, supports `input_reference` for I2V)

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

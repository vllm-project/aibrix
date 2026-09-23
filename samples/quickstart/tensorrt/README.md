# TensorRT-LLM PD quickstart

`tensor-rt-pd.yaml` deploys a text-model 1P1D StormService using TRT-LLM
`1.3.0rc8`: one CONTEXT worker on port 8101 and one GENERATION worker on 8103.
Adjust the model hostPath, image registry and GPU resources for your cluster.
The gateway and StormService controller must already be installed. The commands
below are instructions to run on your cluster, not an automated GPU test.

## Deploy and verify the workers

```bash
kubectl apply -f samples/quickstart/tensorrt/tensor-rt-pd.yaml
kubectl get pods -l model.aibrix.ai/name=Qwen3-8B -o wide
```

After the workers are ready, query **each selected CTX worker directly**, from a
place that can reach the pod network:

```bash
curl --fail "http://${CTX_POD_IP}:8101/server_info"
```

Generation-first requires TRT-LLM's Python KV-cache transceiver on both roles, which
is what `tensor-rt-pd.yaml` sets:

```yaml
cache_transceiver_config:
  backend: DEFAULT   # or NIXL; the Python runtime supports only these two
  transceiver_runtime: PYTHON
```

With the default C++ transceiver the endpoint answers
`{"disaggregated_params":{}}`, because only the Python transceiver reports the
generation-first metadata. The gateway then fails every request before dispatch, so
check this output before enabling the mode.

For generation-first the response must include:

```json
{
  "disaggregated_params": {
    "ctx_info_endpoint": "tcp://<CTX-worker-address>:<coordination-port>",
    "ctx_dp_rank": 0
  }
}
```

`ctx_info_endpoint` is a string (`tcp://host:port`) in the Python transceiver's
response; a single-element array is accepted as an equivalent encoding, while an
array with several endpoints is refused. `encoded_opaque_state` is also propagated
when present. GEN must be able to reach
the advertised coordination endpoint and the worker's KV-transfer transport.
Do not substitute the HTTP port for the advertised coordination port.

`ctx_dp_rank` is attention data-parallel rank, not TP rank or Pod number. The
single-instance example normally uses rank zero. For multiple workers the gateway
uses the exact selected worker's metadata, including nonzero ranks. A front-end
that load-balances a single HTTP endpoint across DP ranks without stable affinity
is not supported by this Pod-level metadata cache. Keep context-first for that
topology until rank-affine endpoints are available and tested.

## Select the scheduling mode

The default is `context_first`, which waits for CTX's HTTP response before GEN.
Enable parallel dispatch on the **gateway plugin**, not on the model containers:

```bash
kubectl set env deployment/aibrix-gateway-plugins -n aibrix-system \
  AIBRIX_TRT_SCHEDULE_STYLE=generation_first
kubectl rollout status deployment/aibrix-gateway-plugins -n aibrix-system
```

Use a gateway image built from a revision containing this feature. Adjust the
Deployment name and namespace if your installation uses different names.
An unrecognized schedule value is logged and the gateway stays on context-first.

The gateway initializes a metadata cache, then loads selected CTX workers lazily
before dispatch. Cache entries live for one minute; lookups have a three-second
timeout. Observed Pod/container replacement or restart uses a new cache key.
Invalid or unavailable metadata fails the request before either inference leg is
sent. There is no silent fallback or automatic replay of a dispatched request.

As with context-first, configure a **distinct** `AIBRIX_TRT_MACHINE_ID` in
`[0, 1024)` for each gateway process sharing TRT workers. Do not set one identical
machine ID on all replicas of a multi-replica gateway Deployment.

Keep Envoy's `failure_mode_allow=false` and confirm upstream disconnects reach the
engine. A terminal CTX failure closes the request; after SSE headers it resets the
stream instead of injecting replacement headers. TRT-LLM aborts inference on HTTP
disconnect, not via SGLang's `/abort_request` API. The CTX HTTP request also retains
client cancellation and `AIBRIX_PREFILL_REQUEST_TIMEOUT` (default: 30 seconds).

## Smoke tests

Both workers must have matching model, tokenizer and chat-template configuration.
In generation-first, GEN tokenizes the original request independently; the gateway
cannot wait for `prompt_token_ids` from CTX. Start with text-only prompts.

Set `ENDPOINT` to the external AIBrix gateway address, then test non-streaming:

```bash
curl --fail-with-body "http://${ENDPOINT}/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -H 'routing-strategy: pd' \
  -d '{"model":"Qwen3-8B","messages":[{"role":"user","content":"Explain prefill and decode in two sentences."}],"max_tokens":128,"stream":false}'
```

Test SSE as well:

```bash
curl --fail-with-body -N "http://${ENDPOINT}/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -H 'routing-strategy: pd' \
  -d '{"model":"Qwen3-8B","messages":[{"role":"user","content":"Explain KV caching."}],"max_tokens":128,"stream":true}'
```

Repeat for `/v1/completions` with a string `prompt`. In a disposable test deployment:

1. Confirm both legs have the same integer `disagg_request_id` and
   `disaggregated_params.schedule_style=1`; GEN has the selected CTX endpoint/rank.
2. Confirm GEN receives the request before the CTX HTTP response completes.
3. Cancel a long request and verify both workers release request/KV resources.
4. Interrupt CTX while GEN waits for KV. Verify a prompt error or stream reset,
   including when GEN has already sent SSE headers, and that GEN releases resources.
5. Restart a CTX container and scale workers; verify fresh metadata is used.
6. Compare TTFT, end-to-end latency, throughput and KV memory against
   context-first with identical prompts and concurrency. Parallel dispatch is not
   a guarantee of lower latency for every workload.

Mock-worker Go tests cover dispatch, JSON preservation, metadata/rank selection,
cache lifecycle, cancellation and gateway failure handling. They do **not** replace
real GPU/Envoy validation of KV transfer, multi-DP routing or resource release.

## Roll back

```bash
kubectl set env deployment/aibrix-gateway-plugins -n aibrix-system \
  AIBRIX_TRT_SCHEDULE_STYLE=context_first
kubectl rollout status deployment/aibrix-gateway-plugins -n aibrix-system
```

No worker image or CRD change is required by the gateway scheduling feature.

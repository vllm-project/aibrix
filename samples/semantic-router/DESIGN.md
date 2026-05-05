# Semantic Router: Design Overview

This document explains the high-level architecture of the semantic router, how it integrates with Envoy Gateway as an External Processor (ext_proc), how requests flow through the system, and how to configure routing decisions and model reasoning.

## Architecture Overview

```
Client
  │
  ▼
Envoy Gateway (aibrix-eg)  ←── EnvoyPatchPolicy injects ext_proc filters
  │
  │  gRPC (ext_proc protocol, port 50051)
  ▼
Semantic Router (vllm-semantic-router-system, ext_proc)
  │  classifies prompt → selects decision → rewrites request
  │  replaces model field: MoM → selected model (e.g. qwen3-8b, llama3-8b-instruct)
  │
  │  returns mutated request to Envoy
  ▼
AIBrix Gateway Plugins (aibrix-gateway-plugins, ext_proc)
  │  applies gateway-level plugins (rate limiting, auth, etc.)
  │
  ▼
  ├──► llama3-8b-instruct.default.svc.cluster.local:8000
  └──► qwen3-8b.default.svc.cluster.local:8000
```

The semantic router is deployed as a standard Kubernetes `Deployment` in the `vllm-semantic-router-system` namespace. It acts purely as an **in-path HTTP request rewriter** — it never terminates the connection to the client. Envoy holds the original request, hands it off to the router over gRPC, receives back a set of mutations (new headers, new body), and then forwards the mutated request to whichever backend the router selected.

---

## Installing the Router as an Envoy ext_proc

### What is ext_proc?

[External Processing (ext_proc)](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/ext_proc_filter) is an Envoy HTTP filter that streams request/response parts (headers, body, trailers) to an external gRPC service and applies the mutations that service returns. It enables arbitrary request manipulation without modifying Envoy itself.

### How the router is wired in

[gwapi-resources.yaml](gwapi-resources.yaml) applies an `EnvoyPatchPolicy` with two JSON patches:

**Patch 1 — add the ext_proc HTTP filter:**

```yaml
name: semantic-router-extproc
typedConfig:
  '@type': type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor
  grpcService:
    envoyGrpc:
      clusterName: semantic-router
      authority: semantic-router.vllm-semantic-router-system:50051
    timeout: 60s
  processing_mode:
    request_header_mode: SEND
    request_body_mode: BUFFERED     # full body is buffered before classification
    response_header_mode: SEND
    response_body_mode: BUFFERED    # full body buffered for semantic cache
```

`BUFFERED` body mode means Envoy accumulates the complete request or response body before sending it to the router. This lets the router read the full JSON payload of every `/v1/chat/completions` request without streaming complexity.

**Patch 2 — add the upstream cluster for the router:**

```yaml
name: semantic-router
type: STRICT_DNS
connect_timeout: 60s
http2_protocol_options: {}   # gRPC requires HTTP/2
load_assignment:
  endpoints:
    address: semantic-router.vllm-semantic-router-system.svc.cluster.local
    port_value: 50051
```

Together, these two patches register the router as an ext_proc filter on the Gateway's listener and tell Envoy how to reach it.

### Router deployment

[semantic-router.yaml](semantic-router.yaml) deploys the router container (`ghcr.io/vllm-project/semantic-router/extproc:latest`) with:

| Port | Protocol | Purpose |
|------|----------|---------|
| 50051 | gRPC | ext_proc interface — receives requests from Envoy |
| 8080 | HTTP | Classification REST API (debug / testing) |
| 9190 | HTTP | Prometheus metrics |

The router mounts the `semantic-router-config` ConfigMap at `/app/config/config.yaml` and loads its embedding model at startup (allow up to ~60 s for model download).

---

## Request Flow

```
1. Client  ──POST /v1/chat/completions──►  Envoy (port 80/8080)
           { "model": "MoM", "messages": [...] }

2. Envoy   ──ProcessingRequest (headers + buffered body)──►  Semantic Router (gRPC :50051)

3. Router
   a. Parse JSON body → extract messages[].content
   b. Run domain classifier  (embedding model → cosine similarity → top domain)
   c. Run keyword scanner    (scan content for keyword sets, e.g. "step by step")
   d. Apply priority strategy: pick the highest-priority matching decision
   e. Apply decision plugins (system_prompt, semantic-cache)
   f. Rewrite request:
        - Replace/prepend system message in messages[]
        - Change "model" field to the target backend model name
        - Inject reasoning parameters if use_reasoning: true (see below)

4. Router  ──ProcessingResponse (mutated headers + mutated body)──►  Envoy

4a. Envoy  ──ProcessingRequest (mutated headers + body)──►  AIBrix Gateway Plugins (ext_proc)
    (rate limiting, auth, and other gateway-level plugins applied here)

4b. AIBrix Gateway Plugins  ──ProcessingResponse──►  Envoy

5. Envoy   ──POST /v1/chat/completions──►  Backend model service
           { "model": "qwen3-8b", "messages": [{"role":"system",...}, ...],
             "chat_template_kwargs": {"enable_thinking": true} }

6. Backend ──response──►  Envoy

7. Envoy   ──ProcessingRequest (response headers + buffered body)──►  Semantic Router
   (semantic-cache decisions may store or serve cached responses here)

8. Envoy   ──response──►  Client
```

The client always uses `"model": "MoM"` (Model of Models). The router rewrites this field to the real backend model name before forwarding.

---

## Input Request Format

The router expects a standard OpenAI-compatible chat completions request:

```json
{
  "model": "MoM",
  "messages": [
    {"role": "user", "content": "What is the derivative of x^3 + 2x?"}
  ],
  "max_tokens": 100
}
```

Key points:

- **`model`** must be `"MoM"` (or whatever virtual model name is defined). The router replaces this with the real model name.
- **`messages`** — the router reads all message content to classify the domain. The `user` turn content is the primary signal.
- Any additional OpenAI parameters (`temperature`, `max_tokens`, `stream`, etc.) are passed through unchanged unless a plugin modifies them.
- The router may inject or replace a `system` message (see `system_prompt` plugin below).

---

## Configuring Routing Decisions

Routing is defined under `routing.decisions[]` in [semantic-router-configmap.yaml](semantic-router-configmap.yaml).

### Decision structure

```yaml
routing:
  decisions:
  - name: math_decision
    description: Mathematics and quantitative reasoning
    priority: 10            # higher wins; tie → first in list wins
    rules:
      operator: OR
      conditions:
      - name: math          # must match a domain listed in signals.domains
        type: domain        # or type: keyword
    modelRefs:
    - model: qwen3-8b
      use_reasoning: true   # controls whether reasoning mode is activated
    plugins:
    - type: system_prompt
      configuration:
        enabled: true
        mode: replace       # replace | prepend | append
        system_prompt: "You are a mathematics expert. ..."
```

### Rule types

| `type` | How it matches |
|--------|----------------|
| `domain` | Embedding-based similarity between the prompt and a named domain label. The router classifies into the domain whose embedding is closest to the prompt. |
| `keyword` | Exact substring search (case-insensitive by default). Matches if any keyword in the set appears in the prompt. |

### Priority strategy

The `global.router.strategy: priority` setting means:

1. All decisions whose rules match the prompt are collected.
2. The decision with the **highest `priority` value** wins.
3. If multiple decisions share the same priority, the one **declared first** in the YAML list wins.

The `thinking` keyword decision has `priority: 15` so it always overrides any domain match (`priority: 10`).

### Signals catalog

Domains and keyword sets must be declared under `routing.signals` before they can be referenced in rules:

```yaml
routing:
  signals:
    domains:
    - name: math
    - name: physics
    # ... add new domain labels here
    keywords:
    - name: thinking
      case_sensitive: false
      operator: OR
      keywords:
      - step by step
      - chain of thought
      # ... extend or add new keyword groups here
```

---

## Configuring Model Reasoning

`use_reasoning: true/false` on a `modelRef` controls whether the router injects reasoning-activation parameters into the forwarded request. The mechanism depends on the model's `reasoning_family`.

### Reasoning families

Defined under `providers.defaults.reasoning_families`:

```yaml
providers:
  defaults:
    default_reasoning_effort: high
    reasoning_families:
      qwen3:
        parameter: enable_thinking
        type: chat_template_kwargs   # injected as {"chat_template_kwargs": {"enable_thinking": true}}
      deepseek:
        parameter: thinking
        type: chat_template_kwargs
      gpt:
        parameter: reasoning_effort
        type: reasoning_effort       # injected as {"reasoning_effort": "high"}
      gpt-oss:
        parameter: reasoning_effort
        type: reasoning_effort
```

### Assigning a reasoning family to a model

```yaml
providers:
  models:
  - name: qwen3-8b
    reasoning_family: qwen3         # links this model to the qwen3 reasoning family
    backend_refs:
    - endpoint: qwen3-8b.default.svc.cluster.local:8000
      name: aibrix-vllm
      weight: 1
  - name: llama3-8b-instruct
    # no reasoning_family → reasoning is never activated for this model
    backend_refs:
    - endpoint: llama3-8b-instruct.default.svc.cluster.local:8000
      name: aibrix-vllm
      weight: 1
```

### End-to-end reasoning activation

When a decision fires with `use_reasoning: true` for `qwen3-8b` (which has `reasoning_family: qwen3`), the router adds:

```json
{
  "chat_template_kwargs": {
    "enable_thinking": true
  }
}
```

to the outbound request body. vLLM reads this field and activates Qwen3's built-in chain-of-thought path.

When `use_reasoning: false` (or the model has no `reasoning_family`), no extra parameters are injected and the model responds in standard mode.

### Summary table

| Decision | Model | `use_reasoning` | Injected parameter |
|----------|-------|-----------------|-------------------|
| `thinking_decision` | `qwen3-8b` | true | `chat_template_kwargs.enable_thinking = true` |
| `math_decision` | `qwen3-8b` | true | `chat_template_kwargs.enable_thinking = true` |
| `physics_decision` | `qwen3-8b` | true | `chat_template_kwargs.enable_thinking = true` |
| `computer_science_decision` | `qwen3-8b` | true | `chat_template_kwargs.enable_thinking = true` |
| `biology_decision` | `qwen3-8b` | true | `chat_template_kwargs.enable_thinking = true` |
| `chemistry_decision` | `qwen3-8b` | true | `chat_template_kwargs.enable_thinking = true` |
| `engineering_decision` | `qwen3-8b` | true | `chat_template_kwargs.enable_thinking = true` |
| `business_decision` | `llama3-8b-instruct` | false | _(none)_ |
| `law_decision` | `llama3-8b-instruct` | false | _(none)_ |
| `other_decision` (catch-all) | `llama3-8b-instruct` | false | _(none)_ |

---

## Plugins

Plugins are applied in declaration order after a decision is selected.

### `system_prompt`

Injects or replaces the system message in `messages[]`.

```yaml
plugins:
- type: system_prompt
  configuration:
    enabled: true
    mode: replace       # replace removes any existing system message and inserts this one
    system_prompt: "You are a mathematics expert. ..."
```

Modes:

| `mode` | Behaviour |
|--------|-----------|
| `replace` | Removes existing system messages; prepends a new `{"role": "system", ...}` |
| `prepend` | Inserts before existing system messages |
| `append` | Inserts after existing system messages |

### `semantic-cache`

Caches responses by prompt embedding similarity. On a cache hit the router short-circuits the backend call and returns the cached response directly to Envoy.

```yaml
plugins:
- type: semantic-cache
  configuration:
    enabled: true
    similarity_threshold: 0.92    # 0.0–1.0; higher = stricter match required
```

Cache settings (TTL, max entries, eviction) are in `global.stores.semantic_cache`:

```yaml
global:
  stores:
    semantic_cache:
      enabled: true
      backend_type: memory
      embedding_model: mmbert
      similarity_threshold: 0.8   # global default; per-decision threshold overrides this
      ttl_seconds: 3600
      max_entries: 1000
      eviction_policy: fifo
```

---

## Adding a New Route

1. **Declare the domain** under `routing.signals.domains`:
   ```yaml
   - name: cybersecurity
   ```

2. **Add the decision** under `routing.decisions`:
   ```yaml
   - name: cybersecurity_decision
     description: Cybersecurity and network security topics
     priority: 10
     rules:
       operator: OR
       conditions:
       - name: cybersecurity
         type: domain
     modelRefs:
     - model: qwen3-8b
       use_reasoning: true
     plugins:
     - type: system_prompt
       configuration:
         enabled: true
         mode: replace
         system_prompt: "You are a cybersecurity expert. ..."
   ```

3. Apply the updated ConfigMap:
   ```bash
   kubectl apply -f samples/semantic-router/semantic-router-configmap.yaml
   ```
   The router reloads config on restart; rolling-restart the deployment to pick up changes immediately:
   ```bash
   kubectl rollout restart deployment/semantic-router -n vllm-semantic-router-system
   ```

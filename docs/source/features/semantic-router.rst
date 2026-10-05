.. _semantic-router:

========================
vLLM Semantic Router
========================

Imagine you're building an AI assistant that handles everything — math homework, legal questions, creative writing, and medical queries. You *could* run a single large model for all of it. But what if you could automatically send math and physics questions to a reasoning-optimized model, and route everyday conversational queries to a faster, lighter model — all transparently, with no changes to how your clients make requests?

That's exactly what the **vLLM Semantic Router** does. It sits in front of your model fleet and classifies each incoming prompt by topic and intent, then forwards it to the best-fit model with the right configuration. Clients always call the same endpoint with the same API — the router handles the rest.

This guide walks you through deploying the semantic router with AIBrix and Envoy Gateway, using a concrete example that routes between ``qwen3-8b`` (for STEM and reasoning tasks) and ``llama3-8b-instruct`` (for business, legal, and general queries).

.. note::
    All sample manifests referenced in this guide live under `samples/semantic-router/ <https://github.com/vllm-project/aibrix/tree/main/samples/semantic-router>`_ in the AIBrix repository.

----

How It Works
------------

The router integrates as an `Envoy External Processor (ext_proc) <https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/ext_proc_filter>`_ — a gRPC sidecar that Envoy consults before forwarding each request. Here's the full request path:

.. code-block:: text

    Client
      │
      ▼
    Envoy Gateway (aibrix-eg)          ← receives the original request
      │  gRPC ext_proc (port 50051)
      ▼
    Semantic Router                    ← classifies prompt, rewrites model field
      │  e.g. "MoM" → "qwen3-8b"
      ▼
    AIBrix Gateway Plugins             ← rate limiting, auth, etc.
      │
      ├──► qwen3-8b (STEM + reasoning)
      └──► llama3-8b-instruct (business, legal, general)

**What the router does for each request:**

1. Receives the full buffered request body from Envoy over gRPC.
2. Extracts the user's message content.
3. Runs an **embedding-based domain classifier** (or a fast **keyword scanner** for explicit signals like "think step by step").
4. Selects the highest-priority matching decision.
5. Rewrites the request — replacing the ``model`` field, injecting a system prompt, and optionally enabling reasoning mode.
6. Returns the mutated request to Envoy, which forwards it to the selected backend.

The client always uses ``"model": "MoM"`` (Model of Models). The router replaces this with the real backend model name. No client changes are needed.

----

Prerequisites
-------------

Before you begin, make sure you have:

- A running Kubernetes cluster with `AIBrix installed <https://github.com/vllm-project/aibrix>`_ (includes Envoy Gateway).
- ``kubectl`` configured to talk to your cluster.
- GPU nodes available for model serving.
- A Hugging Face account and API token (for downloading model weights).
- Model weights pre-staged on your nodes at ``/data01/models/``, or access to pull them from HuggingFace.

.. tip::
    The sample uses ``qwen3-8b`` and ``llama3-8b-instruct``. Each model requires one GPU with at least 48 GB of GPU memory.

----

Step 1 — Create the Namespace and Credentials
---------------------------------------------

Create a dedicated namespace for the semantic router, then store your Hugging Face token as a Kubernetes secret (used by the router to download its embedding model):

.. code-block:: bash

    kubectl create namespace vllm-semantic-router-system

    export HF_TOKEN="<your-huggingface-token>"
    kubectl create secret generic hf-token-secret \
      --from-literal=token="${HF_TOKEN}" \
      -n vllm-semantic-router-system

----

Step 2 — Deploy the Backend Model Services
------------------------------------------

The router needs the actual model servers to be running before it can forward requests. Deploy both backends:

.. code-block:: bash

    kubectl apply -f samples/semantic-router/models/llama3-8b-instruct.yaml
    kubectl apply -f samples/semantic-router/models/qwen3-8b.yaml

Each manifest creates a ``Deployment`` (3 replicas) and a ``Service`` in the ``default`` namespace. The AIBrix controller automatically creates an ``HTTPRoute`` for each model so Envoy Gateway can discover them.

Wait until both deployments are ready before proceeding:

.. code-block:: bash

    kubectl rollout status deployment/llama3-8b-instruct -n default
    kubectl rollout status deployment/qwen3-8b -n default

----

Step 3 — Deploy the Semantic Router
-------------------------------------

Apply the router's ConfigMap (routing rules) and the router Deployment:

.. code-block:: bash

    kubectl apply -f samples/semantic-router/semantic-router-configmap.yaml
    kubectl apply -f samples/semantic-router/semantic-router.yaml

The router container (``ghcr.io/vllm-project/semantic-router/extproc:latest``) downloads its embedding model at startup — allow up to **60 seconds** for this. You can watch progress with:

.. code-block:: bash

    kubectl logs -f deployment/semantic-router -n vllm-semantic-router-system

The router exposes three ports:

.. list-table::
   :header-rows: 1
   :widths: 15 15 70

   * - Port
     - Protocol
     - Purpose
   * - ``50051``
     - gRPC
     - ext_proc interface — receives requests from Envoy
   * - ``8080``
     - HTTP
     - Classification REST API (useful for debugging)
   * - ``9190``
     - HTTP
     - Prometheus metrics

----

Step 4 — Wire the Router into Envoy Gateway
--------------------------------------------

Apply the Gateway API resources that register the semantic router as an ext_proc filter on the Envoy listener:

.. code-block:: bash

    kubectl apply -f samples/semantic-router/gwapi-resources.yaml

This applies an ``EnvoyPatchPolicy`` with two JSON patches:

**Patch 1** adds the ``semantic-router-extproc`` HTTP filter to Envoy's filter chain. It uses ``BUFFERED`` body mode so Envoy accumulates the complete request body before handing it to the router — no streaming complexity.

**Patch 2** registers the router as an upstream cluster (``STRICT_DNS``, HTTP/2 for gRPC) so Envoy knows how to reach it at ``semantic-router.vllm-semantic-router-system.svc.cluster.local:50051``.

Verify the patch was accepted:

.. code-block:: bash

    kubectl describe envoypatchpolicy ai-gateway-prepost-extproc-patch-policy -n aibrix-system

Look for ``Status: True`` under ``Conditions``.

----

Step 5 — Access the Gateway
-----------------------------

Port-forward the Envoy service to test locally:

.. code-block:: bash

    export ENVOY_SERVICE=$(kubectl get svc -n envoy-gateway-system \
      --selector=gateway.envoyproxy.io/owning-gateway-namespace=aibrix-system,gateway.envoyproxy.io/owning-gateway-name=aibrix-eg \
      -o jsonpath='{.items[0].metadata.name}')

    kubectl port-forward -n envoy-gateway-system "svc/${ENVOY_SERVICE}" 8080:80

In production, use the LoadBalancer external IP instead:

.. code-block:: bash

    LB_IP=$(kubectl get svc -n envoy-gateway-system \
      -l "gateway.envoyproxy.io/owning-gateway-name=aibrix-eg" \
      -o jsonpath='{.items[0].status.loadBalancer.ingress[0].ip}')

----

Step 6 — Test Semantic Routing
-------------------------------

All requests use the virtual model name ``"MoM"`` (Model of Models). The router transparently selects the right backend.

**Math question → routed to qwen3-8b (reasoning enabled)**

.. code-block:: bash

    curl http://localhost:8080/v1/chat/completions \
      -H "Content-Type: application/json" \
      -d '{
        "model": "MoM",
        "messages": [
          {"role": "user", "content": "What is the derivative of x^3 + 2x?"}
        ],
        "max_tokens": 200
      }'

The router classifies this as the ``math`` domain, selects ``qwen3-8b``, injects a mathematics expert system prompt, and enables chain-of-thought reasoning (``chat_template_kwargs.enable_thinking: true``).

**Business question → routed to llama3-8b-instruct (standard mode)**

.. code-block:: bash

    curl http://localhost:8080/v1/chat/completions \
      -H "Content-Type: application/json" \
      -d '{
        "model": "MoM",
        "messages": [
          {"role": "user", "content": "What are the key factors to consider when entering a new market?"}
        ],
        "max_tokens": 200
      }'

**Explicit reasoning trigger → overrides domain classification**

If the user explicitly asks for step-by-step thinking, the ``thinking`` keyword rule fires (priority 15, highest in the config) regardless of the detected domain:

.. code-block:: bash

    curl http://localhost:8080/v1/chat/completions \
      -H "Content-Type: application/json" \
      -d '{
        "model": "MoM",
        "messages": [
          {"role": "user", "content": "Walk me through how to structure a business merger proposal."}
        ],
        "max_tokens": 300
      }'

Even though this is a business query, "walk me through" matches the ``thinking`` keyword set, so it routes to ``qwen3-8b`` with reasoning enabled.

----

Routing Rules Reference
------------------------

The complete routing table for the sample (15 rules, defined in `semantic-router-configmap.yaml <https://github.com/vllm-project/aibrix/blob/main/samples/semantic-router/semantic-router-configmap.yaml>`_):

.. list-table::
   :header-rows: 1
   :widths: 25 15 25 15 10

   * - Domain / Keyword
     - Match Type
     - Model
     - Reasoning
     - Priority
   * - ``thinking`` (keywords: *step by step, think step, chain of thought, reason through, show your work, walk me through*)
     - keyword
     - ``qwen3-8b``
     - on
     - 15
   * - ``math``
     - domain
     - ``qwen3-8b``
     - on
     - 10
   * - ``physics``
     - domain
     - ``qwen3-8b``
     - on
     - 10
   * - ``computer science``
     - domain
     - ``qwen3-8b``
     - on
     - 10
   * - ``biology``
     - domain
     - ``qwen3-8b``
     - on
     - 10
   * - ``chemistry``
     - domain
     - ``qwen3-8b``
     - on
     - 10
   * - ``engineering``
     - domain
     - ``qwen3-8b``
     - on
     - 10
   * - ``business``
     - domain
     - ``llama3-8b-instruct``
     - off
     - 10
   * - ``law``
     - domain
     - ``llama3-8b-instruct``
     - off
     - 10
   * - ``psychology``
     - domain
     - ``llama3-8b-instruct``
     - off
     - 10
   * - ``health``
     - domain
     - ``llama3-8b-instruct``
     - off
     - 10
   * - ``economics``
     - domain
     - ``llama3-8b-instruct``
     - off
     - 10
   * - ``history``
     - domain
     - ``llama3-8b-instruct``
     - off
     - 10
   * - ``philosophy``
     - domain
     - ``llama3-8b-instruct``
     - off
     - 10
   * - ``other`` (catch-all)
     - domain
     - ``llama3-8b-instruct``
     - off
     - 5

Higher priority rules are evaluated first. The ``thinking`` keyword rule (priority 15) always overrides domain rules (priority 10).

----

Understanding the Configuration
---------------------------------

All routing behavior is controlled by `semantic-router-configmap.yaml <https://github.com/vllm-project/aibrix/blob/main/samples/semantic-router/semantic-router-configmap.yaml>`_. Here's how the key pieces fit together.

Decision Structure
^^^^^^^^^^^^^^^^^^

Each routing decision looks like this:

.. code-block:: yaml

    routing:
      decisions:
      - name: math_decision
        description: Mathematics and quantitative reasoning
        priority: 10            # higher wins; tie → first in list wins
        rules:
          operator: OR
          conditions:
          - name: math          # must match a signal declared under routing.signals
            type: domain        # or type: keyword
        modelRefs:
        - model: qwen3-8b
          use_reasoning: true   # activates chain-of-thought mode
        plugins:
        - type: system_prompt
          configuration:
            enabled: true
            mode: replace
            system_prompt: "You are a mathematics expert. ..."

Rule Types
^^^^^^^^^^

.. list-table::
   :header-rows: 1
   :widths: 15 85

   * - Type
     - How it matches
   * - ``domain``
     - Embedding-based cosine similarity between the prompt and a named domain label. The router picks the domain whose embedding is closest to the prompt.
   * - ``keyword``
     - Fast exact substring search (case-insensitive by default). Matches if any keyword in the set appears anywhere in the prompt.

Priority Strategy
^^^^^^^^^^^^^^^^^

With ``strategy: priority`` (the default):

1. All decisions whose rules match the prompt are collected.
2. The decision with the **highest ``priority`` value** wins.
3. Ties are broken by **declaration order** in the YAML — first wins.

Signals Catalog
^^^^^^^^^^^^^^^

Every domain or keyword set used in rules must be declared under ``routing.signals``:

.. code-block:: yaml

    routing:
      signals:
        domains:
        - name: math
        - name: physics
        - name: business
        # ... add new domain labels here
        keywords:
        - name: thinking
          case_sensitive: false
          operator: OR
          keywords:
          - step by step
          - chain of thought
          - reason through
          # ... extend here

----

Model Reasoning Activation
----------------------------

The ``use_reasoning: true/false`` flag on a ``modelRef`` controls whether the router injects reasoning-activation parameters into the forwarded request. Different model families use different parameters.

Reasoning Families
^^^^^^^^^^^^^^^^^^

Defined under ``providers.defaults.reasoning_families``:

.. code-block:: yaml

    providers:
      defaults:
        default_reasoning_effort: high
        reasoning_families:
          qwen3:
            parameter: enable_thinking
            type: chat_template_kwargs   # → {"chat_template_kwargs": {"enable_thinking": true}}
          deepseek:
            parameter: thinking
            type: chat_template_kwargs
          gpt:
            parameter: reasoning_effort
            type: reasoning_effort       # → {"reasoning_effort": "high"}

Assigning a Reasoning Family to a Model
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

.. code-block:: yaml

    providers:
      models:
      - name: qwen3-8b
        reasoning_family: qwen3         # links to the qwen3 reasoning family above
        backend_refs:
        - endpoint: qwen3-8b.default.svc.cluster.local:8000
          name: aibrix-vllm
          weight: 1
      - name: llama3-8b-instruct
        # no reasoning_family → reasoning is never activated
        backend_refs:
        - endpoint: llama3-8b-instruct.default.svc.cluster.local:8000
          name: aibrix-vllm
          weight: 1

When ``use_reasoning: true`` fires for ``qwen3-8b``, the router adds to the outbound request body:

.. code-block:: json

    {
      "chat_template_kwargs": {
        "enable_thinking": true
      }
    }

vLLM reads this field and activates Qwen3's built-in chain-of-thought path.

----

Plugins
--------

Plugins are applied in declaration order after a decision is selected.

System Prompt Plugin
^^^^^^^^^^^^^^^^^^^^^

Injects or replaces the system message in ``messages[]``:

.. code-block:: yaml

    plugins:
    - type: system_prompt
      configuration:
        enabled: true
        mode: replace       # replace | prepend | append
        system_prompt: "You are a mathematics expert. ..."

.. list-table::
   :header-rows: 1
   :widths: 15 85

   * - Mode
     - Behaviour
   * - ``replace``
     - Removes any existing system message and prepends a new one.
   * - ``prepend``
     - Inserts before existing system messages.
   * - ``append``
     - Inserts after existing system messages.

Semantic Cache Plugin
^^^^^^^^^^^^^^^^^^^^^^

Caches responses by prompt embedding similarity. On a cache hit the router short-circuits the backend call and returns the cached response directly — great for frequently repeated queries:

.. code-block:: yaml

    plugins:
    - type: semantic-cache
      configuration:
        enabled: true
        similarity_threshold: 0.92    # 0.0–1.0; higher = stricter match required

Global cache settings (TTL, max entries, eviction) are configured under ``global.stores.semantic_cache``:

.. code-block:: yaml

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

.. note::
    The semantic cache is particularly valuable for domains like ``health`` (threshold 0.95 — very strict) and ``other`` (threshold 0.75 — more permissive), where many users ask similar questions with slightly different wording.

----

Adding a New Route
-------------------

Adding a new topic (e.g., "cybersecurity") takes three steps and a config reload:

**1. Declare the domain** under ``routing.signals.domains``:

.. code-block:: yaml

    - name: cybersecurity

**2. Add the decision** under ``routing.decisions``:

.. code-block:: yaml

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
          system_prompt: "You are a cybersecurity expert with deep knowledge of network security, threat modeling, and secure coding practices. ..."

**3. Apply and reload**:

.. code-block:: bash

    kubectl apply -f samples/semantic-router/semantic-router-configmap.yaml

    # Rolling restart picks up the new config immediately
    kubectl rollout restart deployment/semantic-router -n vllm-semantic-router-system

----

Observability
--------------

The router exposes Prometheus metrics on port ``9190``. To scrape them locally:

.. code-block:: bash

    kubectl port-forward -n vllm-semantic-router-system \
      deployment/semantic-router 9190:9190

Then open ``http://localhost:9190/metrics`` in your browser or point Prometheus at it.

To enable distributed tracing (OpenTelemetry / Jaeger), set the following in the ConfigMap:

.. code-block:: yaml

    global:
      services:
        observability:
          tracing:
            enabled: true
            exporter:
              endpoint: jaeger:4317
              insecure: true
              type: otlp

----

Troubleshooting
----------------

**Router pod is slow to start**

The embedding model download can take up to 60 seconds. The startup probe retries for up to 60 minutes (``failureThreshold: 360``), so the pod will eventually become ready. Watch the logs:

.. code-block:: bash

    kubectl logs -f deployment/semantic-router -n vllm-semantic-router-system

**Envoy can't reach the router**

Verify the EnvoyPatchPolicy was accepted:

.. code-block:: bash

    kubectl describe envoypatchpolicy ai-gateway-prepost-extproc-patch-policy -n aibrix-system

Check that the router Service is reachable:

.. code-block:: bash

    kubectl get svc -n vllm-semantic-router-system

**All requests go to the fallback model**

The ``other_decision`` (priority 5) catches any prompt that doesn't match a known domain. Check whether the domain embeddings are loaded by querying the classification API directly:

.. code-block:: bash

    kubectl port-forward -n vllm-semantic-router-system deployment/semantic-router 9080:8080

    curl http://localhost:9080/classify \
      -H "Content-Type: application/json" \
      -d '{"text": "What is the integral of sin(x)?"}'

**Config changes aren't taking effect**

The router reads config at startup. After applying a new ConfigMap, rolling-restart the deployment:

.. code-block:: bash

    kubectl rollout restart deployment/semantic-router -n vllm-semantic-router-system

----

Sample Files
-------------

All manifests for this example are in the AIBrix repository:

- `samples/semantic-router/README.md <https://github.com/vllm-project/aibrix/blob/main/samples/semantic-router/README.md>`_ — quick-start guide
- `samples/semantic-router/DESIGN.md <https://github.com/vllm-project/aibrix/blob/main/samples/semantic-router/DESIGN.md>`_ — deep-dive architecture reference
- `samples/semantic-router/semantic-router-configmap.yaml <https://github.com/vllm-project/aibrix/blob/main/samples/semantic-router/semantic-router-configmap.yaml>`_ — full routing configuration with all 15 rules
- `samples/semantic-router/semantic-router.yaml <https://github.com/vllm-project/aibrix/blob/main/samples/semantic-router/semantic-router.yaml>`_ — router Deployment, Services, and RBAC
- `samples/semantic-router/gwapi-resources.yaml <https://github.com/vllm-project/aibrix/blob/main/samples/semantic-router/gwapi-resources.yaml>`_ — EnvoyPatchPolicy that wires the router into the gateway
- `samples/semantic-router/models/ <https://github.com/vllm-project/aibrix/tree/main/samples/semantic-router/models>`_ — model Deployment and Service manifests

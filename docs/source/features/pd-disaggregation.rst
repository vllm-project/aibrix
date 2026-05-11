.. _pd_disaggregation:

=====================================
Prefill-Decode Disaggregation (PD)
=====================================

Modern LLM inference workloads are rarely uniform. Some requests contain long prompts that benefit from specialized execution pipelines, while others are short and interactive. Designing infrastructure that efficiently handles both types of requests can be challenging.

AIBrix addresses this with intelligent routing across different types of inference pods within the same deployment. Instead of forcing operators to choose one architecture or maintain separate deployments, AIBrix runs both approaches together and automatically decides which pod should handle each request.

LLM inference has two distinct phases. **Prefill** processes the entire input prompt in one shot — it is compute-intensive and fast. **Decode** generates output tokens one at a time — it is memory-bandwidth-bound and slow. In a standard deployment, both phases run on the same GPU, causing them to compete for resources.

**PD disaggregation** separates these phases onto dedicated pods, so each can be sized, tuned, and scaled independently. The result is higher GPU utilization and lower latency at scale.

**Standard inference pods** run both phases end-to-end on a single GPU. They handle short interactive requests efficiently and absorb overflow traffic when PD resources are busy.

.. contents:: On this page
   :local:
   :depth: 2


Intelligent Routing Across Pod Types
--------------------------------------

Each pod type serves a different role:

- **Prefill/Decode pods** — Designed for workloads where separating prefill and decode stages improves efficiency. Particularly effective for long prompts or workloads dominated by heavy prompt processing.
- **Standard inference pods** — Execute the entire request lifecycle within a single process. Well suited for short prompts and interactive requests, and act as a safety valve when PD resources are saturated.

The AIBrix gateway continuously evaluates system conditions and routes each request to the best available pod:

.. code-block:: text

                      +---------------------------+
                      |         Client            |
                      +-------------+-------------+
                                    |
                                    ▼
                        Routing Algorithm (Gateway)
                                    |
          +-------------------------+------------------------+
          |                                                  |
          ▼                                                  ▼
 +------------------------+                    +------------------------+
 |  Prefill/Decode Pods   |                    | Standard Inference Pods|
 | (Disaggregated Stages) |                    | (Single Execution Path)|
 +------------------------+                    +------------------------+
        ▲                                                 ▲
        |  Selected for long prompts or                   |  Selected for short prompts
        |  prefill-heavy workloads                        |  or when PD capacity is busy

The routing decision incorporates several signals: current pod load, queue depth, pod availability, and scoring logic used to rank candidate pods. This allows AIBrix to distribute traffic efficiently while maintaining stable latency.

**Key benefits:**

- **Optimized handling of mixed workloads** — Long prompts are routed to prefill/decode pods; short requests are handled efficiently by standard inference pods.
- **Graceful handling of traffic spikes** — Standard inference pods absorb overflow traffic when PD resources are saturated.
- **Single deployment architecture** — Run multiple execution models for the same model without managing separate clusters.
- **Dynamic routing decisions** — Traffic is distributed based on real-time system conditions instead of static configuration.
- **Improved GPU utilization** — Requests are balanced across available pods to maximize throughput and efficiency.


How PD Disaggregation Works
-----------------------------

.. code-block:: text

    ┌──────────────────────────────────────────────────────────┐
    │                     Incoming request                     │
    └──────────────────────────┬───────────────────────────────┘
                               │
                               ▼
                    ┌──────────────────────┐
                    │   PD Router (gateway)│
                    └──────┬───────────────┘
                           │
            ┌──────────────┴──────────────┐
            ▼                             ▼
    ┌───────────────┐           ┌──────────────────┐
    │  Prefill pod  │  ──KV──▶  │   Decode pod     │
    │ (processes    │  transfer  │  (generates      │
    │  the prompt)  │           │   output tokens)  │
    └───────────────┘           └──────────────────┘

1. The gateway routes the request to a **prefill pod**, which processes the prompt and computes the KV cache.
2. The KV cache is transferred to a **decode pod** via a high-speed interconnect (SHFS for GPU, NIXL for Neuron).
3. The decode pod streams the generated tokens back to the client.

If no matching prefill/decode pair is available (e.g. the prompt is outside all configured buckets), the request falls back to a **standard inference pod** that runs both phases locally.


Supported Engines
-----------------

.. list-table::
   :header-rows: 1
   :widths: 20 20 60

   * - Engine
     - Label value
     - Notes
   * - vLLM
     - ``vllm``
     - Default. No extra labels required.
   * - SGLang
     - ``sglang``
     - Requires ``model.aibrix.ai/sglang-bootstrap-port`` annotation (default: ``8998``).
   * - TensorRT-LLM
     - ``trtllm``
     - Uses NIXL KV transfer backend (``AIBRIX_KV_CONNECTOR_TYPE=nixl``).

Set the engine on each pod with the ``model.aibrix.ai/engine`` label.


Step 1 — Label Your Pods
------------------------

The gateway identifies the role of each pod using two labels:

.. list-table::
   :header-rows: 1
   :widths: 22 30 48

   * - Label
     - Value
     - Purpose
   * - ``role-name``
     - ``prefill`` or ``decode``
     - Tells the gateway which phase this pod handles. Standard inference pods omit this label.
   * - ``roleset-name``
     - any string (e.g. ``group-0``)
     - Groups a prefill pod and a decode pod into a **pair**. The gateway only uses pairs where both prefill and decode pods are present.

A prefill pod template looks like this:

.. code-block:: yaml

    metadata:
      labels:
        model.aibrix.ai/name: my-model
        model.aibrix.ai/port: "8000"
        model.aibrix.ai/engine: vllm
        role-name: prefill
        roleset-name: group-0

A decode pod template looks like this:

.. code-block:: yaml

    metadata:
      labels:
        model.aibrix.ai/name: my-model
        model.aibrix.ai/port: "8000"
        model.aibrix.ai/engine: vllm
        role-name: decode
        roleset-name: group-0

.. note::
    Both pods in a pair must share the same ``roleset-name``. If a roleset has only a prefill pod or only a decode pod, the gateway skips that entire roleset.


Step 2 — Enable PD Routing
---------------------------

Set ``routing-strategy: pd`` on individual requests, or configure it as the default via the model config annotation (recommended for production):

.. code-block:: bash

    # Per-request override
    curl http://${ENDPOINT}/v1/chat/completions \
      -H "routing-strategy: pd" \
      -H "Content-Type: application/json" \
      -d '{"model": "my-model", "messages": [{"role": "user", "content": "Hello"}]}'

To make ``pd`` the default for a model, add the config annotation to the pod template (see `Config Profiles <gateway-plugins.html#config-profiles>`_ in the Gateway Routing guide):

.. code-block:: yaml

    annotations:
      model.aibrix.ai/config: |
        {
          "profiles": {
            "default": { "routingStrategy": "pd" }
          }
        }


Step 3 — Add Standard Inference Pods (Optional)
-------------------------------------------------

.. note::
    Standard inference pods are entirely optional. A pure prefill/decode deployment works without them. Think of them as a power-up: they unlock a second execution path that the gateway can exploit to absorb overflow, handle workloads outside your configured prompt-length buckets, and smooth out traffic spikes — all without spinning up a separate deployment or changing a single line of client code.

Adding standard inference pods turns a rigid two-tier pipeline into a self-healing, adaptive system. When PD capacity is saturated or a request falls outside the bucket ranges you've configured, the gateway automatically falls back to a standard inference pod and keeps the request moving rather than rejecting it or queueing indefinitely. The result is higher effective throughput, more consistent tail latency, and a gentler on-ramp for teams migrating incrementally from a standard deployment to full PD disaggregation.

Standard inference pods run both prefill and decode on a single GPU. They serve as overflow capacity when:

- The request's prompt length falls outside all configured buckets.
- All prefill/decode pairs are at capacity.
- You want a gradual migration path (run standard inference pods alongside disaggregated pairs).

**To configure a standard inference pod**, set ``combined: true`` in the pod's ``routingConfig`` annotation and enable prompt-length bucketing on the gateway:

.. code-block:: yaml

    metadata:
      labels:
        model.aibrix.ai/name: my-model
        model.aibrix.ai/port: "8000"
        model.aibrix.ai/engine: vllm
        # No role-name: prefill/decode — this is a standard inference pod
      annotations:
        model.aibrix.ai/config: |
          {
            "profiles": {
              "default": {
                "routingStrategy": "pd",
                "routingConfig": { "combined": true }
              }
            }
          }

Enable prompt-length bucketing on the gateway plugin (add to its environment):

.. code-block:: yaml

    # In your gateway plugin Helm values or Deployment env
    env:
      - name: AIBRIX_PROMPT_LENGTH_BUCKETING
        value: "true"

With bucketing enabled, the gateway considers a standard inference pod as a candidate only when the request's prompt length falls within the pod's configured range (see below). Without a range configured, a standard inference pod accepts any prompt length.


Prompt-Length Bucketing
------------------------

Bucketing lets you assign different pods to different prompt-length ranges. This is useful when:

- Short prompts are compute-cheap and can share a pod.
- Long prompts need dedicated resources.
- You want to prevent long-prompt requests from starving short-prompt traffic.

Configure the range in the pod's ``routingConfig``:

.. code-block:: yaml

    annotations:
      model.aibrix.ai/config: |
        {
          "profiles": {
            "default": {
              "routingStrategy": "pd",
              "routingConfig": {
                "promptLenBucketMinLength": 0,
                "promptLenBucketMaxLength": 2048
              }
            }
          }
        }

.. list-table::
   :header-rows: 1
   :widths: 32 68

   * - Field (inside ``routingConfig``)
     - Description
   * - ``promptLenBucketMinLength``
     - Minimum prompt token length (inclusive) this pod handles. Default: ``0``.
   * - ``promptLenBucketMaxLength``
     - Maximum prompt token length (inclusive) this pod handles. Default: unlimited. Set to ``0`` for unlimited.
   * - ``combined``
     - ``true`` = this pod is a standard inference pod (runs both prefill and decode). Default: ``false``.
   * - ``prefillScorePolicy``
     - How to score prefill pods. ``prefix_cache`` (default) or ``least_request``.
   * - ``decodeScorePolicy``
     - How to score decode pods. ``load_balancing`` (default) or ``least_request``.

.. note::
    Bucketing only takes effect when ``AIBRIX_PROMPT_LENGTH_BUCKETING=true`` is set on the gateway plugin.


Complete Example
----------------

This example shows a three-tier setup: prefill + decode pods for short prompts, and standard inference pods for long prompts or overflow.

**Prefill pod** (short prompts: 0–2048 tokens):

.. code-block:: yaml

    metadata:
      labels:
        model.aibrix.ai/name: my-model
        model.aibrix.ai/port: "8000"
        model.aibrix.ai/engine: vllm
        role-name: prefill
        roleset-name: group-0
      annotations:
        model.aibrix.ai/config: |
          {
            "profiles": {
              "default": {
                "routingStrategy": "pd",
                "routingConfig": {
                  "promptLenBucketMinLength": 0,
                  "promptLenBucketMaxLength": 2048
                }
              }
            }
          }

**Decode pod** (paired with the prefill pod above):

.. code-block:: yaml

    metadata:
      labels:
        model.aibrix.ai/name: my-model
        model.aibrix.ai/port: "8000"
        model.aibrix.ai/engine: vllm
        role-name: decode
        roleset-name: group-0
      annotations:
        model.aibrix.ai/config: |
          {
            "profiles": {
              "default": {
                "routingStrategy": "pd",
                "routingConfig": {
                  "promptLenBucketMinLength": 0,
                  "promptLenBucketMaxLength": 2048
                }
              }
            }
          }

**Standard inference pod** (long prompts: 2048+ tokens, and overflow):

.. code-block:: yaml

    metadata:
      labels:
        model.aibrix.ai/name: my-model
        model.aibrix.ai/port: "8000"
        model.aibrix.ai/engine: vllm
      annotations:
        model.aibrix.ai/config: |
          {
            "profiles": {
              "default": {
                "routingStrategy": "pd",
                "routingConfig": {
                  "combined": true,
                  "promptLenBucketMinLength": 2048
                }
              }
            }
          }

**Gateway plugin** (enable bucketing):

.. code-block:: yaml

    gatewayPlugin:
      env:
        - name: AIBRIX_PROMPT_LENGTH_BUCKETING
          value: "true"


Environment Variables
---------------------

These are set on the **gateway plugin** deployment.

.. list-table::
   :header-rows: 1
   :widths: 42 15 43

   * - Variable
     - Default
     - Description
   * - ``AIBRIX_PROMPT_LENGTH_BUCKETING``
     - ``false``
     - Enable prompt-length bucket matching for prefill, decode, and standard inference pods.
   * - ``AIBRIX_PREFILL_REQUEST_TIMEOUT``
     - ``30``
     - Seconds before a prefill request to a prefill pod times out.
   * - ``AIBRIX_PREFILL_SCORE_POLICY``
     - ``prefix_cache``
     - Default scoring policy for selecting prefill pods. ``prefix_cache`` or ``least_request``.
   * - ``AIBRIX_DECODE_SCORE_POLICY``
     - ``load_balancing``
     - Default scoring policy for selecting decode pods.
   * - ``AIBRIX_KV_CONNECTOR_TYPE``
     - ``shfs``
     - KV transfer backend. ``shfs`` for GPU (SHFS/KVCacheManager), ``nixl`` for Neuron (TensorRT-LLM).
   * - ``AIBRIX_PREFILL_LOAD_IMBALANCE_MIN_SPREAD``
     - ``16``
     - Minimum request-count spread between prefill pods before load-imbalance routing kicks in.
   * - ``AIBRIX_DECODE_LOAD_IMBALANCE_MIN_SPREAD``
     - ``16``
     - Minimum request-count spread between decode pods before load-imbalance routing kicks in.

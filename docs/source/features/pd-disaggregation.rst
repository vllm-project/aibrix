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

If no complete prefill/decode pair is available for the request's prompt length — for example the prompt is outside all configured buckets, or the matching roleset is incomplete because a decode pod is down — the request falls back to a **standard inference pod** (``combined: true``) that runs both phases locally, when one is configured and its prompt-length range matches.


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
     - ``prefill``, ``decode``, or another value (e.g. ``all``)
     - Tells the gateway which phase this pod handles. Standard inference pods use a role other than ``prefill``/``decode`` (or omit ``role-name``) and set ``combined: true`` in ``routingConfig``.
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

- The request's prompt length falls outside all configured PD buckets.
- The matching PD roleset is incomplete (for example, a decode pod is unavailable).
- All prefill/decode pairs are at capacity (load-imbalance routing may select a combined pod).
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
     - How to score prefill pods. ``prefix_cache`` (default), ``least_request``, or ``conductor``.
   * - ``decodeScorePolicy``
     - How to score decode pods. ``load_balancing`` (default), ``least_request``, or ``conductor``.

.. note::
    Bucketing only takes effect when ``AIBRIX_PROMPT_LENGTH_BUCKETING=true`` is set on the gateway plugin.


Conductor Scoring Policy
-------------------------

The ``conductor`` scoring policy selects pods by estimating latency for both prefill and decode phases, combining real-time metrics with workload characteristics.

**Prefill Scoring**

Estimates Time To First Token (TTFT) by considering:

- **Queue time** — Time waiting behind currently running requests
- **Prefix time** — Time to process tokens already in the KV prefix cache
- **Prefill time** — Time to compute tokens that do not match the cache (accounts for attention complexity)

**Decode Scoring**

Estimates Time Between Tokens (TBT) by considering:

- **Current throughput** — Derived from real-time generation metrics
- **Batch scaling** — TBT increases as batch size grows
- **GPU pressure** — Applies a penalty when cache usage exceeds 90%

**Configuration**

Enable conductor via the routing config:

.. code-block:: yaml

    annotations:
      model.aibrix.ai/config: |
        {
          "profiles": {
            "default": {
              "routingStrategy": "pd",
              "routingConfig": {
                "prefillScorePolicy": "conductor",
                "decodeScorePolicy": "conductor"
              }
            }
          }
        }

Or set gateway-wide via ``AIBRIX_PREFILL_SCORE_POLICY`` and ``AIBRIX_DECODE_SCORE_POLICY``.

**When to use conductor:**

- You want routing based on predicted latency rather than just request count
- Your workload has variable prompt lengths and mixed cache-hit patterns
- You need to account for GPU memory pressure in decode routing


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


Pod Selection Algorithm
-----------------------

Once pods are partitioned into prefill, decode, and combined slices, the gateway selects the best target through a cascade of checks. Bucketing (when ``AIBRIX_PROMPT_LENGTH_BUCKETING=true``) runs first; load-imbalance and scoring steps run only when a PD pair is still in play.

**Step 0 — Prompt-length bucketing (optional)**

When bucketing is enabled, each roleset contributes to the bucket-filtered pool only when **both** its prefill and decode pods declare a range that includes the request's prompt length. Incomplete rolesets (missing decode or prefill) are skipped. If no complete bucket-matched pair exists — including when a decode pod is down in the matching bucket — the gateway routes to a ``combined: true`` pod whose range includes the prompt, or returns an error if none is available. When a bucket match exists but PD pods are heavily loaded while a combined pod is idle, ``shouldPickCombined()`` may select the combined pod instead.

**Step 1 — Prefill load-imbalance fast path**

If the difference between the maximum and minimum number of outstanding prefill requests across prefill pods exceeds ``AIBRIX_PREFILL_LOAD_IMBALANCE_MIN_SPREAD``, the gateway routes the prefill phase to the single least-loaded prefill pod and aligns the decode candidates to the same roleset.

**Step 2 — Decode load-imbalance fast path**

Three ordered checks run against decode pods. The first that fires selects a single decode pod and aligns the prefill candidates to its roleset. Steps 1 and 2 are independent: both can fire on the same request if both sides are imbalanced.

1. *Request count spread* — If ``max(running + pending) − min`` across metric-bearing pods ≥ ``AIBRIX_DECODE_LOAD_IMBALANCE_MIN_SPREAD``, route to the least-loaded pod. Pods without ``RealtimeNumRequestsRunning`` are excluded from the spread to avoid thundering-herd on freshly restarted pods; their pending-decode count is still tracked for the scoring step.

2. *Throughput spread* — If ``max(throughput) − min`` across metric-bearing pods > ``AIBRIX_DECODE_THROUGHPUT_IMBALANCE_MIN_SPREAD``, route to the lowest-throughput pod.

3. *Drain-rate score* — If all pods report a positive ``drain_rate``, score each pod as ``effective_running_reqs / drain_rate``. If ``max_score / min_score`` exceeds ``AIBRIX_DECODE_SCORE_RATIO_THRESHOLD``, route to the pod with the lowest score (fastest estimated queue drain).

**Step 3 — Prefill scoring**

Each prefill pod is scored by the selected policy. Pods with a request count more than ``N`` standard deviations above the mean are skipped (``N = AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR``). The lowest-scoring pod per roleset is kept as the roleset's prefill candidate.

- ``prefix_cache`` (default): ``score = (100 − prefix_match_percent) × 0.1 + req_count / max_req_count`` — lower score means more cache hits and less load.
- ``least_request``: ``score = req_count``.

**Step 4 — Decode scoring**

Each decode pod is scored by the selected policy. Pods without ``RealtimeNumRequestsRunning`` receive a cold-start score (``1.0 + pending_decode_count``) when at least one sibling pod already has metrics, so they compete fairly without attracting all traffic immediately.

- ``load_balancing`` (default): ``score = (w_run × norm_reqs + w_thru × (1 − norm_throughput)) / norm_free_gpu``, where norms are relative to the batch maximum; lower is better.
- ``least_request``: ``score = running_reqs + pending_decode_count``.

**Step 5 — Final pair selection**

For each roleset with both a prefill and decode candidate, the gateway computes:

.. code-block:: text

    final_score = prefill_score / max_prefill_score + decode_score / max_decode_score

The roleset with the lowest combined score wins. The selected prefill and decode pods always come from the same roleset.


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
     - Default scoring policy for selecting prefill pods. ``prefix_cache``, ``least_request``, or ``conductor``.
   * - ``AIBRIX_DECODE_SCORE_POLICY``
     - ``load_balancing``
     - Default scoring policy for selecting decode pods. ``load_balancing``, ``least_request``, or ``conductor``.
   * - ``AIBRIX_KV_CONNECTOR_TYPE``
     - ``shfs``
     - KV transfer backend. ``shfs`` for GPU (SHFS/KVCacheManager), ``nixl`` for Neuron (TensorRT-LLM).
   * - ``AIBRIX_PREFILL_LOAD_IMBALANCE_MIN_SPREAD``
     - ``16``
     - Minimum request-count spread between prefill pods before load-imbalance routing kicks in.
   * - ``AIBRIX_DECODE_LOAD_IMBALANCE_MIN_SPREAD``
     - ``16``
     - Minimum ``(max − min)`` running request count spread between decode pods before request-count load-imbalance routing kicks in.
   * - ``AIBRIX_DECODE_THROUGHPUT_IMBALANCE_MIN_SPREAD``
     - ``2048``
     - Minimum ``(max − min)`` token throughput spread (tok/s) between decode pods before throughput-based load-imbalance routing kicks in.
   * - ``AIBRIX_DECODE_SCORE_RATIO_THRESHOLD``
     - ``1.5``
     - ``max_drain_score / min_drain_score`` ratio above which the drain-rate routing fast path is triggered. Score for each pod is ``effective_running_reqs / drain_rate``.
   * - ``AIBRIX_DECODE_LB_WEIGHT_RUNNING``
     - ``1.0``
     - Weight applied to the normalized running-request term in the ``load_balancing`` decode score numerator.
   * - ``AIBRIX_DECODE_LB_WEIGHT_THROUGHPUT``
     - ``1.0``
     - Weight applied to the normalized inverse-throughput term in the ``load_balancing`` decode score numerator.
   * - ``AIBRIX_TRT_MACHINE_ID``
     - ``0``
     - 10-bit machine ID used in Snowflake-style ``disagg_request_id`` generation for TensorRT-LLM (valid range: ``[0, 1024)``).

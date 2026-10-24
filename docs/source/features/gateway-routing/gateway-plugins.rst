.. _gateway:

===============
Gateway Routing
===============

The AIBrix gateway is built on Envoy Gateway and acts as the single entry point for all LLM inference requests. It handles dynamic model discovery, request routing, rate limiting, and response streaming. For a deep dive into the internal design, see the :doc:`AIBrix Gateway <../../designs/aibrix-gateway>` architecture guide.

This page is the gateway routing reference. Dedicated guides cover the strategies that have their own setup:

- :doc:`prefix-cache-aware-routing`
- :doc:`pd-disaggregation`
- :doc:`agentic-routing`
- :doc:`semantic-router`
- :doc:`omni`
- :doc:`external-replica-routing`

.. contents:: On this page
   :local:
   :depth: 2

Dynamic Routing
---------------

When a model or LoRA adapter is deployed, the respective controller automatically creates an ``HTTPRoute`` object. The gateway dynamically discovers these routes to forward incoming requests without any manual configuration.

Find the gateway endpoint
^^^^^^^^^^^^^^^^^^^^^^^^^

First, retrieve the external IP and port of the Envoy proxy:

.. code-block:: bash

    $ kubectl get svc -n envoy-gateway-system
    NAME                                     TYPE           CLUSTER-IP      EXTERNAL-IP   PORT(S)                                   AGE
    envoy-aibrix-system-aibrix-eg-903790dc   LoadBalancer   10.96.239.246   101.18.0.4    80:32079/TCP                              10d
    envoy-gateway                            ClusterIP      10.96.166.226   <none>        18000/TCP,18001/TCP,18002/TCP,19001/TCP   10d

In most Kubernetes setups, ``LoadBalancer`` is supported by default. Capture the external IP:

.. code-block:: bash

    LB_IP=$(kubectl get svc/envoy-aibrix-system-aibrix-eg-903790dc -n envoy-gateway-system -o=jsonpath='{.status.loadBalancer.ingress[0].ip}')
    ENDPOINT="${LB_IP}:80"

Verify the route
^^^^^^^^^^^^^^^^

Verify that the ``HTTPRoute`` status is ``Accepted`` before sending traffic:

.. code-block:: bash

    $ kubectl get httproute -A
    NAMESPACE       NAME                                  HOSTNAMES   AGE
    aibrix-system   aibrix-reserved-router                            17m # reserved router
    aibrix-system   deepseek-r1-distill-llama-8b-router               14m # created for each model deployment
    ....

.. code-block:: bash

    $ kubectl describe httproute deepseek-r1-distill-llama-8b-router -n aibrix-system
    Name:         deepseek-r1-distill-llama-8b-router
    Namespace:    aibrix-system
    Labels:       <none>
    Annotations:  <none>
    API Version:  gateway.networking.k8s.io/v1
    Kind:         HTTPRoute
    Metadata:
      Creation Timestamp:  2025-02-16T17:56:03Z
      Generation:          1
      Resource Version:    2641
      UID:                 2f3f9620-bf7c-487a-967e-2436c3809178
    Spec:
      Parent Refs:
        Group:      gateway.networking.k8s.io
        Kind:       Gateway
        Name:       aibrix-eg
        Namespace:  aibrix-system
      Rules:
        Backend Refs:
          Group:
          Kind:       Service
          Name:       deepseek-r1-distill-llama-8b
          Namespace:  default
          Port:       8000
          Weight:     1
        Matches:
          Headers:
            Name:   model
            Type:   Exact
            Value:  deepseek-r1-distill-llama-8b
          Path:
            Type:   PathPrefix
            Value:  /
        Timeouts:
          Request:  120s
    Status:
      Parents:
        Conditions:
          Last Transition Time:  2025-02-16T17:56:03Z
          Message:               Route is accepted
          Observed Generation:   1
          Reason:                Accepted
          Status:                True
          Type:                  Accepted
          Last Transition Time:  2025-02-16T17:56:03Z
          Message:               Resolved all the Object references for the Route
          Observed Generation:   1
          Reason:                ResolvedRefs
          Status:                True
          Type:                  ResolvedRefs
        Controller Name:         gateway.envoyproxy.io/gatewayclass-controller
        Parent Ref:
          Group:      gateway.networking.k8s.io
          Kind:       Gateway
          Name:       aibrix-eg
          Namespace:  aibrix-system
    Events:           <none>

Send a request
^^^^^^^^^^^^^^

The model name in the request body must match the ``model.aibrix.ai/name`` label in your deployment:

.. code-block:: bash

    curl -v http://${ENDPOINT}/v1/chat/completions \
    -H "Content-Type: application/json" \
    -d '{
        "model": "deepseek-r1-distill-llama-8b",
        "messages": [{"role": "user", "content": "Say this is a test!"}],
        "temperature": 0.7
    }'

Authentication
^^^^^^^^^^^^^^

.. attention::

    AIBrix exposes a public endpoint to the internet. Enable authentication to secure it.
    For vLLM backends, pass the ``--api-key`` argument or set the ``VLLM_API_KEY`` environment variable to require an API key in the ``Authorization`` header.
    See `vLLM OpenAI-Compatible Server <https://docs.vllm.ai/en/latest/getting_started/quickstart.html#openai-compatible-server>`_ for details.

After enabling authentication, include the ``Authorization`` header in every request:

.. code-block:: bash

    curl -v http://${ENDPOINT}/v1/chat/completions \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer any_key" \
    -d '{
        "model": "deepseek-r1-distill-llama-8b",
        "messages": [{"role": "user", "content": "Say this is a test!"}],
        "temperature": 0.7
    }'

Expose custom paths
^^^^^^^^^^^^^^^^^^^

As of v0.5.0, each model's ``HTTPRoute`` is created with a default set of path prefixes (e.g. ``/v1/chat/completions``, ``/v1/completions``). To expose additional custom paths, add the ``model.aibrix.ai/model-router-custom-paths`` annotation to your ``Deployment``, ``ModelAdapter``, or ``RayClusterFleet`` manifest:

.. code-block:: yaml

    apiVersion: apps/v1
    kind: Deployment
    metadata:
      labels:
        model.aibrix.ai/name: deepseek-r1-distill-llama-8b
        model.aibrix.ai/port: "8000"
      annotations:
        model.aibrix.ai/model-router-custom-paths: /version,/score # comma-separated; spaces and empty entries are ignored
      name: deepseek-r1-distill-llama-8b
      namespace: default


Routing Strategies
------------------

Selecting a strategy
^^^^^^^^^^^^^^^^^^^^

The routing strategy controls how the gateway selects the target pod for each request. It can be set per request via the ``routing-strategy`` header, per model via a :ref:`config profile <config_profiles>`, or globally via the ``ROUTING_ALGORITHM`` environment variable. When more than one is set, :ref:`routing_strategy_priority` decides which one wins.

To override the strategy for a single request, pass the ``routing-strategy`` header with any of the strategy names listed below:

.. code-block:: bash

    curl -v http://${ENDPOINT}/v1/chat/completions \
    -H "routing-strategy: least-request" \
    -H "Content-Type: application/json" \
    -d '{
        "model": "your-model-name",
        "messages": [{"role": "user", "content": "Say this is a test!"}],
        "temperature": 0.7
    }'

Some strategies rely on metrics queried from the Prometheus HTTP API (PromQL). See :ref:`prometheus-api-access` for configuration.

General load balancing
^^^^^^^^^^^^^^^^^^^^^^

* ``random``: routes to a randomly selected pod. Suitable as a baseline or when all pods are equivalent.
* ``least-request``: routes to the pod with the fewest in-flight requests.
* ``least-request-top-k``: like ``least-request``, but spreads load across the least-loaded pods so that several gateway replicas deciding at the same moment don't all send their next request to the same pod.

  - **Candidates:** the ready pods that are both among the ``K`` least-loaded and within ``epsilon`` requests of the least-loaded pod. One of them is picked uniformly at random.
  - **Ramp penalty:** a newly ready pod gets a temporary penalty that decays over a window, so it isn't flooded before it warms up.
  - **Tuning:** ``AIBRIX_ROUTING_LEAST_REQUEST_TOP_K`` (default ``5``), ``AIBRIX_ROUTING_LEAST_REQUEST_TOP_K_EPSILON`` (default ``4``) and ``AIBRIX_ROUTING_LEAST_REQUEST_TOP_K_RAMP_WINDOW`` (default ``300s``, ``0`` disables the ramp). See ``pkg/plugins/gateway/ENV_VARS.md``.

* ``least-busy-time``: routes to the pod with the least cumulative busy processing time.
* ``least-latency``: routes to the pod with the lowest average processing latency.
* ``least-kv-cache``: routes to the pod with the smallest KV cache occupancy (least VRAM used).
* ``least-utilization``: routes to the pod with the lowest overall utilization score.
* ``load-balance``: capacity-aware weighted least-request routing. Faster hardware is learned dynamically and receives proportionally more traffic. See ``pkg/plugins/gateway/algorithms/load_balance.md`` for details.

  Each pod is scored, and the pod with the lowest score is selected:

  .. code-block:: text

      score = (running_requests + λ·queued_requests) / EWMA(output_tokens_per_second) × (1 + α·(1 − kv_free)²)

  - ``EWMA(output_tokens_per_second)`` is the gateway-observed rate at which the pod completes output tokens.
  - ``kv_free`` is the pod's free KV-cache fraction.
  - ``λ`` defaults to ``0`` (the running count already includes requests queued in the engine) and ``α`` to ``2``.
  - **Tuning:** ``λ``, ``α`` and the free-KV-cache threshold below are set with ``AIBRIX_LOAD_BALANCE_QUEUED_WEIGHT``, ``AIBRIX_LOAD_BALANCE_KV_PRESSURE_ALPHA`` and ``AIBRIX_LOAD_BALANCE_KV_CRITICAL_FREE``, respectively.

  Edge cases:

  - **KV guardrail:** a pod with less than 10% free KV cache scores ``+Inf`` and is skipped. If every pod is below that threshold, the request still routes to the pod with the most KV headroom.
  - **Missing KV metrics:** a pod whose engine reports no KV-cache usage, or reports ``NaN``, is treated as fully free (no penalty and no guardrail).
  - **No token-rate estimate yet:** the pod is scored at the mean estimate of the others (uniform capacity when none has one).
  - **Ties:** broken using least combined GPU+CPU KV-cache usage, falling back to a random pick if cache metrics are unavailable for the tied pods.

* ``throughput``: routes to the pod that has processed the fewest total weighted tokens, favoring underloaded pods.
* ``power-of-two``: applies power-of-two-choices — randomly samples two pods and selects the better one.

KV-cache aware
^^^^^^^^^^^^^^

* ``prefix-cache``: routes to a pod that already holds a KV cache matching the request's prompt prefix, selecting the best prefix-matched pod within a configurable stddev threshold. See :doc:`prefix-cache-aware-routing` for the full guide.

  - **Load:** it is purely about prefix caching and does not itself gate on cluster-wide load imbalance; the gateway applies the :ref:`load-imbalance gate <load_imbalance_gate>` centrally ahead of it.
  - **Modes:** standard (local hash table) or KV sync (real-time distributed index, enabled with ``AIBRIX_PREFIX_CACHE_KV_EVENT_SYNC_ENABLED=true``).
  - **Chat requests:** the matched text starts with a canonical rendering of ``tools`` when present, because chat templates commonly render tools ahead of the messages. Set ``AIBRIX_PREFIX_CACHE_INCLUDE_TOOLS=false`` to match on messages only. In KV sync mode, chat requests tokenized through the remote chat template carry their ``tools``, so the template places them.

* ``prefix-cache-preble``: routes considering both prefix cache hits and pod load, based on `Preble: Efficient Distributed Prompt Scheduling for LLM Serving <https://arxiv.org/abs/2407.00023>`_.

Fairness
^^^^^^^^

* ``vtc-basic``: routes using a hybrid score that balances per-user token fairness and pod utilization. A simplified variant of the Virtual Token Counter (VTC) algorithm. See `VTC-artifact <https://github.com/Ying1123/VTC-artifact>`_ for background.

SLO-aware
^^^^^^^^^

* ``slo``: routes with awareness of per-request service-level objectives.
* ``slo-pack-load``: SLO-aware routing that consolidates load onto fewer pods.
* ``slo-least-load``: SLO-aware routing that spreads load to the least loaded pod.
* ``slo-least-load-pulling``: variant of ``slo-least-load`` that pulls metrics directly instead of using cached snapshots.

Specialized
^^^^^^^^^^^

* ``pd``: prefill-decode disaggregation routing. Splits processing between dedicated prefill pods and decode pods for optimized end-to-end latency. See :doc:`pd-disaggregation` for the full guide.

  .. code-block:: bash

      curl -v http://${ENDPOINT}/v1/chat/completions \
      -H "routing-strategy: pd" \
      -H "Content-Type: application/json" \
      -d '{
          "model": "your-model-name",
          "messages": [{"role": "user", "content": "Say this is a test!"}],
          "temperature": 0.7
      }'

* ``session-affinity``: sticky session routing that keeps related requests on the same pod. Two headers can drive it: the gateway-issued ``x-session-id`` and the caller-owned ``x-aibrix-session-key``. See :doc:`agentic-routing` for the full guide.

  .. list-table::
     :header-rows: 1
     :widths: 24 38 38

     * -
       - ``x-session-id``
       - ``x-aibrix-session-key``
     * - Issued by
       - The gateway, in the response header
       - The caller
     * - Available from
       - The second request
       - The first request
     * - Value
       - Base64-encoded pod address (``IP:Port``)
       - Any stable, opaque identifier
     * - Best for
       - Sequential multi-turn conversations
       - Concurrent agentic requests

  Sequential multi-turn chat uses ``x-session-id``: the first response carries the header, and the client sends it back unchanged. Concurrent agentic work uses ``x-aibrix-session-key``: the caller supplies a stable opaque key from the first request. If a request carries both, the gateway uses ``x-session-id`` first and falls back to ``x-aibrix-session-key`` when it does not resolve to a ready pod. Neither header is a security token, and keys longer than 256 bytes are treated as unset.

* ``external``: delegates replica selection to an operator-run HTTP service after AIBrix has resolved the model and filtered unsafe replicas. Experimental. See :doc:`external-replica-routing` for the protocol, policy modes, and configuration.

.. _load_imbalance_gate:

Load-imbalance gate
^^^^^^^^^^^^^^^^^^^

The load-imbalance gate restricts candidates to the least-loaded pods when load is severely skewed. The gateway applies it centrally, ahead of whichever strategy actually routes each request, not just when ``load-balance`` itself is selected. See ``pkg/plugins/gateway/ENV_VARS.md``.

Auto-blended capacity awareness
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

This auto-blend is **enabled by default** — no opt-in is required. The gateway silently blends ``load-balance``'s capacity-aware scoring in behind the strategy you select. This keeps any single strategy from steering traffic at an already-hot pod, even outside the :ref:`load-imbalance gate <load_imbalance_gate>`.

The caller never sees this: ``ctx.Algorithm``, response headers, and ``Validate()`` all still reflect exactly the strategy that was requested.

What is blended, by strategy:

* **Most strategies:** every strategy above gets the ``load-balance`` scorer, except the ones listed below. ``least-request-top-k`` is blended in too, when the selected strategy doesn't already route by request count, to keep multi-port/data-parallel pod routing working under the blend.
* **Bare** ``prefix-cache``: uses a dedicated 5:4 (1.25:1) lean toward cache affinity over ``load-balance`` and does not receive ``least-request-top-k``, so cache locality wins an exact-tie disagreement.
* **Bare** ``session-affinity``: gets no auto-blend at all. Its scoring is binary (the resolved pod vs. everything else), so any ``load-balance`` weight small enough to still lose an exact tie could never override the pin either. Blending it in would be dead weight, not a capacity safety net, so it keeps running its own ``Route()``/``ScoreAll()`` unblended.
* **Exclusive strategies:** ``pd``, ``slo`` / ``slo-*`` and ``external``, as well as an explicit standalone ``load-balance`` selection, are not blended.

Both blend weights default to ``1`` (see ``pkg/plugins/gateway/ENV_VARS.md``). Set ``AIBRIX_ROUTING_AUTO_BLEND_LOAD_BALANCE_WEIGHT=0`` to disable the auto-blend.


.. _config_profiles:

Config Profiles
---------------

A **config profile** lets you embed routing settings directly in a model's pod annotation, so every request to that model picks up the right configuration automatically. You can define multiple named profiles in one annotation and switch between them per-request with a single header.

Defining profiles
^^^^^^^^^^^^^^^^^

Add the ``model.aibrix.ai/config`` annotation to the pod template of your ``Deployment``, ``StormService``, or ``RayClusterFleet``:

.. code-block:: yaml

    annotations:
      model.aibrix.ai/config: |
        {
          "defaultProfile": "default",
          "profiles": {
            "default": {
              "routingStrategy": "least-request"
            },
            "large-input": {
              "routingStrategy": "pd",
              "routingConfig": {
                "promptLenBucketMinLength": 8192,
                "promptTokensGte": 8192,
                "prefillScorePolicy": "prefix_cache",
                "decodeScorePolicy": "load_balancing"
              }
            },
            "cache-friendly": {
              "routingStrategy": "least-kv-cache"
            },
            "per-replica-limits": {
              "routingStrategy": "least-request",
              "requestsPerSecondPerReplica": 5,
              "requestsInflight": 16
            },
            "offline-generation": {
              "routingStrategy": "throughput",
              "routingConfig": {
                "maxTokensGte": 2048
              }
            }
          }
        }

Selecting a profile at request time
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

Two request headers drive the config at request time:

* ``config-profile`` selects a named profile; when absent, ``defaultProfile`` (or ``"default"``) is used. ``config-profile: auto`` asks the gateway to select a concrete profile from request-local hints in each profile's ``routingConfig``.
* ``routing-strategy`` overrides the selected profile's ``routingStrategy``, unless ``lockedRoutingStrategy`` is set (see :ref:`routing_strategy_priority`).

These request-time choices are ignored when ``authoritativeRoutingPolicy`` is ``true`` (see :ref:`authoritative_routing_policy`).

.. code-block:: bash

    # Use the "batch" profile for this request
    curl http://${ENDPOINT}/v1/chat/completions \
      -H "config-profile: batch" \
      -H "Content-Type: application/json" \
      -d '{"model": "my-model", "messages": [{"role": "user", "content": "Summarize..."}]}'

Automatic profile selection
^^^^^^^^^^^^^^^^^^^^^^^^^^^

.. code-block:: bash

    # Let the gateway resolve a concrete profile from routingConfig hints
    curl http://${ENDPOINT}/v1/chat/completions \
      -H "config-profile: auto" \
      -H "Content-Type: application/json" \
      -d '{"model": "my-model", "messages": [{"role": "user", "content": "Summarize..."}], "max_tokens": 4096}'

When ``config-profile: auto`` is used, the gateway evaluates the supported request-local hints inside each profile's ``routingConfig``:

* A profile matches only when all of its supported hints match the request.
* If no profile matches, the gateway uses ``defaultProfile`` (or ``"default"``).
* ``promptTokens*`` uses the gateway's existing prompt text extraction and local token estimation.
* ``maxTokens*`` reads ``max_tokens`` first, then ``max_completion_tokens`` when ``max_tokens`` is absent.

Top-level fields
^^^^^^^^^^^^^^^^

.. list-table::
   :header-rows: 1
   :widths: 30 70

   * - Field
     - Description
   * - ``lockedRoutingStrategy``
     - Pins a single routing strategy model-wide. When set, it takes precedence over the ``routing-strategy`` header, the per-profile ``routingStrategy`` and the ``ROUTING_ALGORITHM`` env. The remaining per-profile knobs, such as ``routingConfig``, are still applied normally.
   * - ``authoritativeRoutingPolicy``
     - When ``true``, treats application-supplied ``routing-strategy``, ``config-profile`` and ``external-filter`` headers as absent. The gateway resolves ``defaultProfile``, retains its internally generated routing headers, and omits routing diagnostic headers from the client response. Defaults to ``false`` for backward compatibility.
   * - ``defaultProfile``
     - Profile name used when no ``config-profile`` header is sent. Falls back to ``"default"`` when omitted.
   * - ``profiles``
     - Map of named profiles, each carrying the per-profile fields below.

Profile fields
^^^^^^^^^^^^^^

.. list-table::
   :header-rows: 1
   :widths: 30 70

   * - Field
     - Description
   * - ``routingStrategy``
     - The routing algorithm for this profile (e.g. ``least-latency``, ``prefix-cache``, ``pd``). See the Routing Strategies section above for the full list.
   * - ``requestsPerSecondPerReplica``
     - Per-replica RPS cap for this profile. The gateway multiplies it by the model's current routable replica count to derive the effective model-wide limit, so the cap scales automatically as replicas are added or removed. Supports fractional values (e.g. ``0.5`` for one request every two seconds). Requests that exceed the limit are rejected with HTTP 429. Also sets the profile's routing strategy to ``least-request``, so traffic is balanced evenly enough for the per-replica figure to hold in aggregate. Omit or set to ``0`` for no limit. It has no environment-variable form. See :doc:`Production Model Deployments <../../production/model-deployment>` for details.
   * - ``requestsInflight``
     - Per-replica concurrency cap: the maximum number of in-flight requests allowed on a single replica. It is enforced per pod, so it needs no replica-count scaling. When every routable replica is at the cap, the request is rejected with HTTP 429 and the ``x-error-model-replica-inflight-exceeded`` header. Also sets the profile's routing strategy to ``least-request``. Independent of ``requestsPerSecondPerReplica``, and the two can be set together. Omit or set to ``0`` for no limit. It has no environment-variable form. See :doc:`Production Model Deployments <../../production/model-deployment>` for details.
   * - ``ttftThresholdS``
     - Time-to-first-token threshold in seconds for this profile's responses, overriding ``AIBRIX_TTFT_THRESHOLD_S``. A first token arriving above this value is classified as delayed in the gateway's first-token-delay metric. Omit it or set ``0`` to keep the process-wide default; ``0`` counts as unset, so a profile cannot set the threshold to zero, only to another positive value. This affects response classification only, not routing.
   * - ``routingConfig``
     - Algorithm-specific settings as a nested JSON object. It carries:

       * Auto-selection hints read by ``config-profile: auto``. Currently supported hints are ``promptTokensGte``, ``promptTokensLt``, ``maxTokensGte`` and ``maxTokensLt``.
       * Strategy-specific fields, such as ``promptLenBucketMinLength`` for ``pd``. See :doc:`Prefill-Decode Disaggregation <pd-disaggregation>` for details.
       * Per-request routing knobs that override the matching gateway environment variables for this profile's requests only. See :ref:`per_request_routing_knobs`.

.. _routing_strategy_priority:

Routing strategy priority
^^^^^^^^^^^^^^^^^^^^^^^^^

From highest to lowest:

1. ``lockedRoutingStrategy`` pinned model-wide in the config — always wins when set, even over the ``routing-strategy`` header.
2. ``routing-strategy`` request header.
3. ``routingStrategy`` from the resolved profile. The resolved profile comes from the concrete ``config-profile`` header, ``config-profile: auto`` routingConfig hints, or ``defaultProfile``.
4. ``ROUTING_ALGORITHM`` environment variable on the gateway plugin.

**Backward compatibility**: ``authoritativeRoutingPolicy`` defaults to ``false``. If a pod has no ``model.aibrix.ai/config`` annotation, the gateway falls back to the ``routing-strategy`` request header and then the ``ROUTING_ALGORITHM`` env (steps 2 and 4 above). No migration is required for existing deployments.

Locking the routing strategy
^^^^^^^^^^^^^^^^^^^^^^^^^^^^

Optionally pin a single routing strategy model-wide so it cannot be overridden at request time (see :ref:`routing_strategy_priority`). ``lockedRoutingStrategy`` only locks the strategy: the remaining per-profile knobs, such as ``routingConfig``, still follow the profile selected by ``config-profile``:

.. code-block:: yaml

    annotations:
      model.aibrix.ai/config: |
        {
          "lockedRoutingStrategy": "pd",
          "defaultProfile": "default",
          "profiles": {
            "default": {
              "routingStrategy": "pd"
            },
            "batch": {
              "routingStrategy": "random"
            }
          }
        }

.. _authoritative_routing_policy:

Authoritative routing policy
^^^^^^^^^^^^^^^^^^^^^^^^^^^^

For platform-managed models whose routing policy must be authoritative, also set ``authoritativeRoutingPolicy``. The gateway then treats the request as if it did not contain the client routing controls ``routing-strategy``, ``config-profile`` (including ``auto``), and ``external-filter``. It clears them before it resolves the profile and routing strategy, and it:

* uses ``defaultProfile``;
* preserves the routing headers that the gateway itself generates after selecting a pod;
* omits ``routing-strategy``, ``target-pod``, ``target-pod-ip``, and ``x-aibrix-config-profile`` from the client response.

.. code-block:: yaml

    annotations:
      model.aibrix.ai/config: |
        {
          "lockedRoutingStrategy": "pd",
          "authoritativeRoutingPolicy": true,
          "defaultProfile": "default",
          "profiles": {
            "default": {
              "routingStrategy": "pd",
              "routingConfig": {
                "prefillScorePolicy": "prefix_cache",
                "decodeScorePolicy": "load_balancing"
              }
            }
          }
        }

``authoritativeRoutingPolicy`` and ``lockedRoutingStrategy`` have separate responsibilities. The authoritative policy removes client routing input; ``lockedRoutingStrategy`` pins the algorithm against both the resolved default profile and ``ROUTING_ALGORITHM``. The normal :ref:`priority order <routing_strategy_priority>` applies unchanged: request profile selection uses ``defaultProfile``, the request strategy is absent, and ``lockedRoutingStrategy`` keeps its existing meaning.

.. _per_request_routing_knobs:

Per-request routing knobs
^^^^^^^^^^^^^^^^^^^^^^^^^

Besides the auto-selection hints above, ``routingConfig`` carries routing thresholds that
override the gateway's process-wide environment variables for this profile's requests only.
An unset knob keeps the environment default, and a value the matching environment variable
would reject (a negative factor, a percentage outside its range, an unknown GPU name) is
ignored, so a profile can only narrow or sharpen routing behavior. Requests to the same model
can therefore be routed with different thresholds by selecting different profiles.

Prompt-length bucketing
~~~~~~~~~~~~~~~~~~~~~~~

.. list-table::
   :header-rows: 1
   :widths: 34 26 40

   * - Field
     - Overrides
     - Description
   * - ``promptLengthBucketing``
     - ``AIBRIX_PROMPT_LENGTH_BUCKETING``
     - Turn prompt-length bucketing on or off for this profile's requests.
   * - ``bucketServe``
     - ``AIBRIX_BUCKET_SERVE``
     - Turn adaptive bucket serving on or off for this profile's requests. It only takes effect while prompt-length bucketing is on.
   * - ``bucketServeMode``
     - ``AIBRIX_BUCKET_SERVE_MODE``
     - What the adaptive cut points balance: ``throughput`` (prompt token mass) or ``rps`` (request counts). An unknown value keeps the environment default.

PD timeouts and aborts
~~~~~~~~~~~~~~~~~~~~~~

.. list-table::
   :header-rows: 1
   :widths: 34 26 40

   * - Field
     - Overrides
     - Description
   * - ``pd.decodeAbortTimeout``
     - ``AIBRIX_DECODE_ABORT_TIMEOUT``
     - Seconds the gateway waits for the decode pod to accept the abort POST after a prefill failure. ``0`` sends the abort without waiting.
   * - ``pd.decodeAbortRetryDelay``
     - ``AIBRIX_DECODE_ABORT_RETRY_DELAY``
     - Seconds between the two abort attempts. ``0`` repeats the abort immediately.
   * - ``pd.decodeFirstResponseTimeout``
     - ``AIBRIX_DECODE_FIRST_RESPONSE_TIMEOUT``
     - Seconds a streaming SGLang PD request waits, after its prefill leg succeeded, for the decode pod's first token (its first response body chunk) before the gateway fails it. ``0`` disables the watchdog.
   * - ``pd.decodeResponseTimeout``
     - ``AIBRIX_DECODE_RESPONSE_TIMEOUT``
     - The same bound for a non-streaming request, where the first body chunk is the finished answer. ``0`` disables the watchdog.
   * - ``pd.decodeStreamIdleTimeout``
     - ``AIBRIX_DECODE_STREAM_IDLE_TIMEOUT``
     - Longest gap, in seconds, between two body chunks of a streaming response before the gateway cuts the stream short. ``0`` disables this phase.
   * - ``pd.prefillRequestTimeout``
     - ``AIBRIX_PREFILL_REQUEST_TIMEOUT``
     - HTTP timeout in seconds for the prefill pod call.

PD load imbalance and decode scoring
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

.. list-table::
   :header-rows: 1
   :widths: 34 26 40

   * - Field
     - Overrides
     - Description
   * - ``pd.prefillLoadImbalanceMinSpread``
     - ``AIBRIX_PREFILL_LOAD_IMBALANCE_MIN_SPREAD``
     - Minimum prefill running-request spread (max minus min) that triggers prefill load-imbalance routing.
   * - ``pd.decodeLoadImbalanceMinSpread``
     - ``AIBRIX_DECODE_LOAD_IMBALANCE_MIN_SPREAD``
     - Minimum decode running-request spread that triggers decode load-imbalance routing.
   * - ``pd.decodeThroughputImbalanceMinSpread``
     - ``AIBRIX_DECODE_THROUGHPUT_IMBALANCE_MIN_SPREAD``
     - Minimum decode token-throughput spread (tokens/s) that triggers throughput-imbalance routing.
   * - ``pd.decodeScoreRatioThreshold``
     - ``AIBRIX_DECODE_SCORE_RATIO_THRESHOLD``
     - Max/min drain-rate score ratio above which the slowest decode pod is excluded.
   * - ``pd.decodeLBWeightRunning``
     - ``AIBRIX_DECODE_LB_WEIGHT_RUNNING``
     - Weight of the running-request term in the decode load-balancing score.
   * - ``pd.decodeLBWeightThroughput``
     - ``AIBRIX_DECODE_LB_WEIGHT_THROUGHPUT``
     - Weight of the token-throughput term in the decode load-balancing score.

PD token load and prefix matching
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

.. list-table::
   :header-rows: 1
   :widths: 34 26 40

   * - Field
     - Overrides
     - Description
   * - ``pd.tokenLoadKVWeight``
     - ``AIBRIX_TOKEN_LOAD_KV_WEIGHT``
     - Weight of resident KV tokens in the token-load charge.
   * - ``pd.tokenLoadRequestCost``
     - ``AIBRIX_TOKEN_LOAD_REQUEST_COST``
     - Fixed per-request cost, in tokens, of the token-load charge.
   * - ``pd.tokenLoadTTLSeconds``
     - ``AIBRIX_TOKEN_LOAD_TTL_SECONDS``
     - How long a charge may stay outstanding. ``0`` disables the sweep for this profile's requests.
   * - ``pd.tokenLoadSessionTTLSeconds``
     - ``AIBRIX_TOKEN_LOAD_SESSION_TTL_SECONDS``
     - How long the last prompt size of a session is kept. ``0`` disables the session delta.
   * - ``pd.hybridCacheLoadFactor``
     - ``AIBRIX_HYBRID_CACHE_LOAD_FACTOR``
     - How much a full prefix match discounts a pod's token load in the hybrid-cache-load prefill policy (0 to 1).
   * - ``pd.minMatchPct``
     - ``AIBRIX_MIN_MATCH_PCT``
     - Prefix-match percentage below which a match is treated as no match (0 to 100).

Load-balance scoring
~~~~~~~~~~~~~~~~~~~~

.. list-table::
   :header-rows: 1
   :widths: 34 26 40

   * - Field
     - Overrides
     - Description
   * - ``loadBalance.imbalanceFactor``
     - ``AIBRIX_LOAD_BALANCE_IMBALANCE_FACTOR``
     - Load-imbalance gate multiplier for pools of three or more replicas.
   * - ``loadBalance.imbalanceMinGap``
     - ``AIBRIX_LOAD_BALANCE_IMBALANCE_MIN_GAP``
     - Minimum running-request gap required to trigger the load-imbalance gate.
   * - ``loadBalance.queuedWeight``
     - ``AIBRIX_LOAD_BALANCE_QUEUED_WEIGHT``
     - Weight of engine-queued requests added to the load-balance score. ``0`` is the running-requests-only formula.
   * - ``loadBalance.kvPressureAlpha``
     - ``AIBRIX_LOAD_BALANCE_KV_PRESSURE_ALPHA``
     - Strength of the KV-pressure penalty in the load-balance score. ``0`` drops the penalty.
   * - ``loadBalance.kvCriticalFree``
     - ``AIBRIX_LOAD_BALANCE_KV_CRITICAL_FREE``
     - Free-KV-cache fraction below which a replica scores ``+Inf`` in the load-balance score.

Prefix cache and Preble
~~~~~~~~~~~~~~~~~~~~~~~

.. list-table::
   :header-rows: 1
   :widths: 34 26 40

   * - Field
     - Overrides
     - Description
   * - ``prefixCache.standardDeviationFactor``
     - ``AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR``
     - How many standard deviations above the mean replica request count a prefix-match candidate may sit. Also read by the PD prefill candidacy filter.
   * - ``preble.targetGPU``
     - ``AIBRIX_ROUTER_PREBLE_TARGET_GPU``
     - GPU the prefix-cache-preble cost model assumes. Known values: ``A6000``, ``V100``.
   * - ``preble.decodingLength``
     - ``AIBRIX_ROUTER_PREBLE_DECODING_LENGTH``
     - Assumed number of decoding tokens per request in the preble cost model.

Candidate stage
~~~~~~~~~~~~~~~

.. list-table::
   :header-rows: 1
   :widths: 34 26 40

   * - Field
     - Overrides
     - Description
   * - ``candidate.k``
     - ``AIBRIX_ROUTING_CANDIDATE_K``
     - Number of ready pods drawn at random for the load-imbalance gate and the router to score, so each request reads per-pod load for about ``k`` pods instead of all of them. ``0`` scores every ready pod. Applies only when every resolved strategy is candidate-safe (see ``pkg/plugins/gateway/ENV_VARS.md``).
   * - ``candidate.prefixM``
     - ``AIBRIX_ROUTING_CANDIDATE_PREFIX_M``
     - Most pods the router may add to the sample; for ``prefix-cache`` these are the pods holding the longest cached prefix of the prompt.

VTC fairness
~~~~~~~~~~~~

.. list-table::
   :header-rows: 1
   :widths: 34 26 40

   * - Field
     - Overrides
     - Description
   * - ``vtc.maxPodLoad``
     - ``AIBRIX_ROUTER_VTC_BASIC_MAX_POD_LOAD``
     - Running-request count at which the VTC utilization score saturates.
   * - ``vtc.fairnessWeight``
     - ``AIBRIX_ROUTER_VTC_BASIC_FAIRNESS_WEIGHT``
     - Weight of the fairness term in the VTC score. ``0`` drops the term.
   * - ``vtc.utilizationWeight``
     - ``AIBRIX_ROUTER_VTC_BASIC_UTILIZATION_WEIGHT``
     - Weight of the utilization term in the VTC score. ``0`` drops the term.
   * - ``vtc.inputTokenWeight``
     - ``AIBRIX_ROUTER_VTC_BASIC_INPUT_TOKEN_WEIGHT``
     - Weight of a request's input tokens in the VTC token tracker. Also scopes the tracker: the profile's requests get one of their own.
   * - ``vtc.outputTokenWeight``
     - ``AIBRIX_ROUTER_VTC_BASIC_OUTPUT_TOKEN_WEIGHT``
     - Weight of a request's output tokens in that tracker.
   * - ``vtc.tokenTrackerWindowSize``
     - ``AIBRIX_ROUTER_VTC_TOKEN_TRACKER_WINDOW_SIZE``
     - Sliding window of the profile's tracker, in ``vtc.tokenTrackerTimeUnit`` units.
   * - ``vtc.tokenTrackerTimeUnit``
     - ``AIBRIX_ROUTER_VTC_TOKEN_TRACKER_TIME_UNIT``
     - Bucket size of that window: ``minutes``, ``seconds`` or ``milliseconds``. An unknown name is ignored.
   * - ``vtc.tokenTrackerMinTokens``
     - ``AIBRIX_ROUTER_VTC_TOKEN_TRACKER_MIN_TOKENS``
     - Floor the profile's tracker reports while its window holds little activity.
   * - ``vtc.tokenTrackerMaxTokens``
     - ``AIBRIX_ROUTER_VTC_TOKEN_TRACKER_MAX_TOKENS``
     - Ceiling the profile's tracker reports while its window holds little activity.

Auto-blend weights
~~~~~~~~~~~~~~~~~~

.. list-table::
   :header-rows: 1
   :widths: 34 26 40

   * - Field
     - Overrides
     - Description
   * - ``autoBlend.loadBalanceWeight``
     - ``AIBRIX_ROUTING_AUTO_BLEND_LOAD_BALANCE_WEIGHT``
     - Weight of the load-balance scorer blended behind every non-exclusive strategy. ``0`` disables the auto-blend for this profile's requests.
   * - ``autoBlend.leastRequestWeight``
     - ``AIBRIX_ROUTING_AUTO_BLEND_LEAST_REQUEST_WEIGHT``
     - Weight of the ``least-request-top-k`` scorer the auto-blend adds. The key keeps its ``leastRequest`` name for compatibility.
   * - ``autoBlend.prefixCacheWeight``
     - ``AIBRIX_ROUTING_AUTO_BLEND_PREFIX_CACHE_WEIGHT``
     - Prefix-cache weight of the dedicated prefix-cache/load-balance ratio a bare ``prefix-cache`` request gets. ``0`` is rejected.
   * - ``autoBlend.prefixCacheLoadBalanceWeight``
     - ``AIBRIX_ROUTING_AUTO_BLEND_PREFIX_CACHE_LOAD_BALANCE_WEIGHT``
     - Load-balance weight of that ratio. ``0`` leaves those requests with prefix-cache scoring alone.

Scoped and environment-only knobs
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

Some knobs configure state normally shared by the whole gateway process. A profile that sets one
of them does not retune the shared state: the gateway scopes an instance to the resolved values,
so profiles that agree share one and a profile that sets none keeps the shared instance exactly
as before.

This is how ``vtc.inputTokenWeight``, ``vtc.outputTokenWeight``,
``vtc.tokenTrackerWindowSize``, ``vtc.tokenTrackerTimeUnit``, ``vtc.tokenTrackerMinTokens`` and
``vtc.tokenTrackerMaxTokens`` scope the VTC token tracker (at most 16 trackers per process; a
profile past the bound keeps the shared tracker).

Knobs that bound state shared by every model of a process stay environment-only and have no
profile field:

* the preble histogram window and eviction loop (``AIBRIX_ROUTER_PREBLE_SLIDING_WINDOW_PERIOD``, ``AIBRIX_ROUTER_PREBLE_EVICTION_LOOP_INTERVAL``)
* the session-affinity local cache capacity (``AIBRIX_SESSION_AFFINITY_MAX_LOCAL_KEYS``)
* the routing string cache bound (``AIBRIX_ROUTER_MAX_CACHED_ALGORITHM_STRINGS``)
* the token-load session table cap (``AIBRIX_TOKEN_LOAD_MAX_SESSIONS``)


External Filter
---------------

The ``external-filter`` header restricts the candidate pod pool using Kubernetes label selector syntax before the routing strategy makes its final selection. It is evaluated before routing and only takes effect when a ``routing-strategy`` is also set.

Supported selector forms:

- ``key=value``
- ``key in (a, b)``
- ``key!=value``
- comma-separated list of selectors

See the `Kubernetes label selector reference <https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/>`_ for the full syntax.

.. code-block:: bash

   curl -v http://${ENDPOINT}/v1/completions \
      -H "Content-Type: application/json" \
      -H "routing-strategy: random" \
      -H "external-filter: environment=production,tier=frontend" \
      -d '{
            "model": "deepseek-r1-distill-llama-8b",
            "prompt": "San Francisco is a",
            "max_tokens": 128,
            "temperature": 0
          }'

.. note::

    - Filtering happens **before** the routing strategy and does not change which pod the strategy considers optimal.
    - It further narrows down the pods already selected by ``model.aibrix.ai/name``.
    - If the filter eliminates all pods, the request fails with ``no ready pods for routing``.
    - ``external-filter`` is optional. When omitted, no extra filtering is applied.


.. _request_priority_tier:

Request Priority Tier
---------------------

Some workloads are not latency-sensitive. A nightly batch job, for example, is usually fine with
answering after the interactive traffic of the same model has been served. The gateway can relay
that intent to the backend engine: when ``AIBRIX_PRIORITY_TIER_ENABLED=true``, a request that
declares its tier with the ``x-aibrix-priority-tier`` header gets the matching ``priority`` value set in
its backend request body.

The gateway relies on the engine's own scheduler rather than queuing requests itself. On vLLM, that
requires the deployment to run with a priority-aware scheduler such as
``--scheduling-policy=priority``, which serves smaller ``priority`` values first. The mapping is
therefore built to only de-prioritize: a tier never makes a request jump ahead of another one, so
enabling the feature cannot delay interactive traffic on a shared endpoint.

.. list-table::
   :header-rows: 1
   :widths: 30 25 45

   * - ``x-aibrix-priority-tier``
     - ``priority``
     - Notes
   * - ``batch``
     - ``100``
     - Below the engine default of ``0``, so batch work yields to interactive requests.
   * - ``background``
     - ``1000``
     - Yields to both interactive and ``batch`` work.
   * - *(other or missing)*
     - *(unchanged)*
     - Unmapped tiers, and requests without the header, are forwarded untouched.

.. code-block:: bash

   curl -v http://${ENDPOINT}/v1/chat/completions \
      -H "Content-Type: application/json" \
      -H "x-aibrix-priority-tier: batch" \
      -d '{
            "model": "deepseek-r1-distill-llama-8b",
            "messages": [{"role": "user", "content": "Say this is a test!"}]
          }'

Behavior notes:

- The feature is off by default and the header is ignored entirely while it is off.
- Requests that already carry a ``priority`` in their body keep the value the caller set, which makes an explicit per-request choice possible even for a tier the table maps.
- Tier names are matched case-insensitively.
- A request that the gateway does not map is forwarded byte for byte, so callers that send something else in this header see no change in behavior.

.. note::

    The supported tier names are fixed in this release. Per-tier values a deployment can tune, and
    giving latency-sensitive tiers an explicit head start, are candidates for a follow-up.


Rate Limiting
-------------

The gateway supports per-user rate limiting on requests per minute (RPM) and tokens per minute (TPM). Pass the ``user`` header with a unique identifier to enable rate-limit enforcement for that client.

Create the user in the metadata service first:

.. code-block:: bash

    kubectl -n aibrix-system port-forward svc/aibrix-metadata-service 8090:8090

    curl http://localhost:8090/CreateUser \
      -H "Content-Type: application/json" \
      -d '{"name": "your-user-id", "rpm": 100, "tpm": 1000}'

.. code-block:: bash

    curl -v http://${ENDPOINT}/v1/chat/completions \
    -H "user: your-user-id" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer any_key" \
    -d '{
        "model": "your-model-name",
        "messages": [{"role": "user", "content": "Say this is a test!"}],
        "temperature": 0.7
    }'

.. note::
    If rate limiting is not needed, the ``user`` header can be omitted. To disable user RPM/TPM and model RPS quota enforcement on the gateway, set ``AIBRIX_DISABLE_RATE_LIMITING=true``. See :doc:`../../production/gateway`.


.. _prometheus-api-access:

Prometheus API Access
---------------------

Some routing strategies rely on metrics queried from the Prometheus HTTP API (PromQL). Configure the endpoint and optional Basic Auth credentials with the following environment variables.

.. list-table::
   :header-rows: 1
   :widths: 40 18 60

   * - Environment Variable
     - Default
     - Description
   * - ``PROMETHEUS_ENDPOINT``
     - (empty)
     - Prometheus HTTP API base URL (e.g. ``http://prometheus-operated.prometheus.svc:9090``). If empty, PromQL-based metrics are skipped.
   * - ``PROMETHEUS_BASIC_AUTH_SECRET_NAME``
     - (empty)
     - Kubernetes Secret name containing Basic Auth credentials. When set, takes precedence over the plaintext env vars below.
   * - ``PROMETHEUS_BASIC_AUTH_SECRET_NAMESPACE``
     - ``aibrix-system``
     - Namespace of the Secret specified by ``PROMETHEUS_BASIC_AUTH_SECRET_NAME``.
   * - ``PROMETHEUS_BASIC_AUTH_USERNAME_KEY``
     - ``username``
     - Key in ``Secret.data`` used as the Basic Auth username.
   * - ``PROMETHEUS_BASIC_AUTH_PASSWORD_KEY``
     - ``password``
     - Key in ``Secret.data`` used as the Basic Auth password.
   * - ``PROMETHEUS_BASIC_AUTH_USERNAME``
     - (empty)
     - Basic Auth username. Used only when ``PROMETHEUS_BASIC_AUTH_SECRET_NAME`` is not set.
   * - ``PROMETHEUS_BASIC_AUTH_PASSWORD``
     - (empty)
     - Basic Auth password. Used only when ``PROMETHEUS_BASIC_AUTH_SECRET_NAME`` is not set.

Example (plaintext env vars)
^^^^^^^^^^^^^^^^^^^^^^^^^^^^

.. code-block:: bash

   export PROMETHEUS_ENDPOINT="http://prometheus-operated.prometheus.svc:9090"
   export PROMETHEUS_BASIC_AUTH_USERNAME="prom_user"
   export PROMETHEUS_BASIC_AUTH_PASSWORD="prom_pass"

Example (Kubernetes Secret)
^^^^^^^^^^^^^^^^^^^^^^^^^^^

.. code-block:: yaml

   apiVersion: v1
   kind: Secret
   metadata:
     name: prometheus-basic-auth
     namespace: aibrix-system
   type: Opaque
   stringData:
     username: prom_user
     password: prom_pass

.. code-block:: bash

   export PROMETHEUS_ENDPOINT="http://prometheus-operated.prometheus.svc:9090"
   export PROMETHEUS_BASIC_AUTH_SECRET_NAME="prometheus-basic-auth"
   export PROMETHEUS_BASIC_AUTH_SECRET_NAMESPACE="aibrix-system"


Message Size Configuration
--------------------------

Two independent size limits apply to requests flowing through the gateway. Both default to **4 MiB** and must be kept in sync when you raise or lower the limit.

Gateway Plugin gRPC Buffer (``AIBRIX_GRPC_MAX_MESSAGE_SIZE_BYTES``)
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

Envoy communicates with the AIBrix gateway plugin over an external-processing (ext_proc) gRPC connection. This env var controls the maximum size of a single gRPC message on that connection. Requests or responses larger than this value are rejected before they reach routing logic.

Configure it under ``gatewayPlugin.container.envs`` in ``values.yaml``:

.. code-block:: yaml

   gatewayPlugin:
     container:
       envs:
         AIBRIX_GRPC_MAX_MESSAGE_SIZE_BYTES: "4194304"  # 4 MiB

Envoy Connection Buffer (``clientTrafficPolicy.connection.bufferLimit``)
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

Envoy itself buffers each incoming HTTP connection up to ``bufferLimit`` bytes before it begins processing. If a request body exceeds this limit Envoy returns a ``413`` before the ext_proc filter is even invoked.

Configure it under ``gateway.clientTrafficPolicy`` in ``values.yaml``:

.. code-block:: yaml

   gateway:
     clientTrafficPolicy:
       connection:
         bufferLimit: 4194304  # 4 MiB

Keeping the two values in sync
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

Because Envoy drops the connection before the gateway plugin sees it when ``bufferLimit`` is the binding limit, both values should always be set to the same size. When you need to support larger payloads (e.g. large KV-cache state-sync messages), update **both** fields together:

.. code-block:: yaml

   gatewayPlugin:
     container:
       envs:
         AIBRIX_GRPC_MAX_MESSAGE_SIZE_BYTES: "16777216"  # 16 MiB

   gateway:
     clientTrafficPolicy:
       connection:
         bufferLimit: 16777216  # 16 MiB


.. _observability_telemetry:

Observability & Telemetry
-------------------------

The gateway plugins have built-in support for distributed tracing using OpenTelemetry (OTel). This allows you to trace end-to-end request lifecycles across the external processing pipeline.

Tracing is disabled by default to conserve system resources. To enable it, complete the following two steps.

Step 1: Enable OpenTelemetry in EnvoyProxy
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

Enable ``openTelemetry.enable`` in your helm chart or add the telemetry configuration directly to your ``EnvoyProxy`` resource.

.. code-block:: yaml

    apiVersion: gateway.envoyproxy.io/v1alpha1
    kind: EnvoyProxy
    metadata:
      name: aibrix-custom-proxy-config
    spec:
      telemetry:
        tracing:
          samplingRate: 100
          provider:
            type: OpenTelemetry
            openTelemetry:
              # -- The hostname or IP address of the OpenTelemetry Collector/OTLP receiver.
              # Do NOT include the protocol scheme (e.g., http://) or port here.
              host: "otel-collector.monitoring.svc.cluster.local"

              # -- The port of the OTLP gRPC receiver (Envoy strictly uses gRPC for this).
              port: 4317

Step 2: Configure the OTLP Exporter Endpoint
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

Next, you must explicitly configure an OTLP exporter endpoint in your environment. You can enable it by setting either the global endpoint or the traces-specific endpoint.

For example, if you are running a cluster local OTel Collector, inject the following environment variables:

.. code-block:: yaml

    env:
      # Option 1: Global OTLP endpoint (Recommended)
      # NOTE: If protocol is HTTP, the SDK automatically appends '/v1/traces' to this URL.
      # If protocol is gRPC, it uses the base URL as-is.
      - name: OTEL_EXPORTER_OTLP_ENDPOINT
        value: "http://otel-collector.monitoring.svc.cluster.local:4317"

      # --- OR ---

      # Option 2: Traces-specific endpoint (Uncomment to use instead of Option 1)
      # The SDK uses this URL EXACTLY as-is without appending any paths.
      # (Example below uses port 4318 for HTTP/Protobuf exports)
      # - name: OTEL_EXPORTER_OTLP_TRACES_ENDPOINT
      #   value: "http://otel-collector.monitoring.svc.cluster.local:4318/v1/traces"

      # Specifies the transport protocol. Valid options: grpc, http, or http/protobuf
      - name: OTEL_EXPORTER_OTLP_PROTOCOL
        value: "grpc"

      # Keeps TLS active but skips server certificate validation (useful for self-signed certs)
      - name: OTEL_EXPORTER_OTLP_INSECURE_SKIP_VERIFY
        value: "true"

      # Set Headers if your OTel Collector or APM backend (e.g., Datadog, Honeycomb) requires authentication
      - name: OTEL_EXPORTER_OTLP_HEADERS
        value: "Authorization=Bearer {{ token }}"

Advanced Configuration
^^^^^^^^^^^^^^^^^^^^^^

For the full list of supported telemetry settings (including sampling rates, TLS options, and custom headers), see ``pkg/plugins/gateway/ENV_VARS.md``.


Headers Reference
-----------------

This section describes the custom headers used in request processing for routing, debugging, and rate limiting.

Target and General Headers
^^^^^^^^^^^^^^^^^^^^^^^^^^

.. list-table::
   :header-rows: 1
   :widths: 25 20 55

   * - Header Name
     - Direction
     - Description
   * - ``request-id``
     - Response
     - Unique request ID associated with the client request. Useful for correlating logs.
   * - ``x-went-into-req-headers``
     - Backend request, Response
     - Indicates whether request headers were processed correctly. Used for debugging header parsing issues.
   * - ``target-pod``
     - Backend request, Response
     - Identifies the destination selected by the routing algorithm: the backend request receives its ``IP:port`` address, while the response reports its pod name.
   * - ``target-pod-ip``
     - Response
     - ``IP:port`` address of the destination pod selected by the routing algorithm.
   * - ``routing-strategy``
     - Request, Backend request, Response
     - Selects the routing strategy for the request, forwards the applied strategy to the backend, and reports it in the response.
   * - ``external-filter``
     - Request
     - Label selector expression used to further filter candidate pods before routing.
   * - ``model``
     - Backend request
     - Model name injected into the backend request when no explicit routing strategy is selected.
   * - ``config-profile``
     - Request
     - Selects a model configuration profile for the request.
   * - ``x-aibrix-config-profile``
     - Response
     - Reports the concrete profile selected when ``config-profile`` is ``auto``.
   * - ``x-session-id``
     - Request, Response
     - Session identifier used by ``session-affinity`` routing. The response value should be sent on subsequent requests to retain affinity.
   * - ``x-aibrix-session-key``
     - Request
     - Caller-owned opaque session key used by ``session-affinity`` routing and, under ``pd`` routing with the ``token_load`` or ``hybrid_cache_load`` prefill policy, to charge a multi-turn conversation only for its new tokens.
   * - ``traceparent``
     - Request, Backend request
     - W3C Trace Context propagated through requests initiated by the gateway, including prefill-decode routing.
   * - ``prefill-target-pod``
     - Response
     - Name of the prefill pod selected by ``pd`` routing.
   * - ``prefill-target-pod-ip``
     - Response
     - IP address of the prefill pod selected by ``pd`` routing.
   * - ``x-aibrix-priority-tier``
     - Request
     - Declares the priority tier of the request, for example ``batch`` or ``background``. When ``AIBRIX_PRIORITY_TIER_ENABLED=true``, the gateway maps the tier to the ``priority`` field of the backend request; see :ref:`request_priority_tier`.

Routing and Error Debugging Headers
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

.. list-table::
   :header-rows: 1
   :widths: 25 75

   * - Header Name
     - Description
   * - ``x-error-user``
     - Identifies errors caused by incorrect user input.
   * - ``x-error-routing``
     - Indicates a routing failure, such as being unable to select a target pod.
   * - ``x-error-response-unmarshal``
     - Signals that the response body could not be parsed, typically due to an internal error.
   * - ``x-error-response-unknown``
     - Generic error header when no specific cause is identified.
   * - ``x-error-request-body-processing``
     - Marks a request body parsing failure, such as invalid JSON.
   * - ``x-error-no-model-in-request``
     - Indicates that no model was specified in the request.
   * - ``x-error-no-model-backends``
     - Indicates that the requested model exists but has no active backend pods.
   * - ``x-error-invalid-routing-strategy``
     - Indicates that an unsupported routing strategy was specified.
   * - ``x-error-pd-prefill``
     - Marks a gateway-generated error because the prefill leg of a ``pd`` request failed. See :doc:`pd-disaggregation`.
   * - ``x-error-pd-decode``
     - Marks a gateway-generated error because the decode watchdog fired after a successful prefill. See :doc:`pd-disaggregation`.
   * - ``x-error-model-rps-exceeded``
     - The request exceeded the model's ``requestsPerSecondPerReplica`` cap. See :doc:`../../production/model-deployment`.
   * - ``x-error-incr-model-rps``
     - Error encountered while incrementing the model RPS counter.
   * - ``x-error-model-replica-inflight-exceeded``
     - Every routable replica is at the profile's ``requestsInflight`` cap.
   * - ``x-error-multipart-parsing``
     - The multipart/form-data body could not be parsed, typically on image, audio, or video endpoints. See :doc:`omni`.
   * - ``x-error-video-not-found``
     - The video job ID is unknown, expired, out of scope, or its owning pod is gone. See :doc:`omni-video`.
   * - ``x-error-video-job-pod-unavailable``
     - The video job's owning pod is temporarily unroutable. See :doc:`omni-video`.

Streaming Headers
^^^^^^^^^^^^^^^^^

.. list-table::
   :header-rows: 1
   :widths: 25 75

   * - Header Name
     - Description
   * - ``x-error-streaming``
     - Signals an error during response streaming.
   * - ``x-error-stream``
     - Indicates an incorrect ``stream`` value in the request body.
   * - ``x-error-no-stream-options-include-usage``
     - Indicates whether usage statistics were included in the streaming response.

Rate Limiting Headers
^^^^^^^^^^^^^^^^^^^^^

.. list-table::
   :header-rows: 1
   :widths: 25 75

   * - Header Name
     - Description
   * - ``x-update-rpm``
     - Indicates the RPM (requests per minute) counter was updated successfully.
   * - ``x-update-tpm``
     - Indicates the TPM (tokens per minute) counter was updated successfully.
   * - ``x-error-rpm-exceeded``
     - Signals that the request exceeded the allowed RPM limit.
   * - ``x-error-tpm-exceeded``
     - Signals that the request exceeded the allowed TPM limit.
   * - ``x-error-incr-rpm``
     - Error encountered while incrementing the RPM counter.
   * - ``x-error-incr-tpm``
     - Error encountered while incrementing the TPM counter.
   * - ``x-error-get-rpm``
     - Error encountered while reading the RPM counter.
   * - ``x-error-get-tpm``
     - Error encountered while reading the TPM counter.


Debugging Guidelines
--------------------

1. **Identify error headers** — inspect ``x-error-routing``, ``x-error-user``, ``x-error-response-unmarshal``, and ``x-error-response-unknown`` to locate the root cause. For request body issues, check ``x-error-request-body-processing`` and ``x-error-no-model-in-request``.

2. **Verify routing and model assignment** — confirm ``target-pod`` is set. If ``x-error-no-model-in-request`` or ``x-error-no-model-backends`` appears, verify the request body includes a valid model name and that the model has active backend pods. If ``x-error-invalid-routing-strategy`` is present, confirm the strategy name is supported.

3. **Diagnose streaming issues** — check ``x-error-streaming`` and ``x-error-stream``. If usage statistics are missing from a streaming response, inspect ``x-error-no-stream-options-include-usage``.

4. **Investigate rate limiting** — if a request was rejected, check ``x-error-rpm-exceeded`` or ``x-error-tpm-exceeded``. If counters failed to update, look for ``x-error-incr-rpm`` or ``x-error-incr-tpm``; if they could not be read, ``x-error-get-rpm`` or ``x-error-get-tpm``. Successful updates are indicated by ``x-update-rpm`` and ``x-update-tpm``.

Check gateway object status
^^^^^^^^^^^^^^^^^^^^^^^^^^^

Verify that all gateway objects have ``status.conditions == Accepted``:

.. code-block:: bash

    kubectl describe gatewayclass -n aibrix-system

    kubectl describe gateway -n aibrix-system

    kubectl describe envoypatchpolicy -n aibrix-system

    # check for all objects
    kubectl describe envoyextensionpolicy -n aibrix-system

    # check for all objects
    kubectl describe httproute -n aibrix-system

Collect logs
^^^^^^^^^^^^

Collect logs from the Envoy proxy and the gateway plugin for deeper investigation:

.. code-block:: bash

    $ kubectl get pods -n envoy-gateway-system
    NAME                                                      READY   STATUS    RESTARTS   AGE
    envoy-aibrix-system-aibrix-eg-903790dc-84ccfcbc6b-hw2lq   2/2     Running   0          13m
    envoy-gateway-7c7659ffc9-rvm5s                            1/1     Running   0          16m

    $ kubectl logs envoy-aibrix-system-aibrix-eg-903790dc-84ccfcbc6b-hw2lq -n envoy-gateway-system

.. code-block:: bash

    $ kubectl get pods -n aibrix-system
    NAME                                        READY   STATUS             RESTARTS   AGE
    aibrix-controller-manager-fb4495448-j9k6g   1/1     Running            0          22m
    aibrix-gateway-plugins-6bd9fcd5b9-2bwpr     1/1     Running            0          22m
    aibrix-gpu-optimizer-df9db96c8-2fctd        1/1     Running            0          22m
    aibrix-kuberay-operator-5bf4985d86-7g4tz    1/1     Running            0          22m
    aibrix-metadata-service-9d4cd7f77-mq7tr     1/1     Running            0          22m
    aibrix-redis-master-7d6b77c794-bcqxc        1/1     Running            0          22m

    $ kubectl logs aibrix-gateway-plugins-6bd9fcd5b9-2bwpr -n aibrix-system

.. seealso::

   :doc:`prefix-cache-aware-routing`
       Prefix-cache aware routing and KV event sync.
   :doc:`pd-disaggregation`
       Prefill-decode disaggregation, bucketing, and scoring policies.
   :doc:`agentic-routing`
       Session affinity for multi-turn and concurrent agentic requests.
   :doc:`semantic-router`
       Topic-based routing through the vLLM Semantic Router ext_proc filter.
   :doc:`omni`
       Multi-modal serving and sticky video job routing.
   :doc:`external-replica-routing`
       Delegating replica selection to an operator-run decision service.
   :doc:`../../designs/aibrix-gateway`
       Gateway architecture and how to add a routing algorithm.
   :doc:`../../production/gateway`
       Production replica counts, Redis, timeouts, and traffic policies.

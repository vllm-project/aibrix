.. _model_deployment_production:

=================================
Production Model Deployments
=================================

This guide covers what to check before a model deployment goes to production: required labels, choosing a routing strategy, capping model-level throughput, configuring readiness, and sizing replicas.

.. contents:: On this page
   :local:
   :depth: 2


Required Labels and Annotations
---------------------------------

Every pod template that AIBrix manages needs at minimum two labels. Without them the gateway cannot route traffic to the pod.

.. list-table::
   :header-rows: 1
   :widths: 35 65

   * - Label
     - Description
   * - ``model.aibrix.ai/name: <model-name>``
     - The model identifier clients send in the ``model`` field of their requests. Must be unique per model.
   * - ``model.aibrix.ai/port: "<port>"``
     - The container port the inference server listens on (e.g. ``"8000"``).

Choosing a Routing Strategy
-----------------------------

Set the default routing strategy for a model via the config annotation rather than relying on the global env default. This makes the intent explicit and allows different models to use different strategies.

.. code-block:: yaml

    annotations:
      model.aibrix.ai/config: |
        {
          "profiles": {
            "default": { "routingStrategy": "least-latency" }
          }
        }

A practical starting point for common workload types:

.. list-table::
   :header-rows: 1
   :widths: 35 65

   * - Workload
     - Recommended strategy
   * - Multi-turn chat (shared system prompts)
     - ``prefix-cache`` — routes repeat prefixes to pods that already hold the KV cache.
   * - Independent requests (batch, summarization)
     - ``least-request`` — evenly distributes load across pods.
   * - Latency-sensitive interactive
     - ``least-latency`` — routes to the pod with the lowest recent latency.
   * - High-throughput inference
     - ``pd`` — separates prefill and decode for maximum GPU utilization.
   * - Multi-user SLO enforcement
     - ``vtc-basic`` — balances fairness across users while keeping pods loaded.

See the `Deploying Gateway <gateway.html>`_ guide for the full strategy list.


Config Profiles for Production
---------------------------------

Config profiles let a single model deployment serve multiple traffic classes without separate deployments. A common pattern for production is to define a profile per client type:

.. code-block:: yaml

    annotations:
      model.aibrix.ai/config: |
        {
          "defaultProfile": "default",
          "profiles": {
            "default": {
              "routingStrategy": "least-latency"
            },
            "batch": {
              "routingStrategy": "throughput"
            },
            "pd": {
              "routingStrategy": "pd"
            }
          }
        }

Clients select a profile with the ``config-profile`` request header:

.. code-block:: bash

    curl http://${ENDPOINT}/v1/chat/completions \
      -H "config-profile: batch" \
      -H "Content-Type: application/json" \
      -d '{"model": "my-model", "messages": [{"role": "user", "content": "Summarize: ..."}]}'

If no header is set, the ``defaultProfile`` is used.


Capping Model Throughput (RPS Limiting)
-----------------------------------------

What it is
~~~~~~~~~~

``requestsPerSecond`` sets a hard cap on how many requests per second the gateway will forward to a given model. Requests that exceed the limit are rejected immediately with HTTP ``429 Too Many Requests`` before any routing or inference work is done.

This is a **model-level** cap (all users combined), distinct from the per-user RPM/TPM limits. Use it to:

- Protect a model from accidental traffic spikes.
- Enforce a cost budget (GPU-hours) for a model in a shared cluster.
- Guarantee headroom for higher-priority models on the same gateway.

How to configure it
~~~~~~~~~~~~~~~~~~~

Add ``requestsPerSecond`` to the relevant profile in the model's config annotation:

.. code-block:: yaml

    annotations:
      model.aibrix.ai/config: |
        {
          "profiles": {
            "default": {
              "routingStrategy": "least-latency",
              "requestsPerSecond": 50
            }
          }
        }

To apply different limits per traffic class, set it per profile:

.. code-block:: yaml

    annotations:
      model.aibrix.ai/config: |
        {
          "defaultProfile": "default",
          "profiles": {
            "default": {
              "routingStrategy": "least-latency",
              "requestsPerSecond": 100
            },
            "batch": {
              "routingStrategy": "throughput",
              "requestsPerSecond": 20
            }
          }
        }

Here, interactive traffic (``default`` profile) is capped at 100 RPS while batch traffic is limited to 20 RPS.

What clients see when the limit is hit
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

The gateway returns:

.. code-block:: text

    HTTP/1.1 429 Too Many Requests
    x-error-model-rps-exceeded: true

    {"error": {"message": "model: my-model has exceeded RPS: 50", "type": "rate_limit_error", "code": "rate_limit_exceeded"}}

How it works internally
~~~~~~~~~~~~~~~~~~~~~~~~

The counter is stored in Redis and incremented atomically on each request. The 1-second window resets automatically. If routing fails after the counter is incremented (e.g. no ready pods), the increment is rolled back so failed requests do not consume quota.

.. note::
    ``requestsPerSecond`` requires Redis to be enabled on the gateway plugin. Without Redis, the counter is in-process and is not shared across gateway replicas. See the `Deploying Gateway <gateway.html>`_ guide — Enabling Redis for Multi-Replica Deployments.

Omit ``requestsPerSecond`` or set it to ``0`` to disable the limit.


Readiness and Health
---------------------

The gateway only routes to pods that Kubernetes considers **Ready**. Ensure your pod's readiness probe waits for the inference server to fully load the model before marking the pod ready. For large models this can take several minutes.

A practical readiness probe for vLLM:

.. code-block:: yaml

    readinessProbe:
      httpGet:
        path: /health
        port: 8000
      initialDelaySeconds: 60
      periodSeconds: 10
      failureThreshold: 30    # allow up to 5 minutes for model load

If a pod fails its readiness probe after previously being ready (e.g. due to OOM), the gateway stops routing to it immediately without dropping in-flight requests.


Replica Sizing
--------------

There is no universal formula, but a useful starting point:

1. **Measure single-replica capacity** — run a short load test at increasing QPS until latency or error rate degrades. Note the sustainable QPS.
2. **Apply a safety margin** — target 60–70% of the measured peak to leave room for spikes.
3. **Add replicas** — ``replicas = ceil(target_QPS / sustainable_QPS_per_replica)``.

For PD deployments, size prefill and decode pods separately: prefill pods are compute-bound (add more for high-input-token workloads), decode pods are memory-bandwidth-bound (add more for long-output workloads).


Rolling Updates
---------------

LLM pods take a long time to start. Configure ``maxUnavailable: 0`` and ``maxSurge: 1`` (or higher) to ensure capacity is not lost during rollouts:

.. code-block:: yaml

    spec:
      strategy:
        type: RollingUpdate
        rollingUpdate:
          maxUnavailable: 0
          maxSurge: 1

With ``maxUnavailable: 0``, Kubernetes only terminates an old pod after the new pod passes its readiness probe. This prevents the gateway from routing to a pod that has not finished loading the model.

For PD deployments, update prefill and decode pods in separate rollouts — updating both simultaneously can temporarily leave some rolesets incomplete, causing the gateway to skip them.


Observability Checklist
------------------------

Before shipping to production, ensure you have visibility into:

- **Request rate per model** — ``aibrix_gateway_requests_total`` counter, broken down by ``model`` label.
- **Routing latency** — time between request arrival and pod selection.
- **RPS limit rejections** — watch for ``x-error-model-rps-exceeded`` response headers or the corresponding metric.
- **Pod readiness churn** — alerts on pods that flip between Ready and NotReady indicate OOM or instability.
- **Prefill timeout rate** (PD only) — ``pd-prefill-request-error`` log entries; a high rate suggests prefill pods are overloaded.

See the `Observability <observability.html>`_ guide for the full metrics reference.

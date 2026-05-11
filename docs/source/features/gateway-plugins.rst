.. _gateway:

===============
Gateway Routing
===============

The AIBrix gateway is built on Envoy Gateway and acts as the single entry point for all LLM inference requests. It handles dynamic model discovery, request routing, rate limiting, and response streaming. For a deep dive into the internal design, see the `AIBrix Router <../designs/aibrix-router.html>`_ architecture guide.

Dynamic Routing
---------------

When a model or LoRA adapter is deployed, the respective controller automatically creates an ``HTTPRoute`` object. The gateway dynamically discovers these routes to forward incoming requests without any manual configuration.

First, retrieve the external IP and port of the Envoy proxy:

.. code-block:: bash

    NAME                                     TYPE           CLUSTER-IP      EXTERNAL-IP   PORT(S)                                   AGE
    envoy-aibrix-system-aibrix-eg-903790dc   LoadBalancer   10.96.239.246   101.18.0.4    80:32079/TCP                              10d
    envoy-gateway                            ClusterIP      10.96.166.226   <none>        18000/TCP,18001/TCP,18002/TCP,19001/TCP   10d

In most Kubernetes setups, ``LoadBalancer`` is supported by default. Capture the external IP:

.. code-block:: bash

    LB_IP=$(kubectl get svc/envoy-aibrix-system-aibrix-eg-903790dc -n envoy-gateway-system -o=jsonpath='{.status.loadBalancer.ingress[0].ip}')
    ENDPOINT="${LB_IP}:80"

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

The model name in the request body must match the ``model.aibrix.ai/name`` label in your deployment:

.. code-block:: bash

    curl -v http://${ENDPOINT}/v1/chat/completions \
    -H "Content-Type: application/json" \
    -d '{
        "model": "deepseek-r1-distill-llama-8b",
        "messages": [{"role": "user", "content": "Say this is a test!"}],
        "temperature": 0.7
    }'

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


Routing Strategies
------------------

The routing strategy controls how the gateway selects the target pod for each request. It can be set per-request via the ``routing-strategy`` header, or globally via the ``ROUTING_ALGORITHM`` environment variable.

Some strategies rely on metrics queried from the Prometheus HTTP API (PromQL). See `Prometheus API Access <../production/observability.html#prometheus-api-access>`_ for configuration.

General load balancing
^^^^^^^^^^^^^^^^^^^^^^

* ``random``: routes to a randomly selected pod. Suitable as a baseline or when all pods are equivalent.
* ``least-request``: routes to the pod with the fewest in-flight requests.
* ``least-busy-time``: routes to the pod with the least cumulative busy processing time.
* ``least-latency``: routes to the pod with the lowest average processing latency.
* ``least-kv-cache``: routes to the pod with the smallest KV cache occupancy (least VRAM used).
* ``least-gpu-cache``: routes to the pod with the lowest GPU cache utilization.
* ``least-utilization``: routes to the pod with the lowest overall utilization score.
* ``throughput``: routes to the pod that has processed the fewest total weighted tokens, favoring underloaded pods.
* ``power-of-two``: applies power-of-two-choices — randomly samples two pods and selects the better one.

KV-cache aware
^^^^^^^^^^^^^^

* ``prefix-cache``: routes to a pod that already holds a KV cache matching the request's prompt prefix, with load balancing and multi-turn conversation support.
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

* ``pd``: prefill-decode disaggregation routing. Splits processing between dedicated prefill pods and decode pods for optimized end-to-end latency.

  .. code-block:: bash

      curl -v http://${ENDPOINT}/v1/chat/completions \
      -H "routing-strategy: pd" \
      -H "Content-Type: application/json" \
      -d '{
          "model": "your-model-name",
          "messages": [{"role": "user", "content": "Say this is a test!"}],
          "temperature": 0.7
      }'

* ``session-affinity``: sticky session routing. Encodes the target pod's address (``IP:Port``) as a base64 value in the ``x-session-id`` response header. Subsequent requests that include this header are routed to the same pod. If that pod is no longer available, the gateway transparently fails over to a new pod and issues a fresh session ID.

  How it works:

  - On the first request (no ``x-session-id``), the gateway picks a ready pod and returns an ``x-session-id`` response header.
  - The client stores and resends this header on follow-up requests (e.g., in multi-turn conversations).
  - The gateway decodes the session ID to recover the original pod address and routes to it.
  - If the pod is unavailable (scaled down or evicted), it falls over to a new pod and issues a new session ID.

  .. note::
      ``x-session-id`` encodes only network location. It is not a security token and must not be used for authentication or authorization.

  .. code-block:: bash

      curl -v http://${ENDPOINT}/v1/chat/completions \
      -H "routing-strategy: session-affinity" \
      -H "Content-Type: application/json" \
      -d '{
          "model": "your-model-name",
          "messages": [{"role": "user", "content": "Say this is a test!"}],
          "temperature": 0.7
      }'

To override the strategy for a single request, pass the ``routing-strategy`` header with any of the values above:

.. code-block:: bash

    curl -v http://${ENDPOINT}/v1/chat/completions \
    -H "routing-strategy: least-request" \
    -H "Content-Type: application/json" \
    -d '{
        "model": "your-model-name",
        "messages": [{"role": "user", "content": "Say this is a test!"}],
        "temperature": 0.7
    }'


Rate Limiting
-------------

The gateway supports per-user rate limiting on requests per minute (RPM) and tokens per minute (TPM). Pass the ``user`` header with a unique identifier to enable rate-limit enforcement for that client.

For details on managing users, see the `User Management documentation <https://github.com/vllm-project/aibrix/blob/main/pkg/metadata/README.md>`_.

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
    If rate limiting is not needed, the ``user`` header can be omitted.


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

    1. Filtering happens **before** the routing strategy and does not change which pod the strategy considers optimal.
    2. ``external-filter`` only takes effect when ``routing-strategy`` is set.
    3. It further narrows down the pods already selected by ``model.aibrix.ai/name``.
    4. If the filter eliminates all pods, the request fails with ``no ready pods for routing``.
    5. ``external-filter`` is optional. When omitted, no extra filtering is applied.


Headers Reference
-----------------

This section describes the custom headers used in request processing for routing, debugging, and rate limiting.

Target and General Headers
^^^^^^^^^^^^^^^^^^^^^^^^^^^

.. list-table::
   :header-rows: 1
   :widths: 25 75

   * - Header Name
     - Description
   * - ``request-id``
     - Unique request ID associated with the client request. Useful for correlating logs.
   * - ``x-went-into-req-headers``
     - Indicates whether request headers were processed correctly. Used for debugging header parsing issues.
   * - ``target-pod``
     - The destination pod selected by the routing algorithm. Useful for verifying routing decisions.
   * - ``routing-strategy``
     - The routing strategy applied to this request.
   * - ``external-filter``
     - Label selector expression used to further filter candidate pods before routing.

Routing and Error Debugging Headers
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

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


Debugging Guidelines
^^^^^^^^^^^^^^^^^^^^

1. **Identify error headers** — inspect ``x-error-routing``, ``x-error-user``, ``x-error-response-unmarshal``, and ``x-error-response-unknown`` to locate the root cause. For request body issues, check ``x-error-request-body-processing`` and ``x-error-no-model-in-request``.

2. **Verify routing and model assignment** — confirm ``target-pod`` is set. If ``x-error-no-model-in-request`` or ``x-error-no-model-backends`` appears, verify the request body includes a valid model name and that the model has active backend pods. If ``x-error-invalid-routing-strategy`` is present, confirm the strategy name is supported.

3. **Diagnose streaming issues** — check ``x-error-streaming`` and ``x-error-stream``. If usage statistics are missing from a streaming response, inspect ``x-error-no-stream-options-include-usage``.

4. **Investigate rate limiting** — if a request was rejected, check ``x-error-rpm-exceeded`` or ``x-error-tpm-exceeded``. If counters failed to update, look for ``x-error-incr-rpm`` or ``x-error-incr-tpm``. Successful updates are indicated by ``x-update-rpm`` and ``x-update-tpm``.

Verify that all gateway objects have ``status.conditions == Accepted``:

.. code-block:: bash

    kubectl describe gatewayclass -n aibrix-system

    kubectl describe gateway -n aibrix-system

    kubectl describe envoypatchpolicy -n aibrix-system

    # check for all objects
    kubectl describe envoyextensionpolicy -n aibrix-system

    # check for all objects
    kubectl describe httproute -n aibrix-system

Collect logs from the Envoy proxy and the gateway plugin for deeper investigation:

.. code-block:: bash

    kubectl get pods -n envoy-gateway-system

    NAME                                                      READY   STATUS    RESTARTS   AGE
    envoy-aibrix-system-aibrix-eg-903790dc-84ccfcbc6b-hw2lq   2/2     Running   0          13m
    envoy-gateway-7c7659ffc9-rvm5s                            1/1     Running   0          16m

    kubectl logs envoy-aibrix-system-aibrix-eg-903790dc-84ccfcbc6b-hw2lq -n envoy-gateway-system

.. code-block:: bash

    kubectl get pods -n aibrix-system

    NAME                                        READY   STATUS             RESTARTS   AGE
    aibrix-controller-manager-fb4495448-j9k6g   1/1     Running            0          22m
    aibrix-gateway-plugins-6bd9fcd5b9-2bwpr     1/1     Running            0          22m
    aibrix-gpu-optimizer-df9db96c8-2fctd        1/1     Running            0          22m
    aibrix-kuberay-operator-5bf4985d86-7g4tz    1/1     Running            0          22m
    aibrix-metadata-service-9d4cd7f77-mq7tr     1/1     Running            0          22m
    aibrix-redis-master-7d6b77c794-bcqxc        1/1     Running            0          22m

    kubectl logs aibrix-gateway-plugins-6bd9fcd5b9-2bwpr -n aibrix-system

.. _config_profiles:

Config Profiles
---------------

A **config profile** lets you embed routing settings directly in a model's pod annotation, so every request to that model picks up the right configuration automatically. You can define multiple named profiles in one annotation and switch between them per-request with a single header.

**Setting up a profile**

Add the ``model.aibrix.ai/config`` annotation to the pod template of your ``Deployment``, ``StormService``, or ``RayClusterFleet``:

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
              "routingStrategy": "throughput"
            }
          }
        }

**Selecting a profile at request time**

Add the ``config-profile`` header. If the header is absent, the ``defaultProfile`` is used:

.. code-block:: bash

    # Use the "batch" profile for this request
    curl http://${ENDPOINT}/v1/chat/completions \
      -H "config-profile: batch" \
      -H "Content-Type: application/json" \
      -d '{"model": "my-model", "messages": [{"role": "user", "content": "Summarize..."}]}'

**Profile fields**

.. list-table::
   :header-rows: 1
   :widths: 30 70

   * - Field
     - Description
   * - ``routingStrategy``
     - The routing algorithm for this profile (e.g. ``least-latency``, ``prefix-cache``, ``pd``). See the Routing Strategies section above for the full list.
   * - ``requestsPerSecond``
     - Model-level RPS cap for this profile. Requests that exceed the limit are rejected with HTTP 429. Omit or set to ``0`` for no limit. See `Production Model Deployments <../production/model-deployment.html>`_ for details.
   * - ``routingConfig``
     - Algorithm-specific settings as a nested JSON object. Currently used by the ``pd`` strategy for prompt-length bucketing and standard inference pod configuration. See `Prefill-Decode Disaggregation <pd-disaggregation.html>`_ for details.

**Routing strategy priority** (highest to lowest):

1. ``routing-strategy`` request header — always wins, even if a profile is active.
2. ``routingStrategy`` from the resolved config profile.
3. ``ROUTING_ALGORITHM`` environment variable on the gateway plugin.

**Backward compatibility**: if a pod has no ``model.aibrix.ai/config`` annotation, the gateway falls back to the ``model.aibrix.ai/routing-strategy`` pod label and the ``ROUTING_ALGORITHM`` env. No migration is required for existing deployments.

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
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

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

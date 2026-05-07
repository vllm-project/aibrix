.. _gateway:

===================
Deploying Gateway
===================

This guide covers production deployment configuration for the AIBrix gateway components.

Configuring Resources and Replica Count
----------------------------------------

For production deployments, we recommend tuning replica counts and resource allocations for both
the **Gateway Plugin** and the **Envoy Proxy** data plane.

Gateway Plugin
~~~~~~~~~~~~~~

The gateway plugin (``gatewayPlugin``) handles request routing logic and external processing.
Set ``replicaCount`` and container resources in your ``values.yaml`` override:

.. code-block:: yaml

    gatewayPlugin:
      replicaCount: 3
      container:
        resources:
          limits:
            cpu: "16"
            memory: 32Gi
          requests:
            cpu: "16"
            memory: 32Gi

Envoy Proxy
~~~~~~~~~~~

The Envoy proxy (``gateway.envoyProxy``) is the data-plane component managed by Envoy Gateway.
Set ``replicas`` and per-container resources as follows:

.. code-block:: yaml

    gateway:
      envoyProxy:
        replicas: 3
        container:
          envoy:
            resources:
              limits:
                cpu: "8"
                memory: 16Gi
              requests:
                cpu: "8"
                memory: 16Gi

Enabling Redis for Multi-Replica Deployments
---------------------------------------------

When running more than one gateway plugin replica, shared state is required so that all
instances agree on routing decisions (e.g. prefix-cache block assignments, rate-limit counters).
The gateway plugin reads the ``REDIS_HOST`` environment variable at startup to connect to a
Redis instance. If the variable is unset or Redis is unreachable, each replica operates with
in-process state only, which causes inconsistent routing across pods.

Enable Redis by pointing the gateway plugin at the bundled Redis instance:

.. code-block:: yaml

    gatewayPlugin:
      dependencies:
        redis:
          host: ""   # leave empty to use the chart-managed Redis (aibrix-redis-master)
          port: 6379

The Helm chart sets ``REDIS_HOST`` automatically from ``gatewayPlugin.dependencies.redis.host``,
defaulting to ``<release-name>-redis-master`` when the field is empty. For an external Redis
cluster, set ``host`` to the service hostname or IP of your Redis endpoint.

Sizing Redis
~~~~~~~~~~~~

The bundled Redis instance is deployed under ``metadata.redis``. For production workloads with
multiple gateway plugin replicas, increase its CPU, memory, and (if using persistence)
storage from the defaults:

.. code-block:: yaml

    metadata:
      redis:
        replicas: 1
        container:
          resources:
            requests:
              cpu: "1"
              memory: 2Gi
            limits:
              cpu: "2"
              memory: 4Gi

The right sizing depends on request throughput and the number of distinct prefix-cache keys
in flight. As a starting point, allocate roughly **1 GiB of memory per 1 000 concurrent
requests** and increase CPU if Redis becomes a latency bottleneck (monitor ``redis_commands_duration_seconds``).

.. note::
    If you are using an externally managed Redis (e.g. AWS ElastiCache, Google Memorystore),
    set ``gatewayPlugin.dependencies.redis.host`` to the external endpoint and remove the
    ``metadata.redis`` block from your override — the chart will not deploy its own Redis when
    a custom host is provided.

Configuring Buffer Limits, Connections, and QPS
------------------------------------------------

AIBrix exposes three policy knobs under ``gateway`` to control how traffic flows between
clients, the Envoy proxy, and backend pods.

Client Traffic Policy
~~~~~~~~~~~~~~~~~~~~~

``clientTrafficPolicy`` governs connections and request sizes arriving from external clients:

- **bufferLimit** — maximum bytes buffered per request/response body (input/output size).
  The default ``4194304`` is 4 MiB. Increase this if your workloads send large prompts or
  receive large completions.
- **connectionLimit** — maximum simultaneous TCP connections accepted by the proxy.
- **http2.maxConcurrentStreams** — maximum concurrent HTTP/2 streams per connection from a client.

.. code-block:: yaml

    gateway:
      clientTrafficPolicy:
        connection:
          bufferLimit: 4194304   # bytes; 4 MiB default
          connectionLimit:
            value: 1024
        http2:
          maxConcurrentStreams: 1024

Backend Traffic Policy
~~~~~~~~~~~~~~~~~~~~~~

``backendTrafficPolicy`` controls how Envoy distributes load across backend pods and
limits the blast radius of a single slow or failing pod:

- **circuitBreaker.maxConnections** — maximum TCP connections to a single backend pod.
- **circuitBreaker.maxParallelRequests** — maximum in-flight requests to a single backend pod (effective QPS cap when combined with latency).
- **circuitBreaker.maxPendingRequests** — maximum requests queued waiting for a connection to a backend pod.
- **circuitBreaker.maxParallelRetries** — maximum concurrent retries to a backend pod.
- **circuitBreaker.maxRequestsPerConnection** — maximum requests served on a single connection before it is recycled.
- **http2.maxConcurrentStreams** — maximum concurrent HTTP/2 streams per connection to a backend pod.

.. code-block:: yaml

    gateway:
      backendTrafficPolicy:
        circuitBreaker:
          maxConnections: 1024
          maxParallelRequests: 1024
          maxParallelRetries: 1024
          maxPendingRequests: 1024
          maxRequestsPerConnection: 1024
        http2:
          maxConcurrentStreams: 1024

Envoy Patch Policy
~~~~~~~~~~~~~~~~~~

``envoyPatchPolicy`` provides lower-level overrides applied directly to the Envoy xDS
configuration, covering route timeouts and the limits for the original-destination cluster:

- **route.timeout** — maximum duration Envoy waits for a backend response. Increase this
  for long-running inference requests.
- **route.connectTimeout** — maximum time to establish a TCP connection to a backend pod.
- **circuitBreakers.maxConnections / maxRequests / maxPendingRequests** — same semantics
  as the backend traffic policy above but applied at the Envoy cluster level.

.. code-block:: yaml

    gateway:
      envoyPatchPolicy:
        route:
          timeout: 120s
          connectTimeout: 6s
        circuitBreakers:
          maxConnections: 1024
          maxRequests: 1024
          maxPendingRequests: 1024

Choosing a Routing Strategy
----------------------------

If no ``routing-strategy`` header is provided, the gateway defaults to ``random`` routing.

.. code-block:: bash

    # No routing-strategy header — gateway picks a random ready pod
    curl -v http://${ENDPOINT}/v1/chat/completions \
    -H "Content-Type: application/json" \
    -d '{
        "model": "your-model-name",
        "messages": [{"role": "user", "content": "Hello!"}],
        "temperature": 0.7
    }'

For production workloads we recommend selecting a strategy based on your traffic pattern:

**Multi-turn conversations or workloads with prompt prefix overlap — use** ``prefix-cache``

When requests share a common prompt prefix (e.g. a system prompt, few-shot examples, or
conversation history), ``prefix-cache`` routes each request to the pod that already holds the
matching KV-cache blocks in GPU memory, reducing redundant computation and improving latency.
See `prefix cache routing details <../../../pkg/plugins/gateway/algorithms/prefix_cache_readme.md>`_
for algorithm internals and configuration options.

.. code-block:: bash

    curl -v http://${ENDPOINT}/v1/chat/completions \
    -H "routing-strategy: prefix-cache" \
    -H "Content-Type: application/json" \
    -d '{
        "model": "your-model-name",
        "messages": [
            {"role": "system", "content": "You are a helpful assistant."},
            {"role": "user", "content": "What is the capital of France?"}
        ],
        "temperature": 0.7
    }'

**Independent requests with no prefix overlap — use** ``least-request``

When each request has a unique prompt and there is no KV-cache reuse benefit, ``least-request``
distributes load evenly by always routing to the pod with the fewest in-flight requests.

.. code-block:: bash

    curl -v http://${ENDPOINT}/v1/chat/completions \
    -H "routing-strategy: least-request" \
    -H "Content-Type: application/json" \
    -d '{
        "model": "your-model-name",
        "messages": [{"role": "user", "content": "Summarize this document: ..."}],
        "temperature": 0.7
    }'

**High-throughput workloads requiring compute separation — use** ``pd``

Prefill-Decode (PD) disaggregation splits the two phases of LLM inference across dedicated
pods: prefill pods process the prompt and decode pods generate tokens. This allows each pod
type to be sized and scaled independently, improving GPU utilization at high request volumes.

.. code-block:: bash

    curl -v http://${ENDPOINT}/v1/chat/completions \
    -H "routing-strategy: pd" \
    -H "Content-Type: application/json" \
    -d '{
        "model": "your-model-name",
        "messages": [{"role": "user", "content": "Explain quantum entanglement."}],
        "temperature": 0.7
    }'

See `prefill-decode disaggregation details <../../../pkg/plugins/gateway/algorithms/pd_readme.md>`_
for deployment requirements and configuration options.

.. note::
    ``random`` is suitable for testing and low-traffic scenarios where routing quality is not
    critical. For any production deployment, explicitly set ``routing-strategy`` to avoid
    relying on the default.

Per-Model Routing Configuration
---------------------------------

When a single gateway deployment serves multiple models, you may need different routing
strategies per model — for example, ``prefix-cache`` for a chat model and ``pd`` for a
batch-inference model. AIBrix supports this through **Model Config Profiles**, which let you
attach a routing configuration directly to each model's pods via an annotation and select a
profile at request time using the ``config-profile`` header.

.. code-block:: bash

    # Select the "low-latency" profile for this request
    curl -v http://${ENDPOINT}/v1/chat/completions \
    -H "config-profile: low-latency" \
    -H "Content-Type: application/json" \
    -d '{
        "model": "your-model-name",
        "messages": [{"role": "user", "content": "Hello!"}],
        "temperature": 0.7
    }'

If the ``config-profile`` header is omitted, the model's ``defaultProfile`` is used.
This allows the same gateway to enforce different routing strategies, prompt-length bucketing,
and PD modes per model without deploying separate gateway instances.

For full schema details, profile examples, and annotation format see `Model Config and Profiles <../designs/model-config-profiles.rst>`_.

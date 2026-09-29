.. _external-replica-routing:

========================
External Replica Routing
========================

The ``external`` strategy delegates replica selection to an operator-run HTTP
service after AIBrix has resolved the requested model and filtered unsafe
replicas. It does not perform semantic or cross-model routing, and it never
sends prompts, request bodies, credentials, arbitrary client headers, or pod
addresses.

The protocol is alpha. Download the normative
:download:`OpenAPI 3.1 contract <../_static/openapi/external-replica-selection-v1alpha1.yaml>`.

Candidate discovery
-------------------

Gateway resolves the model from the inference request, then reads candidate
Pods from its Kubernetes informer-backed cache. Pods are indexed by the AIBrix
model label or annotation, and warm-pool Pods are also indexed through their
ModelClaim bindings. Gateway then applies readiness, routability,
``external-filter``, replica-inflight and configured load filters before the
external router receives the snapshot.

Backend candidate discovery does not read Kubernetes Service
``Endpoints``/``EndpointSlice`` objects. Gateway retains each candidate Pod and
resolves its Pod IP and model-specific serving port locally; a ModelClaim port
of zero remains non-routable. The operator-configured external decision URL may
itself point to a Kubernetes Service, as in the sample, but that Service is the
decision-service transport and is not the source of backend candidates.

Policy modes
------------

``Advisory`` optimizes placement. A single candidate with one port keeps the
local fast path. ``Selected`` uses the validated target and ``NoDecision`` runs
the configured fallback. ``Denied`` is invalid in this mode.

``Authoritative`` enforces business or compliance policy. It calls the service
even for one candidate. ``Denied`` returns an AIBrix-owned HTTP 403 response
with code ``external_policy_denied`` and never reaches an inference backend.
Authoritative requires ``FailClosed``; ``NoDecision`` is invalid.

System failures follow the explicit failure mode. ``FailOpen`` invokes the
configured local fallback. ``FailClosed`` returns HTTP 503 with code
``external_router_unavailable``. Caller cancellation never falls back. The
external reason is diagnostic and is never copied to the client response.

Configuration
-------------

The endpoint, policy mode, and failure mode are process-level settings. An
unset endpoint disables the strategy.

.. list-table::
   :header-rows: 1
   :widths: 38 18 44

   * - Variable
     - Default
     - Meaning
   * - ``AIBRIX_EXTERNAL_ROUTER_ENDPOINT``
     - unset
     - Complete static HTTP or HTTPS operation URL.
   * - ``AIBRIX_EXTERNAL_ROUTER_POLICY_MODE``
     - required
     - ``Advisory`` or ``Authoritative``.
   * - ``AIBRIX_EXTERNAL_ROUTER_FAILURE_MODE``
     - required
     - ``FailOpen`` or ``FailClosed``.
   * - ``AIBRIX_EXTERNAL_ROUTER_FALLBACK``
     - required for Advisory/FailOpen
     - Registered local non-exclusive router other than ``external``. ``pd``
       and ``slo*`` are rejected because they require dedicated preprocessing.
   * - ``AIBRIX_EXTERNAL_ROUTER_TIMEOUT``
     - ``10ms``
     - Deadline for one HTTP decision exchange.
   * - ``AIBRIX_EXTERNAL_ROUTER_MAX_INFLIGHT``
     - ``256``
     - Non-blocking per-process bulkhead capacity.
   * - ``AIBRIX_EXTERNAL_ROUTER_MAX_REQUEST_BYTES``
     - ``256KiB``
     - Maximum encoded request.
   * - ``AIBRIX_EXTERNAL_ROUTER_MAX_RESPONSE_BYTES``
     - ``64KiB``
     - Maximum response or Problem Details body.
   * - ``AIBRIX_EXTERNAL_ROUTER_FAILURE_THRESHOLD``
     - ``5``
     - Consecutive attempted-exchange failures before opening.
   * - ``AIBRIX_EXTERNAL_ROUTER_OPEN_DURATION``
     - ``1s``
     - Open interval before one half-open probe.
   * - ``AIBRIX_EXTERNAL_ROUTER_AUTH_TOKEN_FILE``
     - unset
     - Optional bearer-token file loaded once at startup.
   * - ``AIBRIX_EXTERNAL_ROUTER_CANDIDATE_ATTRIBUTES``
     - empty
     - Comma-separated pod-label keys safe to send.
   * - ``AIBRIX_EXTERNAL_ROUTER_CANDIDATE_METRICS``
     - empty
     - ``runningRequests``, ``engineUtilization``, and/or ``kvCacheUsage``.
   * - ``AIBRIX_EXTERNAL_ROUTER_POLICY_ATTRIBUTES``
     - empty
     - Trusted in-process policy names eligible for sending.

Invalid enabled configuration fails Gateway startup. Authoritative+FailOpen,
Advisory without fallback, recursive/unregistered/exclusive fallback, URL
userinfo, fragments, unknown metrics, invalid limits, and empty token files are
rejected.

Protocol
--------

Gateway sends POST with this media type in both ``Content-Type`` and ``Accept``:

.. code-block:: text

   application/vnd.aibrix.external-routing+json;version=v1alpha1

The configured endpoint is the complete operation URL; Gateway does not append
a path. ``X-Request-Id`` equals ``metadata.requestId``. Gateway also propagates
a valid current W3C ``traceparent`` when available and sends ``Authorization:
Bearer <token>`` only when ``AIBRIX_EXTERNAL_ROUTER_AUTH_TOKEN_FILE`` is set.
It never forwards the inference client's authorization or arbitrary headers.

A request contains sorted candidate IDs and ports. Optional attributes and
metrics are omitted when they are not configured or available, never encoded as
``null``.

.. code-block:: json

   {
     "apiVersion": "routing.aibrix.ai/v1alpha1",
     "kind": "ReplicaSelectionRequest",
     "metadata": {"requestId": "req-123"},
     "spec": {
       "model": "llama-3",
       "policyMode": "Advisory",
       "policyContext": {
         "attributes": {"tenantTier": "gold"}
       },
       "candidates": [{
         "id": "default/llama-3-a",
         "ports": [8000],
         "attributes": {"topology.kubernetes.io/zone": "cn-east-1a"},
         "metrics": {
           "runningRequests": 4,
           "engineUtilization": 0.61,
           "kvCacheUsage": 0.48
         }
       }]
     }
   }

Request fields have these meanings:

* ``apiVersion`` and ``kind`` are fixed to
  ``routing.aibrix.ai/v1alpha1`` and ``ReplicaSelectionRequest``.
* ``metadata.requestId`` is a non-empty correlation ID of at most 256 UTF-8
  bytes. The response must echo it exactly.
* ``spec.model`` is the model already resolved by Gateway. The service cannot
  change it.
* ``spec.policyMode`` is the process-configured ``Advisory`` or
  ``Authoritative`` mode.
* ``spec.policyContext.attributes`` contains at most 32 trusted,
  operator-allowlisted values. Raw client headers do not populate it.
* ``spec.candidates`` is a non-empty request-local snapshot sorted by
  ``namespace/pod-name``. Candidate order has no preference semantics.
* ``candidates[].id`` is the exact ``namespace/pod-name`` identity that a
  response may select. ``ports`` is the sorted, unique set of locally routable
  ports in the range 1-65535.
* ``candidates[].attributes`` contains only pod-label keys in
  ``AIBRIX_EXTERNAL_ROUTER_CANDIDATE_ATTRIBUTES``.

Candidate metrics are fixed typed fields selected through
``AIBRIX_EXTERNAL_ROUTER_CANDIDATE_METRICS``. They are observational cache
snapshots, not reservations or a linearizable load view:

* ``runningRequests`` is a non-negative, pod-wide live count from one batched
  Gateway cache read. It is not a per-port or per-rank value.
* ``engineUtilization`` is a finite ratio in ``[0,1]`` from the requested
  model's cached engine metric. It is currently populated only for engines that
  expose the mapped metric, including xLLM; otherwise it is omitted.
* ``kvCacheUsage`` is a finite ratio in ``[0,1]`` from cached
  ``KVCacheUsagePerc``; ``1.0`` means full.

Missing, non-finite, negative, or above-one optional metric values are omitted
individually without failing routing. Gateway does not scrape engines or make a
per-candidate network call to populate them.

A selected response must echo the request ID. Gateway rejects candidates and
ports that were not present in the exact request.

.. code-block:: json

   {
     "apiVersion": "routing.aibrix.ai/v1alpha1",
     "kind": "ReplicaSelectionResponse",
     "metadata": {"requestId": "req-123", "decisionId": "decision-456"},
     "status": {
       "decision": "Selected",
       "target": {"id": "default/llama-3-a", "port": 8000}
     }
   }

``target.id`` must occur in the exact candidate snapshot. ``target.port`` must
belong to that candidate; it may be omitted only when the candidate advertised
exactly one port. The service cannot return an address or URL.

``NoDecision`` is valid only in Advisory mode and runs the configured fallback:

.. code-block:: json

   {
     "apiVersion": "routing.aibrix.ai/v1alpha1",
     "kind": "ReplicaSelectionResponse",
     "metadata": {"requestId": "req-123"},
     "status": {
       "decision": "NoDecision",
       "reason": "NoApplicablePolicy"
     }
   }

``Denied`` is valid only in Authoritative mode and returns the fixed Gateway
HTTP 403 response without reaching a backend:

.. code-block:: json

   {
     "apiVersion": "routing.aibrix.ai/v1alpha1",
     "kind": "ReplicaSelectionResponse",
     "metadata": {
       "requestId": "req-123",
       "decisionId": "decision-789"
     },
     "status": {
       "decision": "Denied",
       "reason": "CompliancePolicy"
     }
   }

``NoDecision`` and ``Denied`` omit ``target``. ``decisionId`` and ``reason``
are optional strings of at most 256 UTF-8 bytes; the external reason is never
copied to the inference client response.

Only HTTP 200 with the exact protocol media type is a decision. Gateway rejects
empty bodies, malformed JSON, duplicate object members, trailing JSON values,
missing or null required fields, mismatched request IDs, illegal decisions, and
targets outside the request snapshot. Unknown response fields are ignored for
additive alpha evolution. Redirects, compression, and automatic retries are
disabled.

Security and operations
-----------------------

The endpoint URL and credentials are process-controlled and cannot be supplied
or overridden by a client. The ``external`` strategy still participates in the
normal routing-strategy resolution order, including the generic
``routing-strategy`` request header unless an operator locks or otherwise
controls strategy selection. Do not use that header as an authorization
boundary.

Use a Kubernetes Service, NetworkPolicy, least-privilege bearer token, and HTTPS
where required. The decision service must coordinate its own state across
replicas. Gateway candidate membership, address resolution, final admission and
request accounting remain local. Bulkheads and circuit breakers are
deliberately process-local to each Gateway replica.

Monitor these low-cardinality metrics:

* ``aibrix_gateway_external_router_requests_total{outcome}``
* ``aibrix_gateway_external_router_fallback_total{reason}``
* ``aibrix_gateway_external_router_duration_seconds``
* ``aibrix_gateway_external_router_inflight``
* ``aibrix_gateway_external_router_circuit_state{state}``

The fallback counter records each invocation attempt before the local router
runs, so both successful and failed fallbacks are visible.

Start with an Advisory canary, compare fallback and latency rates, then move to
Authoritative only after denial and service-availability tests pass. Roll back
by removing ``external`` from routing selection or unsetting the endpoint and
restarting Gateway.

Troubleshooting
---------------

* HTTP 403 with ``external_policy_denied`` is a valid Authoritative denial, not
  an external-service outage.
* HTTP 503 with ``external_router_unavailable`` means FailClosed handled a
  system or protocol failure. Check the bounded Gateway error log, external
  request and fallback counters, latency, and circuit state.
* If an allowlisted attribute or metric is absent, verify the environment
  allowlist and its trusted/cache data source. Absence does not fail routing.
* Repeated ``open`` circuit state usually indicates transport, timeout,
  non-200, media-type, JSON, request-ID, or target-validation failures.
* A decision service must return the candidate ID exactly as supplied,
  including namespace, and must select only an advertised port.

Sample
------

See the runnable sample under ``samples/external-replica-router/`` for
preferred-zone and premium-accelerator rules with deterministic tie-breaking.
Trusted policy attributes must be populated by validated in-process code; do
not copy raw tenant or authorization headers into them.

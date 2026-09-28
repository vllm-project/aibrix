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
     - Registered local router other than ``external``.
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
Advisory without fallback, recursive/unregistered fallback, URL userinfo,
fragments, unknown metrics, invalid limits, and empty token files are rejected.

Protocol
--------

Gateway sends POST with this media type in both ``Content-Type`` and ``Accept``:

.. code-block:: text

   application/vnd.aibrix.external-routing+json;version=v1alpha1

A request contains sorted candidate IDs and ports. Metrics are opt-in and
observational; they are not reservations or a linearizable load snapshot.

.. code-block:: json

   {
     "apiVersion": "routing.aibrix.ai/v1alpha1",
     "kind": "ReplicaSelectionRequest",
     "metadata": {"requestId": "req-123"},
     "spec": {
       "model": "llama-3",
       "policyMode": "Advisory",
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

``NoDecision`` and ``Denied`` omit ``target`` and may include a bounded
``reason``. Only HTTP 200 with the protocol media type is a decision. Redirects,
compression, and retries are disabled.

Security and operations
-----------------------

The endpoint cannot be selected by a client. Use a Kubernetes Service,
NetworkPolicy, least-privilege bearer token, and HTTPS where required. The
decision service must coordinate its own state across replicas. Gateway
bulkheads and circuits are deliberately process-local.

Monitor these low-cardinality metrics:

* ``aibrix_gateway_external_router_requests_total{outcome}``
* ``aibrix_gateway_external_router_fallback_total{reason}``
* ``aibrix_gateway_external_router_duration_seconds``
* ``aibrix_gateway_external_router_inflight``
* ``aibrix_gateway_external_router_circuit_state{state}``

Start with an Advisory canary, compare fallback and latency rates, then move to
Authoritative only after denial and service-availability tests pass. Roll back
by removing ``external`` from routing selection or unsetting the endpoint and
restarting Gateway.

Sample
------

See the runnable sample under ``samples/external-replica-router/`` for
preferred-zone and premium-accelerator rules with deterministic tie-breaking.
Trusted policy attributes must be populated by validated in-process code; do
not copy raw tenant or authorization headers into them.


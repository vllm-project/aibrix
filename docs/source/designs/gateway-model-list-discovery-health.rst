.. _gateway-model-list-discovery-health:

===================================
Gateway Model List Discovery Health
===================================

This is a proposal for review. It describes an optional reliability mode for
the gateway's ``GET /v1/models`` handler. It works with the existing
known-model list and the optional Ready-Pod list, and only checks discovery
sources that are enabled. Configuration names and error fields below are
candidates, not released interfaces.

Problem
-------

The gateway currently returns cached names even when it cannot establish
that discovery has completed or remained current. A successful empty list and
a failed Kubernetes list therefore look the same to an API client. Initial
discovery also blocks startup before the gateway's HTTP listener is opened.
The handler cannot report a service error while that listener is absent.

Proposed behavior
-----------------

The mode is opt-in so existing users retain the current listing contract.
When enabled, ``GET /v1/models`` returns a complete list only while the
enabled, required discovery sources have a verified snapshot. Otherwise it
returns HTTP 503 with the gateway's JSON error envelope, the existing
``service_unavailable`` code, and a fixed public message. Internal Kubernetes
errors appear only in logs. A healthy installation with no models still
returns HTTP 200 and an empty ``data`` list.

Kubernetes Pods are always required. ModelAdapters are required only when
their discovery is enabled. ModelClaims remain advisory for this endpoint:
the claim informer describes unplaced claims for inference errors but does
not itself add a model to the list. Pod runtime annotations that advertise
claim-backed models are covered by the Pod source. Disabled sources cause no
API calls and do not affect model-list health. Static discovery becomes
healthy after its configuration is loaded; it has no continuing watch.

For the opt-in mode, the proposed states are:

* **Starting:** required informer stores and their registered event handlers
  have not completed the initial list and delivery. Return 503.
* **Healthy:** initial synchronization has completed and a complete current
  LIST matched the required events applied to the derived cache within the
  configured maximum age. Return the selected model-list mode's result.
* **Untrustworthy:** verification fails, an event cannot be applied, a watch
  error invalidates the last proof, or the proof expires. Return 503 until
  a new verification succeeds.
* **Recovering:** the request that initiates a new verification waits for it;
  other requests either share the result or return 503 during the retry delay.

The list response and health state must come from one cache snapshot. A
health check followed by an unrelated cache read could return partial data
during an update.

Watch-led detection and recovery
--------------------------------

Use the existing client-go informers for normal event delivery. Keep the
``ResourceEventHandlerRegistration`` from each required informer and wait for
its ``HasSynced`` result, as well as the informer's initial ``HasSynced``.
The registration proves that the handler has received the initial items;
the informer result alone does not.

Register ``SetWatchErrorHandler`` on each required informer so reported
``ListAndWatch`` errors invalidate the last proof. That callback cannot be
the only signal: client-go can close and restart a watch without invoking it.
The maximum proof age and on-demand LIST detect stale derived state even
when a watch stalls silently. Client-go retains responsibility for normal
watch restart and resource-version recovery.

The Kubernetes provider owns applied-object versions and verification state
under one lock. Required event handlers run while holding that lock, and the
gateway reads health and model names through the same barrier. The existing
``Provider.Watch`` signature remains usable by static and future discovery
providers. Health evidence refers to events actually applied to the derived
cache, rather than only to events received by the informer.

An informer resync only replays local contents. ``HasSynced`` proves initial
delivery, not continuing freshness. A successful API ping proves reachability,
not that the model cache has caught up. Likewise, a quiet healthy cluster may
have no object events, and Kubernetes does not promise watch bookmarks at a
fixed interval or at all. A pure event/error-driven mode can therefore detect
observed failures, but cannot promise to detect a silent stall within a fixed
time bound.

For a bounded silent-stall guarantee, a request whose previous verification
has expired obtains independent progress evidence before returning a list.
The selected minimal implementation uses an opt-in positive maximum age and
an on-demand current LIST of each required source. The LIST is paginated and
bounded by one request timeout. Compare namespace, name, UID, and resource
version with the provider's record of events applied to the derived cache.
One verification runs at a time per gateway instance; callers arriving after
an unsuccessful verification receive 503 until a bounded retry delay expires.
There is no periodic verification worker and no API request on the usual
already-verified response path. When no client asks for the list, no extra
LIST is performed. The first request after expiry may wait for verification;
it never returns an unverified 200.

Other mechanisms remain possible if the on-demand LIST cost is too high:

* **Reconcile after watch loss or reconnect.** Reuse client-go's LIST/watch
  recovery where it gives an authoritative replacement and wait for handler
  completion. This avoids routine extra LISTs, but an otherwise silent,
  permanently stuck watch needs a separate finite detection bound. The
  `pinned client-go reflector`_ already times out and restarts watches;
  its default minimum timeout is five minutes, and a restart from the last
  resource version alone is not proof that the derived cache has caught up.
* **Streaming-list checkpoint where supported.** A watch with
  ``sendInitialEvents=true`` and an initial-events-end bookmark can form a
  snapshot barrier without a separate LIST request; see `streaming lists`_.
  Its server and client support, fallback behavior, and delivery into this
  handler must be tested. In the pinned client-go version the
  `WatchListClient feature`_ is disabled by default, so
  this cannot be assumed available to all installations.

Normal discovery remains informer driven. Watch errors invalidate the last
verification immediately; the next model-list request must verify again.
An inconsistent LIST returns 503 even if a watch appears connected. On a
later matching LIST, recovery is complete only after the corresponding
handler events have been applied. A bounded guarantee still needs some
independent evidence in a quiet cluster; watch events and `optional bookmarks`_
alone do not supply it. The maximum age, request timeout, and
load budget need representative failure and scale tests before a default
opt-in configuration is recommended.

.. _pinned client-go reflector: https://github.com/kubernetes/client-go/blob/v0.31.8/tools/cache/reflector.go
.. _streaming lists: https://kubernetes.io/docs/reference/using-api/api-concepts/#streaming-lists
.. _WatchListClient feature: https://github.com/kubernetes/client-go/blob/v0.31.8/features/known_features.go
.. _optional bookmarks: https://kubernetes.io/docs/reference/using-api/api-concepts/#watch-bookmarks

Startup and delivery
--------------------

In this mode, open the model-list HTTP listener with an initially unavailable
handler before waiting for Kubernetes discovery. After cache initialization,
delegate to the normal gateway handler, which still returns 503 until a
matching current snapshot is obtained. Keep inference routing and the gRPC
readiness check unavailable until initial discovery succeeds. Configuration
errors that cannot be retried remain startup errors. When no gateway process
or ready Service endpoint exists, the proxy owns the response; this in-process
handler cannot format it.

Compatibility and tests
-----------------------

Without the opt-in mode, current known-model and ready-Pod listing, startup,
static discovery, and other cache consumers keep their existing behavior.
An explicitly selected dynamic provider that cannot report the required
health evidence should fail configuration validation rather than silently
claim to be healthy.

Focused tests should cover initial informer and handler synchronization;
reported watch errors; a Pod removal missed by a silently stalled watch;
quiet clusters; failed and incomplete recovery; pagination and request
timeout; adapter discovery enabled and disabled; concurrent cache events and
requests; and the exact HTTP 200/503 JSON responses. A Kubernetes integration
test should verify the recovery path through the public handler.
The race detector should cover the cache, discovery, gateway, and startup
packages touched by the implementation.

Review decisions
----------------

Before merge, reviewers need to agree on the opt-in default, which
sources are required for this endpoint, the freshness guarantee and its
verification cost, the error envelope, early listener startup, and how future
dynamic providers supply equivalent health evidence. This document records
the candidate contract for that discussion; it does not claim
maintainer approval.

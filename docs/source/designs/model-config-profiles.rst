.. _model_config_profiles:

=========================
Model Config and Profiles
=========================

This design describes how to supply **model/gateway configuration** (routing strategy, PD bucket bounds, combined mode, etc.) via a **single annotation** (or ConfigMap), with support for **multiple named profiles** selectable at **runtime** by the client.

Motivation
----------

Today, options are encoded as many pod labels (e.g. ``model.aibrix.ai/name``, ``model.aibrix.ai/port``, ``model.aibrix.ai/routing-strategy``, ``prompt-min-length``, etc.). Adding new options requires new labels and gateway changes to read them. This does not scale. Using a single structured annotation with **multiple profiles** allows:

* One place to add new options (extend the JSON schema).
* Different configurations for the same model (e.g. ``default``, ``pd``, ``low-latency``) selectable per request via a header.

Overview
--------

* **Annotation** (on the pod): ``model.aibrix.ai/config`` holds a JSON object with a ``profiles`` map. Each profile is a set of gateway options: ``routingStrategy``, ``promptLenBucketMinLength``, ``promptLenBucketMaxLength``, ``combined``.
* **Runtime selection**: Client sends header ``config-profile: <profile-name>`` (e.g. ``pd``, ``low-latency``). If omitted, the ``defaultProfile`` (or ``"default"``) is used.

JSON Schema (Implementation)
----------------------------

The implementation parses the following structure. Extra fields (e.g. ``name``, ``port``, ``engine``) in the JSON are ignored.

Root object:

* ``defaultProfile`` (string, optional): Profile name to use when header is empty or profile not found. Default: ``"default"``.
* ``profiles`` (object, required): Map of profile name → profile object.

Profile object (``ModelConfigProfile``):

* ``routingStrategy`` (string): e.g. ``random``, ``pd``, ``least-latency``.
* ``promptLenBucketMinLength`` (int, optional): Lower bound for bucketing. Default: ``0``. If negative, normalized to ``0``.
* ``promptLenBucketMaxLength`` (int, optional): Upper bound for bucketing. Default: ``math.MaxInt32`` when ``0`` or omitted.
* ``combined`` (bool, optional): When true, indicates combined prefill/decode pod for PD routing.

Single profile (backward compatible):

.. code-block:: json

  {
    "profiles": {
      "default": {
        "routingStrategy": "pd",
        "promptLenBucketMinLength": 0,
        "promptLenBucketMaxLength": 2048
      }
    }
  }

Multiple profiles with default:

.. code-block:: json

  {
    "defaultProfile": "pd",
    "profiles": {
      "default": {
        "routingStrategy": "random",
        "promptLenBucketMinLength": 0,
        "promptLenBucketMaxLength": 4096
      },
      "pd": {
        "routingStrategy": "pd",
        "promptLenBucketMinLength": 0,
        "promptLenBucketMaxLength": 2048
      },
      "low-latency": {
        "routingStrategy": "least-latency",
        "promptLenBucketMinLength": 0,
        "promptLenBucketMaxLength": 2048
      }
    }
  }

Runtime Behavior
----------------

1. Gateway resolves config from pod annotation ``model.aibrix.ai/config``. ConfigMap lookup is not yet implemented. If no annotation, fall back to existing label-based resolution.
2. Gateway reads ``config-profile`` from request headers. If missing, use ``defaultProfile`` from the JSON, or ``"default"``.
3. Gateway selects the profile via ``GetProfile(profileName)``: exact match first, then fallback to ``defaultProfile``, then ``"default"``.
4. The resolved profile is stored on ``RoutingContext.ConfigProfile`` (``ResolvedConfigProfile``) for the request.
5. Routing strategy is derived from: request headers → ``ConfigProfile.RoutingStrategy`` → env ``ROUTING_ALGORITHM``.
6. PD router uses ``ResolveProfileFromPod(pod, routingCtx.ReqConfigProfile)`` with fallback to the default profile; prompt bounds and ``combined`` are read from the selected profile.

Annotation Example (StormService pod template)
----------------------------------------------

.. code-block:: yaml

  template:
    metadata:
      labels:
        app: sglang-qwen3-8b-1p1d-0-2k
        model.aibrix.ai/name: qwen3-8B
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "30000"
        prometheus.io/path: "/metrics"
        model.aibrix.ai/config: |
          {
            "defaultProfile": "pd",
            "profiles": {
              "default": {
                "routingStrategy": "random",
                "promptLenBucketMinLength": 0,
                "promptLenBucketMaxLength": 4096
              },
              "pd": {
                "routingStrategy": "pd",
                "promptLenBucketMinLength": 0,
                "promptLenBucketMaxLength": 2048
              }
            }
          }

Client Usage
------------

* Use default profile: do not set any header (or set ``config-profile: default``).
* Use a specific profile: set header ``config-profile: pd`` or ``config-profile: low-latency``.

Implementation
-------------

Package: ``pkg/plugins/gateway/configprofiles/``

* ``ModelConfigProfile``: struct with ``RoutingStrategy``, ``PromptLenBucketMinLength``, ``PromptLenBucketMaxLength``, ``Combined``.
* ``ModelConfigProfiles``: struct with ``DefaultProfile``, ``Profiles map[string]ModelConfigProfile``.
* ``ParseModelConfig(jsonStr)``: parses JSON; normalizes ``promptLenBucketMinLength`` (≥0) and ``promptLenBucketMaxLength`` (0→MaxInt32).
* ``GetProfile(name)``: returns profile by name; falls back to ``defaultProfile`` then ``"default"``.
* ``ResolveProfile(pods, headerProfile)``: iterates pods, returns first non-nil from ``ResolveProfileFromPod``.
* ``ResolveProfileFromPod(pod, headerProfile)``: reads ``model.aibrix.ai/config`` from pod, parses, returns ``GetProfile(headerProfile)``.
* Prompt length bounds normalization occurs in ``ParseModelConfig``: ``promptLenBucketMinLength`` (<0 → 0), ``promptLenBucketMaxLength`` (0 → ``math.MaxInt32``).

Constants: ``ModelAnnoConfig`` (pkg/constants/model.go), ``HeaderConfigProfile`` (pkg/plugins/gateway/types.go).

Gateway flow:

* ``HandleRequestHeaders``: captures ``config-profile`` into ``ReqConfigProfile``.
* ``HandleRequestBody``: calls ``applyConfigProfile`` which resolves config from pod annotation, sets ``routingCtx.ConfigProfile``, and provides routing strategy to ``deriveRoutingStrategyFromContext``.
* ``deriveRoutingStrategyFromContext``: chooses the routing strategy for the request using this precedence: (1) request header ``routing-strategy`` if present and non-empty; (2) ``routingCtx.ConfigProfile.RoutingStrategy`` from the resolved profile (config-profile + pod annotation); (3) environment default. Returns the strategy and whether it was explicitly set (used to validate and set ``routingCtx.Algorithm`` in ``HandleRequestBody``).

PD router:

* ``isPodSuitableForPromptLength(routingCtx, pod, promptLength)``: uses ``ResolveProfileFromPod(pod, routingCtx.ReqConfigProfile)`` for ``promptLenBucketMinLength``/``promptLenBucketMaxLength``.
* ``isCombinedPod(routingCtx, pod)``: uses ``ResolveProfileFromPod(pod, routingCtx.ReqConfigProfile)`` for ``combined``.

Backward Compatibility
----------------------

If no annotation is present, ``ResolveProfile`` returns nil. Gateway continues to use existing pod labels and env for routing strategy, port, engine, etc.

Future Work
----------

* ConfigMap lookup (wire when gateway config supports it).
* Extend profile schema: ``port``, ``metricPort``, ``engine``, ``name`` for full parity with labels.
* Use request-level ``ConfigProfile`` (from ``config-profile``) for PD bucketing instead of per-pod ``"pd"`` profile.

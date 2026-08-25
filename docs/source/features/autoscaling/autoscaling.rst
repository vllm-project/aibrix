===========
Autoscaling
===========

Autoscaling is crucial for deploying Large Language Model (LLM) services on Kubernetes (K8s), as timely scaling up handles peaks in request traffic, and scaling down conserves resources when demand wanes.

AIBrix ships one custom resource for this, ``PodAutoscaler`` in the ``autoscaling.aibrix.ai/v1alpha1``
API group, and several algorithms behind it. This page explains how the pieces fit together and
which page to read next. The two child pages hold the full configuration and examples.

.. toctree::
   :maxdepth: 1

   metric-based-autoscaling
   optimizer-based-autoscaling

How it works
------------

.. mermaid::

   graph LR
       PA["PodAutoscaler CR"] --> C["PodAutoscaler controller"]
       C -->|"pod source"| P["Engine pods<br/>/metrics"]
       C -->|"external source"| O["GPU optimizer<br/>/metrics/ns/deployment"]
       C -->|"HPA / KPA / APA"| D["desired replicas"]
       D -->|updates| T["scaleTargetRef<br/>Deployment, StormService role,<br/>RayClusterFleet"]

1. You create a ``PodAutoscaler`` that points at a workload through ``scaleTargetRef``. The
   target can be a ``Deployment``, a ``StormService`` (optionally one role of it, through
   ``subTargetSelector.roleName``), or a ``RayClusterFleet``.
2. The controller collects the metric named in ``metricsSources``. With ``metricSourceType: pod``
   it scrapes every pod of the target at the given ``port`` and ``path``. With
   ``metricSourceType: external`` it reads a single HTTP endpoint instead, which is how the GPU
   optimizer hands its recommendation to the autoscaler.
3. The algorithm selected by ``scalingStrategy`` (``HPA``, ``KPA`` or ``APA``) turns the observed
   value and ``targetValue`` into a desired replica count, using ``observeWindowSeconds`` and,
   for KPA, ``panicWindowSeconds`` as its windows.
4. The result is clamped to ``minReplicas`` and ``maxReplicas``, or to the bounds of a matching
   entry in ``schedules`` when one is active, and written to the target.

Choosing a strategy
-------------------

.. list-table::
   :header-rows: 1
   :widths: 18 44 38

   * - Strategy
     - How it decides
     - Use it when
   * - ``HPA``
     - Same algorithm as the Kubernetes HorizontalPodAutoscaler, applied to the metric you
       choose.
     - Traffic is steady and you want the most familiar behaviour.
   * - ``KPA``
     - Knative-style: a long stable window plus a short panic window that scales up fast when
       the panic window crosses the threshold. Metrics are fetched by AIBrix directly rather
       than through Prometheus, which shortens reaction time.
     - Bursty or unpredictable traffic where scale-up latency matters most.
   * - ``APA``
     - AIBrix's own algorithm. Like HPA but with a fluctuation tolerance that must be exceeded
       before any scaling happens, which suppresses oscillation.
     - Latency-sensitive services that must not thrash between replica counts.
   * - Optimizer-based
     - Not a ``scalingStrategy`` value. The GPU optimizer computes the replica count from
       offline benchmark profiles and an SLO, then exposes it as a metric that a ``KPA``
       PodAutoscaler consumes through an ``external`` metric source.
     - You have benchmark data per GPU type, an explicit latency SLO, or a mix of GPU types to
       balance for cost. See :doc:`optimizer-based-autoscaling` and
       :doc:`../heterogeneous-gpu`.

Metric sources
--------------

``metricSourceType`` accepts ``pod``, ``external``, ``resource`` and ``custom``. ``domain`` is
a deprecated alias of ``external`` that older samples still use. The two the documentation and
samples use are:

* ``pod``: scrape each target pod. Set ``port`` and ``path`` (for example ``8000`` and
  ``/metrics``) and ``targetMetric`` to one of AIBrix's engine-neutral metric names, such as
  ``num_requests_waiting`` or ``gpu_cache_usage_perc``. AIBrix translates the name to what the
  pod's engine actually exports (``vllm:num_requests_waiting`` for vLLM, ``sglang:num_queue_reqs``
  for SGLang), so do not use the raw engine names here.
* ``external``: read one HTTP endpoint instead of the pods. ``endpoint`` is the host and port;
  ``path`` and ``protocolType`` must be present to pass validation. The fetcher requests
  ``http://<endpoint>/metrics`` and resolves ``targetMetric`` through the same metric registry
  as the ``pod`` source. This is the shape the GPU optimizer sample uses; see
  :doc:`optimizer-based-autoscaling`. If ``endpoint`` is left empty, the controller queries the
  Kubernetes ``external.metrics`` API for ``targetMetric`` instead.

For the engine-neutral names and what each engine exports for them, see the table in
:doc:`../multi-engine`.

PodAutoscaler spec at a glance
------------------------------

.. list-table::
   :header-rows: 1
   :widths: 30 70

   * - Field
     - Meaning
   * - ``scaleTargetRef``
     - ``apiVersion``, ``kind`` and ``name`` of the workload to scale. The name must match the
       workload exactly.
   * - ``subTargetSelector.roleName``
     - When the target is a ``StormService``, scale only this role.
   * - ``scalingStrategy``
     - ``HPA``, ``KPA`` or ``APA``.
   * - ``minReplicas`` / ``maxReplicas``
     - Hard bounds on the result. ``maxReplicas`` is required.
   * - ``metricsSources[]``
     - One or more of ``metricSourceType``, ``protocolType``, ``endpoint``, ``path``, ``port``,
       ``targetMetric``, ``targetValue``. Several sources can be combined; see
       *Multi-Metric Based Autoscaling* in :doc:`metric-based-autoscaling`.
   * - ``observeWindowSeconds`` / ``panicWindowSeconds``
     - Metric windows. ``panicWindowSeconds`` only applies to ``KPA``.
   * - ``schedules[]``
     - Time-boxed overrides of ``minReplicas`` and ``maxReplicas`` with ``name``, ``timezone``,
       ``daysOfWeek``, ``startTime`` and ``endTime``.

Algorithm tunables that are not part of the spec, such as KPA's scale-down delay or APA's
tolerance, are set through annotations on the ``PodAutoscaler``. The full annotation list and
worked YAML examples for every strategy live in :doc:`metric-based-autoscaling`.

.. seealso::

   :doc:`../../designs/aibrix-autoscaler`
       Internal design of the AIBrix autoscaler.

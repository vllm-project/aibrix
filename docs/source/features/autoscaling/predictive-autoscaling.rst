.. _predictive-autoscaling:

========================
Predictive Autoscaling
========================

Reactive autoscaling waits for the metric to move before it adds replicas, which costs at least
one pod start-up time of headroom. Predictive autoscaling closes part of that gap: it projects
the metric trend of the observation window forward and, in ``Auto`` mode, uses the projection as
a floor for scale-up. The reactive decision is never replaced, and a projection never scales a
workload down.

The feature is opt-in through ``spec.predictive``. When the block is absent nothing in this page
applies. Today the block is accepted and validated only: applying it writes no
``status.predictive`` and changes no replica count. The controller that consumes these fields
lands in a follow-up.

Modes
-----

.. list-table::
   :header-rows: 1
   :widths: 15 85

   * - ``mode``
     - Behaviour
   * - ``Preview``
     - The controller computes the projection and records it in ``status.predictive``, but the
       replica decision stays exactly as it is without the block. Use it to watch what the
       projection would have asked for before trusting it. This is the default when ``mode`` is
       omitted.
   * - ``Auto``
     - The projection is converted into a replica count and used as a floor:
       ``final = max(reactive, projected)``. It can raise a scale-up, never trigger or lower a
       scale-down.

.. code-block:: yaml

   spec:
     predictive:
       mode: Auto
       horizonSeconds: 120

Status
------

``status.predictive`` reports the last evaluation:

.. list-table::
   :header-rows: 1
   :widths: 30 70

   * - Field
     - Meaning
   * - ``mode``
     - Effective mode of the evaluation, after defaulting to ``Preview``.
   * - ``metric``
     - ``targetMetric`` of the source the evaluation was derived from.
   * - ``observedValue``
     - Mean of the samples used by the fit, as a decimal string in the same unit as
       the metric sample and ``targetValue``.
   * - ``predictedValue``
     - Value of the fitted line at ``now + horizonSeconds``, as a decimal string in
       the same unit as the metric sample and ``targetValue``.
   * - ``predictedReplicas``
     - Replica count the projection alone asks for.
   * - ``reactiveReplicas``
     - Replica count the reactive path selected in the same round, before the predictive floor.
   * - ``wouldBeReplicas``
     - Replica count the same round would apply in ``Auto``, after the same caps, bounds and
       cooldown. In ``Auto`` it equals the applied count; in ``Preview`` it shows whether
       ``Auto`` would change the decision.
   * - ``lastUpdated``
     - When the prediction was computed.

The block is removed when ``spec.predictive`` is removed.

Example
-------

.. literalinclude:: ../../../../samples/autoscaling/predictive-kpa.yaml
   :language: yaml

Limitations
-----------

* Predictive scaling is not supported with ``scalingStrategy: HPA``. The HPA strategy delegates
  the decision to a native ``HorizontalPodAutoscaler`` resource, which has no channel to receive
  the projection, so the combination is rejected by the validating webhook.
* The projection uses the ``observeWindowSeconds`` window even when KPA is in panic mode. The
  panic window stays a reactive signal, and the projection is deliberately the slower of the
  two.
* The projection is linear. Traffic with a strong daily shape still benefits from
  ``schedules``, which set bounds rather than a trend.

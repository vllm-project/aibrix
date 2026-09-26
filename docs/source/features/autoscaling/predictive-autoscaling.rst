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
applies.

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

How the projection is computed
------------------------------

1. The controller already records one sample per reconcile in the metric history that the trend
   analysis uses. Predictive scaling fits the samples that fall inside
   ``observeWindowSeconds``; a sample older than the window, or newer than the evaluation time,
   is ignored.
2. A least-squares line is fitted to those samples, with time in seconds against the metric
   value.
3. The line is evaluated at the horizon: ``spec.predictive.horizonSeconds``, or 2 minutes when
   the field is unset.
4. Two clamps keep the result sane: the projection never drops below zero, and never rises above
   four times the observed mean, so a single spike cannot ask for an unbounded replica count.
5. A projection is only produced when the window holds at least three samples that span at least
   half of ``observeWindowSeconds``. Until then ``status.predictive`` stays empty, which is the
   normal state right after a ``PodAutoscaler`` is created.

With several ``metricsSources``, every source is projected and the evaluation that asks for the
most replicas is the one reported and applied.

How ``Auto`` applies the floor
------------------------------

The projected metric value is turned back into a replica count with the same formula as the
reactive path of the strategy:

* ``KPA``: ``ceil(projectedValue / targetValue)``
* ``APA``: ``ceil(currentReplicas * projectedValue / targetValue)``

That count is then used as a floor for the reactive recommendation from the same round. Being a
floor, the projection can only raise the decision. The floor is capped by the configured
scale-up rate, and the result still passes through ``minReplicas``, ``maxReplicas``, the active
``schedules`` entry and the scale-up cooldown window before anything changes on the target.

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
     - Mean value of the samples used by the fit.
   * - ``predictedValue``
     - Projected value at the horizon.
   * - ``predictedReplicas``
     - Replica count the projection alone asks for.
   * - ``reactiveReplicas``
     - Replica count the reactive path selected in the same round, before the predictive floor.
   * - ``wouldBeReplicas``
     - Replica count the same round would apply in ``Auto``, after the same caps, bounds and
       cooldown. In ``Auto`` it equals the applied count; in ``Preview`` it shows whether
       ``Auto`` would change the decision.
   * - ``lastUpdated``
     - When this projection changed. Identical projections keep the previous timestamp, so a
       steady series does not rewrite the status on every resync.

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

.. _model-warmup:

=================
ModelWarmup
=================

``ModelWarmup`` preloads inference/runtime container images onto selected
Kubernetes nodes as a finite ``Once`` operation. It exposes image-cache progress
through Kubernetes status; it does not download model weights or start an
inference engine.

Workflow
--------

.. mermaid::

   flowchart LR
      CR[ModelWarmup CR] --> T[Resolve explicit nodes and selectors]
      T --> R[Calculate Job-template revision]
      R --> J[Create node-pinned warmup Job]
      J --> P[Pull each declared image with a per-Job timeout]
      P --> S[Aggregate per-node status]
      S --> OK[Succeeded or Degraded]

Targeting and execution
-----------------------

Targets can combine explicit node names and non-empty label selectors. The
controller takes their union and deduplicates nodes. While a ``Once`` operation
is not terminal, Node create and label events add newly matched nodes. After
Succeeded, Failed, or Degraded, later nodes are ignored; a future Continuous
mode will provide ongoing pool coverage.

The namespace and target nodes must share the
``resource-pool.aibrix.ai/name`` label, and nodes must opt in with
``model.aibrix.ai/warmup-enabled=true``. Control-plane nodes are excluded.
Each target receives a Job scheduled with required node affinity. Jobs use the
configured pull secrets, request no GPU, disable ServiceAccount token mounting,
avoid host access, and do not receive a catch-all toleration. ``parallelism``
limits simultaneously active Jobs, and ``jobTimeoutSeconds`` applies in full
after each Job is created.

Labels and annotations
----------------------

.. list-table:: ModelWarmup metadata keys
   :header-rows: 1

   * - Key
     - Object
     - Purpose
   * - ``resource-pool.aibrix.ai/name``
     - Namespace and Node
     - Authorizes a Node only when its non-empty pool value equals the
       ModelWarmup namespace pool value. Cluster administrators manage it.
   * - ``model.aibrix.ai/warmup-enabled``
     - Node
     - A value of ``true`` explicitly opts the Node into warmup scheduling.
   * - ``model.aibrix.ai/warmup``
     - Job
     - Controller-managed label containing the owning ModelWarmup name.
   * - ``model.aibrix.ai/revision``
     - Job
     - Controller-managed label identifying the immutable image workload
       revision.
   * - ``model.aibrix.ai/target-node``
     - Job
     - Controller-managed annotation recording the exact authorized target
       Node. Users do not set the three Job metadata keys.

.. literalinclude:: ../../../samples/modelwarmup/modelwarmup.yaml
   :language: yaml
   :linenos:

Image contract and status
-------------------------

Every image entry requires a safe command that exits successfully after the
image is pulled. ``imagePullPolicy`` accepts ``Always``, ``IfNotPresent`` and
``Never``; it defaults to ``IfNotPresent``. Use digest references when a
reproducible cache result is required.

``status.phase`` is ``Pending``, ``Running``, ``Succeeded``, ``Failed`` or
``Degraded``. Aggregate counts cover every target. Bounded details are retained
for pending and failed nodes; succeeded nodes are represented by the aggregate
count. ``Complete=True`` means the Once operation finished successfully.
Image cache status is a point-in-time observation: kubelet or container runtime
garbage collection can evict an image after a successful warmup.

Scope
-----

The spec is immutable after creation; create another resource for another Once
operation. This initial release is image-only. Artifact download, Continuous
mode, inference-engine warmup,
workload-derived targets, rollout gating and autoscaler mutation are not
included.

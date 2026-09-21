.. _model-warmup:

=================
ModelWarmup
=================

``ModelWarmup`` preloads inference/runtime container images onto selected
Kubernetes nodes before rollout or scale-out. It exposes image-cache progress
through Kubernetes status; it does not download model weights or start an
inference engine.

Workflow
--------

.. mermaid::

   flowchart LR
      CR[ModelWarmup CR] --> T[Resolve explicit nodes and selectors]
      T --> R[Calculate Job-template revision]
      R --> J[Create node-pinned warmup Job]
      J --> P[Pull each declared image]
      P --> S[Aggregate per-node status]
      S --> OK[Succeeded or Degraded]

Targeting and execution
-----------------------

Targets can combine explicit node names and non-empty label selectors. The
controller takes their union, deduplicates nodes, and records every matching
source in status. Selector expansion adds work only for newly matched nodes.

Each target node receives a node-pinned Job for the current revision. Jobs use
the configured pull secrets, request no GPU, disable ServiceAccount token
mounting, avoid host access, and tolerate target-node taints. ``parallelism``
limits simultaneously active Jobs.

.. literalinclude:: ../../../samples/modelwarmup/modelwarmup.yaml
   :language: yaml
   :linenos:

Image contract and status
-------------------------

Every image entry requires a safe command that exits successfully after the
image is pulled. ``imagePullPolicy`` accepts ``Always``, ``IfNotPresent`` and
``Never``; it defaults to ``IfNotPresent``. Use digest references when a
reproducible cache result is required.

``status.phase`` is ``Pending``, ``Running``, ``Succeeded`` or ``Degraded``.
Per-node status records the revision, Job name, sources and failure reason.
Image cache status is a point-in-time observation: kubelet or container runtime
garbage collection can evict an image after a successful warmup.

Scope
-----

This initial release is image-only. Artifact download, inference-engine warmup,
workload-derived targets, rollout gating and autoscaler mutation are not
included.

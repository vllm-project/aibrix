.. _model-warmup:

=================
ModelWarmup
=================

``ModelWarmup`` performs a finite ``Once`` warmup Job on each selected
Kubernetes node. A resource can be image-only, custom-only, or combined:
image entries make the runtime pull an image and run a safe finite command;
custom actions can check a node or download an artifact; a combined action does
both. It does not start an inference engine or gate workloads.

Workflow and data flow
----------------------

.. mermaid::

   flowchart LR
      CR[ModelWarmup CR] --> T[Resolve explicit nodes and selectors]
      T --> R[Calculate Job-template revision]
      R --> I[Run custom init containers sequentially]
      I --> C[Run image and custom regular containers]
      C --> A[Aggregate per-node Job results in status]
      A --> OK[Succeeded, Failed, or Degraded]

Every target receives a node-pinned Job. Custom init containers run in their
declared order. Once they succeed, the controller-generated image containers
and ``custom.containers`` are regular containers and may run concurrently. All
of them must terminate successfully for that node Job to succeed. ``status``
remains an aggregate observation of the generated Jobs rather than a separate
per-container execution API.

Targeting and execution
-----------------------

Targets can combine explicit node names and non-empty label selectors. The
controller takes their union and deduplicates nodes. While a ``Once`` operation
is not terminal, Node create and label events add newly matched nodes. After
Succeeded, Failed, or Degraded, later nodes are ignored.

The namespace and target nodes must share the
``resource-pool.aibrix.ai/name`` label, and nodes must opt in with
``model.aibrix.ai/warmup-enabled=true``. Control-plane nodes are excluded.
``parallelism`` limits simultaneously active Jobs, and ``jobTimeoutSeconds``
applies after a Job is created. ``retryLimit`` and
``ttlSecondsAfterFinished`` bound retry and retention behavior.

ModelWarmup does not infer tolerations from the target Node. A Node with a
``NoSchedule`` or ``NoExecute`` taint remains subject to normal Kubernetes
scheduling policy. Image-only actions request no GPU, while a custom container
can explicitly request a device such as ``nvidia.com/gpu``.

API fields
----------

``spec.imagePreload.images`` accepts image, command, optional args, and pull
policy. Every image entry needs a command that exits safely after the image is
pulled. ``spec.custom`` accepts the following Pod-template fragments:

.. code-block:: yaml

   custom:
     initContainers: []       # finite, sequential node checks
     containers: []           # finite regular actions
     volumes: []              # declared volume sources used by the actions
     imagePullSecrets: []     # pull credentials for custom images

Container names must be unique across generated image containers and custom
containers, and volume mounts must name a declared ``custom.volumes`` entry.
At least one image preload or custom regular container is required; init-only
actions cannot complete a ModelWarmup by themselves.

Examples
--------

The image-only sample is unchanged and remains compatible with the original v1
behavior:

.. literalinclude:: ../../../samples/modelwarmup/modelwarmup.yaml
   :language: yaml
   :linenos:

Each image-only entry must use a command that exits successfully without
starting the inference server. The BusyBox sample uses ``sh -c "exit 0"``.
For the public ``vllm/vllm-openai`` image, its CLI can print help and exit:

.. code-block:: yaml

   imagePreload:
     images:
     - image: vllm/vllm-openai:<version-or-digest>
       command: ["vllm", "serve"]
       args: ["--help=all"]

Verify the command against the exact image tag or digest before creating a
ModelWarmup; other engines and distroless images may expose different binaries.

``model-download.yaml`` is custom-only. It uses the public
``aibrix/runtime:v0.7.0`` image to run ``aibrix_download`` for the small public
``sshleifer/tiny-gpt2`` artifact and writes it to a node-local cache:

.. literalinclude:: ../../../samples/modelwarmup/model-download.yaml
   :language: yaml
   :linenos:

``node-precheck.yaml`` checks cache write access and at least 1 GiB free space;
``gpu-precheck.yaml`` runs ``nvidia-smi`` with an ``nvidia.com/gpu: "1"``
limit and therefore requires a GPU node with the NVIDIA device plugin; and
``combined-warmup.yaml`` combines a generic cache precheck, finite BusyBox image
preload, and the custom downloader. See the sample directory README for apply
commands and prerequisites.

.. literalinclude:: ../../../samples/modelwarmup/combined-warmup.yaml
   :language: yaml
   :linenos:

Security and cache durability
-----------------------------

The namespace-to-node resource-pool label plus explicit node opt-in form the
scheduling authorization boundary. They do not make arbitrary custom actions
safe. Custom actions receive exactly the volumes, images, and resource requests
declared in the resource, so creation permission is sensitive. Use
least-privilege RBAC and dedicated target pools; restrict who can create
ModelWarmups that use ``hostPath`` or privileged settings.

Automatic service-account token mounting is disabled, but explicit projected
``serviceAccountToken`` volumes are allowed and use the Pod's service account
permissions (normally the namespace default service account). Secret volumes can
also expose namespace credentials. Keep service-account permissions minimal and
restrict which token and secret volumes ModelWarmup authors may request.

ModelWarmup admission does not perform full native Job/Pod validation. If the
API server rejects Job creation, the controller logs the error and requeues it.
With no Job to inspect, bounded Job diagnostics and ModelWarmup failure status
may not record that rejection; check controller logs when progress stalls.

The bundled cache samples use ``hostPath`` at ``/var/lib/aibrix/models`` with
``DirectoryOrCreate``. That exposes a node filesystem path to the Job and may
be disallowed by Pod Security admission. The samples do not require privileged
containers; do not broaden privilege, host networking, PID namespaces, or host
mount scope solely to run them.

``Succeeded`` is a point-in-time aggregate Job result. Kubelet or runtime image
garbage collection can evict pulled images, and node replacement, cleanup, or
storage failures can remove host-path model artifacts. Use immutable image and
model references when reproducibility matters, and retain a durable artifact
source for production workloads.

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
     - Controller-managed label containing the owning ModelWarmup UID.
   * - ``model.aibrix.ai/warmup-name``
     - Job
     - Controller-managed annotation containing the human-readable ModelWarmup
       name.
   * - ``model.aibrix.ai/revision``
     - Job
     - Controller-managed label identifying the immutable workload revision.
   * - ``model.aibrix.ai/target-node``
     - Job
     - Controller-managed annotation recording the exact authorized target
       Node. Users do not set these Job metadata keys.

Scope
-----

The spec is immutable after creation; create another resource for another Once
operation. Separate pipeline stages, arbitrary lifecycle hooks, workload or
rollout gating, autoscaler mutation, and workload-derived targets remain out of
scope. Custom actions are finite Job work, not a general-purpose deployment or
orchestration framework.

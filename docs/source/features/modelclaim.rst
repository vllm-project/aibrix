.. _modelclaim:

==========================================
ModelClaim High-Density GPU Runtime Pools
==========================================

.. warning::

   ModelClaim is an experimental feature. It currently supports one engine
   replica per claim, uses a fixed GPU topology per warm pool, and requires a
   dedicated kvcached runtime image. Validate the feature with your engine,
   model, GPU, and workload before using it for production traffic.

ModelClaim lets several independently managed model engines share a warm GPU
runtime Pod. Instead of creating one Kubernetes Deployment per model, an
operator first creates a pool of topology-homogeneous GPU Pods. A user then
creates a ``ModelClaim`` for each model that should run in the pool.

The ModelClaim controller selects a compatible Pod, asks the AIBrix runtime
agent to start a separate engine process, and publishes the engine's port to the
AIBrix gateway only after the engine is ready. The default experimental image
contains vLLM; SGLang requires a custom compatible runtime image. The kvcached
framework provides elastic KV-cache memory across colocated engines. An
optional pool policy can redistribute KV capacity according to observed
requests and put idle vLLM engines into sleep mode.

Architecture
------------

.. mermaid::

   flowchart LR
     MC["ModelClaim"] --> C["ModelClaim controller"]
     C -->|"select Pod and activate"| R["AIBrix runtime agent"]
     subgraph P["Warm GPU Pod"]
       R --> E1["Engine A / port A / IPC A"]
       R --> E2["Engine B / port B / IPC B"]
       E1 <--> K["kvcached physical pages"]
       E2 <--> K
     end
     C -->|"model, port, state annotation"| G["AIBrix gateway"]
     U["OpenAI client"] --> G --> E1
     G -. "sleeping: wake and retryable 503" .-> R

The main responsibilities are:

* **ModelClaim** declares the served model, artifact, pool selector, engine,
  and engine arguments. It does not request GPUs directly.
* **Warm pool Deployment** owns the GPU resources and fixes the Pod-visible
  GPU topology.
* **ModelClaim controller** performs placement, lifecycle reconciliation,
  route gating, and the optional pool policy.
* **AIBrix runtime agent** starts and supervises one process per claim and
  exposes actual engine, memory, and request state through a runtime snapshot.
* **AIBrix gateway** routes a served model to its per-engine port. It can
  trigger a wake for a sleeping model but does not hold the original request.

Prerequisites
-------------

You need:

* an existing Kubernetes cluster with NVIDIA GPUs and the NVIDIA device
  plugin;
* the latest AIBrix nightly control plane from the ``main`` branch;
* a kvcached-compatible image for the selected engine;
* enough host memory for vLLM sleep level 1 and enough ``/dev/shm`` capacity
  for kvcached metadata and IPC;
* a node-local or otherwise persistent model-weight cache;
* ``kubectl`` access to create CRDs, Deployments, Services, and ModelClaims.

Install the nightly control plane
---------------------------------

ModelClaim is included in the current ``main`` branch, including its CRD,
controller reconciliation, and gateway wake support. Follow the **Nightly
Version** section of the
:doc:`AIBrix installation guide <../getting_started/installation/installation>`.
The standard ``config/crd`` and ``config/default`` manifests install the
ModelClaim API and use the public nightly controller and gateway images. No
custom control-plane build is required.

Verify the installation before creating a runtime pool:

.. code-block:: bash

   kubectl get crd modelclaims.model.aibrix.ai
   kubectl -n aibrix-system rollout status \
     deployment/aibrix-controller-manager --timeout=10m
   kubectl -n aibrix-system rollout status \
     deployment/aibrix-gateway-plugins --timeout=10m

Nightly tags are mutable. For a repeatable experiment, record the AIBrix
commit and resolved controller and gateway image digests together with the
runtime image digest.

Build the experimental runtime image
------------------------------------

Only the kvcached runtime agent image needs a feature-specific build. Its
dedicated target is intentionally not part of ``docker-build-all``,
``docker-push-all``, or the public nightly image workflow.

Use the same current ``main`` checkout used by the nightly installation. Build
the image and push it to a registry accessible from the cluster:

.. code-block:: bash

   git clone https://github.com/vllm-project/aibrix.git
   cd aibrix

   export AIBRIX_CONTAINER_REGISTRY_NAMESPACE=ghcr.io/your-organization/aibrix
   export IMAGE_TAG="$(git rev-parse HEAD)"

   IS_MAIN_BRANCH=false make docker-build-kvcached-runtime
   IS_MAIN_BRANCH=false make docker-push-kvcached-runtime

``KVCACHED_RUNTIME_BASE_IMAGE`` defaults to
``ghcr.io/ovg-project/kvcached-vllm:latest``. Pin it to a tested digest or tag
for a reproducible deployment:

.. code-block:: bash

   KVCACHED_RUNTIME_BASE_IMAGE=ghcr.io/ovg-project/kvcached-vllm@sha256:<digest> \
   IMAGE_TAG=modelclaim-test IS_MAIN_BRANCH=false \
     make docker-build-kvcached-runtime

The default base image contains vLLM. The runtime launcher also has an SGLang
path, but using ``engine: sglang`` requires a custom base image that contains a
compatible SGLang and kvcached integration. Automatic idle sleep currently
applies only to vLLM.

Update the warm-pool manifest to use
``${AIBRIX_CONTAINER_REGISTRY_NAMESPACE}/kvcached-runtime:${IMAGE_TAG}``. Keep
the runtime source revision aligned with the nightly control plane revision
used for the test.

Create a warm runtime pool
--------------------------

The repository sample creates one single-GPU runtime Pod and a metrics
Service:

.. literalinclude:: ../../../samples/modelclaim/warm-runtime-pool.yaml
   :language: yaml
   :linenos:

Before applying it, review these fields:

``pool.aibrix.ai/name``
   The logical pool selected by ModelClaims. Put the label on both the
   Deployment and Pod template.

``pool.aibrix.ai/enabled: "true"``
   Required on candidate Pods. Removing or changing it keeps the Pod out of
   ModelClaim placement.

``image``
   Replace ``aibrix/kvcached-runtime:dev`` when using a remote registry or a
   pinned image.

``nvidia.com/gpu``
   Defines the fixed topology for this pool. Every Pod in one pool should
   expose the same number of GPUs.

``/dev/shm``
   The sample uses a 16 GiB memory-backed ``emptyDir``. Size it for the chosen
   kvcached and engine configuration.

``AIBRIX_WEIGHT_CACHE_DIR``
   Points to the model-weight cache inspected by runtime snapshots. The sample
   mounts a node-local ``hostPath`` so recreated Pods on that node can reuse
   downloads.

``/var/run/aibrix``
   Stores the engine registry. The ``emptyDir`` survives an agent process
   restart within the Pod, but not a Pod replacement.

The runtime image still contains kvcached, ``kvctl``, and the shared-memory
control interfaces when ``ENABLE_KVCACHED=false``. This variable controls
whether the current Python process is patched; it does not remove kvcached from
the image. Keep ``ENABLE_KVCACHED=false`` and ``KVCACHED_AUTOPATCH=0`` on the
runtime agent. The launcher overrides both values and assigns a unique
``KVCACHED_IPC_NAME`` for each child engine. Autopatching the agent itself can
load the engine stack into the supervisor, create an unrelated CUDA context,
and blur the isolation boundary between independently managed engines.

Apply the pool and wait for the runtime agent:

.. code-block:: bash

   kubectl apply -f samples/modelclaim/warm-runtime-pool.yaml
   kubectl rollout status deployment/warm-runtime-pool-b300 --timeout=10m
   kubectl get pods -l pool.aibrix.ai/name=b300-pool-a -o wide

Create ModelClaims
------------------

The sample attaches two small Qwen models to the same pool:

.. literalinclude:: ../../../samples/modelclaim/modelclaims.yaml
   :language: yaml
   :linenos:

Apply the claims and watch their lifecycle:

.. code-block:: bash

   kubectl apply -f samples/modelclaim/modelclaims.yaml
   kubectl get modelclaims -w

   kubectl wait --for=jsonpath='{.status.phase}'=Active \
     modelclaim/qwen3-0-6b --timeout=15m
   kubectl wait --for=jsonpath='{.status.phase}'=Active \
     modelclaim/qwen25-0-5b --timeout=15m

The supported spec fields are:

.. list-table::
   :header-rows: 1
   :widths: 24 12 64

   * - Field
     - Required
     - Description
   * - ``modelName``
     - No
     - Served model identifier used in OpenAI-compatible requests. Defaults to
       the ModelClaim object name.
   * - ``podSelector``
     - Yes
     - Selects eligible warm-pool Pods. Candidates must also have the enabled
       pool label.
   * - ``artifactURL``
     - Yes
     - Model artifact location, such as ``huggingface://``, ``s3://``, or
       ``gcs://``.
   * - ``engine``
     - No
     - ``vllm`` or ``sglang``. Defaults to ``vllm``. SGLang requires a
       compatible custom runtime image.
   * - ``replicas``
     - No
     - Defaults to 1. The current API only accepts 1.
   * - ``engineConfig.args``
     - No
     - Engine CLI flags mapped to string values. Use an empty string for a
       boolean flag.

For example:

.. code-block:: yaml

   engineConfig:
     args:
       --max-model-len: "4096"
       --enable-prefix-caching: ""

Do not set ``--gpu-memory-utilization``. kvcached owns elastic KV-cache
allocation, and the ModelClaim path rejects that flag. Data parallelism is not
supported; ``--data-parallel-size`` must remain 1.

Configure TP and PP pools
-------------------------

ModelClaim uses a deliberately simple fixed-topology rule for vLLM:

.. code-block:: text

   tensor-parallel-size * pipeline-parallel-size
     == GPUs visible to the warm runtime Pod

For a TP=2 model, create a separate pool whose Pods each request two GPUs, then
use:

.. code-block:: yaml

   engineConfig:
     args:
       --tensor-parallel-size: "2"
       --pipeline-parallel-size: "1"

Do not use one four-GPU pool to mix TP=1, TP=2, and TP=4 claims. Create one
topology-homogeneous pool per shape. Automatic request-driven KV policy and
idle sleep are currently limited to single-GPU runtime Pods, even though the
fixed TP/PP activation path is supported.

Inspect actual runtime state
----------------------------

ModelClaim status summarizes the lifecycle:

.. list-table::
   :header-rows: 1
   :widths: 22 78

   * - Status
     - Meaning
   * - ``Pending`` / ``Scheduling``
     - The claim is new or the controller is selecting a compatible Pod.
   * - ``Loading`` / ``Activating``
     - The runtime is downloading or starting the engine. It remains
       non-routable with port 0.
   * - ``Active``
     - The runtime reports the engine alive and ready; the gateway has a real
       per-engine port.
   * - ``Sleeping``
     - The engine remains resident but is intentionally non-routable. A request
       can trigger a wake.
   * - ``Failed``
     - Activation or local restart recovery reached a terminal failure.

Inspect claim status and the routing annotation:

.. code-block:: bash

   kubectl get modelclaim qwen3-0-6b -o yaml

   POD=$(kubectl get pod -l pool.aibrix.ai/name=b300-pool-a \
     -o jsonpath='{.items[0].metadata.name}')
   kubectl get pod "$POD" -o json \
     | jq '.metadata.annotations
       | with_entries(select(.key | startswith("modelclaim.aibrix.ai/")))'

An annotation has this form:

.. code-block:: json

   {"model":"qwen3-0.6b","port":20000,"state":"active"}

``port: 0`` means the model is known but not currently routable. It is used
while the engine is activating, restarting, sleeping, or failed.

For detailed engine and memory state, port-forward the runtime API:

.. code-block:: bash

   kubectl port-forward "pod/$POD" 8080:8080

   curl -fsS http://localhost:8080/v1/runtime/snapshot | jq .

The snapshot includes per-engine port, IPC name, phase, liveness, readiness,
restart count, last error, KV usage and capacity, best-effort HBM peak, request
activity, and cached-artifact markers.

Send inference requests
-----------------------

Find the Envoy Service by its ownership labels and start a port-forward:

.. code-block:: bash

   ENVOY_SERVICE=$(kubectl -n envoy-gateway-system get service \
     --selector=gateway.envoyproxy.io/owning-gateway-namespace=aibrix-system,gateway.envoyproxy.io/owning-gateway-name=aibrix-eg \
     -o jsonpath='{.items[0].metadata.name}')
   test -n "$ENVOY_SERVICE"
   kubectl -n envoy-gateway-system port-forward \
     "service/$ENVOY_SERVICE" 8888:80

Send an OpenAI-compatible request using ``spec.modelName``:

.. code-block:: bash

   curl -fsS http://localhost:8888/v1/chat/completions \
     -H 'Content-Type: application/json' \
     -d '{
       "model": "qwen3-0.6b",
       "messages": [{"role": "user", "content": "Say hello."}],
       "max_tokens": 32
     }' | jq .

Enable automatic KV and sleep policy
------------------------------------

Policy is optional and is configured as one strict JSON annotation on the warm
pool Deployment. It does not add fields to ModelClaim:

.. code-block:: bash

   kubectl annotate deployment warm-runtime-pool-b300 \
     'pool.aibrix.ai/policy={"reclaim":{"mode":"kv-first","capacityBytes":4294967296,"guaranteedFloorPercent":20},"lifecycle":{"sleepAfterSeconds":60}}' \
     --overwrite

The fields mean:

``reclaim.capacityBytes``
   Total KV capacity that the policy may distribute on each eligible single-GPU
   runtime. This is not total GPU HBM. Choose it only after accounting for
   model weights, engine overhead, and headroom.

``reclaim.guaranteedFloorPercent``
   Per-model minimum as a percentage of the configured capacity. The policy
   also protects currently used and preallocated KV pages. If protected usage
   already exceeds the total capacity, no new plan is applied.

``lifecycle.sleepAfterSeconds``
   How long a vLLM engine must have complete, initialized, and idle request
   observations before sleep level 1 is applied.

The controller distributes remaining KV capacity among active models using
bounded inflight requests and completion deltas. A configured limit is a
kvcached capacity ceiling, not an immediate physical HBM allocation and not an
OOM guarantee.

The JSON parser rejects unknown fields. An invalid policy is disabled and
reported with an ``InvalidPoolPolicy`` Event:

.. code-block:: bash

   kubectl describe deployment warm-runtime-pool-b300
   kubectl get events --field-selector reason=InvalidPoolPolicy

Sleeping request behavior
-------------------------

The gateway does not hold or replay a request while a model wakes. When a
binding is sleeping, the gateway starts one deduplicated asynchronous wake and
returns:

.. code-block:: text

   HTTP/1.1 503 Service Unavailable
   Retry-After: 10

The client or an outer gateway must retry. While the engine is waking, later
requests can continue to receive 503. The controller restores the real port
only after the runtime reports the engine active and ready.

An activating model also returns 503 with ``Retry-After``. A terminally failed
model returns 503 without ``Retry-After``.

Runtime reliability
-------------------

Each ModelClaim engine has an independent process group and supervisor. An
engine crash is removed from routing and restarted with exponential backoff;
other engines in the Pod are not restarted. After five local restarts, the
engine remains ``Failed`` with its error visible in the runtime snapshot.

The kvcached runtime image uses ``tini`` and a small restart loop around the
AIBrix agent. If only the agent process crashes, child engines stay alive. The
new agent reads ``/var/run/aibrix/engines.json``, verifies the PID start time
and health endpoint, and re-adopts valid engines without changing their port
or IPC name.

This recovery does not cross a Pod restart. If the Pod disappears, the
controller removes the stale instance and can activate the claim on another
compatible Pod. There is no live migration or transparent preservation of
in-flight requests.

Observability
-------------

The sample metrics Service is selected by
``observability/monitor/service_monitor_modelclaim_runtime.yaml``. Import
``observability/grafana/AIBrix_ModelClaim_Runtime_Dashboard.json`` for the
current dashboard.

Controller metrics include:

* ``aibrix_modelclaim_desired_replicas``;
* ``aibrix_modelclaim_ready_replicas``;
* ``aibrix_modelclaim_activating``;
* ``aibrix_modelclaim_activation_total{result}``;
* ``aibrix_modelclaim_pool_policy_valid``.

Runtime metrics include:

* ``aibrix:modelclaim_models_resident``;
* ``aibrix:modelclaim_kv_used_bytes{model}``;
* ``aibrix:modelclaim_kv_total_bytes{model}``;
* ``aibrix:modelclaim_hbm_peak_bytes{model}``.

HBM attribution is best effort and is used for observation and placement
ranking. It is not a hard admission or reservation signal.

Troubleshooting
---------------

Claim remains ``Scheduling`` with zero candidates
   Confirm that the Pod is Running, has a Pod IP, matches ``podSelector``, and
   has ``pool.aibrix.ai/enabled: "true"``. For vLLM, confirm that TP times PP
   exactly matches the Pod-visible GPU count.

Claim remains ``Activating``
   Inspect the runtime snapshot and engine logs. Weight download, CUDA graph
   initialization, or engine compilation may take time. The controller
   intentionally keeps the route at port 0 until ``/health`` succeeds.

Activation rejects ``--gpu-memory-utilization``
   Remove the flag. The kvcached framework replaces the engine's fixed
   KV-memory fraction in this deployment path.

Policy does not change KV limits
   Automatic policy requires exactly one visible accelerator, valid request
   metrics with matching model labels, and at least one observed active model.
   It does not shrink when observations are incomplete or protected KV usage
   exceeds the configured capacity.

Model never enters automatic sleep
   Automatic idle sleep currently applies only to vLLM. Check that request
   activity is initialized and observable and that no request is running or
   waiting.

Requests return 503
   Inspect the routing annotation and ModelClaim status. A sleeping or
   activating model is retryable; a failed model requires operator attention.

Agent restarted but engines also disappeared
   Check that the container uses
   ``build/container/Dockerfile.kvcached-runtime`` and its tini supervisor, and
   that the engine registry is mounted at ``/var/run/aibrix``. A full Pod or
   container restart does not preserve engine processes.

Cleanup
-------

Delete claims before the warm pool so the finalizer can stop their engines and
remove routing annotations:

.. code-block:: bash

   kubectl delete -f samples/modelclaim/modelclaims.yaml --wait=true
   kubectl delete -f samples/modelclaim/warm-runtime-pool.yaml --wait=true

Validation guides
-----------------

Use the
`ModelClaim manual GPU validation runbook <https://github.com/vllm-project/aibrix/blob/main/docs/modelclaim/manual-validation-runbook.md>`_
for the full minikube and real-GPU acceptance procedure. A
`Chinese version <https://github.com/vllm-project/aibrix/blob/main/docs/modelclaim/manual-validation-runbook-zh.md>`_
is also available. To validate kvcached and vLLM sleep independently of the
AIBrix control plane, use the
`mechanism guide <https://github.com/vllm-project/aibrix/blob/main/docs/modelclaim/kvcached-vllm-sleep-test-guide.md>`_.

Upstream references:

* `kvcached <https://github.com/ovg-project/kvcached>`_;
* `vLLM Sleep Mode <https://docs.vllm.ai/en/stable/features/sleep_mode/>`_;
* `Prism: Cost-Efficient Multi-LLM Serving via GPU Memory Ballooning <https://arxiv.org/abs/2505.04021>`_.

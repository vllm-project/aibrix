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
   * - ``perGPU``
     - No
     - What one instance costs on a GPU. A claim without it is accepted, and
       is not placed until it declares one.
   * - ``perGPU.maximumFootprint``
     - Yes, in ``perGPU``
     - A quantity, such as ``30Gi``. The largest non-KV GPU memory one
       instance holds on a device: weights, captured CUDA graphs, activation
       workspaces and allocator retention.
   * - ``perGPU.kvFloor``
     - Yes, in ``perGPU``
     - A quantity, such as ``10Gi``. The KV cache one instance must keep on a
       device to serve at all.

For example:

.. code-block:: yaml

   engineConfig:
     args:
       --max-model-len: "4096"
       --enable-prefix-caching: ""

Do not set ``--gpu-memory-utilization``. kvcached owns elastic KV-cache
allocation, and the ModelClaim path rejects that flag. Data parallelism is not
supported; ``--data-parallel-size`` must remain 1.

Declare what a model costs on a card
------------------------------------

``perGPU`` tells placement what one instance of this model takes off a GPU.
Both figures describe a single device rather than the whole model, because a
card is what an instance has to fit on. Under tensor or pipeline parallelism,
declare the heaviest device: tensor parallel ranks hold the same slice, while
pipeline stages do not. A Pod with several cards is judged by its smallest
one, because which card an engine lands on is the device plugin's choice
rather than placement's.

The control plane does not profile a model to find these numbers. Most of an
engine's non-KV memory is allocator retention that does not scale with the
weights, so the artifact size does not predict it. Take
``maximumFootprint`` from a run of this model with these engine arguments, and
``kvFloor`` from one request of the engine's maximum model length at this
model's bytes per token, rounded up to the KV allocator's page granularity.
Both are quantities, so write ``30Gi`` rather than a count of bytes.
Declaring more than an instance needs wastes room and is safe; declaring less
is not.

With both declared, a claim is placed only on a Pod whose card can be shown to
have room for it, on two counts.

The first is what the card could ever offer: its size, less the maximum
footprint and KV floor of every instance already recorded on it. A model that
needs more than this cannot be placed here while those instances stay, however
long it waits.

The second is what the card can offer today: its size, less each instance's
footprint and whichever is larger of its declared floor and the KV its engine
has actually mapped. Lowering a KV limit evicts nothing, so pages an engine
already holds are not room anyone else can be given. A Pod refused on this
count could take the model once its engines release those pages.

A Pod that cannot be accounted for is not used at all. That covers a runtime
that did not answer, a card the runtime could not measure, a Pod carrying an
instance of a claim that declares nothing, and a Pod running an engine that
no recorded instance answers for. Such an engine counts until the runtime no
longer lists it, or lists it as failed and stopped. No Pod is used either
while the claims cannot be listed.

A Pod goes through the account when its containers request ``nvidia.com/gpu``,
or when its runtime reports accelerators. The second covers GPUs given to a Pod
some other way, such as a dynamic resource claim. A card reported with no memory
at all does not count. That is the card the runtime's mock mode reports on CPU
pools. A reading can report no card, as when NVML fails once. It still counts
one while an engine on the Pod holds a KV segment, or while an instance on the
Pod records a limit. A Pod with none of these is treated as one without a GPU,
and nothing is accounted for on it. A Pod whose runtime did not answer is turned
away whatever it requests, since its runtime is what says that it has cards. An
instance on a Pod without a card records no limit, since no card was divided for
it.

A Pod given its GPUs some other way must report as many of them as one vLLM
instance runs on, its tensor parallel size times its pipeline parallel size. A
Pod that requests ``nvidia.com/gpu`` has to request that many as well.

Upgrade an existing pool
------------------------

A pool from before ``perGPU`` is moved over in this order:

1. Apply the new CRD. An older CRD drops ``perGPU`` from a claim that is
   applied, and ``helm upgrade`` does not replace a CRD.
2. Rebuild the runtime image from the same revision, and roll the warm pools.
   A runtime from before ``perGPU`` does not report what a card can hold. No
   claim is placed on its Pods, and the refusal says that the runtime is older
   than the controller. Engines that already run keep their routes.
3. Roll the controller. An older controller removes ``perGPU`` from a claim
   when it adds its finalizer, so a claim created while it still runs has to
   be applied again.
4. Add ``perGPU`` to each claim.

Every card in a declared pool is divided as a whole
---------------------------------------------------

An account alone would not stop an engine from growing its KV cache into the
space held for another instance, so each engine is also held to a limit, which
its instance records in ``status.instances[].kvLimitBytes``.

The limits on one card are worked out together. Each engine keeps what it
already holds, its declared floor or the KV it has mapped, and the room left
over is shared out by demand: each engine's part is weighted by one plus its
requests in flight, with the requests capped at four as the pool policy below
caps them. A serving engine whose request metrics could not be read counts as
the busiest, since a scrape that timed out says nothing about its load.
Every footprint, every engine's held KV, and every share together come to
exactly what the card can hold, so an engine growing into its new limit cannot
grow into another engine's memory.

An engine that is asleep weighs nothing. It keeps only what it holds, which
after a sleep is normally its floor, and the rest goes to the engines that are
awake. When every engine on a card is asleep, the room left over stays
unassigned until one of them wakes, or a model is placed on the card. An engine
that wakes gets its part back when the card is divided on the next pass. Until
then, it runs under what it held asleep.

An engine that has failed for good is gone: the runtime stops it once its
restarts run out. Its seat and its KV go back to the card, for the engines
beside it and for the next model placed there.

The plan is carried out in an order that never leaves two engines entitled to
the same byte, and never holds an engine to more than its record. The limits
that shrink an engine are written first, and a fresh reading has to confirm them
before anything else happens. A lower limit evicts nothing, so the room a shrink
makes is not there until the engine is seen inside its new limit. Reading back
is not a formality. The CLI that the runtime drives exits zero when there is no
segment to write into. So the limit that is read back is the only evidence there
is. Each new limit is then recorded on its own claim, the ones that go down
first. The limits that grow an engine are written next, and read back the same
way. A model being placed is recorded last, with the limit it is to run under.

A division can fail at each of these steps:

* A shrink that fails changes no record. The controller then holds the engines
  that the step wrote to what they were held to before, and to no more than
  their records. This is a best effort. It is not read back, and it stops at the
  first call that fails.
* A record that cannot be written leaves records that come to no more than the
  card. The shrinks stay in force, and nothing grows.
* A grow that fails leaves the engine below its new record, where it keeps its
  route. A later round grows it, once its plan moves some limit on the card by
  the threshold.

A model stays non-routable until its own limit is in force, and stays routable
only while it is held to no more than that limit. A card whose room could not
be made is skipped, and the next Pod in line is tried.

A card is also planned again once a round, which is about 10 seconds, however
many claims sit on it. The round carries the plan out in three cases:

* The plan gives more to an engine that is short of KV, and the plan of the
  round before gave that engine more as well. An engine is short when it has
  mapped half of the limit it is held to, or when it has requests waiting. An
  engine that serves, and whose load could not be read, is short as well. An
  engine that is asleep is never short. One reading is one sample, so an engine
  that turns short waits for its second round, which is up to 20 seconds.
* Some engine that has a KV segment is held to a limit other than the one its
  instance records. That is what a write that had no effect leaves behind. An
  engine whose instance records no limit counts as well.
* The card is at rest, and some engine is held to less than half of the limit
  that the plan gives it. A card is at rest when every load on it was read, and
  nothing is in flight. That is what a burst on the engine beside it leaves
  behind.

In any other case, the card is left alone. A limit is a ceiling, and the
requests in flight come and go. Carrying out every plan would cost the writes,
and would give nothing to an engine that is far from its limit. A card that is
close to its plan is left alone as well: the threshold is the larger of half a
gibibyte and a hundredth of the card. A KV allocator hands out whole bundles of
pages, and a change smaller than a bundle moves no memory at all. These
divisions raise no Event. The controller logs them at verbosity 2.

A card whose engines change is divided on the next pass, without waiting for its
round. That covers a model removed or failed, an engine that sleeps or wakes,
and a declaration that changes. A model being placed divides its card itself, as
above. Every move is carried out, however small. This is also how the room comes
back when an engine cannot be started after its card was divided for it. The
room an engine leaves goes to the engines beside it. A claim that waits for that
card can still take it, until those engines have mapped it. Sometimes, a card
cannot be divided for a change yet, as while an engine that left is still
exiting. The round then tries again, and still as for a change. The controller
keeps what each card was divided for in memory only. After a restart, the first
round of a card divides it whatever its load, unless the card is close to its
plan. If that division fails, every round tries it again until one works.

When the division of a card fails three times in a row, each claim on the card
gets a ``KVLimitFailed`` warning, and another after every thirty more failures.
That is every five minutes while every round fails. A run of failures ends with
a division that works, or after more than five minutes without a failure. A card
that cannot be accounted for is left as it is, and the log says why.

Placement raises an Event on each claim whose limit it moves. So does a division
after the engines change, and so does the health loop when it writes a limit
back. A division that fails can leave a limit moved without an Event. To see the
Events:

.. code-block:: bash

   kubectl get events --field-selector reason=KVLimitSet
   kubectl get events --field-selector reason=KVLimitFailed

The automatic pool policy below is not applied on these Pods. Two writers on one
KV allocator would only overwrite each other.

A claim without ``perGPU`` is not placed. Nothing could be put there in its
place: what an engine holds beyond its weights does not follow from the
artifact, so a claim that does not say is a card nobody can account for. One
such claim would make its whole card unusable to every other model. The claim
stays ``Pending``, and its ``Scheduled`` condition reads ``InvalidPerGPU``.

The schema leaves ``perGPU`` optional, and the controller refuses the claim
instead. A claim stored before the field existed has to stay valid. Were the
field required, such a claim would fail validation on its next update. On an
API server without CRD validation ratcheting, its finalizer could then not be
removed, so the claim could not be deleted either. A ``perGPU`` that is given
has to carry both figures, and an apply without one of them is rejected.

Both figures have to be positive. A quantity carries no schema bounds, so a
``0`` is caught by the controller instead: the claim is not placed, and its
``Scheduled`` condition reads ``InvalidPerGPU`` and names the figure. A zero is
never read as a model that takes no room. A figure may be no more than ``1Pi``.
One below ``1Mi`` also has to be a whole number of bytes. That catches a mistake
such as ``30m`` for ``30M``, which would otherwise be read as one byte. A larger
figure, such as ``5.6Gi``, is rounded up to whole bytes.

A claim stored before the field existed decodes with its declaration missing,
and it is not placed again, for the same reason. An engine it already runs keeps
running and keeps its route. The card under it cannot be accounted for until the
claim declares its cost.

Sleeping does not free a seat. An instance that is asleep keeps its place in
the account, at the full footprint and floor its claim declared, because the
assignment has to survive the sleep for a wake to find its engine again.
Normally, an engine that goes to sleep gives back the KV it had mapped. The
models beside it can take that KV, and so can a claim that was turned away
because of it.

A failed instance does free its seat. The runtime stops an engine once its
restarts run out, and reports it as not alive. The account then charges the
instance nothing.

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
pool Deployment. It does not add fields to ModelClaim.

.. note::

   ``reclaim`` is replaced by ``spec.perGPU`` and will be removed. Its
   ``capacityBytes`` is a figure an operator types in, unrelated to what the
   card actually holds, so a pool configured this way can be both wrong and
   confident. A claim declares ``perGPU`` instead, which has its card measured
   and divided, and the policy is not applied on those Pods. ``lifecycle`` is
   not affected.

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

The policy leaves a Pod alone when an instance recorded on it already runs
under a KV limit of its own. That is the case for every instance placed on a
card, since a claim without a ``perGPU`` declaration is not placed. Until
``reclaim`` is removed, this annotation reaches only instances placed before
``perGPU`` existed, and Pods without a card.

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

An activating model also returns 503 with ``Retry-After``. So does a claim
that is not placed yet. Its message gives the controller's reason: from the
claim's ``Scheduled`` condition while it waits, such as ``NoMatchingPods``, or
from its ``Ready`` condition once it has failed. The controller tries such a
claim again by itself, so the client is asked to retry as well. That includes a
model whose engine failed for good, with ``EngineFailed``, since the controller
moves it to another Pod once one can take it. A claim that has to be changed
first, such as one with ``InvalidEngineConfig`` or ``InvalidPerGPU``, gets no
``Retry-After``. A model that no ModelClaim serves returns 400.

The answer for a claim that is not placed comes from the ModelClaim object,
not from a Pod, so it wakes nothing. If two claims serve one name, the first
by namespace and name answers. The gateway's role needs to get, list and watch
ModelClaims. A gateway that has just started answers such a claim with 400
until its first list of them finishes. If it cannot list them at all, it says
so once in its log, and keeps answering 400, as for a model that does not
exist. Access granted later takes effect when the gateway restarts.

Runtime reliability
-------------------

Each ModelClaim engine has an independent process group and supervisor. An
engine crash is removed from routing and restarted with exponential backoff;
other engines in the Pod are not restarted. After five local restarts, the
engine remains ``Failed`` with its error visible in the runtime snapshot.

The controller then moves the claim. It stops the failed engine and places the
claim on another Pod, the way it places a new one. The new engine goes only on
a card with room for it, and the card is divided before the engine starts. The
Pod where the engine failed is left out, and the other engines there keep
running. If no other Pod can take the claim, it stays ``Failed`` until one can.

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

HBM attribution is best effort and is used for observation. It is not an
admission signal: admission works from the cost a claim declares and the size
the runtime measures for a card. Ranking puts a Pod that already has the
artifact first, then orders the admitted Pods by the room their account shows.
Free memory only breaks a tie between two cards whose account shows the same
room, because it moves with traffic.

Troubleshooting
---------------

Claim remains ``Scheduling`` with zero candidates
   Confirm that the Pod is Running, has a Pod IP, matches ``podSelector``, and
   has ``pool.aibrix.ai/enabled: "true"``. For vLLM, confirm that TP times PP
   exactly matches the Pod-visible GPU count.

Claim remains ``Pending`` with ``NoMatchingPods`` about GPU memory
   Candidates exist, but no card can be shown to have room for
   ``perGPU.maximumFootprint`` plus ``perGPU.kvFloor``. The message names the
   Pod that is closest to holding the model, and says why it was turned away.
   There are three kinds. A card could never hold the model. A card could hold
   it, and its room is held by the engines already on it. A card had room, and
   could not be divided. A card of the last two kinds is named first, since
   waiting can help there. Among cards of one kind, the one with the most room
   is named. For the first two kinds, the message also shows the card's account.
   That is how much it holds, and how much of that is promised to, or held by,
   the instances on it.

   A Pod is also turned away when it cannot be accounted for. Its runtime did
   not answer, or one of its cards could not be measured. A claim on it declares
   no usable ``perGPU``. An engine there belongs to no claim, or is still
   exiting. The claims could not be listed. When no Pod could be accounted for,
   the message names one of them and the reason.

   The claim is tried again on every pass, and the ``NoMatchingPods`` Event is
   raised only when the refusal changes. The ``Scheduled`` condition always
   carries the current one.

Claim remains ``Pending`` with ``InvalidPerGPU``
   ``perGPU`` is missing, or one of its figures cannot be used, and the
   message names which. The claim is not placed anywhere until it declares
   its cost, because a card carrying it could not be accounted for. It is
   tried on the next pass after the claim is fixed.

A Pod is turned away because an engine on it belongs to no claim
   The refusal names the model that engine serves. Nobody knows what such an
   engine holds. So its card is kept out of placement until the runtime no
   longer lists the engine, or lists it as failed and stopped. An engine that
   was told to stop reads as still exiting until its last process has gone. It
   normally goes by itself within seconds. The runtime only asks it to stop, so
   an engine that does not react stays. Restart the Pod then. Any other such
   engine is not stopped by the controller. Stop it through the runtime API of
   that Pod, or restart the Pod:

   .. code-block:: bash

      kubectl port-forward "pod/$POD" 8080:8080
      curl -fsS -X POST http://localhost:8080/v1/runtime/models/deactivate \
        -H 'Content-Type: application/json' \
        -d '{"model_name": "<model>", "mode": "stop"}'

Claim remains ``Activating``
   Inspect the runtime snapshot and engine logs. Weight download, CUDA graph
   initialization, or engine compilation may take time. The controller
   intentionally keeps the route at port 0 until ``/health`` succeeds. An
   instance is recorded before its engine is started, so the runtime may know
   no engine for it, for example after the controller stopped between the two
   steps. The controller then starts the engine again. If the runtime
   refuses, the instance is dropped with an ``ActivateFailed`` Event, its card
   is given back, and the claim is tried again. The next try may ask the same
   Pod. If the runtime took the call and its answer was lost, the engine may
   have started. The instance then stays, and the first pass that can read the
   runtime finds out.

Claim remains ``Activating`` after ``/health`` succeeds
   With ``perGPU`` declared, the engine also has to report the KV limit it was
   given before it becomes routable. kvcached applies a new limit at its next
   allocation, so a short wait here is expected. A ``KVLimitFailed`` Event
   names the error. A snapshot whose ``kv_capacity_bytes`` is negative means
   the engine has not built its KV segment yet, and there is nothing to write
   into.

``KVLimitFailed`` Events during placement
   A card had room, and the engines on it could not be held to their new
   shares. The Event names the engine: one that did not take its limit has no
   segment to write into, and one holding more than its new limit grew between
   the plan and the reading that confirms it. The claim moves on to the next
   Pod. If none is left it stays ``Pending``, and its ``NoMatchingPods``
   message names the card that had room and could not be divided.

A ``KVLimitFailed`` warning says a card could not be divided several times
   Every engine on the card keeps serving under the limit it is held to. A
   division that fails at its shrinks changes no record, and the controller
   tries to hold the engines to what they were held to before. One whose records
   cannot be written leaves the shrinks in force. One that fails at its grows
   has made the room and recorded it, and leaves an engine below its record
   until a round grows it. The message quotes the last failure. An engine that
   did not take a KV limit points at its runtime or its segment, as above. One
   that holds more than its new limit is still growing, and the next round plans
   around it. The warning comes on the third failure in a row, and again after
   every thirty more. A division that works ends the run, and so do more than
   five minutes without a failure.

A routable model becomes non-routable with ``KVLimitNotHeld``
   Its engine is held to more KV than its limit, most often because it
   restarted and its allocator put its own default back. It could grow into
   memory the card holds for its neighbours, so the route is withdrawn while
   the controller writes the limit again, and returns once the engine reports
   it.

Activation rejects ``--gpu-memory-utilization``
   Remove the flag. The kvcached framework replaces the engine's fixed
   KV-memory fraction in this deployment path.

Policy does not change KV limits
   Automatic policy requires exactly one visible accelerator, valid request
   metrics with matching model labels, and at least one observed active model.
   It does not shrink when observations are incomplete or protected KV usage
   exceeds the configured capacity. Its KV part is also not applied on a Pod
   where an instance records its own KV limit. Idle sleep still runs there.

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

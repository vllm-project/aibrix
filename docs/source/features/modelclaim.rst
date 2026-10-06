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
     G -. "sleeping: wake request on the Pod, retryable 503" .-> C
     C -. "wake" .-> R

The main responsibilities are:

* **ModelClaim** declares the served model, artifact, pool selector, engine,
  and engine arguments. It does not request GPUs directly.
* **Warm pool Deployment** owns the GPU resources and fixes the Pod-visible
  GPU topology.
* **ModelClaim controller** performs placement, lifecycle reconciliation,
  route gating, and the optional pool policy.
* **AIBrix runtime agent** starts and supervises one process per claim and
  exposes actual engine, memory, and request state through a runtime snapshot.
* **AIBrix gateway** routes a served model to its per-engine port. It asks the
  controller to wake a sleeping model, and does not hold the original request.

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
   ModelClaim placement. A Pod wakes a waiting claim, as described under
   Troubleshooting, only while it carries both labels.

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

Only ``perGPU`` can be changed after a claim is created. The other fields
decide which engine runs and where, and the engine is started with them only
once, so the API server rejects any change to them. To change one, create a
new ModelClaim.

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
the busiest, since a scrape that timed out says nothing about its load. An
engine that is still starting or waking is not routed yet, so it has no load,
and it counts as idle.
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
only while it is held to no more than that limit. An engine that is not on the
route is read back as soon as its limit is written. So an engine coming up is
routed in the same pass, and so is one that woke. A card whose room could not
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
close to its plan is left alone as well, even in the three cases above. The
threshold is the larger of half a gibibyte and a hundredth of the card. A KV
allocator hands out whole bundles of pages, and a change smaller than a bundle
moves no memory at all. These divisions raise no Event. The controller logs them
at verbosity 2.

A card whose engines change is divided on the next pass, without waiting for its
round. That covers a model removed or failed, an engine that sleeps or wakes,
and a declaration that changes. A model being placed divides its card itself, as
above. Every move is carried out, however small. This is also how the room comes
back when an engine cannot be started after its card was divided for it. That
card is divided in the same pass. The room an engine leaves goes to the engines
beside it. A claim that waits for that card can still take it, until those
engines have mapped it. Sometimes, a card cannot be divided for a change yet, as
while an engine that left is still exiting. The round then tries again, and
still as for a change. The controller keeps what each card was divided for in
memory only. After a restart, the first round of a card divides it whatever its
load, unless the card is close to its plan. If that division fails, every round
tries it again until one works.

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

By default, sleeping does not free a seat. An instance that is asleep keeps its
place in the account, at the full footprint and floor its claim declared. This
is its wake reserve, and it makes sure the engine can always wake again.
Normally, an engine that goes to sleep gives back the KV it had mapped. The
models beside it can take that KV, and so can a claim that was turned away
because of it.

A pool can have its sleeping engines keep no wake reserve, with
``lifecycle.noWakeReserveWhileAsleep``. After each sleep, the runtime measures
the memory the engine still holds, its sleeping footprint. The account then
charges a sleeping instance only that, and other models can use the rest, to
be placed or as KV. A request to wake the engine charges its wake reserve again
at once. An engine whose memory asleep could not be measured keeps its reserve,
and its claim raises a ``SleepingFootprintUnknown`` Warning.

In such a pool, a new claim that no Pod has room for can have room made for it.
Room is made for one claim in a pool at a time: the claim that has waited
longest among those that room can be made for. A claim that no room can be
made for on any Pod puts no engine to sleep and does not hold up the claims
behind it. Kueue calls this order ``BestEffortFIFO``. The controller picks the
Pod where the fewest engines would have to sleep, the ones idle longest, as
told by what each engine held the last time it slept. Where that is not known
for an engine, it picks the Pod nearest to fitting the claim, and decides
again once that engine has slept. It puts the engines to sleep one at a time,
once each has been idle for ``sleepToMakeRoomAfterSeconds``, and their claims
raise ``SleptToMakeRoom`` Events. Meanwhile, the claim's ``Scheduled``
condition says ``MakingRoom``, and the claim raises a ``MakingRoom`` Event. The
Pod is held for the claim for up to two minutes, or until the claim is
deleted. During that time, no other new claim is placed in that room or takes
its turn, and the card does not lend the room out as KV. Requests to wake
sleeping engines still go first, because clients are waiting for them while a
new claim has not served yet. Such a request may wake into that room or put an
engine there to sleep, and no room is made for a new claim on a card where a
wake request can be met. The claim is placed once the room is there. A Pod
with nothing left to put to sleep is let go, and the claim waits for room as
before.

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
topology-homogeneous pool per shape. The automatic request-driven KV policy is
currently limited to single-GPU runtime Pods. Idle sleep and sleeping to make
room also work on Pods with several GPUs.

Inspect actual runtime state
----------------------------

ModelClaim status summarizes the lifecycle:

.. list-table::
   :header-rows: 1
   :widths: 22 78

   * - Status
     - Meaning
   * - ``Pending`` / ``Scheduling``
     - The claim is new or the controller is selecting a compatible Pod. A
       claim that loses its last engine, for example with its Pod, is
       ``Pending`` again until it is placed, and its ``Ready`` condition is
       ``False`` with reason ``NotPlaced``.
   * - ``Loading`` / ``Activating``
     - The runtime is downloading or starting the engine. It remains
       non-routable with port 0. While the engine boots, the controller looks
       at it every 2 seconds, so it is routed within about 4 seconds of being
       ready. Each boot is watched this way for 5 minutes. An engine that
       still boots after that is looked at every 10 seconds. An engine that
       is being stopped is watched the same way until it has gone, so that
       the engine that replaces it starts soon. One whose stop keeps failing
       is looked at every 10 seconds.
   * - ``Active``
     - The runtime reports the engine alive and ready; the gateway has a real
       per-engine port.
   * - ``Sleeping``
     - The engine remains resident but is intentionally non-routable. A request
       asks the controller to wake it.
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

   {"model":"qwen3-0.6b","port":20000,"state":"active","wakeByRequest":true}

A route that is not served may also carry a ``reason``, such as
``WaitingForRoom``.

``port: 0`` means the model is known but not currently routable. It is used
while the engine is activating, restarting, sleeping, or failed.

For detailed engine and memory state, port-forward the runtime API:

.. code-block:: bash

   kubectl port-forward "pod/$POD" 8080:8080

   curl -fsS http://localhost:8080/v1/runtime/snapshot | jq .

The snapshot includes per-engine port, IPC name, phase, liveness, readiness,
restart count, last error, KV usage and capacity, best-effort HBM peak, the
memory a sleeping engine still holds, request activity, and cached-artifact
markers.

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
   observations before sleep level 1 is applied. An engine's idle time starts
   no earlier than when it was last routed, so an engine that has just woken is
   not idle before its route is back. Before an engine sleeps, its route is
   taken back and it is read again. If it is serving a request, or has
   completed one since it was last read, it stays awake and its route is put
   back. The field may be left out when ``noWakeReserveWhileAsleep`` is true.
   No engine is then put to sleep for being idle, only to make room.

``lifecycle.noWakeReserveWhileAsleep``
   By default, a model that sleeps keeps a wake reserve. The footprint and
   floor its claim declared stay reserved, so its wake always fits. When true,
   it keeps no wake reserve while asleep. The account charges it only its
   sleeping footprint, the memory its engine still holds, and other models can
   use the rest. A wake then has to find room again.

``lifecycle.sleepToMakeRoomAfterSeconds``
   How long an engine must have served nothing before the controller may put
   it to sleep to make room for a model that wakes, or for a new one. It
   defaults to 30 seconds, or to ``sleepAfterSeconds`` when that is shorter.
   When set, it must be positive and no longer than ``sleepAfterSeconds``.

Turn ``noWakeReserveWhileAsleep`` on only once both the controller and the
gateway are upgraded. An older gateway wakes an engine through its runtime,
and such a wake skips the check for room. For the same reason, do not wake an
engine through its runtime while the switch is on. Turning the switch off is
safe. Every wake checks for room first, so on a card promised more than it
has, a wake waits or moves rather than overrun the card.

.. code-block:: bash

   kubectl annotate deployment warm-runtime-pool-b300 \
     'pool.aibrix.ai/policy={"lifecycle":{"sleepAfterSeconds":300,"noWakeReserveWhileAsleep":true}}' \
     --overwrite

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
binding is sleeping, the gateway asks the controller to wake the model. It
writes a wake request on the Pod, unless one is already there. The request is
the annotation ``wake.modelclaim.aibrix.ai/<claim>``, and it holds the time of
the request. Then the gateway returns:

.. code-block:: text

   HTTP/1.1 503 Service Unavailable
   Retry-After: 10

The client or an outer gateway must retry. While the engine is waking, later
requests can continue to receive 503. The controller restores the real port
only after the runtime reports the engine active and ready.

The controller wakes the engine through the runtime, once its card is promised
no more than it has. A request charges the engine its wake reserve again at
once, so no other model can take that room from then on. An engine that kept
its reserve fits, unless a declaration grew while it slept. In a pool that
keeps no wake reserve, the engine's neighbours may hold the room it left as KV.
The controller holds them to their shares again first, and raises the engine to
its floor. Only then does it wake the engine. A claim whose card cannot be
accounted for is woken all the same where its reserve was kept, since its room
is still there. In a pool that keeps no wake reserve, such a wake waits until
the card can be accounted for. The request stays on the Pod while the engine
boots. The controller removes it once the engine serves, or after five minutes
if the engine is still booting then. ``Waking`` and ``Woken`` Events mark a
wake that went through.

When the card has no room for the engine, the controller makes room in a pool
that keeps no wake reserve. The floors of its neighbours may leave no room, or
the neighbours may hold KV beyond their floors, which the card lent them while
the engine slept. A smaller KV limit does not make a busy engine give its KV
back, and only a sleep does. So the controller puts to sleep the neighbour that
has served nothing for longest, one at a time, until the engine fits. A
neighbour may be put to sleep once it has been idle for
``sleepToMakeRoomAfterSeconds``. Its claim raises a ``SleptToMakeRoom`` Event,
which names the model the room is made for. A neighbour that serves is never
put to sleep, so a wake beside busy neighbours moves, or waits until one of
them has been idle long enough.

Wake requests on a card are handled in the order they were made, and each one
counts the room that the earlier requests on the card will take. A request
that fits wakes at once. The first request that needs room made gets it, and
later requests that also need room wait behind it. A request that would not
fit even with every idle neighbour asleep puts no neighbour to sleep and does
not hold up the requests after it. It waits until it fits or room can be made
for it, or until it expires. No room is made on a card where the memory of a
sleeping engine could not be measured, since another sleep there would most
likely free nothing.

An engine that cannot wake where it is moves, when another Pod can take its
claim. That is an engine whose card is promised more than it has, and one whose
runtime reports that it could not wake it. The instance is marked ``Failed``,
and records why in ``status.instances[].reason``, as ``NoRoomToWake`` or
``WakeFailed``. The claim raises a ``Moving`` Event. In the same pass, the
controller stops the engine and starts the claim on the other Pod, as it does
for an engine that failed for good. A ``Rescheduled`` Event says where the
claim went. A move that cannot finish in that pass is tried again with the
usual backoff. Until it finishes, the claim's ``Ready`` condition says
``Moving``. A move for want of room is called off while the engine still
sleeps where it was, once that card can take it back. The instance reads
``Sleeping`` again, and the claim raises a ``MoveCalledOff`` Event.

When no other Pod can take it, an engine whose card cannot take it back stays
asleep. Its instance records ``WaitingForRoom``, and so does the claim's
``Ready`` condition. The claim raises a ``WaitingForRoom`` Event once, when the
wait starts. A wake that fails with no other Pod to go to raises a
``WakeFailed`` Event, and its request is removed, so the next request for the
model asks again. A wake whose runtime cannot be reached, does not answer in
time, or fails without a report of its own, such as an error from a proxy on
the way, is asked again on a later pass. A request that is not met within five
minutes is removed, with a ``WakeRequestExpired`` Event, and a client that
still asks writes a new one.

The gateway asks a client to wait longer while the controller makes room. It
asks for 20 seconds while a wake waits for room, or while room is made for a
new claim, and for 30 seconds while a claim waits to move. It reads the reason
from the route, where the controller writes it beside the state, or from the
claim's ``Scheduled`` or ``Ready`` condition. The message of the 503 gives the
reason too.

A controller that wakes engines itself says so in the binding, with
``"wakeByRequest":true``. A gateway that finds no such field asks the runtime
to wake the engine directly, as an older controller expects. So does a gateway
that runs without Kubernetes. The gateway writes wake requests with its
permission to patch Pods.

An activating model also returns 503 with ``Retry-After``. So does a claim
that is not placed yet. Its message gives the controller's reason: from the
claim's ``Scheduled`` condition while it waits, such as ``NoMatchingPods``, or
from its ``Ready`` condition once it has failed. The controller tries such a
claim again by itself, so the client is asked to retry as well. That includes a
model whose engine failed for good, with ``EngineFailed``, since the controller
moves it to another Pod once one can take it. A claim that has to be changed
first, such as one with ``InvalidEngineConfig`` or ``InvalidPerGPU``, gets no
``Retry-After``. Neither does one that no card in its pool can hold, with
``TooLargeForAnyCard``. A model that no ModelClaim serves returns 400.

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
Until then, it is tried again as a refused claim is, less and less often. So is
a claim whose replacement the runtime refused to start.

The kvcached runtime image uses ``tini`` and a small restart loop around the
AIBrix agent. If only the agent process crashes, child engines stay alive. The
new agent reads ``/var/run/aibrix/engines.json``, verifies the PID start time
and health endpoint, and re-adopts valid engines without changing their port
or IPC name.

This recovery does not cross a Pod restart. If the Pod disappears, the
controller removes the stale instance and can activate the claim on another
compatible Pod. There is no live migration or transparent preservation of
in-flight requests.

The controller reads each runtime's snapshot with a 10-second deadline. A read
normally takes a fraction of a second, because the runtime asks its engines
concurrently and does not wait for an engine that is being started, put to
sleep or woken. The runtime still changes the engines of a Pod one at a time.
Calls that change state, such as starting an engine, wait up to 60 seconds. The
runtime gives vLLM up to 50 seconds to put an engine to sleep or wake it, since
a first sleep copies the model's weights to host memory. vLLM can fail a sleep
after it has offloaded the weights, when another engine on the card takes
memory at the same time. The runtime then tries again, up to three attempts in
all. With nothing left to offload, the next attempt normally completes the
sleep.

vLLM aborts the requests an engine is serving when it sleeps. So before a
sleep, the runtime asks vLLM to stop taking new requests and waits up to one
second for the engine to finish the requests it is serving. A request that
reached the engine just before the sleep then still completes. An engine that
is still serving after that second is not idle, so the runtime lets it take
requests again and refuses the sleep with HTTP 409. The controller then gives
the engine its route back. If vLLM cannot pause the engine, the sleep goes
ahead.

A runtime that does not answer in time is left alone for 10 seconds, which is
one round. Calls to it fail at once until then, so one runtime that stopped
answering does not hold up every claim that uses it. Each further timeout in a
row doubles that time, up to a minute, and any answer ends it. A runtime that
was slow once is therefore read again a round later. One that stays down is
left alone for a minute at a time, from its fourth timeout on. The call that
follows each minute waits for its own deadline, which is 10 seconds for a
read. A runtime that answers between its timeouts is asked again a round after
each of them.

An answer counts once all of it has arrived, or its first mebibyte. A runtime
that sends the start of an answer and then stalls did not answer in time. The
controller knows a runtime by the address of its Pod. A Pod that is given the
address of one that is left alone is left alone for the rest of that time.

While a runtime is left alone, nothing is known about its Pod. The engines on
it keep the routing they had, whatever happens to them. A call to start an
engine there is not sent, and placement tries the next Pod in rank instead. A
Pod skipped this way is not tried again in the same pass. A claim stays
``Pending`` only when no other Pod can take it, and it is not marked
``Failed``, since no call was sent. A claim whose engine failed for good is
moved past such a Pod the same way. An instance there whose engine is missing
keeps its place, and its engine is started once the runtime answers again.
Stopping an engine is still sent, since an engine left running would keep its
memory.

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
* ``aibrix:modelclaim_hbm_peak_bytes{model}``;
* ``aibrix:modelclaim_sleeping_footprint_bytes{model}``.

A sleeping engine reports the memory it still holds, as the runtime measured
it right after the sleep. The runtime reads the cards just before and just
after it puts an engine to sleep, and takes the memory of the engine's own
processes. If that did not drop as a sleep would, there is no figure. Some
drivers report host process IDs to a container, and then nothing matches. The
runtime then takes, on each card, the process whose memory dropped by far the
most, since a sleep gives back the engine's weights and the runtime puts one
engine to sleep at a time. An engine on several cards, under tensor or
pipeline parallelism, reports what it holds on its heaviest card, and no
figure if one of its cards gives none. There is no figure either when the
driver reports zero for every process. The figure is cleared when the engine
wakes, and when a wake fails.

HBM attribution is best effort, and is otherwise used for observation.
Admission works from the cost a claim declares and the size the runtime
measures for a card. The one exception is a sleeping engine in a pool that
keeps no wake reserve, which is charged the sleeping footprint its runtime
measured. Ranking puts a Pod that already has the
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

   The ``NoMatchingPods`` Event is raised only when the refusal changes. The
   ``Scheduled`` condition always carries the current one.

   A refused claim backs off. Each refusal in a row doubles the wait before
   the next try: 10, 20 and 40 seconds, then a minute at most. A model that
   waits for room therefore does not have every runtime in the pool read for
   it every 10 seconds. The wait is kept in the controller's memory only, so
   a restart tries every waiting claim at once.

   A waiting claim does not sit through its wait when room may have appeared.
   These changes wake it at once, and it starts again from the shortest wait:

   * Another claim is deleted, is scaled down, fails or goes to sleep.
   * A declaration shrinks, or becomes usable.
   * A Pod joins the pool, or turns ready.
   * The claim's own spec changes.

   Room on a card counts only when every claim on the card declares a usable
   ``perGPU``. While one of them does not, the card is turned away, so a
   neighbour that leaves it frees no room.

   A Pod wakes a waiting claim once by turning ready, so a Pod that keeps
   turning not ready and ready again leaves the wait as it is. A claim with
   several replicas that loses an instance starts over as well, since it needs
   another one.

   A claim that is deleted wakes the others before its engine has exited, and
   an engine holds its memory until it has. So the try that a deletion wakes
   is usually refused once, and the room is found on the next try, 10 seconds
   later. Engines that give back mapped KV while they serve send no signal at
   all. The claim finds that room on its next try, a minute later at most.

   Claims that are woken together are tried oldest first. Three limits
   remain. A claim that has waited long tries less often than one that has
   just arrived, so room that appears without a signal usually goes to the
   newer claim. Nothing holds room for a claim, so a large claim can keep
   waiting while smaller ones keep fitting. A wake also has every waiting
   claim whose candidates changed read each of their runtimes once.

Claim reads ``Failed`` with ``ActivateFailed``
   The claim found a card, and its engine could not be started there. The
   message quotes the error. Most often, the runtime of that Pod refused the
   start, and the card is given back. If the answer of the runtime was lost,
   the engine may have started. The instance then stays, as described under
   "Claim remains ``Activating``".

   The claim is tried again as a refused claim is: after 10, 20 and 40
   seconds, then once a minute. The controller does not give up on it. A claim
   with no instance reads ``Failed`` between two tries. A claim with an
   instance reads as its instances do.

   A Pod that joins the pool or turns ready wakes the claim at once, and so
   does a change to its own spec. Room freed on a card does not, since the
   claim had found a card. Each try goes to the Pod that ranks first. While
   the ranking stands, that is the Pod that refused before.

Claim remains ``Pending`` with ``TooLargeForAnyCard``
   Every candidate card was measured. Each is smaller than
   ``perGPU.maximumFootprint`` plus ``perGPU.kvFloor``, even with nothing
   else on it. So no candidate Pod can ever hold the model. The message says
   what the model needs on a card and what the best Pod offers. A Pod with
   several cards offers what its smallest card holds. Declare less if the
   figures overstate the model, or give it a pool with larger cards.

   The claim keeps backing off. A Pod that joins the pool wakes it at once,
   and so does a change to its own spec. Room freed on a card does not, since
   no card is large enough. While any card cannot be measured, the claim
   reads ``NoMatchingPods`` instead, since that card might hold it.

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
   given before it becomes routable. The pass that first sees the engine ready
   writes the limit and reads it back. So this normally lasts about 4 seconds at
   most. It can last up to 10 seconds in any of these cases. The boot took
   more than 5 minutes, or the runtime does not date it. The runtime did not
   answer the reading of a pass. The controller had to start the engine again.
   Another start of the same claim failed in that pass.

   If it lasts longer, the limit is not in force, and the controller writes it
   again every 10 seconds. If the engine reports another limit, a
   ``KVLimitFailed`` Event says which one. If the write fails, the Event
   names the error. That Event is raised at most once every five minutes for
   an engine, so that a limit that keeps failing does not crowd out the
   claim's other Events. For an engine that comes up, ``KVLimitSet`` is raised once
   the limit reads back. When the runtime does not answer the read-back,
   neither is raised, and the next pass reads the limit. A snapshot whose
   ``kv_capacity_bytes`` is negative means the engine has not built its KV
   segment yet, and there is nothing to write into.

   An engine that woke waits the same way when its limit does not read back.
   Its instance reads ``Activating`` until it does.

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
   memory that the card holds for its neighbours. So the route is withdrawn
   while the controller writes the limit again, and it returns once the engine
   reports the limit. The ``KVLimitSet`` Event of that write says "written
   over", since the limit is read on the next pass. ``KVLimitNotHeld`` is also
   raised when the KV segment of the engine cannot be read. Nothing is written
   then.

   On a Pod that carries both ``pool.aibrix.ai`` labels, the change to the Pod
   starts the next pass at once. So the route is normally back within a few
   seconds. On a Pod without ``pool.aibrix.ai/name``, it is back on the
   claim's next pass, 10 seconds later at most.

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

When a claim is deleted, its engine finishes the requests it is serving before
it stops. The controller takes the claim's route back first and keeps the
engine running for 5 seconds while the gateway stops sending it requests. It
then waits until the engine has no running or waiting request, up to 90
seconds after the deletion, and raises a ``DrainTimedOut`` Event if it has to
stop the engine with requests left. An engine that is asleep, starting or
failed is stopped at once. The claim is removed once its engines have been
asked to stop.

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

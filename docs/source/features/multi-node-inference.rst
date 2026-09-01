.. _distributed_inference:

====================
Multi-Node Inference
====================

Distributed inference splits and processes an LLM across multiple nodes or devices.
This approach is needed for large models that exceed the memory capacity of a single machine.

AIBrix provides two orchestration paths for multi-node inference:

1. Ray-based Orchestration (``RayClusterFleet`` / ``RayClusterReplicaSet``): Uses KubeRay for intra-application worker placement and coordination, with Kubernetes managing replica scaling and rollouts.
2. Native PodSet Orchestration (``StormService``): Kubernetes-native multi-role and multi-node grouping via ``podGroupSize`` without requiring KubeRay.

.. contents:: On this page
   :local:
   :depth: 2


Choosing an Orchestration Abstraction
--------------------------------------

Operators can pick the abstraction matching their deployment topology and infrastructure setup:

.. list-table::
   :widths: 25 35 40
   :header-rows: 1

   * - Abstraction
     - Infrastructure Requirement
     - Best Suited For
   * - RayClusterFleet
     - KubeRay operator installed
     - Standard multi-node vLLM deployments where Ray handles process placement and worker coordination.
   * - StormService
     - Native Kubernetes (no KubeRay required)
     - Prefill-Decode (PD) disaggregated setups, custom multi-role architectures, or direct engine-native distributed backends (like SGLang or vLLM with MPI/NCCL and RDMA networking).


KubeRay Orchestration (RayClusterFleet)
----------------------------------------

In distributed computing, managing multi-node inference requires coordination at two layers: fine-grained task execution inside the cluster, and standard operational management from Kubernetes.

Ray handles intra-application task scheduling and worker communication well, but relies on external systems for cluster lifecycle operations. Kubernetes excels at container scheduling, autoscaling, and rolling updates.

AIBrix combines both: Ray handles internal distributed computation, while Kubernetes manages replica lifecycle and environment setup.

Two key APIs manage Ray clusters: ``RayClusterReplicaSet`` and ``RayClusterFleet``.
These mirror Kubernetes ``ReplicaSet`` and ``Deployment`` patterns. In most cases, ``RayClusterFleet`` is the primary resource to configure.

.. figure:: ../assets/images/mix-grain-orchestration.png
  :alt: mix-grain-orchestration
  :width: 70%
  :align: center

- Ray Framework Focus: Ray handles intra-application orchestration. Each application instance corresponds to a single Ray cluster.
- Kubernetes Layer: Kubernetes operates at the outer layer, handling Ray cluster creation, autoscaling, and rolling updates.
- Service Encapsulation: Services map to Ray clusters representing application instances rather than single pods.

.. attention::
    We already submitted our ideas to the KubeRay community.


How it works
^^^^^^^^^^^^

.. mermaid::

   graph TD
       Fleet["RayClusterFleet<br/>rollout, revision history, pause"] -->|owns| RS["RayClusterReplicaSet<br/>keeps N Ray clusters alive"]
       RS -->|creates| RC1["RayCluster (KubeRay)"]
       RS -->|creates| RC2["RayCluster (KubeRay)"]
       subgraph RC1
           H1["head pod<br/>inference engine + GPU"]
           W1["worker pod(s)<br/>GPU"]
       end
       subgraph RC2
           H2["head pod"]
           W2["worker pod(s)"]
       end
       GW["Gateway"] -.routes only to head pods.-> H1
       GW -.-> H2

The layers, from the outside in:

* ``RayClusterFleet`` carries the rollout semantics of a ``Deployment``: a rolling update or
  recreate strategy, revision history, ``paused``, ``minReadySeconds`` and a progress deadline.
  Every change to ``spec.template`` produces a new ``RayClusterReplicaSet`` and the fleet shifts
  replicas between old and new sets according to ``strategy``.
* ``RayClusterReplicaSet`` keeps a fixed number of KubeRay ``RayCluster`` objects running and
  replaces any that disappear.
* ``RayCluster`` is KubeRay's resource. Its spec comes from ``spec.template.spec`` of the
  fleet, with only the fleet-name label added to the head and worker pod templates, so anything
  KubeRay supports (``rayVersion``,
  ``headGroupSpec``, ``workerGroupSpecs``, ``rayStartParams``) is available.
* The engine runs on the **head pod** with Ray as its distributed executor
  (``--distributed-executor-backend ray`` for vLLM). Worker pods only run ``ray start`` and
  contribute their GPUs to the Ray cluster.

**Readiness.** A Ray cluster counts as ready only when KubeRay reports both the
``RayClusterProvisioned`` and ``HeadPodReady`` conditions as ``True`` and every desired worker is
ready. Those conditions are produced by KubeRay's ``RayClusterStatusConditions`` feature gate,
which the AIBrix installation instructions enable. Without the gate the fleet can never report
ready replicas.

**Routing.** The gateway discovers model pods by the ``model.aibrix.ai/name`` label, but it
ignores pods labelled ``ray.io/node-type: worker``. Requests are therefore routed to head pods
only. The fleet controller stamps every pod with
``orchestration.aibrix.ai/raycluster-fleet-name`` so that metrics and routing state can be mapped
back to the fleet that owns the pod.

Prerequisites
^^^^^^^^^^^^^

* The KubeRay operator. It is optional for the rest of AIBrix and only needed for
  ``RayClusterFleet`` and ``RayClusterReplicaSet``. Install it with the Helm command in
  :doc:`../getting_started/installation/installation`; that command pins a patched operator
  image and turns on the ``RayClusterStatusConditions`` feature gate that readiness depends on.
* GPU nodes for the head pod and each worker pod.
* An engine image that contains Ray. Official vLLM images from v0.6.6 onward work out of the
  box; for older versions see `Container Image Requirements`_ below.

Configuration reference
^^^^^^^^^^^^^^^^^^^^^^^

Both resources live in the ``orchestration.aibrix.ai/v1alpha1`` API group.

**RayClusterFleet spec**

.. list-table::
   :header-rows: 1
   :widths: 28 14 58

   * - Field
     - Type
     - Description
   * - ``replicas``
     - int32
     - Number of Ray clusters to run. Defaults to 1.
   * - ``selector``
     - LabelSelector
     - Must match the labels in ``template.metadata.labels``. Required.
   * - ``template``
     - RayClusterTemplateSpec
     - ``metadata`` and ``spec`` for each Ray cluster. ``spec`` is a KubeRay ``RayClusterSpec``
       and is passed through, with the fleet-name label added to the pod templates.
   * - ``strategy``
     - DeploymentStrategy
     - ``Recreate`` or ``RollingUpdate`` with ``maxSurge`` and ``maxUnavailable``, same
       semantics as a ``Deployment``.
   * - ``minReadySeconds``
     - int32
     - How long a Ray cluster must stay ready before it counts as available.
   * - ``revisionHistoryLimit``
     - int32
     - Number of old ``RayClusterReplicaSet`` objects to keep for rollback.
   * - ``paused``
     - bool
     - Stop the controller from acting on template changes.
   * - ``progressDeadlineSeconds``
     - int32
     - Seconds after which a stalled rollout is reported as failed in ``status.conditions``.

**RayClusterFleet status** reports ``replicas``, ``updatedReplicas``, ``readyReplicas``,
``availableReplicas``, ``unavailableReplicas``, ``observedGeneration``, ``conditions`` and
``scalingTargetSelector``. The fleet exposes the Kubernetes ``scale`` subresource, so
``kubectl scale rayclusterfleet <name> --replicas=N`` works, and a :doc:`PodAutoscaler
<autoscaling/autoscaling>` can use ``kind: RayClusterFleet`` as its ``scaleTargetRef``.

**RayClusterReplicaSet spec** is the subset a ``ReplicaSet`` would have: ``replicas``,
``selector``, ``template`` and ``minReadySeconds``. You normally never create one directly.

**Labels and annotations that matter**

.. list-table::
   :header-rows: 1
   :widths: 42 58

   * - Key
     - Purpose
   * - ``model.aibrix.ai/name`` (label)
     - Set it on the head and worker pod templates. The gateway discovers the model's pods
       through this label. (The ``PodAutoscaler`` uses the fleet's scale selector instead.)
   * - ``ray.io/overwrite-container-cmd: "true"`` (annotation on the Ray cluster template)
     - Tells KubeRay to respect the container ``command`` and ``args`` you wrote instead of
       generating its own ``ray start`` command. KubeRay still injects the generated command
       into the env var ``KUBERAY_GEN_RAY_START_CMD`` so you can run it yourself, which is what
       the sample does. The generated variable does not include ``ulimit``, so set that in
       your own command.
   * - ``ray.io/node-type`` (label, set by KubeRay)
     - ``head`` or ``worker``. The gateway skips ``worker`` pods when routing.
   * - ``orchestration.aibrix.ai/raycluster-fleet-name`` (label, set by the fleet controller)
     - Maps a pod back to its fleet. Do not set it yourself.

**Parallelism sizing.** With the Ray executor, the engine's tensor-parallel size must equal the
number of GPUs in the whole Ray cluster (head plus workers). The sample below runs
``--tensor-parallel-size 2`` on a head pod with one GPU and one worker pod with one GPU.

RayClusterFleet Example
^^^^^^^^^^^^^^^^^^^^^^^

Below is a ``RayClusterFleet`` example deploying a two-node distributed inference cluster:

.. literalinclude:: ../../../samples/distributed/fleet-two-node.yaml
   :language: yaml

What the sample is doing, section by section:

* The **head container** raises the file-descriptor limit, installs the Ray dashboard
  dependencies, runs the KubeRay-generated ``ray start`` command in the background, waits until
  the Ray dashboard on port 8265 answers, and only then launches ``vllm serve`` with
  ``--distributed-executor-backend ray``. Waiting for the dashboard matters: vLLM connects to
  the Ray cluster at startup and fails if the head is not up yet.
* The **worker container** runs the generated ``ray start`` command with its own pod IP and
  then blocks with ``tail -f /dev/null``. A ``preStop`` hook calls ``ray stop`` so the node
  leaves the cluster cleanly.
* The **AI Runtime sidecar** on the head pod exposes standardized metrics on port 8080 and
  provides the liveness and readiness probes for the pod. See :doc:`runtime`.
* The **Service** selects pods by ``model.aibrix.ai/name`` and carries the
  ``prometheus-discovery: "true"`` label so metrics are scraped.
* The **HTTPRoute** attaches the model to the AIBrix gateway by matching the ``model``
  header. This is the same route shape used for single-pod deployments; see
  :doc:`../production/gateway`.

Verify the deployment
^^^^^^^^^^^^^^^^^^^^^

.. code-block:: bash

    # Fleet, its replica set, and the KubeRay clusters it created
    kubectl get rayclusterfleet
    kubectl get rayclusterreplicaset
    kubectl get raycluster

    # Head and worker pods
    kubectl get pods -l ray.io/node-type=head
    kubectl get pods -l ray.io/node-type=worker

The fleet CRD defines no extra printer columns, so compare the counts directly:

.. code-block:: bash

    kubectl get rayclusterfleet qwen-coder-7b-instruct \
      -o jsonpath='{.status.readyReplicas}/{.spec.replicas}{"\n"}'

The fleet is healthy when both numbers match.
Then send a request through the gateway exactly as you would for a single-pod model:

.. code-block:: bash

    kubectl -n envoy-gateway-system port-forward service/envoy-aibrix-system-aibrix-eg-903790dc 8888:80 &

    curl http://localhost:8888/v1/chat/completions \
      -H "Content-Type: application/json" \
      -H "model: qwen-coder-7b-instruct" \
      -d '{"model": "qwen-coder-7b-instruct", "messages": [{"role": "user", "content": "hello"}]}'

Troubleshooting
^^^^^^^^^^^^^^^

**The fleet never reports ready replicas.**
Run ``kubectl describe raycluster <name>`` and look at ``Status.Conditions``. AIBrix requires
``RayClusterProvisioned`` and ``HeadPodReady`` to be ``True``. If those conditions are absent
entirely, the KubeRay operator was installed without the ``RayClusterStatusConditions`` feature
gate; reinstall it with the command from the installation guide.

**Head pod restarts, or vLLM exits with a Ray connection error.**
The engine started before the Ray head was up. Keep the dashboard wait loop from the sample in
front of ``vllm serve``. Also confirm ``rayVersion`` in the template matches the Ray version
inside the image; a mismatch prevents workers from joining.

**Worker pods stay Pending.**
Each worker requests a GPU. Check node capacity with ``kubectl describe node`` and confirm the
``nvidia.com/gpu`` request in ``workerGroupSpecs`` can be satisfied.

**The gateway returns an error for the model although the pods are Running.**
The gateway only routes to head pods that carry ``model.aibrix.ai/name`` and are Ready. Check
that the label is on the head pod template (not only on the fleet) and that the readiness probe
on the runtime sidecar (port 8080, ``/ready``) is passing.

Native PodSet Orchestration (StormService)
------------------------------------------

For deployments that do not run KubeRay, or for disaggregated architectures requiring explicit role separation (such as separate Prefill and Decode roles), AIBrix provides native multi-node grouping via ``StormService``.

Using ``podGroupSize`` within a role template, ``StormService`` allocates multiple synchronized pods for each replica instance and injects deterministic distributed environment variables (such as ``$POD_GROUP_INDEX`` and ``$PODSET_NAME``). This enables engine-native Tensor Parallelism (TP) across multiple nodes.

Key capabilities:

- No external dependencies: Runs directly on Kubernetes without installing KubeRay.
- Multi-Role and Disaggregation support: Allows defining separate roles (such as routing, prefill, and decode) with distinct resource profiles and pod group sizes within a single service definition.
- Deterministic rank and discovery: Pods within a group discover peers via predictable headless service DNS entries (such as ``${PODSET_NAME}-0.${STORM_SERVICE_NAME}``).

StormService Multi-Node TP Sample
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

Below is a complete multi-node Tensor Parallelism example with Prefill/Decode disaggregation (2-node prefill and 2-node decode with ``podGroupSize: 2`` and ``--nnodes 2 --tp-size 2``):

.. literalinclude:: ../../../samples/disaggregation/sglang/tp-1p1d.yaml
   :language: yaml


Container Image Requirements
-----------------------------

.. attention::

    Starting from v0.6.6, essential packages to run distributed inference with the official vLLM container image distribution are included out of the box.
    If you use earlier versions, follow the guidance below to build a compatible image.

If you are using an earlier vLLM version, you have two options:

* Use our built image ``aibrix/vllm-openai:v0.6.1.post2-distributed``.
* Build your own image following these steps:

.. code-block:: Dockerfile

    FROM vllm/vllm-openai:v0.6.1.post2
    RUN apt update && apt install -y wget
    RUN pip3 install ray[default]
    ENTRYPOINT [""]

.. code-block:: bash

    docker build -t aibrix/vllm-openai:v0.6.1.post2-distributed .

.. seealso::

   :doc:`pd-disaggregation`
       The complete guide to prefill/decode disaggregation with ``StormService``.

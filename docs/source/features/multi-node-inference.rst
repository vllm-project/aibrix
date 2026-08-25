.. _distributed_inference:

====================
Multi-Node Inference
====================

Distributed inference splits one model across several nodes or devices. It is the path to take
when a model does not fit into the GPU memory of a single machine, or when you want tensor or
pipeline parallelism to span more accelerators than one node provides.

AIBrix implements multi-node inference on top of `KubeRay <https://github.com/ray-project/kuberay>`_.
Each replica of a model is a Ray cluster: one head pod that runs the inference engine and one or
more worker pods that contribute GPUs. AIBrix adds two custom resources, ``RayClusterFleet`` and
``RayClusterReplicaSet``, that manage those Ray clusters the way a ``Deployment`` and a
``ReplicaSet`` manage pods.

How it works
------------

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
-------------

* The KubeRay operator. It is optional for the rest of AIBrix and only needed for
  ``RayClusterFleet`` and ``RayClusterReplicaSet``. Install it with the Helm command in
  :doc:`../getting_started/installation/installation`; that command pins a patched operator
  image and turns on the ``RayClusterStatusConditions`` feature gate that readiness depends on.
* GPU nodes for the head pod and each worker pod.
* An engine image that contains Ray. Official vLLM images from v0.6.6 onward work out of the
  box; for older versions see `vLLM Version`_ below.

Key API Design
--------------

In the landscape of distributed computing, the need for efficient orchestration of multi-node inference tasks has become paramount.
Kubernetes has established itself as a leading platform for managing containerized applications, offering robust resource management and scalability.
On the other hand, Ray has emerged as a powerful framework for building and running distributed applications, particularly well-suited for handling complex machine learning workflows.
However, the existing approaches to orchestration often fall short in terms of flexibility and simplicity.
Kubernetes operators, while powerful, can become overly complex when dealing with fine-grained orchestration of distributed applications.
Ray, although excellent for internal task scheduling and resource management, lacks the broader resource orchestration capabilities provided by Kubernetes.

To address these challenges, we propose a new orchestration approach that synergizes the strengths of both Kubernetes and Ray.
This approach leverages Ray for ``internal fine-grained application orchestration``, allowing users to utilize Ray's APIs for distributed computation Simultaneously,
Kubernetes will handle the overall application resource orchestration, focusing on ``coarse-grained resource allocation`` and environment configuration.
This division of responsibilities simplifies the design of Kubernetes operators and enhances the overall flexibility and efficiency of the orchestration process.

We introduce two key APIs for RayCluster Management, it's ``RayClusterReplicaSet`` and ``RayClusterFleet``.
It's similar like Kubernetes core concept ``ReplicaSet`` and ``Deployment``. Most of the time, you only need to use ``RayClusterFleet``.

.. figure:: ../assets/images/mix-grain-orchestration.png
  :alt: mix-grain-orchestration
  :width: 70%
  :align: center

- Ray Framework Focus: In this model, Ray is emphasized solely for its role in intra-application orchestration. Each application instance corresponds to a single Ray Cluster, and multiple service instances of an application equate to multiple Ray Clusters. This ensures that Ray handles the distributed nature of the application internally without interference from external orchestration systems.

- Kubernetes Layer: Kubernetes operates at the outer layer, responsible for initiating Ray Clusters and managing standard Kubernetes functionalities such as autoscaling and rolling updates. The Kubernetes layer doesn't orchestrate the roles inside the application anymore. These features are well-established within the Kubernetes ecosystem, ensuring robust and reliable resource management, scaling, and update processes. By leveraging Kubernetes for these operations, we can achieve a seamless integration of Ray’s distributed computing capabilities with Kubernetes’ mature operational management.

- Service Encapsulation and Mapping: At a higher level, services are encapsulated in a manner analogous to Kubernetes Deployments and ReplicaSets. The key difference lies in the mapping: instead of Pods, we now have Ray Clusters representing application instances. Traditionally, a single Pod would constitute an application instance; however, in this distributed model, a Ray Cluster serves this purpose, encapsulating the complexity of distributed execution within itself.

.. attention::
    We already submit our ideas to KubeRay community. Hopefully, we can merge into the repo pretty soon.

Configuration reference
-----------------------

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

Workloads Examples
------------------

.. attention::

    Starting from v0.6.6, we've added essential packages to run distributed inference with vLLM official container image distribution out of the box.
    If you use earlier versions, you can follow guidance below to build your own image compatible with multi-node inference.


This is the ``RayClusterFleet`` example, you can apply this yaml in your cluster.

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
---------------------

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
---------------

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

vLLM Version
------------

If you are using vLLM earlier version, you have two options.

* Use our built image ``aibrix/vllm-openai:v0.6.1.post2-distributed``.
* Build your own image and follow steps here.

.. code-block:: Dockerfile

    FROM vllm/vllm-openai:v0.6.1.post2
    RUN apt update && apt install -y wget # important for future healthcheck
    RUN pip3 install ray[default] # important for future healthcheck
    ENTRYPOINT [""]


.. code-block:: bash

    docker build -t aibrix/vllm-openai:v0.6.1.post2-distributed .

.. seealso::

   :doc:`pd-disaggregation`
       Prefill/decode disaggregation with ``StormService``, the other multi-pod topology AIBrix supports.

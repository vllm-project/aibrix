.. _aibrix-stormservice:

===================
AIBrix StormService
===================

**StormService** is a specialized component designed to manage and orchestrate the lifecycle of inference containers in Prefill/Decode disaggregated architectures. Additionally, it can be utilized to oversee various deployment modes, such as Tensor Parallelism (TP), Pipeline Parallelism (PP), and even single GPU model deployments.

Three-layer Architecture
------------------------

StormService is implemented using several Custom Resource Definitions (CRDs) following a three-layer architecture. An illustration of this architecture is shown below:

.. image:: ../assets/images/stormservice/aibrix-stormservice-illustration.png
   :alt: AIBrix StormService Architecture
   :width: 100%
   :align: center

- **StormService**: This is the top-level CRD that wraps the entire service. It defines the specification of a service unit and tracks its status, including the number of replicas (i.e., RoleSet), a unified template for RoleSets, update strategy, and other configurations. For the detailed definition, see the `stormservice_types.go`_ file.

.. _stormservice_types.go: https://github.com/vllm-project/aibrix/tree/main/api/orchestration/v1alpha1/stormservice_types.go

- **RoleSet**: A RoleSet represents a collection of roles, where each role can serve a specific function (e.g., Prefill or Decode). For more information, see the `roleset_types.go`_ file.

.. _roleset_types.go: https://github.com/vllm-project/aibrix/tree/main/api/orchestration/v1alpha1/roleset_types.go

- **Pods**: Each role within a RoleSet contains multiple Pods, which are the actual containers executing the inference tasks.

Following this layered design, updates to the spec propagate from the StormService to its RoleSets, and then to the individual roles. The reconciler at the StormService level synchronizes the status of RoleSets with the StormService spec (primarily the `Replicas` field), while the reconciler at the RoleSet level synchronizes the status of individual roles with the RoleSet spec.

StormService supports two operational modes at its level: **Rolling Update** and **Inplace Update**. At the RoleSet level, three update modes are supported: **Parallel**, **Sequential**, and **Interleaved**. These are explained in detail below.


Deployment Mode
---------------

Stormservice supports two deployment modes: **Replica Mode** and **Pooled Mode**.

.. note::
    1. These two modes are mutually exclusive. The mode is declared through the `stormservice.spec.mode` field, which accepts `Replica` or `Pooled`.
    2. `spec.mode` is optional and is not defaulted. When it is omitted the mode is inferred for backward compatibility from `stormservice.spec.replicas`: replica mode when `replicas > 1`, otherwise pooled mode.
    3. When `spec.mode` is set to `Pooled`, `spec.replicas` must stay at `1`; roles are scaled through `spec.template.spec.roles[].replicas`.


Replica Mode
^^^^^^^^^^^^

**Replica Mode** treats each `RoleSet` as an independent replica of the service. If you already know P/D ratio, you can directly configure the RoleSet and replicate it.

**Characteristics**

- **Independent Replicas**: Each `RoleSet` operates independently, and changes to one `RoleSet` do not directly affect others.
- **Scaling at RoleSet Level**: Scaling operations are performed by adding or removing entire `RoleSet` instances.


Pooled Mode
^^^^^^^^^^^

**Pooled Mode** views each role within a `RoleSet` as part of a shared pool. In this mode, each role is supposed to be independently scalable. It is designed to handle scenarios where different roles have different scaling needs.

**Characteristics**

- **Resource Pool**: Prefill or Decode instance form a shared pool.
- **Independent Role Scaling**: Each role can be scaled independently based on its specific load and requirements.


Drain Before Controller-Managed Deletion
----------------------------------------

RoleSet can mark Pods as draining before controller-managed deletion. This is
useful for scale-in and recreate rollouts where the gateway should stop sending
new requests to a Pod before the Pod is deleted.

Configure drain per role through ``spec.template.spec.roles[].drain`` on a
StormService, or ``spec.roles[].drain`` when managing a RoleSet directly:

.. code-block:: yaml

   spec:
     template:
       spec:
         roles:
         - name: decode
           replicas: 4
           drain:
             timeoutSeconds: 30

``timeoutSeconds`` is optional and must be non-negative. When it is omitted or
set to ``0``, the controller keeps the previous behavior and deletes selected
Pods immediately.

When ``timeoutSeconds`` is greater than ``0``, the RoleSet controller handles a
selected Pod in two phases:

1. Patch the Pod with drain annotations and requeue until the timeout expires.
2. Delete the Pod after the configured timeout has elapsed.

The controller writes the following annotations:

.. list-table:: Drain annotations
   :header-rows: 1
   :widths: 35 65

   * - Annotation
     - Purpose
   * - ``aibrix.ai/draining=true``
     - Marks the Pod as draining. Gateway ready-pod filtering excludes Pods
       with this value.
   * - ``aibrix.ai/drain-start-time``
     - RFC3339 UTC timestamp used by the controller to calculate timeout
       expiry.
   * - ``aibrix.ai/drain-reason``
     - Diagnostic reason, such as ``scale-in`` or ``rollout``.
   * - ``aibrix.ai/drain-target-action=delete``
     - Confirms the controller intends to delete this Pod after drain. If this
       value is missing or unexpected, the controller resets the drain state
       instead of deleting immediately.

The controller emits ``PodDrainStarted`` when drain begins,
``PodDrainCompleted`` when the Pod is deleted after the timeout, and
``PodDrainStateInvalid`` when it repairs malformed drain state. It does not emit
an event while simply waiting for the timeout; waiting is logged at verbose
level only.

For roles using ``podGroupSize > 1``, the RoleSet controller propagates the role
drain configuration to the internal PodSet so grouped Pods follow the same
drain-before-delete behavior.


Topology Policy
---------------

StormService supports ``spec.template.spec.topologyPolicy`` to request Pod
co-location through Kubernetes pod affinity. The same field is available on
``RoleSet.spec.topologyPolicy`` when managing RoleSets directly.

This is useful when roles have topology-sensitive data paths, such as keeping
Prefill and Decode Pods in the same host or zone to reduce cross-domain traffic.
The controller injects the generated affinity into directly managed Pods and
into PodSet templates for roles that use ``podGroupSize > 1``.

Assume a StormService has two RoleSets, and each RoleSet has two roles:
``prefill`` and ``decode``.

.. code-block:: text

   scope: StormService
   All Pods in the StormService share one topology domain.

   Topology Key: kubernetes.io/hostname -> all Pods on node-a

   +-----------------------------------------+
   | StormService                            |
   | topology domain: node-a                 |
   +-----------------------------------------+
   | RoleSet-1                               |
   |   prefill: pod-1, pod-2                 |
   |   decode:  pod-3, pod-4                 |
   | RoleSet-2                               |
   |   prefill: pod-5, pod-6                 |
   |   decode:  pod-7, pod-8                 |
   +-----------------------------------------+

.. code-block:: text

   scope: RoleSet
   Pods within each RoleSet share one topology domain.
   Different RoleSets may use different topology domains.

   Topology Key: topology.kubernetes.io/zone

   +-------------------------+    +-------------------------+
   | RoleSet-1               |    | RoleSet-2               |
   | topology domain: zone-a |    | topology domain: zone-b |
   +-------------------------+    +-------------------------+
   | prefill: pod-1, pod-2   |    | prefill: pod-5, pod-6   |
   | decode:  pod-3, pod-4   |    | decode:  pod-7, pod-8   |
   +-------------------------+    +-------------------------+

.. code-block:: text

   scope: Role
   Pods with the same role share one topology domain across RoleSets.

   Topology Key: kubernetes.io/hostname

   +-------------------------+    +-------------------------+
   | prefill role            |    | decode role             |
   | topology domain: node-a |    | topology domain: node-b |
   +-------------------------+    +-------------------------+
   | RoleSet-1: pod-1, pod-2 |    | RoleSet-1: pod-3, pod-4 |
   | RoleSet-2: pod-5, pod-6 |    | RoleSet-2: pod-7, pod-8 |
   +-------------------------+    +-------------------------+

``scope`` controls which Pods the injected affinity matches:

.. list-table:: Topology policy scope
   :header-rows: 1
   :widths: 20 50 30

   * - Scope
     - Co-location behavior
     - Common use case
   * - ``StormService``
     - All Pods in the StormService share the same topology value.
     - Keep a small service replica inside one host or zone.
   * - ``RoleSet``
     - All Pods in each RoleSet share a topology value. Different RoleSets may
       land in different topology domains.
     - Keep each Prefill/Decode replica pair together.
   * - ``Role``
     - Pods with the same role share a topology value across RoleSets.
     - Keep role-specific pools, such as all Prefill Pods, together.

``key`` selects the Kubernetes node label used as the topology domain. Common
values are ``kubernetes.io/hostname`` for node-level co-location and
``topology.kubernetes.io/zone`` for zone-level co-location.

You can also use an internal resource-pool label as the topology key. For
example, label nodes by business pool:

.. code-block:: shell

   kubectl label node <node-a> resource-pool.aibrix.ai/name=latency-pool --overwrite
   kubectl label node <node-b> resource-pool.aibrix.ai/name=latency-pool --overwrite
   kubectl label node <node-c> resource-pool.aibrix.ai/name=throughput-pool --overwrite

Then use that label key in the topology policy:

.. code-block:: yaml

   spec:
     template:
       spec:
         topologyPolicy:
           scope: RoleSet
           mode: Preferred
           key: resource-pool.aibrix.ai/name

With this policy, Pods inside each RoleSet prefer to schedule into the same
business resource pool, such as ``latency-pool`` or ``throughput-pool``.

``mode`` selects scheduling strength:

- ``Preferred`` adds a strong pod affinity preference and is the default. Pods
  can still schedule in another topology domain when the preferred domain does
  not have enough resources.
- ``Required`` adds hard pod affinity. Pods can remain ``Pending`` when no node
  in the selected topology domain can satisfy the request.

Example
^^^^^^^

The following policy prefers to place all Pods inside each RoleSet replica on
the same node:

.. code-block:: yaml

   apiVersion: orchestration.aibrix.ai/v1alpha1
   kind: StormService
   metadata:
     name: pd-colocated
   spec:
     selector:
       matchLabels:
         app: pd-colocated
     template:
       metadata:
         labels:
           app: pd-colocated
       spec:
         topologyPolicy:
           scope: RoleSet
           mode: Preferred
           key: kubernetes.io/hostname
         roles:
           - name: prefill
             replicas: 1
             template:
               spec:
                 containers:
                   - name: vllm
                     image: vllm/vllm-openai:latest
           - name: decode
             replicas: 1
             template:
               spec:
                 containers:
                   - name: vllm
                     image: vllm/vllm-openai:latest

Before using a custom topology key, label the nodes with that key. For example:

.. code-block:: shell

   kubectl label node <node-a> topology.kubernetes.io/zone=zone-a --overwrite
   kubectl label node <node-b> topology.kubernetes.io/zone=zone-b --overwrite

Apply one of the topology policy samples and inspect Pod placement:

.. code-block:: shell

   kubectl apply -f samples/orchestration/topology-policy/roleset-zone-preferred.yaml
   kubectl get pods -l storm-service-name=tp-rs-zone-pref -o wide

Topology policy updates affect only newly created or replaced Pods because
Kubernetes Pod affinity is immutable after Pod creation. Existing Pods and
PodSet templates pick up the new affinity after replacement or recreation.

See the complete `topology policy samples`_ in the AIBrix repository.

.. _topology policy samples: https://github.com/vllm-project/aibrix/tree/main/samples/orchestration/topology-policy


Historical-Node Replacement Scheduling
--------------------------------------

RoleSet historical-node scheduling lets replacement Pods prefer nodes that
previously ran the same role workload. It is useful for inference workloads
where node locality can preserve useful warm state, such as downloaded model
files, runtime caches, or KV cache artifacts.

Enable the policy on a role with
``spec.roles[].updateStrategy.replacementScheduling.historicalNode``. The same
field can be used through ``StormService.spec.template.spec.roles[]`` because
StormService templates carry RoleSet role specs.

.. code-block:: yaml

   updateStrategy:
     type: Recreate
     maxSurge: 0
     maxUnavailable: 1
     replacementScheduling:
       historicalNode:
         mode: Preferred

``mode: Preferred`` injects a preferred ``kubernetes.io/hostname`` node
affinity into replacement Pods. The preference is soft: Kubernetes can still
place the Pod on another node when the historical node is unavailable or lacks
capacity.

The controller records historical node bindings in the RoleSet annotation
``orchestration.aibrix.ai/historical-node-bindings``. Because the history is
persisted on the RoleSet, replacement scheduling can continue to use remembered
nodes after the controller restarts.

Stateful and stateless roles use different history scopes because they have
different identity semantics:

.. list-table::
   :header-rows: 1
   :widths: 25 35 40

   * - Role type
     - History scope
     - Reason
   * - Stateful
     - Replica slot, for example ``worker/0 -> node-a``
     - A stateful slot remains the same logical replica after its Pod is
       deleted and recreated, so the controller can safely bind history to the
       slot.
   * - Stateless
     - Role-level recent-node list, for example ``worker -> [node-a, node-b]``
     - Stateless Pods do not have stable per-replica identity, so a replacement
       Pod cannot safely inherit one exact old Pod's slot binding.

For stateless roles, the controller injects the remembered node list as one
preferred affinity term. The Kubernetes scheduler chooses among those nodes with
normal scheduling scoring; the RoleSet controller does not choose one node from
the list, and multiple replacement Pods can still land on the same historical
node when that node is the scheduler's best fit.

The policy applies when the RoleSet controller creates replacement Pods during
recreate-style rollouts. For stateful roles, empty-slot creation can also reuse
an existing slot binding. Stateless scale-up is not replacement and does not use
historical-node affinity because the new Pod adds capacity instead of replacing
a remembered logical replica.

Historical-node scheduling only adds preferred node affinity. The controller
skips injection when the Pod template already defines required node affinity,
when the RoleSet has a required topology policy using
``kubernetes.io/hostname``, or when the role uses ``podGroupSize`` greater than
1. When injection is skipped, the controller logs the reason.

See
``samples/orchestration/stormservice-historical-node-scheduling.yaml`` for stateful
and stateless examples with inline comments.


Update Strategy
---------------

StormService supports multiple strategies to update the managed RoleSets. These strategies are designed to handle different operational modes and ensure service availability during the update process. Below is a detailed explanation of each strategy:

Rolling Update
^^^^^^^^^^^^^^

**Designed for replica mode**, the rolling update strategy gradually replaces old RoleSets with new ones. This approach ensures that the service remains available throughout the update process by respecting the `MaxUnavailable` and `MaxSurge` settings.

**How it Works**

1. **Initial State**: At the start, all RoleSets are running the old revision.
2. **Create New RoleSets**: The controller creates new RoleSets with the updated revision, ensuring that the total number of RoleSets (old + new) does not exceed the sum of the desired replicas and `MaxSurge`.
3. **Delete Old RoleSets**: Once the new RoleSets are ready, the controller starts deleting old RoleSets. It ensures that the number of unavailable RoleSets does not exceed `MaxUnavailable` at any time.
4. **Repeat**: Steps 2 and 3 are repeated until all old RoleSets are replaced by new ones.

**Configuration Parameters**

- **MaxUnavailable**: This parameter defines the maximum number of RoleSets that can be unavailable during the update process. It ensures that a minimum number of RoleSets are always available to serve requests.
- **MaxSurge**: This parameter defines the maximum number of RoleSets that can be created above the desired number of replicas during the update process. It allows the controller to create additional RoleSets temporarily to speed up the update.

**Example**

Suppose we have a `StormService` with 3 replicas, `MaxUnavailable` set to 1, and `MaxSurge` set to 1. The rolling update process might look like this:

.. mermaid::

    graph LR
    classDef old fill:#FFCCCC,stroke:#CC0000,stroke-width:2px;
    classDef new fill:#CCFFCC,stroke:#00CC00,stroke-width:2px;

        A(Initial: 3 old RoleSets):::old --> B(Create 1 new RoleSet):::new
        B --> C(Delete 1 old RoleSet):::old
        C --> D(Create 1 new RoleSet):::new
        D --> E(Delete 1 old RoleSet):::old
        E --> F(Create 1 new RoleSet):::new
        F --> G(Delete 1 old RoleSet):::old
        G --> H(Result: 3 new RoleSets):::new


InPlace Update
^^^^^^^^^^^^^^

**Designed for pooled mode**, the StormService ``InPlaceUpdate`` strategy updates
the existing RoleSets instead of deleting and recreating them. This is an outer
update layer: preserving a RoleSet does not, by itself, preserve the Pods owned
by that RoleSet.

**How it Works**

1. The StormService controller identifies RoleSets that do not use the latest
   StormService revision.
2. With ``StormService.spec.updateStrategy.type: InPlaceUpdate``, the controller
   updates those RoleSet objects in place.
3. Each RoleSet then updates its roles. The role-level strategy determines
   whether Pods are patched or replaced.

**Advantages**

- **Stable RoleSets**: No replacement RoleSets are created during the update.
- **Fast propagation**: The latest template is applied directly to the existing
  RoleSets.
- **No extra RoleSet capacity**: The outer update does not require surge
  RoleSets.

.. mermaid::

    graph LR
    classDef old fill:#FFCCCC,stroke:#CC0000,stroke-width:2px;
    classDef new fill:#CCFFCC,stroke:#00CC00,stroke-width:2px;

        A(Initial: 3 old RoleSets):::old --> B(Update 3 RoleSets in-place)
        B --> C(Result: 3 new RoleSets):::new

StormService and role update strategies
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

StormService and role in-place strategies operate at different layers. To
update a Pod image through a StormService while preserving the Pod name and
UID, configure both layers.

.. list-table:: Update strategy comparison
   :header-rows: 1
   :widths: 24 18 24 34

   * - Strategy
     - Target
     - Identity behavior
     - Configuration path
   * - StormService ``InPlaceUpdate``
     - RoleSet
     - Preserves RoleSet identity; does not guarantee Pod identity
     - ``StormService.spec.updateStrategy.type``
   * - Role ``InPlaceIfPossible``
     - Pod
     - Preserves Pod name and UID for eligible image-only updates
     - ``RoleSet.spec.roles[].updateStrategy.type`` or
       ``StormService.spec.template.spec.roles[].updateStrategy.type``
   * - Role ``Recreate``
     - Pod
     - Replaces Pods during a role update; this is the default
     - ``RoleSet.spec.roles[].updateStrategy.type`` or
       ``StormService.spec.template.spec.roles[].updateStrategy.type``

For a StormService-managed rollout, configure the outer strategy on the
StormService and the Pod strategy on each role:

.. code-block:: yaml

   spec:
     updateStrategy:
       type: InPlaceUpdate       # Update existing RoleSets.
     template:
       spec:
         roles:
           - name: server
             updateStrategy:
               type: InPlaceIfPossible  # Try to preserve this role's Pods.

When managing a RoleSet directly, only the role-level strategy is required:

.. code-block:: yaml

   spec:
     roles:
       - name: server
         updateStrategy:
           type: InPlaceIfPossible

See the complete `StormService sample`_ and `RoleSet sample`_ in the AIBrix
repository.

.. _StormService sample: https://github.com/vllm-project/aibrix/blob/main/samples/orchestration/stormservice-inplace-update.yaml
.. _RoleSet sample: https://github.com/vllm-project/aibrix/blob/main/samples/orchestration/roleset-inplace-update.yaml

Pod in-place update eligibility
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

``InPlaceIfPossible`` updates existing Pods only when container images are the
only fields changed in the Pod template. The controller patches the requested
images, waits for the runtime container image IDs and readiness to reflect the
new images, and then completes the role revision. ``maxUnavailable`` limits how
many role Pods can be unavailable while the images restart; ``maxSurge`` still
controls any replacement Pods required by a fallback.

Changes to commands, arguments, environment variables, resources, volumes, or
other Pod template fields are not eligible. The controller emits a normal
``InPlaceFallback`` event and recreates the affected Pods instead of blocking
the rollout. Roles with ``podGroupSize > 1`` are managed through PodSet and also
fall back to recreation. Omitting the role strategy selects the default
``Recreate`` behavior.

Trigger and observe an image update
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

Apply the StormService sample and record the original Pod UID:

.. code-block:: shell

   kubectl apply -f samples/orchestration/stormservice-inplace-update.yaml
   kubectl get pods -l storm-service-name=stormservice-inplace-update -w
   kubectl get pods -l storm-service-name=stormservice-inplace-update \
     -o jsonpath='{.items[0].metadata.uid}{"\n"}'

Patch only the image field to trigger an eligible in-place update:

.. code-block:: shell

   kubectl patch stormservice stormservice-inplace-update --type=json -p='[
     {"op":"replace","path":"/spec/template/spec/roles/0/template/spec/containers/0/image",
      "value":"registry.k8s.io/e2e-test-images/agnhost:2.54"}
   ]'

Watch the image and compare the UID with the value recorded before the update:

.. code-block:: shell

   kubectl get pods -l storm-service-name=stormservice-inplace-update \
     -o custom-columns='NAME:.metadata.name,UID:.metadata.uid,IMAGE:.spec.containers[*].image'

While an update is in progress, the controller can set the Pod readiness
condition ``stormservice.orchestration.aibrix.ai/in-place-update-ready`` and the
annotation
``stormservice.orchestration.aibrix.ai/in-place-update-pending-reason``. Inspect
them with ``kubectl describe pod <pod-name>``. If the controller falls back to
recreation, inspect the reason on the RoleSet event:

.. code-block:: shell

   kubectl get events --field-selector reason=InPlaceFallback \
     --sort-by=.lastTimestamp

Rolling Strategy
----------------

StormService supports multiple rolling strategies to update roles within RoleSets. These strategies offer different ways to manage updates while maintaining service stability.

- **Sequential**: Roles are updated one at a time, in sequence.

- **Parallel**: All roles are updated simultaneously.

- **Interleaved**: Roles are updated in an interleaved manner.  This strategy partitions the update process for every Role into distinct steps. Each update step is coordinated across all roles to progress synchronously. In each operational cycle, the controller determines a global progress state based on the least-advanced role. It instructs roles that have not reached the current step to proceed with their updates, while skipping those that have.

Stateful vs Stateless
---------------------

This is determined by the `Stateful` field in both StormService and RoleSet specs. It defines whether the RoleSet uses a `StatefulRoleSyncer` or a `StatelessRoleSyncer`, which leads to different behaviors.

- **Stateful**: `StatefulRoleSyncer` treats each Pod as a unique, non-interchangeable entity, assigning a stable and unique index to each Pod. There are exactly *n* slots for *n* replicas, and updates are performed slot-by-slot in a controlled manner.

- **Stateless**: `StatelessRoleSyncer` treats all Pods as identical replicas. Any Pod can be replaced without affecting the overall application. Pods are managed as a collective pool, and scaling actions simply add or randomly remove Pods. Updates are performed at the pool level rather than targeting specific Pods.

Autoscaling
-----------

- **Replica Mode**: StormService enables the `/scale` subresource on its CRD. The scale unit is `RoleSet`. It involves extending the StormService status with a dynamic label selector and implementing the controller logic to ensure this selector is correctly populated, thereby allowing external autoscalers to manage StormService replicas effectively.
- **Pooled Mode**: In pooled mode, each role in the RoleSet is supposed to be independently scalable.

.. warning::
   Pooled mode autoscaling (independent scaling of each role) is not yet supported. See Issue `#1260 <https://github.com/vllm-project/aibrix/issues/1260>`_ for more details.
   As an alternative, you can adjust replicas of each role in the RoleSet spec.


ControllerRevision
------------------

In the Kubernetes ecosystem, `ControllerRevision` is a crucial resource object used to record the version information of controllers (such as Deployments, StatefulSets, etc.). In the AIBrix project, the `ControllerRevision` mechanism is employed to track the version changes of `StormService`, providing strong support for version management, rollback operations, and system state traceability.

- **Version Recording**: `ControllerRevision` stores the configuration information of a specific version of `StormService`, primarily the `spec` section. Whenever the configuration of `StormService` changes, the system creates a new `ControllerRevision` object and stores the changed configuration in a serialized form within this object. In this way, the system can clearly record the configuration states of `StormService` at different time points.
- **Version Rollback**: When it is necessary to restore `StormService` to a previous configuration state, a rollback operation can be performed based on the historical configuration information saved in `ControllerRevision`. By specifying the version number of the target `ControllerRevision`, the system can restore the configuration of `StormService` to the state corresponding to that version.
- **Historical Traceability**: `ControllerRevision` provides system operation and development personnel with the ability to trace historical configurations. By viewing different versions of `ControllerRevision` objects, one can understand the change history of the `StormService` configuration, which is helpful for issue troubleshooting and system auditing.

.. code::

    kubectl get controllerrevisions
    NAME                  CONTROLLER                                      REVISION   AGE
    llm-xpyd-69df6b87d8   stormservice.orchestration.aibrix.ai/llm-xpyd   1          73s
    llm-xpyd-75ddc56d8c   stormservice.orchestration.aibrix.ai/llm-xpyd   2          3s

.. seealso::

   :doc:`../features/pd-disaggregation`
       Prefill/decode disaggregation built on StormService.

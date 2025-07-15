.. _aibrix-stormservice:

===================
AIBrix StormService
===================

**StormService** is a specialized component designed to manage and orchestrate the lifecycle of inference containers in Prefill/Decode disaggregated architectures.

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

.. note::
   Pooled mode (independent scaling of each role) is not yet supported. See Issue `#1260 <https://github.com/vllm-project/aibrix/issues/1260>`_.

Following this layered design, updates to the spec propagate from the StormService to its RoleSets, and then to the individual roles. The reconciler at the StormService level synchronizes the status of RoleSets with the StormService spec (primarily the `Replicas` field), while the reconciler at the RoleSet level synchronizes the status of individual roles with the RoleSet spec.

StormService supports two operational modes at its level: **Rolling Update** and **Inplace Update**. At the RoleSet level, three update modes are supported: **Parallel**, **Sequential**, and **Interleaved**. These are explained in detail below.

Update Strategy
---------------
StormService supports multiple strategies to update the managed RoleSets:

- **Rolling Update**: Designed for replica mode. This strategy gradually replaces old RoleSets with new ones by creating RoleSets with the updated revision and deleting old ones. The update process respects both the `MaxUnavailable` and `MaxSurge` settings to ensure service availability. See `StormServiceReconciler.rollingUpdate()` in `stormservice_controller.go`_ file.

.. _stormservice_controller.go: https://github.com/vllm-project/aibrix/tree/main/pkg/controller/stormservice/stormservice_controller.go

- **Inplace Update**: Designed for pooled mode. Instead of deleting and creating RoleSets, this strategy propagates changes from the StormService to all associated RoleSets directly, without deleting and creating new RoleSets. The reconciler at RoleSet level will then sync its status according to the updated spec.

Rolling Strategy
----------------
- **Sequential**: Roles are updated one at a time, in sequence.

- **Parallel**: All roles are updated simultaneously.

- **Interleaved**: Roles are updated in an interleaved manner.  This strategy partitions the update process for every Role into distinct steps. Each update step is coordinated across all roles to progress synchronously. In each operational cycle, the controller determines a global progress state based on the least-advanced role. It instructs roles that have not reached the current step to proceed with their updates, while skipping those that have.

Stateful vs Stateless
---------------------

This is determined by the `Stateful` field in both StormService and RoleSet specs. It defines whether the RoleSet uses a `StatefulRoleSyncer` or a `StatelessRoleSyncer`, which leads to different behaviors.

- **Stateful**: `StatefulRoleSyncer` treats each Pod as a unique, non-interchangeable entity, assigning a stable and unique index to each Pod. There are exactly *n* slots for *n* replicas, and updates are performed slot-by-slot in a controlled manner.

- **Stateless**: `StatelessRoleSyncer` treats all Pods as identical replicas. Any Pod can be replaced without affecting the overall application. Pods are managed as a collective pool, and scaling actions simply add or randomly remove Pods. Updates are performed at the pool level rather than targeting specific Pods.

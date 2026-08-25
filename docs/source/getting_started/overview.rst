.. _overview:

===================
Overview & Concepts
===================

This page explains what AIBrix is, the vocabulary you need to read the rest of the
documentation, and which feature solves which problem. Read it before the
:doc:`quickstart` if you have not worked with AIBrix before.

What AIBrix is
==============

AIBrix is a **control plane for LLM inference on Kubernetes**. It is not an inference
engine. You bring the engine, such as vLLM or SGLang, and AIBrix provides the
infrastructure around it: request routing, autoscaling, KV cache management, model and
adapter lifecycle, and the observability to run all of it in production.

Concretely, AIBrix gives you:

- **LLM-aware routing.** A gateway that picks a target pod from live engine state, such as
  KV cache occupancy, queue depth and prefix cache hits, rather than round-robin.
- **Inference-shaped autoscaling.** Scaling on engine metrics instead of CPU, which is a
  poor proxy for GPU inference load.
- **Model and adapter lifecycle.** Declarative deployment of base models and high-density
  LoRA adapters through Kubernetes custom resources.
- **Distributed KV cache.** Cross-engine KV reuse to cut redundant prefill work.

If you are running a single replica of a single model, you do not need AIBrix. It earns its
place when you have many models, many replicas, heterogeneous GPUs, or cost and latency
targets to hold.

How the pieces fit together
===========================

AIBrix splits into a **control plane** and a **data plane**. Most confusion when reading the
rest of these docs comes from not knowing which plane a component belongs to.

.. mermaid::

   graph TD
       Client[Client] --> Envoy[Envoy Gateway]
       Envoy <-->|ext_proc| GW["Gateway plugins<br/>return a target-pod header"]
       Envoy -->|forwards the request| ENG

       subgraph Pod["Engine pod"]
           ENG["Inference engine<br/>vLLM / SGLang"]
           RT["AI Runtime sidecar"]
       end

       CTRL["Controllers"] -->|reconcile| Pod
       RT --- ENG
       GW -.reads live engine state.-> Pod

**Control plane**: the components that decide what *should* exist, such as how many
replicas, which adapters are loaded where, and which pods form a distributed inference
group. This is the Kubernetes controllers that reconcile the custom resources listed below,
and also the **AI Runtime**, a sidecar in each engine pod that downloads model weights, loads
and unloads LoRA adapters, and normalizes engine metrics into a consistent shape. See
:doc:`../designs/architecture` and :doc:`../features/runtime`.

**Data plane**: the request path. Envoy terminates the connection and AIBrix runs as an
Envoy ``ext_proc`` extension: for each request it resolves the model, selects a pod, and
returns a ``target-pod`` header. Envoy forwards the request to that pod, so traffic never
flows through the AIBrix plugin. Routing is decided per request, not per deployment. See
:doc:`../features/gateway-plugins`.

Core concepts
=============

AIBrix extends Kubernetes with custom resources. These are the nouns used throughout the
documentation.

.. list-table::
   :header-rows: 1
   :widths: 22 20 58

   * - Resource
     - API group
     - What it does
   * - ``PodAutoscaler``
     - ``autoscaling.aibrix.ai``
     - Scales a workload on inference metrics. Supports HPA, KPA, and APA strategies.
       See :doc:`../features/autoscaling/autoscaling`.
   * - ``ModelAdapter``
     - ``model.aibrix.ai``
     - Registers a LoRA adapter and manages loading it onto matching pods.
       See :doc:`../features/lora-dynamic-loading`.
   * - ``ModelClaim``
     - ``model.aibrix.ai``
     - *(experimental)* Lets several independently managed model engines share a warm GPU
       runtime pod, instead of one Deployment per model. See :doc:`../features/modelclaim`.
   * - ``KVCache``
     - ``orchestration.aibrix.ai``
     - Provisions a distributed KV cache backend for offloading and cross-engine reuse.
       See :doc:`../features/kvcache-offloading`.
   * - ``StormService``
     - ``orchestration.aibrix.ai``
     - Orchestrates multi-role serving topologies, including prefill/decode
       disaggregation. See :doc:`../designs/aibrix-stormservice`.
   * - ``RoleSet``
     - ``orchestration.aibrix.ai``
     - A collection of roles, each serving a specific function such as prefill or decode.
       ``StormService`` manages ``RoleSet`` replicas.
   * - ``PodSet``
     - ``orchestration.aibrix.ai``
     - Internal API used by the ``RoleSet`` controller to group pods that must be scheduled
       together. Not addressed directly by users.
   * - ``RayClusterFleet``
     - ``orchestration.aibrix.ai``
     - Manages multi-node inference on Ray for models too large for one node.
       See :doc:`../features/multi-node-inference`.

Two more terms appear constantly:

**Routing strategy**: the algorithm the gateway uses to pick a pod (``least-request``,
``prefix-cache``, ``pd``, and others). It is resolved per request from a header, a config
profile, or an environment default.

**Prefill / decode disaggregation**: splitting the two phases of inference across separate
pod pools so each can be sized and scaled independently.
See :doc:`../features/pd-disaggregation`.

Which feature solves which problem
==================================

If you arrived with a specific problem, start here.

.. list-table::
   :header-rows: 1
   :widths: 45 55

   * - Your problem
     - Start with
   * - "Requests pile up on some replicas while others idle"
     - :doc:`../features/gateway-plugins`: load-aware routing strategies
   * - "Repeated prompt prefixes are re-computed every request"
     - :doc:`../features/kvcache-offloading` and prefix-cache routing in
       :doc:`../features/gateway-plugins`
   * - "I serve many fine-tunes of one base model and GPUs are idle"
     - :doc:`../features/lora-dynamic-loading`: many adapters per pod
   * - "Replica count does not track real load"
     - :doc:`../features/autoscaling/autoscaling`: KPA/APA on engine metrics
   * - "Time to first token is too high under load"
     - :doc:`../features/pd-disaggregation`: separate prefill from decode
   * - "The model does not fit on one node"
     - :doc:`../features/multi-node-inference`
   * - "I have a mix of GPU types and want to control cost"
     - :doc:`../features/heterogeneous-gpu` *(experimental)*
   * - "I need to run large offline jobs, not live traffic"
     - :doc:`../features/batch-api`
   * - "I cannot see what the system is doing"
     - :doc:`../production/observability`
   * - "I need to measure whether any of this helped"
     - :doc:`../features/benchmark-and-generator` and :doc:`../features/brixbench`

.. seealso::

   :doc:`../designs/architecture`
       Component-level architecture, with the control plane and data plane broken out.

.. card:: What's next?
   :class-card: sd-border-1

   * :doc:`quickstart`: install AIBrix and serve your first model
   * :doc:`installation/installation`: production installation, per platform
   * :doc:`../designs/architecture`: how the components fit together in depth

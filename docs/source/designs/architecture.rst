.. _architecture:

===================
AIBrix Architecture
===================

An overview of AIBrix’s architecture. This guide introduces the AIBrix ecosystem and explains how its components integrate into the LLM inference lifecycle.

The following diagram gives an overview of the AIBrix Ecosystem and how it relates to the wider Kubernetes and LLM landscapes.

.. figure:: ../assets/images/aibrix-architecture-v1.jpeg
  :alt: aibrix-architecture-v1
  :width: 100%
  :align: center

AIBrix contains both :strong:`control plane` components and :strong:`data plane` components. The components of the control plane manage the registration of model metadata, autoscaling, model adapter registration, and enforce various types of policies. Data plane components provide configurable components for dispatching, scheduling, and serving inference requests, enabling flexible and high-performance model execution.

AIBrix Control Plane
--------------------

AIBrix currently provides several control plane components.

- :strong:`Model Adapter (Lora) controller`: enables multi-LoRA-per-pod deployments, significantly improving scalability and resource efficiency.
- :strong:`RayClusterFleet`: orchestrates multi-node inference, ensuring optimal performance across distributed environments.
- :strong:`LLM-Specific Autoscale`: enables real-time, second-level scaling, leveraging KV cache utilization and inference-aware metrics to dynamically optimize resource allocation
- :strong:`GPU Optimizer`: a profiler based optimizer which optimizes heterogeneous serving, dynamically adjusting allocations to maximize cost-efficiency while maintaining service guarantee
- :strong:`AI Engine Runtime`: a lightweight sidecar that offloads management tasks, enforces policies, and abstracts engine interactions.
- :strong:`Accelerator Diagnose Tools`: provides automated failure detection and mock-up testing to improve fault resilience.


AIBrix Data Plane
-----------------

AIBrix currently provides several data plane components:

- :strong:`Request Router`: serves as the central request dispatcher, enforcing fairness policies, rate control (TPM/RPM), and workload isolation.
- :strong:`Distributed KV Cache Runtime`: provides scalable, low-latency cache access across nodes. By enabling KV cache reuse, it reduces redundant computation and improves token generation efficiency.

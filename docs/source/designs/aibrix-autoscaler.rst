.. _aibrix_autoscaler:

=================
AIBrix Autoscaler
=================

Overview
--------

AIBrix provides a comprehensive autoscaling framework tailored for large language model (LLM) serving on Kubernetes (K8s).
It supports multiple scaling paradigms, integrating native Kubernetes components with enhancements specifically designed for LLM workloads.
This enables users to flexibly choose the most appropriate autoscaler for their workload patterns and cost-performance objectives.

Supported Autoscaling Mechanisms
--------------------------------

AIBrix currently implements **two distinct categories of autoscaling mechanisms**, each suited to different needs:

1. **Metrics-based Autoscaling**

   This family relies on runtime metrics (like CPU/GPU utilization, concurrent requests, or token throughput) to make scaling decisions.
   It reacts immediately to observed changes in system load.

   - **HPA (Horizontal Pod Autoscaler)**
     The standard Kubernetes HPA scales pod replicas based on CPU, memory, or custom metrics.
     Best suited for generic workloads with stable utilization patterns.

   - **KPA (Knative Pod Autoscaler)**
     Uses a dual-window approach (stable + panic) to enable rapid scale-up on sudden traffic spikes.
     AIBrix enhances this by fetching metrics internally instead of relying solely on Prometheus scraping.

   - **APA (Advanced Pod Autoscaler)**
     AIBrix’s own scaler designed specifically for LLM workloads.
     Adds fluctuation tolerance to reduce oscillations and is evolving to incorporate profiling-informed scaling.

2. **Optimizer-based Autoscaling**

   This is a fundamentally different approach that does *not* depend on online metric thresholds.
   Instead, it leverages **offline profiling data** and **optimization solvers** to proactively determine optimal resource allocation.
   It includes (1) LLM Request Monitoring and (2) GPU Optimizer. The following figure shows the overall architecture. First, the LLM Request Monitoring component is responsible for monitoring past inference requests and their request patterns. Second, the GPU Optimizer component is responsible for calculating the optimal GPU number recommendation based on the request patterns and sending the recommendation to the K8s KPA.

   .. figure:: ../assets/images/autoscaler/optimizer-based-podautoscaler.png
     :alt: optimizer-based-podautoscaler
     :width: 100%
     :align: center

   This method is particularly suited for:

   - Scheduled or tidal workloads with known patterns.
   - Heterogeneous GPU setups where optimal cost-performance balancing is required.
   - SLO-driven environments that benefit from planning resources before the load actually arrives.


    +--------------------------+---------------------------------------------------+---------------------------------------------+
    | Type                     | Description                                       | Best suited for                             |
    +==========================+===================================================+=============================================+
    | HPA                      | Standard Kubernetes HPA scaling pods based on     | Generic workloads with steady utilization.  |
    |                          | CPU/memory/custom metrics.                        |                                             |
    +--------------------------+---------------------------------------------------+---------------------------------------------+
    | KPA                      | Knative-style scaler with stable & panic windows  | Burst-prone, unpredictable traffic patterns.|
    +--------------------------+---------------------------------------------------+---------------------------------------------+
    | APA                      | AIBrix’s native scaler for LLM, with tolerance    | LLM workloads requiring stability,          |
    |                          | buffers to minimize thrashing.                    | fine-grained optimizations.                 |
    +--------------------------+---------------------------------------------------+---------------------------------------------+
    | Optimizer-based          | Plans scaling using offline profiling + solver,   | Scheduled workloads, multi-GPU cost/SLO     |
    |                          | without relying on live metric thresholds.        | optimization, proactive scaling needs.      |
    +--------------------------+---------------------------------------------------+---------------------------------------------+

Example Scenarios
-----------------

- **HPA**: Scaling a demonstration deployment purely on CPU utilization.
- **KPA**: Handling short-term spikes for a ``vllm``-backed Llama2-7b model with aggressive panic scaling.
- **APA**: Applying fluctuation tolerance to reduce scale oscillations for latency-sensitive LLM services.
- **Optimizer-based**: Using offline profiling results plus an optimization model to proactively scale GPUs ahead of a large scheduled campaign, minimizing costs while safeguarding SLOs.

Scaling Approaches
-------------------

These autoscaling mechanisms broadly cover two methodologies:

- **Reactive Autoscaling**
  Observes current system metrics and reacts by scaling resources up or down immediately.
  This includes all metrics-based approaches: HPA, KPA, APA.

- **Proactive Autoscaling**
  Makes decisions based on forecasts or offline-derived optimal plans, scaling ahead of time
  rather than waiting for metrics to cross thresholds.
  This is embodied by AIBrix’s optimizer-based approach, which uses workload profiles, cost models,
  and solvers to plan the best scaling actions.

Metrics
-------

The AIBrix autoscaling framework integrates tightly with the serving layer,
directly consuming metrics exposed by engines like ``vllm``.

Examples include:

- ``request_count``
- ``token_in_count`` / ``token_out_count``
- ``engine_latency_ms``
- ``kv_cache_size``
- ``engine_gpu_utilization``

For full details, see the `vLLM Metrics Documentation <https://docs.vllm.ai/en/stable/serving/metrics.html>`_.

Extending or Selecting Autoscalers
----------------------------------

AIBrix lets you declaratively select your desired autoscaler type in deployment specifications.
This makes it easy to experiment with different strategies or integrate new autoscaling plugins as your infrastructure evolves.

Future improvements include:

- Adding integer linear programming (ILP) based scheduling to enable even more advanced optimizer-driven planning.
- Combining proactive (optimizer-based) and reactive (metrics-based) mechanisms into hybrid autoscalers
  for maximum robustness and efficiency.

Next Steps
----------

.. note::

   If you would like, we can also add:

   - A pros/cons matrix comparing HPA, KPA, APA, and Optimizer-based approaches.
   - A sequence diagram for APA’s scaling loop vs. a schematic of the optimizer-driven workflow.
   - Example YAML specifications for each autoscaler type.

Let us know which would be most helpful!

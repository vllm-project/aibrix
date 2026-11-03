.. _architecture:

===================
AIBrix Architecture
===================

An overview of AIBrix’s architecture. This guide introduces the AIBrix ecosystem and explains how its components integrate into the LLM inference lifecycle.

AIBrix contains both :strong:`control plane` components and :strong:`data plane` components. The control plane decides what runs and where: it turns AIBrix custom resources into workloads, routes, caches and scaling decisions, and it hosts the management services around them. The data plane carries the traffic: inference requests and responses, and the KV cache data that engines share. Envoy forwards each request directly to an engine pod, so inference traffic does not pass through the controllers.

The following diagram shows how the two planes fit together on Kubernetes.

.. figure:: ../assets/images/aibrix-architecture-v2.png
  :alt: aibrix-architecture-v2
  :width: 100%
  :align: center

AIBrix Control Plane
--------------------

Most control plane logic runs in one controller manager, ``aibrix-controller-manager`` in the ``aibrix-system`` namespace. Each controller in the table below can be turned on or off with the manager's ``--controllers`` flag, for example ``--controllers=*,-model-claim-controller``. The manager also serves the validating and defaulting admission webhooks for the AIBrix custom resources. A mutating webhook on Deployments injects the AI Engine Runtime sidecar into Deployments annotated with ``model.aibrix.ai/sidecar-injection: "true"``.

.. list-table::
   :header-rows: 1
   :widths: 24 22 54

   * - Controller
     - Resources
     - What it does
   * - ``model-route-controller``
     - Deployment, ModelAdapter, RayClusterFleet, LeaderWorkerSet (when its CRD is installed)
     - Creates and removes the Gateway API ``HTTPRoute`` for each served model, so the gateway can reach new models without manual configuration.
   * - ``model-adapter-controller``
     - ModelAdapter
     - Registers a LoRA adapter on the matching pods through the AI Engine Runtime, configures service discovery and reports the adapter's status. This enables multi-LoRA-per-pod deployments.
   * - ``stormservice-controller``
     - StormService, RoleSet, PodSet
     - Manages multi-role inference services such as prefill/decode disaggregation. Updates flow from StormService to RoleSets to roles and Pods, with rolling and in-place updates and graceful Pod draining on rollout and scale-in.
   * - ``pod-autoscaler-controller``
     - PodAutoscaler
     - Scales Deployments, RayClusterFleets and StormService roles with the HPA, KPA or APA strategy, using inference-aware engine metrics. It also supports daily scheduled replica bounds and optional predictive scaling.
   * - ``distributed-inference-controller``
     - RayClusterFleet, RayClusterReplicaSet
     - Orchestrates multi-node inference on KubeRay. It starts only when the KubeRay CRD (``rayclusters.ray.io``) is installed.
   * - ``kv-cache-controller``
     - KVCache
     - Deploys and manages a distributed KV cache cluster with an InfiniStore, HPKV or Vineyard backend. For InfiniStore and HPKV, an optional watcher Pod records the cache-node membership in Redis.
   * - ``model-claim-controller`` (experimental)
     - ModelClaim
     - Serves several models from the same GPUs. It places each claim on a warm-pool Pod with room for it, starts the engine through the runtime agent, and can put idle models to sleep. The gateway asks it to wake a sleeping model when a request arrives.
   * - ``model-warmup-controller`` (experimental)
     - ModelWarmup
     - Runs a one-time, node-pinned Job on each selected node to pull images or download artifacts ahead of use. It does not start an engine.

Several control plane components run outside the controller manager:

- :strong:`AI Engine Runtime`: a lightweight sidecar next to each engine. It standardizes engine metrics, downloads model weights from HuggingFace, S3 or TOS, loads and unloads LoRA adapters, and starts, sleeps and wakes engine processes for ModelClaim. It does not proxy inference traffic.
- :strong:`GPU Optimizer`: a profiler-based optimizer for heterogeneous GPU serving. It adjusts allocations to maximize cost-efficiency while maintaining service guarantees.
- :strong:`Metadata service`: serves the OpenAI-compatible Files and Batch APIs and runs batches as Kubernetes Jobs.
- :strong:`Console`: a web UI with a Go gRPC and REST backend. It manages the model catalog, deployments, batch jobs, API keys, secrets and quotas, and offers a chat playground that proxies to the gateway.

AIBrix Data Plane
-----------------

AIBrix currently provides several data plane components:

- :strong:`AIBrix gateway`: built on Envoy Gateway, with the AIBrix gateway plugin attached as an external-processing (``ext_proc``) extension. It is the single entry point for inference requests. It routes from a local cache of ready pods and engine metrics, so most strategies need no live query to a pod. Gateway replicas share rate-limit counters, and optionally the prefix cache index, through Redis.
- :strong:`Inference engines`: the Pods that serve the model. AIBrix supports vLLM, SGLang, xLLM and TRT-LLM, selected with the ``model.aibrix.ai/engine`` label. In prefill/decode disaggregation, dedicated prefill and decode Pods split the two phases of a request.
- :strong:`Distributed KV Cache Runtime`: provides scalable, low-latency cache access across nodes. Engines connect through the AIBrix KV cache offloading framework, with an L1 DRAM cache and an optional L2 distributed cache. The L2 cache lets engines reuse each other's KV cache entries and avoid redundant computation.
- :strong:`KV event sync`: an optional feature in which engines publish KV cache events over ZMQ. The gateway builds a global prefix index from them, so prefix-cache routing follows the real state of each engine.

A request passes through the data plane as follows:

1. Envoy passes the request to the gateway plugin over ``ext_proc``.
2. The plugin identifies the model. When configured, it also checks the bearer token.
3. The plugin selects a ready pod with a routing strategy and returns it to Envoy in the ``target-pod`` header. Strategies fall into these groups:

   - load balancing, such as ``random``, ``least-request``, ``load-balance`` and ``power-of-two``
   - KV-cache aware, such as ``prefix-cache``
   - fairness, ``vtc-basic``
   - SLO-aware, such as ``slo``
   - session affinity, ``session-affinity``
   - prefill/decode disaggregation, ``pd``
   - delegation to an operator-run service, ``external``

4. Envoy forwards the request straight to that engine pod, which uses the KV cache layers when they are enabled.
5. The response returns through the plugin.

Where to go next
----------------

- :doc:`aibrix-gateway`: the gateway and its routing strategies in detail.
- :doc:`../features/batch-inference/batch-api`: asynchronous, OpenAI-compatible batch inference.
- :doc:`aibrix-stormservice`: StormService, RoleSet and prefill/decode orchestration.
- :doc:`aibrix-autoscaler`: the autoscaling mechanisms.
- :doc:`aibrix-engine-runtime`: the AI Engine Runtime sidecar.
- :doc:`aibrix-kvcache-offloading-framework`: KV cache offloading and cross-engine reuse.
- :doc:`../features/model-serving/model-claim` and :doc:`../features/model-serving/model-warmup`: ModelClaim and ModelWarmup.

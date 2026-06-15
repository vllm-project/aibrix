.. _multi-engine:

====================
Multi-Engine Support
====================

The AIBrix system now supports **multi-engine scheduling**, allowing developers to deploy and serve multiple engines (e.g., different LLMs or engine backends) under a single AIBrix instance. This enables flexible routing of incoming requests to different engines based on model name, scheduling policies, or performance characteristics.

Key Features
------------

- Support other engines beyond vLLM (e.g., SGLang, xLLM, TRT-LLM) in a single deployment.
- Configure engine by adding `model.aibrix.ai/engine` as label in the deployment YAML file.
- Support for interpreting metrics from different engine types.

Motivation
----------

Prior to this feature, AIBrix supports vLLM only while serving models. This limited flexibility in experimenting with or comparing different engines within the same workload or benchmarking scenario.

With multi-engine support, AIBrix enables:

- **Side-by-side comparisons** of latency, throughput, and behavior across engines.
- **Deployment flexibility**, supporting model sharding or migration strategies.
- **Metrics Adaptation** to interpret metrics from different engine types.

System Overview
---------------

Incoming requests will use the deployment label to determine correct ways of interpreting metrics retrieved from Prometheus API, which are later used by the `Router` to delegate execution. To configure a specific engine, apply the following labels in the deployment YAML file:

.. code-block:: yaml

    labels:
        model.aibrix.ai/name: deepseek-llm-7b-chat
        model.aibrix.ai/engine: "sglang"
        model.aibrix.ai/metric-port: "8000" # Configure this if Prometheus port is different from default port.
        model.aibrix.ai/port: "8000"

AIBrix will use the `model.aibrix.ai/engine` label to determine which engine to use for the deployment and search for correct format of metrics to retrieve from all metrics read from Prometheus.

Supported engine label values: ``vllm``, ``sglang``, ``xllm``, ``trtllm``.

Supported Metrics
-----------------

We only support limited number of metrics from different engines and we will continuously add more metrics -- for routing algorithms implemented through `routing policy API <https://github.com/vllm-project/aibrix/tree/main/pkg/plugins/gateway/algorithms>`_, make sure you use metrics that is supported by your target engine. For existing AIBrix routing policies, the router will fall back to default (i.e., random) policy if it fails to fetch a target metric.

.. list-table::
   :header-rows: 1
   :widths: 25 30 30 20 30

   * - Metric
     - vllm
     - sglang
     - xllm
     - trtllm
   * - num_requests_running
     - vllm:num_requests_running
     - sglang:num_running_reqs
     - N/A
     - N/A
   * - num_requests_waiting
     - vllm:num_requests_waiting
     - sglang:num_queue_reqs
     - N/A
     - N/A
   * - num_requests_swapped
     - vllm:num_requests_swapped
     - sglang:num_retracted_reqs
     - N/A
     - N/A
   * - avg_prompt_throughput_toks_per_s
     - vllm:avg_prompt_throughput_toks_per_s
     - N/A
     - N/A
     - N/A
   * - avg_generation_throughput_toks_per_s
     - vllm:avg_generation_throughput_toks_per_s
     - sglang:gen_throughput
     - N/A
     - N/A
   * - iteration_tokens_total
     - vllm:iteration_tokens_total
     - N/A
     - N/A
     - N/A
   * - time_to_first_token_seconds
     - vllm:time_to_first_token_seconds
     - sglang:time_to_first_token_seconds
     - N/A
     - trtllm_time_to_first_token_seconds
   * - time_per_output_token_seconds
     - vllm:time_per_output_token_seconds
     - sglang:inter_token_latency_seconds
     - N/A
     - trtllm_time_per_output_token_seconds
   * - e2e_request_latency_seconds
     - vllm:e2e_request_latency_seconds
     - sglang:e2e_request_latency_seconds
     - N/A
     - trtllm_e2e_request_latency_seconds
   * - request_queue_time_seconds
     - vllm:request_queue_time_seconds
     - N/A
     - N/A
     - trtllm_request_queue_time_seconds
   * - request_inference_time_seconds
     - vllm:request_inference_time_seconds
     - N/A
     - N/A
     - N/A
   * - request_decode_time_seconds
     - vllm:request_decode_time_seconds
     - N/A
     - N/A
     - N/A
   * - request_prefill_time_seconds
     - vllm:request_prefill_time_seconds
     - N/A
     - N/A
     - N/A
   * - gpu_cache_usage_perc
     - vllm:gpu_cache_usage_perc
     - sglang:token_usage [1]_
     - kv_cache_utilization
     - N/A
   * - kv_cache_usage_perc
     - vllm:kv_cache_usage_perc
     - sglang:token_usage [1]_
     - kv_cache_utilization
     - trtllm_kv_cache_utilization
   * - kv_cache_hit_rate
     - N/A
     - N/A
     - N/A
     - trtllm_kv_cache_hit_rate
   * - engine_utilization
     - N/A
     - N/A
     - engine_utilization
     - N/A
   * - cpu_cache_usage_perc
     - vllm:cpu_cache_usage_perc
     - N/A
     - N/A
     - N/A

.. [1] `https://github.com/sgl-project/sglang/issues/5979 <https://github.com/sgl-project/sglang/issues/5979>`_

TRT-LLM Quickstart
------------------

To use TRT-LLM as the inference engine, set the ``model.aibrix.ai/engine: trtllm`` label on your deployment. TRT-LLM must be configured to expose performance metrics by enabling ``return_perf_metrics: true`` and ``enable_iter_perf_stats: true`` in its server config.

Sample configurations are available at:

- `samples/quickstart/tensorrt/tensor-rt.yaml <https://github.com/vllm-project/aibrix/blob/main/samples/quickstart/tensorrt/tensor-rt.yaml>`_ — standard single-instance deployment
- `samples/quickstart/tensorrt/tensor-rt-pd.yaml <https://github.com/vllm-project/aibrix/blob/main/samples/quickstart/tensorrt/tensor-rt-pd.yaml>`_ — prefill/decode disaggregated deployment using StormService

Example deployment label configuration for TRT-LLM:

.. code-block:: yaml

    labels:
        model.aibrix.ai/name: Qwen3-8B
        model.aibrix.ai/engine: trtllm
        model.aibrix.ai/port: "8000"

TRT-LLM Limitations
--------------------

- **No queue-depth metrics**: TRT-LLM does not expose ``num_requests_running`` or ``num_requests_waiting``. Routing policies that rely on queue depth (e.g., least-request) will fall back to random routing.
- **Metrics require explicit config**: Performance metrics are only emitted when ``return_perf_metrics: true``, ``enable_iter_perf_stats: true``, and ``enable_iter_req_stats: true`` are set in the TRT-LLM server configuration.

Adding New Engines
------------------

To support a new engine or metrics type:

1. Adding engine type to metrics name mapping at `aibrix/pkg/metrics/metrics.go`.
2. Adding engine name to `model.aibrix.ai/engine` label in the deployment YAML file.

For more details, see the `cache_metrics.go` and `metrics.go` in:

- `aibrix/pkg/cache/cache_metrics.go <https://github.com/vllm-project/aibrix/blob/main/pkg/cache/cache_metrics.go>`_
- `aibrix/pkg/metrics/metrics.go <https://github.com/vllm-project/aibrix/blob/main/pkg/metrics/metrics.go>`_

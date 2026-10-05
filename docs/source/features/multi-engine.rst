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

How it works
------------

1. The AIBrix cache watches pods that carry ``model.aibrix.ai/name`` and reads
   ``model.aibrix.ai/engine`` from the same pod. Every metric AIBrix uses internally has one
   abstract name (for example ``num_requests_waiting``) and a per-engine mapping to the name the
   engine actually exports. The mapping lives in
   `pkg/metrics/metrics.go <https://github.com/vllm-project/aibrix/blob/main/pkg/metrics/metrics.go>`_
   and is reproduced in the table below.
2. Metrics are scraped from each pod on the port given by ``model.aibrix.ai/metric-port``, or
   port ``8000`` when the label is absent, at ``/metrics`` (``/prometheus/metrics`` for
   ``trtllm``). Requests are forwarded to ``model.aibrix.ai/port``.
3. Routing policies ask for abstract names. When the engine has no mapping for a metric a
   policy needs, most policies (``least-request``, ``least-kv-cache``, ``least-latency``,
   ``least-busy-time``, ``least-gpu-cache``, ``least-util``, ``throughput``) fall back to a
   random pod for that request. The SLO family (``slo-least-load``, ``slo-pack-load``,
   ``slo-least-load-pulling``) returns an error instead, which surfaces to the client as
   HTTP 503.

The optional :doc:`runtime` sidecar is a separate mechanism: it re-exports engine metrics in a
standardized shape on its own port. The engine label works without it.

Configuration reference
-----------------------

Everything is configured with pod labels. Set them on the pod template of the ``Deployment``
(or of the ``StormService`` role) rather than on the workload object itself.

.. list-table::
   :header-rows: 1
   :widths: 32 14 54

   * - Label
     - Default
     - Meaning
   * - ``model.aibrix.ai/name``
     - required
     - Model name used in requests and for pod discovery.
   * - ``model.aibrix.ai/engine``
     - ``vllm``
     - Engine type. One of ``vllm``, ``sglang``, ``xllm``, ``trtllm``. Selects the metric
       mapping.
   * - ``model.aibrix.ai/port``
     - ``8000``
     - Port the engine serves requests on.
   * - ``model.aibrix.ai/metric-port``
     - ``8000``
     - Port the engine exposes ``/metrics`` on, if different from the serving port.

Supported Metrics
-----------------

We only support limited number of metrics from different engines and we will continuously add more metrics. For routing algorithms implemented through `routing policy API <https://github.com/vllm-project/aibrix/tree/main/pkg/plugins/gateway/algorithms>`_, make sure you use metrics that is supported by your target engine. Most existing AIBrix routing policies fall back to the default (i.e., random) policy if they fail to fetch a target metric; see *How it works* above for the exceptions.

.. list-table::
   :header-rows: 1
   :widths: 26 22 22 15 15

   * - AIBrix metric
     - vLLM
     - SGLang
     - xLLM
     - TRT-LLM
   * - ``num_requests_running``
     - ``vllm:num_requests_running``
     - ``sglang:num_running_reqs``
     - N/A
     - N/A
   * - ``num_requests_waiting``
     - ``vllm:num_requests_waiting``
     - ``sglang:num_queue_reqs``
     - N/A
     - N/A
   * - ``num_requests_swapped``
     - ``vllm:num_requests_swapped``
     - ``sglang:num_retracted_reqs``
     - N/A
     - N/A
   * - ``engine_sleep_state``
     - ``vllm:engine_sleep_state``
     - N/A
     - N/A
     - N/A
   * - ``http_requests_total``
     - ``vllm:http_requests_total``
     - N/A
     - N/A
     - N/A
   * - ``num_preemptions_total``
     - ``vllm:num_preemptions_total``
     - N/A
     - N/A
     - N/A
   * - ``request_success_total``
     - ``vllm:num_requests_success_total``
     - ``sglang:num_requests_total``
     - N/A
     - ``trtllm_request_success_total``
   * - ``num_prefill_prealloc_queue_reqs``
     - N/A
     - ``sglang:num_prefill_prealloc_queue_reqs``
     - N/A
     - N/A
   * - ``num_decode_prealloc_queue_reqs``
     - N/A
     - ``sglang:num_decode_prealloc_queue_reqs``
     - N/A
     - N/A
   * - ``e2e_request_latency_seconds``
     - ``vllm:e2e_request_latency_seconds``
     - ``sglang:e2e_request_latency_seconds``
     - N/A
     - ``trtllm_e2e_request_latency_seconds``
   * - ``request_queue_time_seconds``
     - ``vllm:request_queue_time_seconds``
     - N/A
     - N/A
     - ``trtllm_request_queue_time_seconds``
   * - ``request_inference_time_seconds``
     - ``vllm:request_inference_time_seconds``
     - N/A
     - N/A
     - N/A
   * - ``per_stage_req_latency_seconds``
     - N/A
     - ``sglang:per_stage_req_latency_seconds``
     - N/A
     - N/A
   * - ``http_request_duration_seconds``
     - ``http_request_duration_seconds``
     - N/A
     - N/A
     - N/A
   * - ``http_request_duration_highr_seconds``
     - ``http_request_duration_highr_seconds``
     - N/A
     - N/A
     - N/A
   * - ``prompt_tokens_total``
     - ``vllm:prompt_tokens_total``
     - N/A
     - N/A
     - N/A
   * - ``request_prompt_tokens``
     - ``vllm:request_prompt_tokens``
     - N/A
     - N/A
     - N/A
   * - ``generation_tokens_total``
     - ``vllm:generation_tokens_total``
     - N/A
     - N/A
     - N/A
   * - ``request_generation_tokens``
     - ``vllm:request_generation_tokens``
     - N/A
     - N/A
     - N/A
   * - ``request_max_num_generation_tokens``
     - ``vllm:request_max_num_generation_tokens``
     - N/A
     - N/A
     - N/A
   * - ``iteration_tokens_total``
     - ``vllm:iteration_tokens_total``
     - N/A
     - N/A
     - N/A
   * - ``time_to_first_token_seconds``
     - ``vllm:time_to_first_token_seconds``
     - ``sglang:time_to_first_token_seconds``
     - N/A
     - ``trtllm_time_to_first_token_seconds``
   * - ``time_per_output_token_seconds``
     - ``vllm:time_per_output_token_seconds``
     - ``sglang:inter_token_latency_seconds``
     - N/A
     - ``trtllm_time_per_output_token_seconds``
   * - ``inter_token_latency_seconds``
     - ``vllm:inter_token_latency_seconds``
     - ``sglang:inter_token_latency_seconds``
     - N/A
     - ``trtllm_time_per_output_token_seconds``
   * - ``request_decode_time_seconds``
     - ``vllm:request_decode_time_seconds``
     - N/A
     - N/A
     - N/A
   * - ``request_prefill_time_seconds``
     - ``vllm:request_prefill_time_seconds``
     - N/A
     - N/A
     - N/A
   * - ``request_time_per_output_token_seconds``
     - ``vllm:request_time_per_output_token_seconds``
     - N/A
     - N/A
     - N/A
   * - ``gpu_cache_usage_perc``
     - ``vllm:gpu_cache_usage_perc``
     - ``sglang:token_usage``
     - ``kv_cache_utilization``
     - N/A
   * - ``engine_utilization``
     - N/A
     - N/A
     - ``engine_utilization``
     - N/A
   * - ``cpu_cache_usage_perc``
     - ``vllm:cpu_cache_usage_perc``
     - N/A
     - N/A
     - N/A
   * - ``kv_cache_usage_perc``
     - ``vllm:kv_cache_usage_perc``
     - ``sglang:token_usage``
     - ``kv_cache_utilization``
     - ``trtllm_kv_cache_utilization``
   * - ``kv_cache_hit_rate``
     - N/A
     - N/A
     - N/A
     - ``trtllm_kv_cache_hit_rate``
   * - ``prefix_cache_queries_total``
     - ``vllm:prefix_cache_queries_total``
     - N/A
     - N/A
     - N/A
   * - ``prefix_cache_hits_total``
     - ``vllm:prefix_cache_hits_total``
     - N/A
     - N/A
     - N/A
   * - ``external_prefix_cache_queries_total``
     - ``vllm:external_prefix_cache_queries_total``
     - N/A
     - N/A
     - N/A
   * - ``external_prefix_cache_hits_total``
     - ``vllm:external_prefix_cache_hits_total``
     - N/A
     - N/A
     - N/A
   * - ``nixl_xfer_time_seconds``
     - ``vllm:nixl_xfer_time_seconds``
     - N/A
     - N/A
     - N/A
   * - ``nixl_post_time_seconds``
     - ``vllm:nixl_post_time_seconds``
     - N/A
     - N/A
     - N/A
   * - ``nixl_bytes_transferred``
     - ``vllm:nixl_bytes_transferred``
     - N/A
     - N/A
     - N/A
   * - ``nixl_num_descriptors``
     - ``vllm:nixl_num_descriptors``
     - N/A
     - N/A
     - N/A
   * - ``nixl_num_failed_transfers_total``
     - ``vllm:nixl_num_failed_transfers``
     - N/A
     - N/A
     - N/A
   * - ``nixl_num_failed_notifications_total``
     - ``vllm:nixl_num_failed_notifications``
     - N/A
     - N/A
     - N/A
   * - ``avg_prompt_throughput_toks_per_s``
     - ``vllm:avg_prompt_throughput_toks_per_s``
     - N/A
     - N/A
     - N/A
   * - ``avg_generation_throughput_toks_per_s``
     - ``vllm:avg_generation_throughput_toks_per_s``
     - ``sglang:gen_throughput``
     - N/A
     - N/A
   * - ``max_lora``
     - ``vllm:lora_requests_info``
     - N/A
     - N/A
     - N/A
   * - ``running_lora_adapters``
     - ``vllm:lora_requests_info``
     - N/A
     - N/A
     - N/A
   * - ``waiting_lora_adapters``
     - ``vllm:lora_requests_info``
     - N/A
     - N/A
     - N/A

The SGLang entries for ``gpu_cache_usage_perc`` and ``kv_cache_usage_perc`` map to ``sglang:token_usage`` [1]_.

.. [1] `https://github.com/sgl-project/sglang/issues/5979 <https://github.com/sgl-project/sglang/issues/5979>`_

TRT-LLM Quickstart
------------------

To use TRT-LLM as the inference engine, set the ``model.aibrix.ai/engine: trtllm`` label on your deployment. TRT-LLM must be configured to expose performance metrics by enabling ``return_perf_metrics: true``, ``enable_iter_perf_stats: true``, and ``enable_iter_req_stats: true`` in its server config.

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

SGLang Decisions API
--------------------

SGLang serves ``POST /v1/decisions``, which answers typed ``choice``, ``score`` and ``yes_no``
questions about an input by scoring answer labels, without generating text. The gateway routes
it to the pods of the requested model, like the other model endpoints:

.. code-block:: bash

    curl http://${GATEWAY}/v1/decisions \
      -H "Authorization: Bearer ${API_KEY}" \
      -H "Content-Type: application/json" \
      -d '{
        "model": "deepseek-llm-7b-chat",
        "input": "The customer reports a double charge on their last invoice.",
        "questions": [{
          "id": "team",
          "type": "choice",
          "question": "Which team should handle this?",
          "options": [{"name": "billing"}, {"name": "support"}]
        }]
      }'

What differs from the other endpoints:

- ``model`` is required. SGLang treats it as optional and echoes a default, but the gateway needs
  it to select a pod, so a request without one is rejected with ``400``.
- ``input`` is required and may be a string, an object or an array. The gateway uses it, with
  objects and arrays compacted to JSON text, as the routing text for prefix-cache-aware routing.
  ``questions`` must be a non-empty array. The question schema itself is validated by SGLang.
- There is no streaming and no generation. Usage reports ``prompt_tokens`` and ``total_tokens``
  only, with ``completion_tokens`` at ``0``.
- ``images`` are forwarded as sent. The gateway buffers the whole request body, so large base64
  images count against the ``bufferLimit`` of the gateway's ``ClientTrafficPolicy`` (4 MiB by
  default). Raise it, together with ``AIBRIX_GRPC_MAX_MESSAGE_SIZE_BYTES``, if you send big images.

The gateway does not check the engine, so a decisions request for a model served by another engine
is forwarded and fails at the pod. Only route it to ``model.aibrix.ai/engine: sglang`` models.

SGLang System One API
---------------------

SGLang's ``POST /v1/systemone`` serves the same decisions in the System One request shape: a
``state`` and a map of ``noul``, ``choice`` and ``score`` questions keyed by your own ids. The
gateway routes it to the pods of the requested model, like ``/v1/decisions``:

.. code-block:: bash

    curl http://${GATEWAY}/v1/systemone \
      -H "Authorization: Bearer ${API_KEY}" \
      -H "Content-Type: application/json" \
      -d '{
        "model": "deepseek-llm-7b-chat",
        "state": "The customer reports a double charge on their last invoice.",
        "questions": {
          "team": {
            "type": "choice",
            "instructions": "Which team should handle this?",
            "criteria": {"billing": null, "support": "Bugs or integration problems"}
          },
          "urgent": {"type": "noul", "instructions": "The customer needs an answer today."}
        }
      }'

What differs from ``/v1/decisions``:

- ``model`` is required and must be a model the gateway serves. SGLang accepts any name, but the
  gateway selects a pod with it, so a model that is not deployed is rejected with ``400``
  ``model_not_found``. A client library that sends a default model name, such as the TypeSafe
  SDKs' ``jev-latest``, has to be told to send your model's name instead.
- ``state`` is required and may be a string, an object or an array. Unlike a decisions ``input`` it
  may be empty, because SGLang accepts an empty state. The gateway uses it, with objects and arrays
  compacted to JSON text, as the routing text for prefix-cache-aware routing: SGLang puts the state
  at the head of every question's prompt, so it is the part the questions share.
- ``questions`` must be a non-empty object keyed by your ids. An array, the ``/v1/decisions``
  shape, is rejected with ``400``. The question schema itself is validated by SGLang.
- There is no streaming and no generation. Usage reports ``input_tokens``, which counts the prompt
  of every question, so the state is counted once per question, and ``output_tokens`` at ``0``.
  There is no ``total_tokens``: the gateway counts input plus output as the total for rate-limit
  and request metrics, and forwards the usage block unchanged.
- ``images`` are forwarded as sent, with the same ``bufferLimit`` caveat as for ``/v1/decisions``.
- The TypeSafe SDKs time out after 10 seconds and retry. Each retry reaches the gateway as a
  separate request, so request counts and token usage include the retries.

The same engine caveat applies: a request for a model served by another engine is forwarded and
fails at the pod. SGLang also serves the route only for models with a Jinja chat template; its
decision-models documentation lists what it refuses.

Adding New Engines
------------------

To support a new engine or metrics type:

1. Adding engine type to metrics name mapping at `aibrix/pkg/metrics/metrics.go`.
2. Adding engine name to `model.aibrix.ai/engine` label in the deployment YAML file.

For more details, see the `cache_metrics.go` and `metrics.go` in:

- `aibrix/pkg/cache/cache_metrics.go <https://github.com/vllm-project/aibrix/blob/main/pkg/cache/cache_metrics.go>`_
- `aibrix/pkg/metrics/metrics.go <https://github.com/vllm-project/aibrix/blob/main/pkg/metrics/metrics.go>`_

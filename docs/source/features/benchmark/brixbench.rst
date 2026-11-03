.. _brixbench:

==========
Brixbench
==========

``brixbench`` is a Go test based benchmark harness for comparing
AIBrix-routed serving paths with direct-engine baselines. It uses
reproducible scenario YAML files, runs benchmark clients inside the target
Kubernetes cluster, and stores shared result artifacts for later analysis.

Use ``brixbench`` when you want to compare AIBrix gateway behavior, routing
configuration, release versions, source commits, or direct vLLM baselines
under repeatable benchmark scenarios.

For the older Python benchmark, dataset generator, and workload generator,
see :doc:`benchmark-and-generator`.


Requirements
------------

Before running benchmarks, make sure the local environment has:

- Go 1.22 or newer.
- ``kubectl`` on ``PATH``.
- Access to the target Kubernetes cluster through the active kubeconfig
  context.
- Permission to create, update, list, watch, and delete resources in the
  benchmark namespace.
- Optional: ``uv``, ``.venv``, and ``matplotlib`` for comparison figure
  generation.

Quick cluster preflight:

.. code-block:: bash

   kubectl config current-context
   kubectl auth can-i create pods -n brixbench-adhoc
   kubectl auth can-i delete namespace brixbench-adhoc
   kubectl get ns


Core Concepts
-------------

Each benchmark run is driven by a scenario file under
``brixbench/benchmark/testdata/scenarios/``. A scenario contains one or more
test cases. Each test case wires together three kinds of inputs:

- ``engine.manifest``: Kubernetes workload manifest for the model serving
  path.
- ``benchmark``: request workload YAML consumed by the benchmark client.
- ``gateway``: optional AIBrix gateway image, environment, or resource
  overrides.

The runner deploys the selected serving path, waits for readiness, runs the
benchmark client in the cluster, persists artifacts, and then tears down the
run-scoped benchmark namespace.

By default, the runner resets the benchmark namespace before each test case
and cleans it up after the case finishes. Set
``BENCHMARK_RESET_BEFORE_TEST=false`` or pass ``-benchmark.reset=false`` to
reuse the existing benchmark namespace at the start of a case. Set
``BENCHMARK_CLEANUP_AFTER_TEST=false`` or pass
``-benchmark.cleanup=false`` to leave resources in place after a test case
finishes.


Scenario Files
--------------

Scenario files define the benchmark cases to run and the deployment inputs
for each case.

Example:

.. code-block:: yaml

   Scenario: aibrix-batching-comparison
   Tests:
     - name: aibrix-v0.6.0-disaggregated
       provider: aibrix
       fullstack: false
       version: v0.6.0
       engine:
         type: vllm
         manifest: testdata/deployments/aibrix/models/pd-model.yaml
       benchmark: testdata/benchmarks/vllm-chat-smoke-pd.yaml
       gateway:
         env:
           ROUTING_ALGORITHM: pd
         resources:
           - testdata/deployments/aibrix/gateway/overrides/gateway-plugins-scaleup.yaml

Important scenario fields:

.. list-table::
   :header-rows: 1
   :widths: 28 72

   * - Field
     - Meaning
   * - ``provider: aibrix``
     - Use the AIBrix deployer.
   * - ``provider: null``
     - Use the plain vLLM direct baseline.
   * - ``fullstack: false``
     - Reuse an existing shared AIBrix control plane and update only
       run-scoped workloads and gateway overrides.
   * - ``fullstack: true``
     - Reinstall the AIBrix control plane from the checked-out workspace and
       Helm chart.
   * - ``version``, ``commit``, or ``localPath``
     - Select the AIBrix release, source revision, or local source tree used
       by the test case.
   * - ``engine.type``
     - Serving engine type. The supported benchmark paths currently use
       ``vllm``.
   * - ``engine.manifest``
     - Kubernetes model deployment manifest.
   * - ``benchmark``
     - Request workload configuration consumed by the benchmark client.


Benchmark Workload Config
-------------------------

The ``benchmark`` field points to request-side workload YAML. This is where
RPS, concurrency, arrival rate, dataset shape, and routing-specific benchmark
settings live. It is separate from ``engine.manifest``, which only describes
how to deploy the model workload.

Example benchmark config:

.. code-block:: yaml

   kind: vllm-bench
   execution: cluster
   image: aibrix-public-release-cn-beijing.cr.volces.com/aibrix/vllm-bench:v0.21.0-20260521
   namespace: brixbench-adhoc
   podName: vllm-bench-client
   modelHostPath: /data01/models
   rootHostPath: /root
   artifacts:
     resultFilename: bench_results.json
     logDir: testdata/logs
   vllmArgs:
     base-url: http://aibrix-gateway-plugins.aibrix-system.svc.cluster.local:10080
     endpoint: /v1/chat/completions
     backend: openai-chat
     model: qwen3-8b
     tokenizer: /data01/models/Qwen3-8B
     num-prompts: 200
     request-rate: 16
     concurrency: 32
     metric-percentiles: 50,90,95,99
     percentile-metrics: ttft,tpot,itl,e2el
     goodput: tpot:250
     routing-strategy: pd
     dataset-name: prefix_repetition
     prefix-repetition-num-prefixes: 20
     prefix-repetition-prefix-len: 6000
     prefix-repetition-suffix-len: 2000
     prefix-repetition-output-len: 1024

Key request-side fields:

- ``base-url``: gateway or direct service endpoint used by the benchmark
  client. The runner can override this with the resolved serving endpoint at
  runtime.
- ``endpoint``: OpenAI-compatible API path, such as
  ``/v1/chat/completions``.
- ``model``: served model name sent in benchmark requests.
- ``num-prompts``: total number of benchmark requests.
- ``request-rate``: target request arrival rate used by
  ``vllm bench serve``.
- ``concurrency``: maximum in-flight request concurrency.
- ``dataset-name``: workload generator, such as ``random`` or
  ``prefix_repetition``.
- ``goodput``: optional latency SLO expression used by ``vllm bench``
  reporting.
- ``routing-strategy``: optional routing label for routing-specific
  benchmark cases.
- ``prefix-repetition-*``: prefix-repetition dataset shape for shared-prefix
  workload tests.

Existing request workload examples:

- ``brixbench/benchmark/testdata/benchmarks/vllm-chat-smoke.yaml``
- ``brixbench/benchmark/testdata/benchmarks/vllm-chat-smoke-pd.yaml``
- ``brixbench/benchmark/testdata/benchmarks/routing/*.yaml``


Running Benchmarks
------------------

Run from the ``brixbench/`` directory.

Run the scenario selected by ``BENCHMARK_SCENARIO``:

.. code-block:: bash

   export BENCHMARK_SCENARIO=testdata/scenarios/aibrix-batching-comparison.yaml
   go test -v -count=1 -timeout 30m ./benchmark/...

Run one scenario directly:

.. code-block:: bash

   go test -v ./benchmark -run TestAIBrixBenchmarkSuite \
     -scenario testdata/scenarios/aibrix-hello-world.yaml -count=1

Keep resources after the run for inspection:

.. code-block:: bash

   BENCHMARK_CLEANUP_AFTER_TEST=false \
     go test -v ./benchmark -run TestAIBrixBenchmarkSuite \
     -scenario testdata/scenarios/aibrix-hello-world.yaml -count=1

Reuse existing benchmark namespace resources and keep them after the run:

.. code-block:: bash

   BENCHMARK_RESET_BEFORE_TEST=false BENCHMARK_CLEANUP_AFTER_TEST=false \
     go test -v ./benchmark -run TestAIBrixBenchmarkSuite \
     -scenario testdata/scenarios/aibrix-hello-world.yaml -count=1

Run a single test case:

.. code-block:: bash

   go test -v -count=1 -timeout 30m \
     -run 'TestAIBrixBenchmarkSuite/aibrix-v0.6.0-disaggregated$' \
     ./benchmark/...


Deployment Inputs
-----------------

For ``provider: aibrix``, the runner supports these source modes:

- ``version``: download release artifacts on demand into ``.tmp/releases/``.
- ``commit``: check out the requested AIBrix source revision and use the
  workspace chart and overlays.
- ``localPath``: stage a local AIBrix source tree and use its workspace chart
  and overlays.

The benchmark suite should not check in large generated AIBrix core or
dependency manifests. Version-based runs should use downloaded release
artifacts, and source-based runs should use the checked-out workspace and Helm
chart path.


Metrics Export
--------------

Metrics export is disabled by default.

- Without ``BENCHMARK_PUSHGATEWAY_URL``, the runner skips external metric
  export.
- With ``BENCHMARK_PUSHGATEWAY_URL``, the runner enables the Pushgateway
  exporter.
- The current Pushgateway exporter returns an explicit not-implemented error
  until real export behavior is implemented.


Artifacts
---------

Run artifacts are written under:

.. code-block:: text

   brixbench/benchmark/testdata/logs/<timestamp>-UTC-<scenario>/

Typical artifacts include:

- ``metadata.json``
- ``summary.json``
- ``summary.csv``
- per-case ``bench_results.json``
- per-case ``vllm-bench-client.log``
- per-case ``vllm-bench-pod.yaml``
- optional comparison figures under ``figures/``


Optional Figure Generation
--------------------------

If ``brixbench/.venv/bin/python`` and ``matplotlib`` are available, the Go
runner generates figures automatically after writing the scenario summary.

Manual setup:

.. code-block:: bash

   uv venv .venv
   source .venv/bin/activate
   uv pip install matplotlib

Manual regeneration:

.. code-block:: bash

   python benchmark/plot_summary_vllm_bench.py \
     benchmark/testdata/logs/<timestamp>-UTC-<scenario>/summary.csv


Troubleshooting
---------------

If a run stalls before benchmark execution, check pending model pods:

.. code-block:: bash

   kubectl get pods -n brixbench-adhoc
   kubectl describe pod -n brixbench-adhoc <pending-pod-name>

Common causes include insufficient GPU capacity or a missing shared AIBrix
control plane when using ``fullstack: false``.

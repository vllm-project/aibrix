# AIBrix Benchmark Suite

`brixbench` is a Go test based benchmark harness for comparing AIBrix-routed
and direct-engine serving paths with reproducible scenario YAML and shared
result artifacts.

## Requirements

- Go 1.22 or newer
- `kubectl` on `PATH`
- Access to the target Kubernetes cluster through the active kubeconfig context
- Permission to create, update, list, watch, and delete resources in the benchmark namespace
- Optional: `uv`, `.venv`, and `matplotlib` for comparison figure generation

Quick cluster preflight:

```bash
kubectl config current-context
kubectl auth can-i create pods -n brixbench-adhoc
kubectl auth can-i delete namespace brixbench-adhoc
kubectl auth can-i create pods -n brixbench-dynamo
kubectl auth can-i delete namespace brixbench-dynamo
kubectl get ns
```

## Core Concepts

Each benchmark run is driven by a scenario file under `benchmark/testdata/scenarios/`.
Each scenario test case wires together three kinds of inputs:

- `engine.manifest`: Kubernetes workload manifest for the model serving path.
- `benchmark`: Request workload YAML consumed by the benchmark client.
- `gateway`: Optional AIBrix gateway image, environment, or resource overrides.

The runner deploys the selected serving path, waits for readiness, runs the
benchmark client in the cluster, persists artifacts, and then tears down the
run-scoped benchmark namespace. AIBrix and direct vLLM runs use
`brixbench-adhoc`; Dynamo runs use `brixbench-dynamo`.

By default, the runner resets the benchmark namespace before each test case and
cleans it up after the case finishes. Set `BENCHMARK_RESET_BEFORE_TEST=false`
or pass `-benchmark.reset=false` to reuse the existing benchmark namespace at
the start of a case. Set `BENCHMARK_CLEANUP_AFTER_TEST=false` or pass
`-benchmark.cleanup=false` to leave resources in place after a test case
finishes. Use both options together when you want to preserve an existing
deployment across a benchmark run.

## Scenario Example

```yaml
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
```

Important scenario fields:

- `provider: aibrix`: use the AIBrix deployer.
- `provider: null`: use the plain vLLM direct baseline.
- `fullstack: false`: reuse an existing shared AIBrix control plane and update only run-scoped workloads and gateway overrides.
- `fullstack: true`: reinstall the AIBrix control plane from the checked-out workspace and Helm chart.
- `version`, `commit`, or `localPath`: choose the AIBrix source or release input.
- `engine.type`: currently `vllm` for supported benchmark paths.
- `engine.manifest`: model deployment manifest.
- `benchmark`: request workload config.

## Benchmark Workload Config

The `benchmark` field points to the request-side workload YAML. This is where
RPS, concurrency, arrival rate, dataset shape, and routing-specific benchmark
settings live. It is separate from `engine.manifest`, which only describes how
to deploy the model workload.

Example benchmark config:

```yaml
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
```

Key request-side fields:

- `base-url`: Gateway or direct service endpoint used by the benchmark client. The runner can override this with the resolved serving endpoint at runtime.
- `endpoint`: OpenAI-compatible API path, such as `/v1/chat/completions`.
- `model`: Served model name sent in benchmark requests.
- `num-prompts`: Total number of benchmark requests.
- `request-rate`: Target request arrival rate used by `vllm bench serve`.
- `concurrency`: Maximum in-flight request concurrency.
- `dataset-name`: Workload generator, such as `random` or `prefix_repetition`.
- `goodput`: Optional latency SLO expression used by `vllm bench` reporting.
- `routing-strategy`: Optional routing label for routing-specific benchmark cases.
- `prefix-repetition-*`: Prefix-repetition dataset shape for shared-prefix workload tests.

Existing request workload examples:

- `benchmark/testdata/benchmarks/vllm-chat-smoke.yaml`
- `benchmark/testdata/benchmarks/vllm-chat-smoke-pd.yaml`
- `benchmark/testdata/benchmarks/routing/*.yaml`

## Running Benchmarks

Run from the `brixbench/` directory:

```bash
export BENCHMARK_SCENARIO=testdata/scenarios/aibrix-batching-comparison.yaml
go test -v -count=1 -timeout 30m ./benchmark/...
```

Run one scenario directly:

```bash
go test -v ./benchmark -run TestAIBrixBenchmarkSuite -scenario testdata/scenarios/aibrix-hello-world.yaml -count=1
```

Keep resources after the run for inspection:

```bash
BENCHMARK_CLEANUP_AFTER_TEST=false go test -v ./benchmark -run TestAIBrixBenchmarkSuite -scenario testdata/scenarios/aibrix-hello-world.yaml -count=1
```

Reuse existing benchmark namespace resources and keep them after the run:

```bash
BENCHMARK_RESET_BEFORE_TEST=false BENCHMARK_CLEANUP_AFTER_TEST=false go test -v ./benchmark -run TestAIBrixBenchmarkSuite -scenario testdata/scenarios/aibrix-hello-world.yaml -count=1
```

Run a single test case:

```bash
go test -v -count=1 -timeout 30m -run 'TestAIBrixBenchmarkSuite/aibrix-v0.6.0-disaggregated$' ./benchmark/...
```

## Deployment Inputs

For `provider: aibrix`, the runner supports these source modes:

- `version`: download release artifacts on demand into `.tmp/releases/`.
- `commit`: check out the requested AIBrix source revision and use the workspace chart/overlays.
- `localPath`: stage a local AIBrix source tree and use its workspace chart/overlays.

The benchmark suite should not check in large generated AIBrix core/dependency
manifests. Version-based runs should use downloaded release artifacts, and
source-based runs should use the checked-out workspace and Helm chart path.

For `provider: dynamo`, the runner currently supports:

- `version`: stable Dynamo release tag input only, normalized as `vMAJOR.MINOR.PATCH`
- `engine.manifest`: user-provided `DynamoGraphDeployment` manifest

`provider: dynamo` rejects `commit`, `localPath`, and `controlplane`.

## Dynamo Deployment

`provider: dynamo` is a release-based path:

1. validate the requested release tag against `https://github.com/ai-dynamo/dynamo.git`
2. check out `.tmp/dynamo/<version>`
3. verify the tag commit matches upstream `release/<version>` branch head
4. prepare Helm dependency repositories and run `helm dependency build --skip-refresh` for `deploy/helm/charts/platform`
5. prepare common reference-runtime prerequisites in `brixbench-dynamo`: optional registry secret preflight/create, `models-pvc`, `mpi-run-ssh-secret`
6. install the Dynamo platform chart into `brixbench-dynamo` with operator namespace restriction enabled and default reference-style platform values
7. apply the user-provided `DynamoGraphDeployment`
8. best-effort label/apply Dynamo PodMonitors when the monitoring CRD exists
9. wait for Frontend service, component pods, and OpenAI-compatible model/inference readiness when the model name can be inferred

Dynamo platform is installed with Helm. The serving/runtime image is selected by
the `DynamoGraphDeployment` manifest, not by the platform chart alone.
The provider writes default platform values matching the checked reference
environment, including disabling NATS JetStream PVCs for clusters without a
default StorageClass. `platform.valuesFile` is still supported as an explicit
override, but the default `dynamo-hello-world` scenario does not require it.

Example scenario:

```yaml
Scenario: dynamo-hello-world
Tests:
  - name: dynamo-v1.2.1-qwen3-8b-round-robin-1p1d-tp2
    provider: dynamo
    version: 1.2.1
    engine:
      type: vllm
      manifest: testdata/deployments/dynamo/qwen3-8b-round-robin-1p1d-tp2.yaml
    benchmark: testdata/benchmarks/vllm-chat-smoke-qwen3-8b.yaml
```

Run it with:

```bash
go test -v ./benchmark -run TestAIBrixBenchmarkSuite \
  -scenario testdata/scenarios/dynamo-hello-world.yaml \
  -count=1 -timeout 90m
```

### Dynamo Prerequisites On The Current Server

These requirements apply to the machine where `go test`, `git`, `helm`, and
`kubectl` run. They are separate from cluster-side scheduling or storage
requirements.

- `git` on `PATH`
- `helm` 3.x on `PATH`
- `kubectl` on `PATH`
- writable workspace cache under `.tmp/dynamo/`
- writable isolated Helm state under `.tmp/dynamo-helm/`
- active kubeconfig context that can reach the target cluster
- outbound HTTPS access to:
  - `github.com` for Dynamo tag lookup and release checkout
  - `nats-io.github.io` for the NATS Helm repo
  - `charts.bitnami.com` for the Bitnami Etcd chart
  - `ghcr.io` for OCI-hosted dependencies such as `kai-scheduler` and `grove`

Why this matters:

- `helm dependency build` runs on the current server, not inside the cluster
- if the current server cannot fetch remote Helm repo `index.yaml` files or OCI
  artifacts, deployment fails before any Dynamo workload is created in the
  cluster

Quick local preflight:

```bash
git ls-remote --tags --refs https://github.com/ai-dynamo/dynamo.git refs/tags/v1.2.1
helm version
kubectl config current-context
```

### Dynamo Prerequisites In The Cluster

These requirements apply after the platform chart has been downloaded and Helm
starts creating resources in Kubernetes.

- permission to create, list, watch, patch, and delete resources in
  `brixbench-dynamo`
- permission to create the benchmark model PV/PVC and release the PV claimRef
  during teardown
- cluster capacity for Dynamo platform dependencies such as NATS
- image pull access for platform and engine images referenced by the chart and
  `engine.manifest`
- for private registries only: pre-created `aibrix-registry-secret`, or
  `DYNAMO_REGISTRY_USERNAME` and `DYNAMO_REGISTRY_PASSWORD` so the provider can
  create it without storing credentials in the repository. If the images are
  already pullable without a secret, the provider continues without
  `imagePullSecrets`.
- any cluster-specific networking, RDMA, nodeSelector, and runtime image settings
  required by the user-provided `DynamoGraphDeployment`

Typical failure split:

- `helm dependency build` fails:
  - current server to remote Helm repo or OCI registry problem
- `helm upgrade --install ... --wait` fails:
  - cluster-side scheduling, storage, image pull, or readiness problem

## Metrics Export

Metrics export is disabled by default.

- Without `BENCHMARK_PUSHGATEWAY_URL`, the runner skips external metric export.
- With `BENCHMARK_PUSHGATEWAY_URL`, the runner enables the Pushgateway exporter.
- The current Pushgateway exporter returns an explicit not-implemented error until real export behavior is implemented.

## Workload PodMonitoring

The runner creates Prometheus Operator `PodMonitor` resources for benchmark
workloads after the serving deployment becomes ready. This is separate from the
benchmark result Pushgateway export above: PodMonitoring lets Prometheus or VMP
scrape serving workload `/metrics` endpoints for Grafana dashboards.

PodMonitoring is enabled by default. Disable it with either option:

```bash
BENCHMARK_POD_MONITORING=false go test -v ./benchmark -run TestAIBrixBenchmarkSuite -count=1
go test -v ./benchmark -run TestAIBrixBenchmarkSuite -benchmark.pod-monitoring=false -count=1
```

By default, missing PodMonitor CRDs or apply permission errors are logged as
warnings so benchmark execution can continue. Use strict mode when dashboard
metric collection is required for validation:

```bash
BENCHMARK_POD_MONITORING_STRICT=true go test -v ./benchmark -run TestAIBrixBenchmarkSuite -count=1
```

Created PodMonitors are labeled with `volcengine.vmp=true` and
`brixbench.aibrix.ai/managed=true`. For the integrated benchmark dashboard,
select datasource `AIBrix_Benchmark` and keep `vllm_job` as `All`; plain vLLM
and AIBrix StormService vLLM metrics are relabeled to `job=brixbench-vllm`.

Custom engine manifests must expose the metrics-serving container port with the
name expected by the generated PodMonitor. For vLLM and AIBrix vLLM profiles,
brixbench generates PodMonitors with `podMetricsEndpoints.port: http`, so the
serving container must declare a named port:

```yaml
ports:
- name: http
  containerPort: 8000
```

Without this named container port, the PodMonitor resource may be created
successfully, but Prometheus Operator may not discover a valid scrape target.

Useful checks:

```bash
kubectl get podmonitor -n brixbench-adhoc -l brixbench.aibrix.ai/managed=true
kubectl get podmonitor brixbench-aibrix-vllm-metrics -n brixbench-adhoc -o yaml
```

## Artifacts

Run artifacts are written under:

```text
benchmark/testdata/logs/<timestamp>-UTC-<scenario>/
```

Typical artifacts include:

- `metadata.json`
- `summary.json`
- `summary.csv`
- per-case `bench_results.json`
- per-case `vllm-bench-client.log`
- per-case `vllm-bench-pod.yaml`
- optional comparison figures under `figures/`

## Optional Figure Generation

If `brixbench/.venv/bin/python` and `matplotlib` are available, the Go runner
generates figures automatically after writing the scenario summary.

Manual setup:

```bash
uv venv .venv
source .venv/bin/activate
uv pip install matplotlib
```

If a run logs that figure generation was skipped because
`.venv/bin/python` is not available, recreate the local plotting environment:

```bash
rm -rf .venv
uv venv .venv
uv pip install matplotlib
```

Manual regeneration:

```bash
.venv/bin/python benchmark/plot_summary_vllm_bench.py \
  benchmark/testdata/logs/<timestamp>-UTC-<scenario>/summary.csv
```

## Troubleshooting

If a run stalls before benchmark execution, check pending model pods:

```bash
kubectl get pods -n brixbench-adhoc
kubectl describe pod -n brixbench-adhoc <pending-pod-name>
kubectl get pods -n brixbench-dynamo
kubectl describe pod -n brixbench-dynamo <pending-pod-name>
```

Common causes include insufficient GPU capacity or a missing shared AIBrix
control plane when using `fullstack: false`.

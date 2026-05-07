# AIBrix Benchmark Suite - Walkthrough Guide

## 🚀 Quick Start

AIBrix Benchmark Suite is a CI-friendly benchmark tool for comparing the performance of LLM inference platforms (AIBrix, LLM-d, Dynamo, etc.).

## Requirements

### Runtime Dependencies

- Go 1.22 or newer
- `kubectl`
- `uv` for Python environment management
- A Python virtual environment at `.venv` for optional plotting helpers
- `matplotlib` installed inside that virtual environment when generating summary charts

### Python Environment for Plotting

The project plotting helper is expected to run from the repository-local virtual environment at `.venv`.

Recommended setup flow:

```bash
cd brixbench

# 1) Create the local virtual environment if it does not exist yet
uv venv .venv

# 2) Activate it
source .venv/bin/activate

# 3) Install plotting dependency into that environment
uv pip install matplotlib
```

If `.venv` already exists, you only need:

```bash
cd brixbench
source .venv/bin/activate
uv pip install matplotlib
```

If your environment requires a proxy for external downloads, export the standard proxy environment variables for your shell before running the `uv` commands above.

### Hello World (Smoke E2E)

This is the fastest way to validate the current AIBrix v0.6 Go E2E flow
(deploy -> sanity -> vllm-bench -> artifact capture) for both random and PD
routing modes.

```bash
cd brixbench/benchmark

export BENCHMARK_SCENARIO=testdata/scenarios/aibrix-batching-comparison.yaml

# Run the full smoke suite (nondisaggregated random + disaggregated PD)
go test -count=1 -timeout 30m -v ./...

# Or run a single case
go test -count=1 -timeout 30m -run 'TestAIBrixBenchmarkSuite/aibrix-v0.6.0-nondisaggregated$' -v ./...
go test -count=1 -timeout 30m -run 'TestAIBrixBenchmarkSuite/aibrix-v0.6.0-disaggregated$' -v ./...
```

Current smoke scenario details:

- `aibrix-v0.6.0-nondisaggregated` uses `vllm-chat-smoke-random.yaml`
- `aibrix-v0.6.0-disaggregated` uses `vllm-chat-smoke-pd.yaml`
- Both cases apply the gateway override
  `testdata/deployments/aibrix/gateway/overrides/gateway-plugins-scaleup.yaml`
  to increase `aibrix-gateway-plugins` capacity during benchmark execution

### Commit-to-Commit Comparison

You can also compare two AIBrix source commits in the same scenario run.

- If a test uses `commit`, the runner resolves that ref and checks out the exact commit in detached mode.
- If a test uses `version`, the runner also resolves the version tag to a real commit before continuing.
- In both cases, the checked-out workspace is used as the source of truth for Helm charts and overlays.

```bash
cd brixbench/benchmark

export BENCHMARK_SCENARIO=testdata/scenarios/aibrix-recent-commits.yaml

# Run both recent commits in one suite run
go test -count=1 -timeout 90m -v ./...

# Or run a single commit case
go test -count=1 -timeout 90m -run 'TestAIBrixBenchmarkSuite/aibrix-main-d41653b-disaggregated$' -v ./...
go test -count=1 -timeout 90m -run 'TestAIBrixBenchmarkSuite/aibrix-main-d5b6d5b-disaggregated$' -v ./...
```

Example scenario:

```yaml
Scenario: aibrix-recent-commits
Tests:
  - name: aibrix-main-d41653b-disaggregated
    provider: aibrix
    fullstack: false
    commit: d41653b26cf1257157e34ff35ee5f028644473a8
    engine:
      type: vllm
      manifest: testdata/deployments/aibrix/models/pd-model.yaml
    benchmark: testdata/benchmarks/vllm-chat-smoke-pd.yaml
    gateway:
      env:
        ROUTING_ALGORITHM: pd
      resources:
        - testdata/deployments/aibrix/gateway/overrides/gateway-plugins-scaleup.yaml

  - name: aibrix-main-d5b6d5b-disaggregated
    provider: aibrix
    fullstack: false
    commit: d5b6d5b8b151e05025310aee13969a119b41c756
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

### Scenario Options

The most important AIBrix scenario fields are:

- `provider: aibrix`: use the AIBrix deployer.
- `fullstack: false`: reuse the existing shared control plane and only update the benchmark run namespace plus gateway runtime overrides.
- `fullstack: true`: run the full control-plane reinstall flow from the checked-out AIBrix workspace.
- `vkeDev: true`: apply local `vke-dev` overlays after the base full-stack install. This is only valid when `fullstack: true`.
- `version`, `commit`, or `localPath`: choose exactly one source selection mode for AIBrix workspace resolution.

Current `fullstack: true` behavior follows this order:

1. Remove the previous Helm release and old cluster-scoped leftovers.
2. Reinstall VKE dependencies from `config/overlays/vke/dependency`.
3. Apply CRDs from `dist/chart/crds`.
4. Install the core chart from `dist/chart` with `dist/chart/vke.yaml`.
5. Optionally apply `config/overlays/vke-dev/{manager,gpu-optimizer,gateway-plugin}` when `vkeDev: true`.

Recommended usage:

- Use `fullstack: false` for normal benchmark iteration in a shared cluster.
- Use `fullstack: true` only when the shared control plane is missing, broken, or when a clean reinstall is the goal.
- Use `vkeDev: true` only when you intentionally want local dev overlays on top of the base full-stack install.

Important cautions:

- `vkeDev: true` with `fullstack: false` is rejected during scenario validation.
- `localPath` is supported only for `provider: aibrix`, and it cannot be combined with `version` or `commit`.
- `fullstack: true` is heavier and more disruptive than the gateway-only path because it reinstalls cluster-wide AIBrix components.
- `fullstack: false` does not rebuild the whole control plane. It assumes the shared `aibrix-system` stack already exists.

Minimal full-stack example:

```yaml
Scenario: aibrix-fullstack-example
Tests:
  - name: aibrix-v0.6.0-fullstack
    provider: aibrix
    fullstack: true
    vkeDev: true
    version: v0.6.0
    engine:
      type: vllm
      manifest: testdata/deployments/aibrix/models/model.yaml
    benchmark: testdata/benchmarks/vllm-chat-smoke.yaml
```

### Step 1: Select a Scenario

Choose a scenario from `testdata/scenarios/` or create your own YAML file.

```yaml
# Example: current smoke comparison (aibrix-batching-comparison.yaml)
Scenario: aibrix-batching-comparison
Tests:
  - name: aibrix-v0.6.0-nondisaggregated
    provider: aibrix
    fullstack: false
    version: v0.6.0
    engine:
      type: vllm
      manifest: testdata/deployments/aibrix/models/model.yaml
    benchmark: testdata/benchmarks/vllm-chat-smoke-random.yaml
    gateway:
      env:
        ROUTING_ALGORITHM: random
      resources:
        - testdata/deployments/aibrix/gateway/overrides/gateway-plugins-scaleup.yaml

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

### Step 2: Run Locally

```bash
# Set the scenario path via environment variable and run
export BENCHMARK_SCENARIO=testdata/scenarios/aibrix-batching-comparison.yaml
go test -count=1 -timeout 30m -v ./benchmark/...
```

### Step 3: Check Results

Once the test completes, you'll see metrics like:

```
TTFT (Time to First Token): 120.5
TPOT (Time Per Output Token): 45.2
```

Benchmark artifacts are stored under a top-level run directory: `benchmark/testdata/logs/<timestamp>-PT-<scenario>/`.

Example layout:

```text
benchmark/testdata/logs/
  20260410-120000-PT-aibrix-version-comparison/
    metadata.json
    summary.json
    summary.csv
    figures/
    aibrix-v0.5.0-disaggregated/
      bench_results.json
      vllm-bench-client.log
      vllm-bench-pod.yaml
      sanity-models.txt
      sanity-chat.txt
    aibrix-v0.6.0-disaggregated/
      ...
```

- `metadata.json` stores the run ID, scenario name, and generation timestamp.
- `summary.json` stores the scenario-level aggregated results for all test cases.
- `summary.csv` stores a flat comparison table with key metrics and status per test case.
- If `brixbench/.venv/bin/python` and `matplotlib` are available, the Go runner also generates `figures/*.png` automatically right after writing the summary files.
- Each test case keeps its own raw benchmark artifacts in its own subdirectory inside the same run directory.

### Step 5: Generate Comparison Figures

If the repository-local `.venv` already exists and includes `matplotlib`, `go test` generates the figures automatically after the scenario summary is written.

You can also regenerate them manually:

The helper script `benchmark/plot_summary_vllm_bench.py` reads a run directory, `summary.json`, or `summary.csv`, then creates a `figures/` directory next to the summary files for `vllm-bench` scenarios.

```bash
cd brixbench

# If needed, export proxy variables for your shell before installing packages
# export HTTPS_PROXY=http://proxy.example:port
# export HTTP_PROXY=http://proxy.example:port
# export NO_PROXY=127.0.0.1,localhost,.cluster.local

# Create the repository-local venv if it does not exist yet
uv venv .venv

# Activate it
source .venv/bin/activate

# Install plotting dependency if it is not already present
uv pip install matplotlib

# Use either the scenario directory, summary.json, or summary.csv as input
python benchmark/plot_summary_vllm_bench.py \
  benchmark/testdata/logs/20260410-120000-PT-aibrix-version-comparison/summary.csv
```

Generated files:

- `benchmark/testdata/logs/<timestamp>-PT-<scenario>/figures/01-throughput-and-mean-latency.png`
- `benchmark/testdata/logs/<timestamp>-PT-<scenario>/figures/02-ttft-latency.png`
- `benchmark/testdata/logs/<timestamp>-PT-<scenario>/figures/03-tpot-latency.png`

Chart layout:

- Plot 1: `request_throughput`, `output_throughput`, `mean_ttft_ms`, `mean_tpot_ms`
- Plot 2: `mean_ttft_ms`, `p95_ttft_ms`, `p99_ttft_ms`
- Plot 3: `mean_tpot_ms`, `p95_tpot_ms`, `p99_tpot_ms`

***

## 📁 Project Structure

```
brixbench/
├── scripts/                    # Source-of-truth prototype shell flows
├── benchmark/                  # Test runner and scenarios
│   ├── runner_test.go         # Main orchestrator
│   └── testdata/
│       ├── scenarios/         # Scenario YAML files
│       └── deployments/       # Deployment YAML files
├── internal/
│   ├── deployers/             # Platform-specific deployment logic
│   │   ├── deployer.go        # Common interface
│   │   └── aibrix.go          # AIBrix implementation
│   ├── drivers/               # Benchmark drivers
│   │   └── vllm.go            # vllm-bench execution and parsing
│   └── observability/         # Metrics export
│       └── exporter.go        # Prometheus Pushgateway
├── tools/                     # External benchmark scripts
│   └── vllm-bench/
└── docs/                      # Documentation
```

***

## 🔧 Key Features

### 3-Tier Deployment Architecture

All platforms are deployed in three stages:

1. **Control Plane**: Cluster state management and scheduling
2. **Gateway**: Traffic routing and rate limiting
3. **Engine**: Actual model inference (e.g., vLLM)

### Test Isolation

Benchmark model and client workloads run in the `brixbench-adhoc` namespace, which is cleaned up after each test run.

### Extensible Design

To support new platforms (e.g., LLM-d, Dynamo), simply add a new Deployer in `internal/deployers/` and register it in `runner_test.go`.

***

## 🛠️ Troubleshooting

### kubectl Timeout

If your environment requires a network proxy, export the standard proxy variables in your shell before running `uv`, `go`, or image download commands.

```bash
export HTTPS_PROXY=http://proxy.example:port
export HTTP_PROXY=http://proxy.example:port
export NO_PROXY=127.0.0.1,localhost,.cluster.local
```

### Gateway Overrides

Scenario files can provide gateway overrides through `gateway.resources`.

- Resource files are applied after gateway env patching.
- If a resource file is a patch fragment with a `patchTarget` block, the
  deployer applies it through `kubectl patch`.
- The current smoke scenario uses
  `testdata/deployments/aibrix/gateway/overrides/gateway-plugins-scaleup.yaml`
  to increase `aibrix-gateway-plugins` replicas and CPU before the benchmark.

Example:

```yaml
gateway:
  env:
    ROUTING_ALGORITHM: random
  resources:
    - testdata/deployments/aibrix/gateway/overrides/gateway-plugins-scaleup.yaml
```

### Pending Pods During E2E

If a commit-to-commit or version-to-version run stalls before benchmark execution, check whether the `vllm-pd` Pods are stuck in `Pending`.

Common cause:

- Insufficient `nvidia.com/gpu` capacity on the target nodes

Useful command:

```bash
kubectl get pods -n brixbench-adhoc
kubectl describe pod -n brixbench-adhoc <pending-pod-name>
```

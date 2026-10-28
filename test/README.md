# AIBrix Testing Framework

This directory contains the comprehensive testing suite for AIBrix, including unit tests, integration tests, end-to-end tests, and performance regression testing.

## Test Structure

```
test/
├── e2e/                    # End-to-end tests against live clusters
│   ├── framework/           # Shared live-cluster test infrastructure
│   ├── gateway/             # Gateway API, routing, and PD tests
│   └── controller/          # Controller-owned lifecycle tests
├── integration/           # Integration tests using Ginkgo framework
├── regression/           # Performance regression tests for releases
├── utils/               # Shared test utilities and helpers
├── run-e2e-tests.sh    # E2E test runner script
└── README.md           # This file
```

## Verification principles

Start with the narrowest relevant check and broaden verification for changes
that cross package or language boundaries. Run commands from the repository
root unless a guide specifies otherwise.

- Add or update tests for behavior changes, including failure and compatibility
  cases where applicable. Read nearby implementations and tests before
  introducing a new pattern.
- For cluster-level behavior, reuse the helpers and organization under `test/e2e/`;
  use polling rather than arbitrary sleeps.
- Report the commands actually executed and their results, including relevant
  checks that could not be run.

For code-generation requirements, see
[build and verification](../development/README.md#build-and-verification).
Python runtime checks use `make python-ci` (Ruff, mypy, and tests) and follow
[its local guidance](../python/aibrix/AGENTS.md).

## Development Workflow

1. **Write tests first** - Add regression coverage at the cheapest level that
   reproduces the changed behavior.
2. **Run locally** - Start with the affected package or test. Run `make test`
   for broader unit coverage and `make test-integration` for component interactions.
3. **E2E validation** - Run `make test-e2e` for changes to cluster-level behavior,
   deployment, or interactions that narrower tests cannot verify. Follow any
   additional checks required by the affected component's guide.

Documentation-only changes follow the [documentation guide](../docs/README.md);
they do not require Go or cluster tests.

> Note: Performance regression benchmarks under `test/regression/` run as part
> of the release process, not against every commit. Behavior regression tests
> for fixes remain part of normal change verification.

## Running Tests

### Unit Tests

Unit tests are located alongside source code (`*_test.go` files), not in current test folder.

```bash
# Run all unit tests with coverage
make test

# Run unit tests with the race detector
make test-race-condition

# Run tests for specific package
go test ./pkg/controller/...
```

### Integration Tests

Integration tests use Ginkgo framework to test component interactions.

```bash
# Run all integration tests
make test-integration

# Run specific integration tests
make test-integration-controller
make test-integration-webhook

# Run Go tests behind the integration build tag (envtest)
make test-integration-tagged

```


### End-to-End Tests

E2E tests validate complete AIBrix functionality against a running Kubernetes cluster.

#### Development Environment (Local Testing)

For local development where cluster and AIBrix are already running:

**Prerequisites for Development Mode:**
1. Kubernetes cluster is running and accessible
2. AIBrix is deployed and healthy

```bash
# Development mode - run tests against existing setup
# make sure env `KUBECONFIG` is set correctly
make test-e2e

# Or run script directly
# Note: Required port-forwards should be active in this mode
go test -p 1 ./test/e2e/gateway/... ./test/e2e/controller/... -v -timeout 0
```

#### CI Environment (Automated Testing)
For CI pipelines that need full cluster setup and teardown:

```bash
# Full CI setup - creates Kind cluster and installs AIBrix
KIND_E2E=true INSTALL_AIBRIX=true make test-e2e

or

./test/run-e2e-tests.sh
```

**Environment Variables:**
- `KIND_E2E=true` - Creates Kind cluster with proper configuration
- `INSTALL_AIBRIX=true` - Builds images, installs dependencies, and deploys AIBrix
- `AIBRIX_ROLESET_INPLACE_E2E=true` - Runs the additional RoleSet in-place update e2e tests; `INSTALL_AIBRIX=true` builds the shared v1/v2 images whenever the `all` or `controller` suite is selected
- `AIBRIX_ROLESET_INPLACE_E2E_KEEP_ON_FAILURE=true` - Preserves RoleSet in-place e2e resources for debugging failed runs
- `AIBRIX_E2E_SUITE=all|gateway|controller|gateway-pd` - Selects the e2e suite; defaults to `all`
- `AIBRIX_E2E_GATEWAY_URL`, `AIBRIX_E2E_NAMESPACE`, `AIBRIX_E2E_API_KEY`, `AIBRIX_E2E_GATEWAY_NAMESPACE` - Override live-cluster e2e endpoints and namespaces
- `AIBRIX_E2E_KEEP_RESOURCES_ON_FAILURE=true` - Preserves installed e2e resources after a failed local run
- `SKIP_KUBECTL_INSTALL=true` - Skip kubectl installation (default: true)
- `SKIP_KIND_INSTALL=true` - Skip Kind installation (default: true)

#### ModelRouter Controller E2E

The ModelRouter package runs as part of the default `controller` and `all`
suites. It creates an isolated namespace and validates Deployment and
ModelAdapter discovery, generated HTTPRoute and ReferenceGrant resources, a
request through the gateway to a mock backend, shared ReferenceGrant cleanup,
and controller restart behavior.

To run only this package against an installed test cluster:

```bash
go test -p 1 ./test/e2e/controller/modelrouter/... -v -count=1
```

The package uses the existing `AIBRIX_E2E_KEEP_RESOURCES_ON_FAILURE` setting
to retain its namespace and routing resources for diagnostics after a failure.

#### RayClusterFleet Controller E2E

The RayCluster package runs as part of the default `controller` and `all`
suites. It validates the Fleet to ReplicaSet to RayCluster ownership chain,
KubeRay head readiness and status aggregation, scaling and scale-down ordering,
controller restart convergence, pause/resume behavior, and foreground cleanup.
The tests use the lightweight `aibrix/inplace-e2e:v1` head image with the real
KubeRay operator and do not require GPUs, model weights, or a Ray runtime.

To run only this package against an installed test cluster:

```bash
go test -p 1 ./test/e2e/controller/raycluster/... -v -count=1
```

When running the package outside the standard `INSTALL_AIBRIX=true` flow,
ensure that the KubeRay CRDs/operator and `aibrix/inplace-e2e:v1` are available
to cluster nodes. The KubeRay operator must enable
`RayClusterStatusConditions=true` so readiness assertions can observe
`RayClusterProvisioned` and `HeadPodReady`. Set
`AIBRIX_E2E_KEEP_RESOURCES_ON_FAILURE=true` to retain the isolated test
namespace and its resources for diagnostics after a failure.

#### PodSet Controller E2E

The PodSet package runs as part of the default `controller` and `all` suites.
It validates creation and status, scale-up, drain-aware scale-down and
cancellation, timeout-based deletion, recovery after manual Pod deletion, and
owned-resource cleanup. The tests use the lightweight
`aibrix/inplace-e2e:v1` image that the standard controller-suite installation
builds and loads.

To run only this package against an installed test cluster:

```bash
go test -p 1 ./test/e2e/controller/podset/... -v -count=1
```

When running the package outside the standard `INSTALL_AIBRIX=true` flow,
ensure that `aibrix/inplace-e2e:v1` is available to cluster nodes. Set
`AIBRIX_E2E_KEEP_RESOURCES_ON_FAILURE=true` to retain the isolated test
namespace and its resources for diagnostics after a failure.

#### StormService Controller E2E

The StormService controller package contains three lifecycle tests that run as
part of the default controller suite: Replica-mode creation and scaling,
pause/resume with in-place and fallback updates, and progress-deadline failure
and recovery. The installation E2E job builds and loads
`aibrix/inplace-e2e:v1` and `aibrix/inplace-e2e:v2`, which these tests require.

Volcano gang scheduling is opt-in because it requires a cluster with the
Volcano scheduler and `scheduling.volcano.sh/v1beta1` PodGroup CRD installed.
With `KUBECONFIG` pointing at a prepared cluster and the StormService, RoleSet,
and PodSet controllers running, execute only that test with:

```bash
make test-e2e-stormservice-volcano
```

Set `AIBRIX_STORMSERVICE_E2E_KEEP_ON_FAILURE=true` to retain StormService,
RoleSet, PodSet, Pod, ControllerRevision, Service, and PodGroup resources after
a failure for inspection.

The CI-tested scheduler pair is Kubernetes 1.31.0 with Volcano 1.11.2. AIBrix
currently compiles against `volcano.sh/apis` v1.11.2 and validates the
`minMember` and `minTaskMember` PodGroup fields. Volcano 1.14 and newer add the
different `subGroupPolicy` model used by newer RBG tests; that model is outside
this suite. Any Volcano upgrade must review the Go API module, installed CRD
schema, and Volcano's Kubernetes compatibility matrix together.

### Performance Regression Testing

The `regression/` directory contains benchmark configurations for release testing:

- **v0.2.1/**: performance benchmark baseline
- **v0.3.0/**: KV cache variants
- **v0.4.0/**: Helm-based templates for SGLang/VLLM testing

Before each **release**, run performance benchmarks using configurations in `regression/vX.Y.Z/`:

1. Deploy test configurations (YAML manifests or Helm charts)
2. Run benchmark clients against different setups
3. Collect and analyze performance metrics
4. Compare against previous release baselines

See individual `regression/*/README.md` files for detailed benchmark procedures.

## CI/CD Integration

Tests are automatically executed in CI pipelines:
- Unit tests: Every commit
- Integration tests: Pull requests
- E2E tests: Nightly builds and releases

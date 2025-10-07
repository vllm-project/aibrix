# AIBrix Testing Framework

This directory contains the comprehensive testing suite for AIBrix, including unit tests, integration tests, end-to-end tests, and performance regression testing.

## Test Structure

```
test/
├── e2e/                    # End-to-end tests against live clusters
├── integration/           # Integration tests using Ginkgo framework  
├── regression/           # Performance regression tests for releases
├── utils/               # Shared test utilities and helpers
├── run-e2e-tests.sh    # E2E test runner script
└── README.md           # This file
```

## Development Workflow

1. **Write tests first** - Add unit/integration tests for new features
2. **Run locally** - Use `make test` and `make test-integration`
3. **E2E validation** - Run `make test-e2e` before submitting changes

> Note: Regression tests are run as part of the release process. You do not need to run them against every commit.

## Running Tests

### Unit Tests

Unit tests are located alongside source code (`*_test.go` files), not in current test folder.

```bash
# Run all unit tests with coverage
make test

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
go test ./test/e2e/ -v -timeout 0
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
- `SKIP_KUBECTL_INSTALL=true` - Skip kubectl installation (default: true)
- `SKIP_KIND_INSTALL=true` - Skip Kind installation (default: true)

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
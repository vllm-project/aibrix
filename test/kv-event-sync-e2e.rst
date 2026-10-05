======================================================
End-to-End Testing Guide for KV Event Synchronization
======================================================

Overview
--------

This guide provides comprehensive instructions for running and debugging E2E tests for the KV cache event synchronization system in AIBrix. The tests validate the complete event flow from vLLM to gateway routing decisions.

Prerequisites
-------------

Local Development
~~~~~~~~~~~~~~~~~

1. **Go 1.22.6+** (managed via asdf: ``source .envrc``)
2. **Docker** for building images
3. **Kind** for local Kubernetes cluster
4. **kubectl** configured to access the cluster
5. **ZMQ libraries** (required for gateway-plugins tests):

   - Ubuntu/Debian: ``sudo apt-get install -y libzmq3-dev pkg-config``
   - macOS: ``brew install zeromq pkg-config``
   - Verify: ``pkg-config --cflags --libs libzmq``

Test Environment Setup
----------------------

1. Create Kind Cluster
~~~~~~~~~~~~~~~~~~~~~~

.. code-block:: bash

   # Create cluster with specific configuration
   kind create cluster --name aibrix-e2e --config kind-config.yaml

   # Example kind-config.yaml:
   kind: Cluster
   apiVersion: kind.x-k8s.io/v1alpha4
   nodes:
   - role: control-plane
     extraPortMappings:
     - containerPort: 30000
       hostPort: 30000
       protocol: TCP

2. Build Test Images
~~~~~~~~~~~~~~~~~~~~

.. code-block:: bash

   # Build all images (ZMQ automatically included where needed)
   make docker-build-all

   # Or specific components
   make docker-build-gateway-plugins    # Automatically builds with ZMQ
   make docker-build-controller-manager # Default build (no ZMQ)
   make docker-build-kvcache-watcher   # Automatically builds with ZMQ

3. Load Images to Kind
~~~~~~~~~~~~~~~~~~~~~~

.. code-block:: bash

   kind load docker-image aibrix/controller-manager:nightly --name aibrix-e2e
   kind load docker-image aibrix/gateway-plugins:nightly --name aibrix-e2e
   kind load docker-image aibrix/vllm-mock:nightly --name aibrix-e2e

4. Deploy AIBrix Components
~~~~~~~~~~~~~~~~~~~~~~~~~~~

.. code-block:: bash

   # Install CRDs
   kubectl apply -k config/dependency --server-side

   # Deploy controllers
   kubectl apply -k config/test

   # Wait for readiness
   kubectl wait --for=condition=ready pod -l app.kubernetes.io/name=controller-manager \
     -n aibrix-system --timeout=300s

Running E2E Tests
-----------------

Full Test Suite
~~~~~~~~~~~~~~~

.. code-block:: bash

   # Run all KV sync E2E tests (uses Makefile targets)
   make test-kv-sync-e2e  # Runs simple E2E test with proper build tags

Specific Test Scenarios
~~~~~~~~~~~~~~~~~~~~~~~

.. code-block:: bash

   # Run specific test functions
   go test -v -tags="zmq" ./test/e2e/kv_sync_e2e_simple_test.go \
     ./test/e2e/kv_sync_helpers.go ./test/e2e/util.go \
     -run TestKVSyncE2ESimple

   # Note: Full E2E test suite (kv_sync_e2e_test.go) is available
   # but simplified version is used for CI stability

Unit and Integration Tests
~~~~~~~~~~~~~~~~~~~~~~~~~~

.. code-block:: bash

   # Run ZMQ-specific unit tests
   make test-zmq

   # Run KV sync integration tests
   make test-kv-sync

   # Run with coverage
   make test-zmq-coverage

Test Structure
--------------

Test Categories
~~~~~~~~~~~~~~~

1. **Unit Tests**

   - ``pkg/cache/kvcache/*_test.go`` - ZMQ client and codec tests
   - ``pkg/utils/syncprefixcacheindexer/*_test.go`` - Indexer tests
   - ``pkg/cache/kv_event_*_test.go`` - Event manager tests

2. **Integration Tests**

   - ``test/integration/kv_event_sync_test.go`` - Component integration
   - Tests use mock ZMQ publishers for controlled scenarios

3. **E2E Tests**

   - ``test/e2e/kv_sync_e2e_simple_test.go`` - Simplified E2E for CI
   - ``test/e2e/kv_sync_e2e_test.go`` - Full E2E test suite
   - ``test/e2e/kv_sync_helpers.go`` - Shared test utilities

Key Test Helpers
~~~~~~~~~~~~~~~~

.. code-block:: go

   // KVEventTestHelper provides utilities for E2E tests
   type KVEventTestHelper struct {
       k8sClient   *kubernetes.Clientset
       namespace   string
       modelName   string
       deployments []*appsv1.Deployment
   }

   // Common test operations
   helper := NewKVEventTestHelper(client, namespace)
   helper.CreateTestNamespace(t)
   helper.CreateVLLMPodWithKVEvents(t, "test-deployment", 1)
   helper.ValidateKVEventConnection(t, podIP)
   helper.Cleanup(t)

Debugging Test Failures
-----------------------

1. Enable Debug Logging
~~~~~~~~~~~~~~~~~~~~~~~

.. code-block:: bash

   # Set log level for tests
   export AIBRIX_LOG_LEVEL=debug
   export AIBRIX_KV_EVENT_DEBUG=true

   # Run tests with verbose output
   go test -v -tags="zmq" ./test/e2e -run TestName

2. Check Component Logs
~~~~~~~~~~~~~~~~~~~~~~~

.. code-block:: bash

   # Gateway logs
   kubectl logs -n aibrix-system -l app.kubernetes.io/name=gateway-plugins --tail=100

   # Controller logs
   kubectl logs -n aibrix-system -l app.kubernetes.io/name=controller-manager --tail=100

   # vLLM mock logs
   kubectl logs -l app=vllm-mock --tail=100

3. Verify ZMQ Connectivity
~~~~~~~~~~~~~~~~~~~~~~~~~~

.. code-block:: bash

   # Test ZMQ connection from gateway pod
   kubectl exec -it <gateway-pod> -n aibrix-system -- sh -c \
     "nc -zv <vllm-pod-ip> 5557"

   # Check if gateway has ZMQ support
   kubectl exec <gateway-pod> -n aibrix-system -- sh -c \
     "ldd /app/gateway-plugin | grep zmq || echo 'No ZMQ support'"

4. Check Event Flow
~~~~~~~~~~~~~~~~~~~

.. code-block:: bash

   # Monitor events in real-time
   kubectl logs -f <gateway-pod> -n aibrix-system | grep "KV event"

   # Check sync indexer state
   kubectl exec <gateway-pod> -n aibrix-system -- curl localhost:8080/debug/sync-indexer

CI/CD Integration
-----------------

GitHub Actions
~~~~~~~~~~~~~~

.. code-block:: yaml

   name: KV Sync E2E Tests

   on: [push, pull_request]

   jobs:
     e2e-tests:
       runs-on: ubuntu-latest
       steps:
       - uses: actions/checkout@v3
       
       - name: Install ZMQ
         run: |
           sudo apt-get update
           sudo apt-get install -y libzmq3-dev pkg-config
       
       - name: Setup Go
         uses: actions/setup-go@v4
         with:
           go-version: '1.22'
       
       - name: Create Kind cluster
         run: |
           kind create cluster --name test
           kubectl cluster-info
       
       - name: Run E2E tests
         run: |
           make test-kv-sync-e2e

Makefile Targets
~~~~~~~~~~~~~~~~

Available test targets:

.. code-block:: bash

   # Unit tests
   make test-zmq              # ZMQ client tests
   make test-kv-sync         # KV sync integration tests
   make test-zmq-coverage    # Tests with coverage report

   # E2E tests
   make test-kv-sync-e2e     # Run E2E tests

   # Performance tests
   make test-kv-sync-benchmark  # Run benchmarks

   # Chaos tests
   make test-kv-sync-chaos   # Run chaos tests (requires Chaos Mesh)

   # All tests
   make test-kv-sync-all     # Run all KV sync related tests

Performance Testing
-------------------

Benchmark Tests
~~~~~~~~~~~~~~~

.. code-block:: bash

   # Run sync indexer benchmarks
   make test-kv-sync-benchmark

   # Or directly with options
   go test -bench=. -benchmem -benchtime=10s -tags="zmq" \
     ./test/benchmark/kv_sync_indexer_bench_test.go

Load Testing
~~~~~~~~~~~~

.. code-block:: go

   // Example load test configuration
   func TestKVSyncE2ELoad(t *testing.T) {
       config := LoadTestConfig{
           NumPods:        10,
           EventsPerPod:   1000,
           EventInterval:  100 * time.Millisecond,
           TestDuration:   5 * time.Minute,
       }
       RunLoadTest(t, config)
   }

Best Practices
--------------

1. **Test Isolation**: Use unique namespaces for each test
2. **Resource Cleanup**: Always defer cleanup operations
3. **Timeout Management**: Set appropriate timeouts for operations
4. **Mock Services**: Use mock vLLM for functional tests
5. **Real Services**: Test with actual vLLM for integration validation

Common Issues and Solutions
---------------------------

ZMQ Build Errors
~~~~~~~~~~~~~~~~

.. code-block:: bash

   # Ensure ZMQ is properly installed
   sudo apt-get update && sudo apt-get install -y libzmq3-dev pkg-config

   # Verify installation
   pkg-config --cflags --libs libzmq

   # For build issues, ensure CGO is enabled
   export CGO_ENABLED=1

   # Build with explicit tags
   go build -tags="zmq" -v ./cmd/plugins/main.go

Kind Cluster Issues
~~~~~~~~~~~~~~~~~~~

.. code-block:: bash

   # Reset Kind cluster
   kind delete cluster --name aibrix-e2e
   kind create cluster --name aibrix-e2e

   # Check cluster status
   kubectl cluster-info --context kind-aibrix-e2e

Test Timeouts
~~~~~~~~~~~~~

- Increase timeout values for slow environments
- Check for resource constraints (CPU/Memory)
- Verify network connectivity between pods

Test Coverage
-------------

Current test coverage by component:

**Unit Test Coverage** (target: 90%):

- ✅ ZMQ Client: 95%
- ✅ Sync Indexer: 90%
- ✅ Event Manager: 90%
- ✅ MessagePack Codec: 100%

**Integration Test Coverage**:

- ✅ Event flow validation
- ✅ Mock vLLM publisher tests
- ✅ Error handling scenarios
- ✅ Configuration validation

**E2E Test Coverage**:

- ✅ Basic KV event publishing
- ✅ Gateway routing decisions
- ✅ Pod lifecycle handling
- ✅ LoRA adapter support
- ✅ Metrics collection
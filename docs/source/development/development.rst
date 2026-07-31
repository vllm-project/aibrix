.. _development:

===========
Development
===========

Source Code Development
-----------------------

Build from Source
~~~~~~~~~~~~~~~~~

Build all binaries:

.. code-block:: bash

    make build

Build specific components:

.. code-block:: bash

    # Build controller
    make build-controller-manager

    # Build gateway plugins
    make build-gateway-plugins
    
    # Build metadata service
    make build-metadata-service

Code Generation
~~~~~~~~~~~~~~~

After modifying APIs or custom resources:

.. code-block:: bash

    # Generate CRDs, RBAC, and webhook configurations
    make manifests-all

    # Generate DeepCopy methods and client code
    make generate

Code Quality
~~~~~~~~~~~~

Run linter to check code style and catch issues:

.. code-block:: bash

    # Run golangci-lint linter
    make lint
    
    # Run golangci-lint with auto-fixes
    make lint-fix

Testing Framework
-----------------

AIBrix includes a comprehensive testing suite with unit tests, integration tests, and end-to-end tests.

Development Workflow
~~~~~~~~~~~~~~~~~~~~

1. **Write tests first** - Add unit/integration tests for new features
2. **Run locally** - Use ``make test`` and ``make test-integration``
3. **E2E validation** - Run ``make test-e2e`` before submitting changes

Unit Tests
~~~~~~~~~~

Unit tests are located alongside source code (``*_test.go`` files):

.. code-block:: bash

    # Run all unit tests with coverage
    make test

    # Run tests for specific package. e.g. lora controller
    go test ./pkg/controller/modeladapter...

Integration Tests
~~~~~~~~~~~~~~~~~

Integration tests use Ginkgo framework to test component interactions:

.. code-block:: bash

    # Run all integration tests
    make test-integration

    # Run specific integration tests
    make test-integration-controller
    make test-integration-webhook


End-to-End Tests
~~~~~~~~~~~~~~~~

E2E tests validate complete AIBrix functionality against a running Kubernetes cluster.

**Development Environment (Local Testing):**

For local development where cluster and AIBrix are already running:

Prerequisites for Development Mode:

- Kubernetes cluster is running and accessible
- AIBrix is deployed and healthy

.. code-block:: bash

    # Development mode - run tests against existing setup
    # make sure env KUBECONFIG is set correctly
    make test-e2e

    # Or run script directly
    # Note: Required port-forwards should be active in this mode
    go test ./test/e2e/ -v -timeout 0

**CI Environment (Automated Testing):**

For CI pipelines that need full cluster setup and teardown:

.. code-block:: bash

    # Full CI setup - creates Kind cluster and installs AIBrix
    KIND_E2E=true INSTALL_AIBRIX=true make test-e2e

    # Or run script directly
    ./test/run-e2e-tests.sh

Environment Variables:
- ``KIND_E2E=true`` - Creates Kind cluster with proper configuration
- ``INSTALL_AIBRIX=true`` - Builds images, installs dependencies, and deploys AIBrix
- ``SKIP_KUBECTL_INSTALL=true`` - Skip kubectl installation (default: true)
- ``SKIP_KIND_INSTALL=true`` - Skip Kind installation (default: true)

Container Images and Deployment
--------------------------------

Building Container Images
~~~~~~~~~~~~~~~~~~~~~~~~~

We encourage contributors to build and test AIBrix on local dev environment for most cases.
If you use Macbook, `Docker for Desktop <https://www.docker.com/products/docker-desktop/>`_ is the most convenient tool to use.

Build ``nightly`` docker images:

.. code-block:: bash

    make docker-build-all

    # build specific images
    make docker-build-controller-manager

Quick Deployment to Dev Environment
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

Run following command to quickly deploy the latest code changes to your dev kubernetes environment:

.. code-block:: bash

    kubectl apply -k config/dependency --server-side
    kubectl apply -k config/default

If you want to clean up everything and reinstall the latest code:

.. code-block:: bash

    kubectl delete -k config/default
    kubectl delete -k config/dependency

Complete Development Setup with Kind
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

For a complete local development environment with monitoring:

.. code-block:: bash

    # Complete AIBrix installation in Kind cluster
    make dev-install-in-kind
    
    # Start port forwarding for development services
    make dev-port-forward
    
    # Stop port forwarding
    make dev-stop-port-forward
    
    # Clean removal from Kind cluster
    make dev-uninstall-from-kind

Manual Testing with CPU-only vLLM
----------------------------------

This section explains how to manually test AIBrix with vLLM in a local Kubernetes cluster using CPU-only environments (e.g., for macOS or Linux dev).

Setup Local Environment
~~~~~~~~~~~~~~~~~~~~~~~~

Download model locally using Hugging Face CLI:

.. code-block:: bash

   huggingface-cli download facebook/opt-125m

Start local cluster with kind (edit ``kind-config.yaml`` to mount your model cache):

.. code-block:: bash

   kind create cluster --config=./development/vllm/kind-config.yaml

Build and load images:

.. code-block:: bash

   make docker-build-all
   kind load docker-image aibrix/runtime:nightly

Load CPU environment image for your platform:

**For macOS:**

.. code-block:: bash

   docker pull aibrix/vllm-cpu-env:macos
   kind load docker-image aibrix/vllm-cpu-env:macos

**For Linux:**

.. code-block:: bash

   docker pull aibrix/vllm-cpu-env:linux-amd64
   kind load docker-image aibrix/vllm-cpu-env:linux-amd64

Deploy and Test vLLM Model
~~~~~~~~~~~~~~~~~~~~~~~~~~

Deploy vLLM model in kind cluster:

**For macOS:**

.. code-block:: bash

   kubectl create -k development/vllm/macos

**For Linux:**

.. code-block:: bash

   kubectl create -k development/vllm/linux

Access model endpoint:

.. code-block:: bash

   kubectl port-forward svc/facebook-opt-125m 8000:8000 &

Query locally:

.. code-block:: bash

   curl -v http://localhost:8000/v1/completions \
     -H "Content-Type: application/json" \
     -H "Authorization: Bearer test-key-1234567890" \
     -d '{
        "model": "facebook-opt-125m",
        "prompt": "Say this is a test",
        "temperature": 0.5,
        "max_tokens": 512
      }'

Practical Notes
~~~~~~~~~~~~~~~

- ``vllm-cpu-env`` is ideal for development and debugging. Inference latency will be high due to CPU-only backend.
- Be sure to mount your Hugging Face model cache directory, or the container will re-download it online.
- Confirm both ``runtime`` and ``env`` images are loaded into kind.
- Use ``kubectl logs`` or ``kubectl exec`` to debug model pod issues.

Debugging
---------

Debugging Gateway IPs
~~~~~~~~~~~~~~~~~~~~~

.. code-block:: bash

   kubectl get svc -n envoy-gateway-system

.. code-block::

   NAME                                     TYPE           CLUSTER-IP      EXTERNAL-IP   PORT(S)                                   AGE
   envoy-aibrix-system-aibrix-eg-903790dc   LoadBalancer   10.96.239.246   101.18.0.4    80:32079/TCP                              10d

Please also follow `debugging guidelines <https://aibrix.readthedocs.io/latest/features/gateway-plugins.html#debugging-guidelines>`_.

Mocked CPU App
~~~~~~~~~~~~~~

In order to run the control plane and data plane e2e in development environments, we build a mocked app to mock a model server.
Now, it supports basic model inference, metrics and lora feature. Feel free to enrich the features. Check ``development`` folder for more details.

GPU Testing Environment
-----------------------

If you need to test the model in real GPU environment, we highly recommend `Lambda Labs <https://lambdalabs.com/>`_ platform to install and test kind based deployment.

.. attention::
    Kind itself doesn't support GPU yet. In order to use the kind version with GPU support, feel free to checkout `nvkind <https://github.com/klueska/nvkind>`_.

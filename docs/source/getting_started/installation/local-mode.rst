.. _local_mode:

==========
Local Mode
==========

Local mode runs the AIBrix gateway data path on a single machine without
Kubernetes. It starts Envoy and the AIBrix gateway plugin as local processes and
routes traffic to model servers listed in a static endpoint file.

Use local mode for gateway routing development, quick OpenAI-compatible request
testing, and debugging routing algorithms. Use a Kubernetes installation when
you need controllers, CRDs, autoscaling, model downloading, runtime sidecars, or
the full batch and metadata stack.

Architecture
------------

.. code-block:: text

   Client
      |
      v
   Envoy (:10080)
      |
      v
   Gateway plugin
      |
      v
   Static endpoints file
      |
      v
   vLLM or another OpenAI-compatible backend

Prerequisites
-------------

Install these tools on the local machine:

* Go 1.22 or newer
* Envoy
* one or more OpenAI-compatible model servers, such as vLLM

On macOS, Envoy can be installed with Homebrew:

.. code-block:: bash

   brew install envoy

On Linux, download an Envoy release binary:

.. code-block:: bash

   curl -L -o envoy \
     https://github.com/envoyproxy/envoy/releases/download/v1.37.1/envoy-1.37.1-linux-x86_64
   chmod +x envoy
   sudo mv envoy /usr/local/bin/envoy

Build the local gateway plugin binary from the repository root:

.. code-block:: bash

   make build-gateway-plugins-nozmq

Start a backend model server. This example uses vLLM:

.. code-block:: bash

   vllm serve Qwen/Qwen2.5-1.5B-Instruct --port 8000

Configure endpoints
-------------------

Local mode reads model endpoints from
``deployment/local/configs/endpoints.yaml``. A minimal configuration looks like
this:

.. code-block:: yaml

   models:
     - name: Qwen/Qwen2.5-1.5B-Instruct
       endpoints:
         - address: 127.0.0.1
           port: 8000

The request model name must match the configured model name.

The same file can also describe prefill and decode role sets for
prefill/decode disaggregation testing:

.. code-block:: yaml

   rolesets:
     - name: qwen-pd
       roles:
         prefill:
           - address: 127.0.0.1
             port: 8100
         decode:
           - address: 127.0.0.1
             port: 8200

Run local mode
--------------

On Linux, the helper script starts Envoy and the gateway plugin:

.. code-block:: bash

   cd deployment/local
   ./run-local.sh

Send a request through Envoy:

.. code-block:: bash

   curl http://localhost:10080/v1/chat/completions \
     -H "Content-Type: application/json" \
     -d '{
       "model": "Qwen/Qwen2.5-1.5B-Instruct",
       "messages": [{"role": "user", "content": "Hello from AIBrix local mode"}]
     }'

Stop the local processes:

.. code-block:: bash

   ./stop-local.sh

On macOS, or when you want direct process control, start the processes in
separate terminals from the repository root:

.. code-block:: bash

   bin/gateway-plugins \
     --standalone \
     --endpoints-config=deployment/local/configs/endpoints.yaml

.. code-block:: bash

   envoy \
     -c deployment/local/configs/envoy.yaml \
     --use-dynamic-base-id \
     --log-level warn

Use a custom endpoint or Envoy config with the helper script:

.. code-block:: bash

   cd deployment/local
   ./run-local.sh \
     -e /path/to/endpoints.yaml \
     -c /path/to/envoy.yaml

Routing algorithms
------------------

Set ``ROUTING_ALGORITHM`` before starting local mode:

.. code-block:: bash

   ROUTING_ALGORITHM=least-request ./run-local.sh

Use local mode with the same gateway routing algorithms that are supported by
the gateway plugin, such as random routing, least-request routing,
prefix-cache-aware routing, and prefill/decode routing. For production gateway
configuration and algorithm behavior, see :ref:`gateway`.

Ports and logs
--------------

.. list-table::
   :header-rows: 1

   * - Component
     - Default
     - Purpose
   * - Envoy HTTP listener
     - ``localhost:10080``
     - OpenAI-compatible request entry point.
   * - Envoy admin
     - ``localhost:9901``
     - Envoy admin and diagnostics.
   * - Gateway plugin metrics
     - ``localhost:8080``
     - Gateway plugin metrics endpoint.
   * - Health check
     - ``localhost:10080/healthz``
     - Local gateway health check.

The helper script writes logs under ``deployment/local/logs``:

* ``gateway-plugin.log``
* ``envoy.log``

Troubleshooting
---------------

* ``no healthy upstream``: confirm the backend model server is running and the
  host and port in ``endpoints.yaml`` are correct.
* ``model not found``: confirm the request ``model`` value exactly matches the
  configured model name.
* Envoy cannot bind a port: stop the previous local mode process or change the
  listener ports in the Envoy config.
* External processing errors: check ``gateway-plugin.log`` first, then verify
  Envoy is using ``deployment/local/configs/envoy.yaml``.
* Prefix-cache or prefill/decode routing does not behave as expected: confirm
  the selected ``ROUTING_ALGORITHM`` and the endpoint or roleset shape in
  ``endpoints.yaml``.

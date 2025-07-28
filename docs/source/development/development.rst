.. _development:

===========
Development
===========

Build and Run
-------------

We encourage contributors to build and test aibrix on Local dev environment for most of the cases.
If you use Macbook, `Docker for Desktop <https://www.docker.com/products/docker-desktop/>`_ is the most convenient tool to use.

Following commands will build ``nightly`` docker images.

.. code-block:: bash

    make docker-build-all

Run following command to quickly deploy the latest code changes to your dev kubernetes environment.

.. code-block:: bash

    kubectl apply -k config/dependency --server-side
    kubectl apply -k config/default


If you want to clean up everything and reinstall the latest code

.. code-block:: bash

    kubectl delete -k config/default
    kubectl delete -k config/dependency

Local Development with CPU-only vLLM
------------------------------------

This section explains how to run vLLM in a local Kubernetes cluster using CPU-only environments (e.g., for macOS or Linux dev).

Download model locally
~~~~~~~~~~~~~~~~~~~~~~

Use Hugging Face CLI:

.. code-block:: bash

   huggingface-cli download facebook/opt-125m

Start local cluster with kind
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

Edit ``kind-config.yaml`` to mount your model cache, then:

.. code-block:: bash

   kind create cluster --config=./development/vllm/kind-config.yaml

Build and load images
~~~~~~~~~~~~~~~~~~~~~

.. code-block:: bash

   make docker-build-all
   kind load docker-image aibrix/runtime:nightly

Load CPU environment image
~~~~~~~~~~~~~~~~~~~~~~~~~~

**For macOS:**

.. code-block:: bash

   docker pull aibrix/vllm-cpu-env:macos
   kind load docker-image aibrix/vllm-cpu-env:macos

**For Linux:**

.. code-block:: bash

   docker pull aibrix/vllm-cpu-env:linux-amd64
   kind load docker-image aibrix/vllm-cpu-env:linux-amd64

Deploy vLLM model in kind cluster
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

**For macOS:**

.. code-block:: bash

   kubectl create -k development/vllm/macos

**For Linux:**

.. code-block:: bash

   kubectl create -k development/vllm/linux

Access model endpoint
~~~~~~~~~~~~~~~~~~~~~

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

Debugging Gateway IPs
~~~~~~~~~~~~~~~~~~~~~

.. code-block:: bash

   kubectl get svc -n envoy-gateway-system

.. code-block::

   NAME                                     TYPE           CLUSTER-IP      EXTERNAL-IP   PORT(S)                                   AGE
   envoy-aibrix-system-aibrix-eg-903790dc   LoadBalancer   10.96.239.246   101.18.0.4    80:32079/TCP                              10d

Please also follow `debugging guidelines <https://aibrix.readthedocs.io/latest/features/gateway-plugins.html#debugging-guidelines>`_.

For Dev & Testing Local Setup with Monitoring
---------------------------------------------

.. code-block:: bash

    make dev-install-in-kind
    make dev-port-forward
    make dev-stop-port-forward
    make dev-uninstall-from-kind


Mocked CPU App
--------------

In order to run the control plane and data plane e2e in development environments, we build a mocked app to mock a model server.
Now, it supports basic model inference, metrics and lora feature. Feel free to enrich the features. Check ``development`` folder for more details.


Test on GPU Cluster
-------------------

If you need to test the model in real GPU environment, we highly recommended `Lambda Labs <https://lambdalabs.com/>`_ platform to install and test kind based deployment.

.. attention::
    Kind itself doesn't support GPU yet. In order to use the kind version with GPU support, feel free to checkout `nvkind <https://github.com/klueska/nvkind>`_.

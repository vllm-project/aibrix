.. _mac-for-desktop:

===============
Mac for Desktop
===============

This guide provides a minimal setup for deploying AIBrix locally on a Mac desktop for development and testing purposes.

Why Mac Desktop?
----------------

Mac desktops are ideal for quick testing of AIBrix control plane components in your local laptop.
It is highly recommended for AIBrix development and for testing lightweight vLLM CPU images.

Prerequisites
-------------

- Install Docker Desktop (with Kubernetes enabled)
- Install kubectl and Helm

Install AIBrix
--------------

1. Clone the AIBrix repository:

.. code-block:: bash

    git clone https://github.com/vllm-project/aibrix.git
    cd aibrix

2. Install dependencies and core components:

.. code-block:: bash

    kubectl apply -k config/dependency --server-side
    kubectl apply -k config/default

Expose Gateway
--------------

Since LoadBalancer is not available by default, use port-forward to access the gateway:

.. code-block:: bash

    kubectl port-forward svc/envoy-aibrix-system-aibrix-eg-903790dc 8888:80 -n envoy-gateway-system

You can now access the gateway at `http://localhost:8888`.

Conclusion
----------

This setup provides a lightweight way to develop and test AIBrix on Mac without special configuration.
Happy developing!
.. _installation:

============
Installation
============

This guide describes how to install AIBrix manifests in different platforms.

Currently, AIBrix installation does rely on other cloud specific features. It's fully compatible with vanilla Kubernetes.


Install AIBrix on Cloud Kubernetes Clusters
-------------------------------------------

.. attention::
    AIBrix will install `Envoy Gateway <https://gateway.envoyproxy.io/>`_ and `KubeRay <https://github.com/ray-project/kuberay>`_ in your environment.
    If you already have these components installed, you can use corresponding manifest to skip them.


Stable Version
^^^^^^^^^^^^^^

.. code:: bash

    # Install component dependencies
    kubectl apply -f https://github.com/aibrix/aibrix/releases/download/v0.2.0-rc.1/aibrix-dependency-v0.2.0-rc.1.yaml

    # Install aibrix components
    kubectl apply -f https://github.com/aibrix/aibrix/releases/download/v0.2.0-rc.1/aibrix-core-v0.2.0-rc.1.yaml


Nightly Version
^^^^^^^^^^^^^^^

.. code:: bash

    # clone the latest repo
    git clone https://github.com/aibrix/aibrix.git
    cd aibrix

    # Install component dependencies
    kubectl create -k config/dependency
    kubectl create -k config/default


Install Individual AIBrix Components
------------------------------------


Autoscaler
^^^^^^^^^^

.. code:: bash

    kubectl apply -k config/standalone/autoscaler-controller/


Distributed Inference
^^^^^^^^^^^^^^^^^^^^^

.. code:: bash

    kubectl apply -k config/standalone/distributed-inference-controller/



Model Adapter(Lora)
^^^^^^^^^^^^^^^^^^^

.. code:: bash

    kubectl apply -k config/standalone/model-adapter-controller



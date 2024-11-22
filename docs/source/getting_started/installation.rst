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

    kubectl apply -f https://github.com/aibrix/aibrix/releases/download/v0.1.1/aibrix-dependency-v0.1.1.yaml
    kubectl apply -f https://github.com/aibrix/aibrix/releases/download/v0.1.1/aibrix-core-v0.1.1.yaml


Nightly Version
^^^^^^^^^^^^^^^

.. code:: bash

    # clone the latest repo
    git clone https://github.com/aibrix/aibrix.git
    cd aibrix

    # Install component dependencies
    kubectl create -k config/dependency

    # Install aibrix components
    kubectl create -k config/default


Install AIBrix on Kind Cluster
------------------------------

.. attention::
    Kind itself doesn't support GPU yet. In order to use the kind version with GPU support, feel free to checkout `nvkind <https://github.com/klueska/nvkind>`_.

We use `Lambda Labs <https://lambdalabs.com/>`_ platform to install and test kind based deployment.

TODO

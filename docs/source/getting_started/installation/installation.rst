.. _installation:

============
Installation
============

This guide describes how to install AIBrix manifests in different platforms.

Currently, AIBrix installation does rely on other cloud specific features. It's fully compatible with vanilla Kubernetes.

Install AIBrix on Cloud Kubernetes Clusters
-------------------------------------------

.. attention::
    AIBrix will install `Envoy Gateway <https://gateway.envoyproxy.io/>`_ and `KubeRay <https://github.com/ray-project/kuberay>`_ in your environment. If you already have these components installed, you can use corresponding manifest to skip them.


Stable Version
~~~~~~~~~~~~~~

.. code:: bash

    # Install component dependencies
    kubectl apply -f https://github.com/vllm-project/aibrix/releases/download/v0.4.1/aibrix-dependency-v0.4.1.yaml --server-side
    # Install aibrix components
    kubectl apply -f https://github.com/vllm-project/aibrix/releases/download/v0.4.1/aibrix-core-v0.4.1.yaml


Stable Version Using Helm
~~~~~~~~~~~~~~~~~~~~~~~~~

Prerequisites
^^^^^^^^^^^^^

**Envoy Gateway**

.. code:: bash

    # Install envoy-gateway, this is not aibrix component. you can also use helm package to install it.
    helm install eg oci://docker.io/envoyproxy/gateway-helm --version v1.2.8 -n envoy-gateway-system --create-namespace

.. note::
    If you are experiencing network issues with `docker.io`, you can install the helm chart from the code repo https://github.com/envoyproxy/gateway/tree/main/charts/gateway-helm instead.

**KubeRay operator**

.. code:: bash

    # Install KubeRay operator
    helm repo add kuberay https://ray-project.github.io/kuberay-helm/
    helm repo update
    helm install kuberay-operator kuberay/kuberay-operator --namespace aibrix-system \
    --create-namespace \
    --version 1.2.1 \
    --set-string 'env[0].name=ENABLE_PROBES_INJECTION' \
    --set-string 'env[0].value=false' \
    --set fullnameOverride=kuberay-operator \
    --set 'featureGates[0].name=RayClusterStatusConditions' \
    --set 'featureGates[0].enabled=true' \
    --set image.repository=aibrix/kuberay-operator \
    --set image.tag=v1.2.1-patch-20250726


Install AIBrix with Helm
^^^^^^^^^^^^^^^^^^^^^^^^

.. code:: bash

    # Install AIBrix CRDs. `--install-crds` is not available in local chart installation.
    kubectl apply -f dist/chart/crds/
    # Install AIBrix with the pinned release version:
    helm install aibrix dist/chart -f dist/chart/stable.yaml -n aibrix-system --create-namespace


Upgrade AIBrix with Helm
^^^^^^^^^^^^^^^^^^^^^^^^

.. code:: bash

    helm upgrade aibrix dist/chart -f dist/chart/values.yaml -n aibrix-system


Uninstall AIBrix with Helm
^^^^^^^^^^^^^^^^^^^^^^^^^^

.. code:: bash

    helm uninstall aibrix -n aibrix-system
    helm uninstall kuberay-operator -n aibrix-system
    helm uninstall eg -n envoy-gateway-system


Nightly Version
~~~~~~~~~~~~~~~

.. code:: bash

    # clone the latest repo
    git clone https://github.com/vllm-project/aibrix.git
    cd aibrix
    # Install component dependencies
    kubectl apply -k config/dependency --server-side
    kubectl apply -k config/default


Install AIBrix in testing Environments
--------------------------------------

.. toctree::
   :maxdepth: 1
   :caption: Getting Started

   lambda.rst
   mac-for-desktop.rst
   aws.rst
   gcp.rst
   vke.rst


Install Individual AIBrix Components
------------------------------------


Autoscaler
~~~~~~~~~~

.. code:: bash

    kubectl apply -k config/standalone/autoscaler-controller/


Distributed Inference
~~~~~~~~~~~~~~~~~~~~~

.. code:: bash

    kubectl apply -k config/standalone/distributed-inference-controller/


Model Adapter(Lora)
~~~~~~~~~~~~~~~~~~~

.. code:: bash

    kubectl apply -k config/standalone/model-adapter-controller

KV Cache
~~~~~~~~

.. code:: bash

    kubectl apply -k config/standalone/kv-cache-controller

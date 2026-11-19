.. _installation:

============
Installation
============

This guide describes how to install AIBrix manifests in different platforms.

Currently, AIBrix installation does rely on other cloud specific features. It's fully compatible with vanilla Kubernetes.

Install AIBrix on Cloud Kubernetes Clusters
-------------------------------------------

.. attention::
    AIBrix requires `Envoy Gateway <https://gateway.envoyproxy.io/>`_ for request routing.

    The current checkout pins Envoy Gateway v1.9.4 with Envoy v1.39.6. The
    `upstream compatibility matrix <https://gateway.envoyproxy.io/news/releases/matrix/>`_
    lists Kubernetes v1.33 through v1.36 and Gateway API v1.6.1 for this release
    line. This is the upstream-tested range, not a claim that every AIBrix
    workload has been tested on every Kubernetes version. Older externally
    installed Envoy Gateway releases are not validated by this migration.

    Release bundles install their own pinned dependencies; these checkout
    versions do not change an already published release bundle.

    `KubeRay <https://github.com/ray-project/kuberay>`_ is **optional** and only required if you plan to use Ray as your distributed inference runtime (RayClusterFleet, RayClusterReplicaSet).

    AIBrix can run without KubeRay - use StormService for both single-node and multi-node inference workloads. The distributed inference controller will be automatically skipped if KubeRay CRDs are not detected.


Stable Version
~~~~~~~~~~~~~~

.. code:: bash

    # Install component dependencies
    kubectl apply -f https://github.com/vllm-project/aibrix/releases/download/v0.8.0/aibrix-dependency-v0.8.0.yaml --server-side
    # Install AIBrix CRDs (separate from the operator so uninstalls don't wipe user CRs)
    kubectl apply -f https://github.com/vllm-project/aibrix/releases/download/v0.8.0/aibrix-core-crds-v0.8.0.yaml --server-side
    # Install aibrix components
    kubectl apply -f https://github.com/vllm-project/aibrix/releases/download/v0.8.0/aibrix-core-v0.8.0.yaml


Stable Version Using Helm
~~~~~~~~~~~~~~~~~~~~~~~~~

Prerequisites
^^^^^^^^^^^^^

**Envoy Gateway**

.. code:: bash

    # Install envoy-gateway, this is not aibrix component. you can also use helm package to install it.
    helm install eg oci://docker.io/envoyproxy/gateway-helm --version v1.9.4 -n envoy-gateway-system \
    --create-namespace \
    --set config.envoyGateway.extensionApis.enableEnvoyPatchPolicy=true

.. note::
    If you are experiencing network issues with `docker.io`, you can install the helm chart from the code repo https://github.com/envoyproxy/gateway/tree/main/charts/gateway-helm instead.

**KubeRay operator (Optional)**

.. note::
    Skip this step if you don't need Ray-based distributed inference. AIBrix will work without KubeRay for StormService-based workloads.

.. code:: bash

    # Install KubeRay operator (only if you need RayClusterFleet/RayClusterReplicaSet)
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

When upgrading the gateway from v1.2.8, update the CRDs before the
controller, and update the controller before applying policies that use
``targetRefs``. Do not apply the full Kustomize dependency onto a Helm-owned
Envoy Gateway installation. Keep each controller's existing installation owner.
Back up your resources and values first, and review the
`upstream upgrade notes <https://gateway.envoyproxy.io/v1.9/install/install-helm/>`_.
This migration does not validate every intermediate release or third-party
controller that may share the Gateway API CRDs.

First render the target CRDs separately and apply them with server-side apply.
The example retains the experimental Gateway API channel bundled by Envoy
Gateway. If your provider manages these CRDs, coordinate a compatible upgrade
with that provider instead of overwriting its resources. If you use the standard
channel, review the upstream TCPRoute/UDPRoute API and storage-version migration
requirements before upgrading.

.. code:: bash

    helm template eg-crds oci://docker.io/envoyproxy/gateway-crds-helm \
      --version v1.9.4 \
      --set crds.gatewayAPI.enabled=true \
      --set crds.gatewayAPI.channel=experimental \
      --set crds.envoyGateway.enabled=true > gateway-crds-v1.9.4.yaml
    kubectl apply --server-side -f gateway-crds-v1.9.4.yaml
    kubectl get -f gateway-crds-v1.9.4.yaml -o name \
      | grep '^customresourcedefinition' \
      | xargs kubectl wait --for=condition=Established --timeout=5m

Resolve any field-ownership conflict before continuing; do not blindly force
ownership away from another controller. Then choose the matching controller path.

For a **Helm-owned Envoy Gateway**, keep its actual release name and namespace
(the example uses ``eg`` in ``envoy-gateway-system``). Save explicit user values,
review them against the new chart, remove stale image-version overrides and
retain deployment-specific settings such as scheduling, resource limits and
logging. ``--reset-values`` starts from target defaults, then reapplies those
reviewed settings rather than retaining old chart defaults:

.. code:: bash

    helm get values eg -n envoy-gateway-system -o yaml > eg-values-before.yaml
    cp eg-values-before.yaml eg-values-reviewed.yaml
    # Review eg-values-reviewed.yaml before running the upgrade.
    helm upgrade eg oci://docker.io/envoyproxy/gateway-helm \
      --version v1.9.4 -n envoy-gateway-system \
      --reset-values -f eg-values-reviewed.yaml \
      --set crds.enabled=false \
      --set config.envoyGateway.extensionApis.enableEnvoyPatchPolicy=true \
      --wait --timeout=5m

For a **Kustomize-owned Envoy Gateway**, after the same CRD-first gate, apply
the checkout's dependency and wait for its controller:

.. code:: bash

    kubectl apply -k config/dependency/envoy-gateway --server-side
    kubectl -n envoy-gateway-system rollout status deployment/envoy-gateway --timeout=5m

Only after the target controller is available, update AIBrix CRDs and upgrade
the AIBrix chart. Preserve your own reviewed AIBrix values, including image
repositories, authentication, resource limits and routing customizations, rather
than replacing them with an unreviewed copy of ``values.yaml``:

.. code:: bash

    kubectl apply -f dist/chart/crds --server-side
    kubectl wait --for=condition=Established --timeout=5m -f dist/chart/crds
    helm get values aibrix -n aibrix-system -o yaml > aibrix-values-before.yaml
    cp aibrix-values-before.yaml aibrix-values-reviewed.yaml
    # Review aibrix-values-reviewed.yaml and remove stale gateway image pins.
    helm upgrade aibrix dist/chart -n aibrix-system \
      --reset-values -f aibrix-values-reviewed.yaml --wait --timeout=10m

Do not accept a successful rollout, traffic, or xDS counters until the target
images have reconciled. Immediately after the chart upgrade, the previous
Deployment revision can still satisfy those checks. Use the Gateway ownership
labels to find its proxy Deployment, not a generated name or container position.
The example below assumes one proxy Deployment; stop if the selection is empty
or ambiguous. Keep the actual controller Deployment name and container name
from your existing installation (inspect them first):

.. code:: bash

    kubectl -n envoy-gateway-system get deployments -o yaml
    export EG_NAMESPACE=envoy-gateway-system
    export EG_DEPLOYMENT=envoy-gateway
    export EG_CONTAINER=envoy-gateway
    export GATEWAY_NAMESPACE=aibrix-system
    export GATEWAY_NAME=aibrix-eg
    export ENVOY_IMAGE=envoyproxy/envoy:v1.39.6
    export GATEWAY_IMAGE=envoyproxy/gateway:v1.9.4
    export CONTROLLER_IMAGE=docker.io/envoyproxy/gateway:v1.9.4
    export OWNER_LABEL=gateway.envoyproxy.io/owning-gateway
    export PROXY_SELECTOR="$OWNER_LABEL-namespace=$GATEWAY_NAMESPACE"
    export PROXY_SELECTOR="$PROXY_SELECTOR,$OWNER_LABEL-name=$GATEWAY_NAME"

Use your verified registry/repository references instead when they differ from
the defaults above. Independently verify that those artifacts are Envoy
v1.39.6 and Gateway v1.9.4 for your platform, and record their expected digests.
A version-looking tag alone is not proof. Do not substitute an unavailable
mirror tag or bypass this gate. The proxy's named ``envoy`` container must use
the Envoy artifact; its named ``shutdown-manager`` container and the actual
Gateway controller container must use the Gateway artifact.

Run the following in a shell with failure handling enabled. Each target-image
wait must succeed before rollout acceptance; a timeout, missing named container,
or failed selection must stop the procedure, not fall through to traffic tests:

.. code:: bash

    set -eu
    set -- $(kubectl -n "$EG_NAMESPACE" get deployment -l "$PROXY_SELECTOR" \
      -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
    test "$#" -eq 1
    export PROXY_DEPLOYMENT="$1"
    export IMAGE_PATH='.spec.template.spec.containers'
    kubectl -n "$EG_NAMESPACE" wait "deployment/$EG_DEPLOYMENT" \
      --for="jsonpath={$IMAGE_PATH[?(@.name=='$EG_CONTAINER')].image}=$CONTROLLER_IMAGE" \
      --timeout=5m
    kubectl -n "$EG_NAMESPACE" wait "deployment/$PROXY_DEPLOYMENT" \
      --for="jsonpath={$IMAGE_PATH[?(@.name=='envoy')].image}=$ENVOY_IMAGE" \
      --timeout=5m
    kubectl -n "$EG_NAMESPACE" wait "deployment/$PROXY_DEPLOYMENT" \
      --for="jsonpath={$IMAGE_PATH[?(@.name=='shutdown-manager')].image}=$GATEWAY_IMAGE" \
      --timeout=5m
    kubectl -n "$EG_NAMESPACE" rollout status "deployment/$EG_DEPLOYMENT" --timeout=5m
    kubectl -n "$EG_NAMESPACE" rollout status "deployment/$PROXY_DEPLOYMENT" --timeout=5m
    kubectl -n "$EG_NAMESPACE" get deployment "$EG_DEPLOYMENT" "$PROXY_DEPLOYMENT" \
      -o json > gateway-deployments.json
    kubectl -n "$EG_NAMESPACE" get replicasets -o json > gateway-replicasets.json
    kubectl -n "$EG_NAMESPACE" get pods -o json > gateway-pods.json

Before accepting these snapshots, match Pods to the current Deployment and
ReplicaSet owner UIDs, including the controller Pods. Require the current
Deployment generation to be observed, all desired replicas updated and
available, and each current Pod Ready with every required container ready.
Check each named container's Pod-spec image and running ``imageID`` against the
verified artifact and platform digest recorded above. Ignore terminating old
Pods as proof of the new revision. Missing/mismatched image IDs, unready Pods,
wrong owners, or incomplete reconciliation mean stop and investigate.

Record the proxy Pod UIDs, then restart the selected proxy Deployment and repeat
the selection, target-image waits, rollout checks, and ready-Pod image-ID checks.
Re-select by the same Gateway ownership labels in case reconciliation replaced
the Deployment. Require replacement proxy Pod UIDs and verify both named
containers again; do not accept a surviving old Pod or reuse an old snapshot.
Only after this gate succeeds should post-restart traffic and xDS checks count
as migration evidence. These checks do not establish zero-downtime upgrades.

Check that the Gateway and its policies are accepted for their current
``metadata.generation``. Check Envoy's xDS rejection counters and send traffic
after restarting a proxy as well as before it: a proxy can keep serving its
previous configuration when a newer configuration is rejected. A ready Pod
alone does not prove that the new routing configuration works.


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
    # Install AIBrix CRDs (separate from the operator so uninstalls don't wipe user CRs)
    kubectl apply -k config/crd --server-side
    kubectl apply -k config/default


Install AIBrix in testing Environments
--------------------------------------

.. toctree::
   :maxdepth: 1
   :caption: Getting Started

   local-mode.rst
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


Distributed Inference (Ray-based)
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

.. attention::
    This requires KubeRay operator to be installed first.

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


Disabling Controllers
---------------------

If you install AIBrix with all controllers enabled by default but don't have KubeRay installed, the distributed inference controller will be automatically skipped with a log message.

To explicitly disable specific controllers, use the ``--controllers`` flag:

.. code:: bash

    # Example: Enable all controllers except distributed-inference-controller
    --controllers=*,-distributed-inference-controller

    # Example: Only enable specific controllers
    --controllers=stormservice-controller,model-adapter-controller,pod-autoscaler-controller

Available controllers:

- ``pod-autoscaler-controller``: HPA/KPA-style autoscaling
- ``distributed-inference-controller``: Ray-based distributed inference (requires KubeRay)
- ``model-adapter-controller``: LoRA adapter management
- ``model-route-controller``: Gateway API ``HTTPRoute`` creation for each served model
- ``kv-cache-controller``: Distributed KV cache server orchestration with consistent hashing
- ``model-claim-controller``: Multiple models sharing GPUs through ModelClaim (experimental)
- ``model-warmup-controller``: Node preparation ahead of use through ModelWarmup (experimental)
- ``stormservice-controller``: Multi-node orchestration with P/D and AFD support

.. card:: What's next?
   :class-card: sd-border-1

   * :doc:`../quickstart`: deploy your first model
   * :doc:`../../production/model-deployment`: production model deployment guidance
   * :doc:`../../production/gateway`: production gateway configuration
   * :doc:`../../production/observability`: metrics, dashboards, and telemetry

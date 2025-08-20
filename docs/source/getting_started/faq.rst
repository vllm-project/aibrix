.. _faq:

===
FAQ
===

FAQ - Installation
------------------

Failed to delete the AIBrix
---------------------------

.. figure:: ../assets/images/delete-namespace-stuck-1.png
  :alt: aibrix-architecture-v1
  :width: 70%
  :align: center

.. figure:: ../assets/images/delete-namespace-stuck-2.png
  :alt: aibrix-architecture-v1
  :width: 70%
  :align: center

In this case, you just need to find the model adapter, edit the object, and remove the ``finalizer`` pair. the pod would be deleted automatically.


Gateway error messages
----------------------

* model does not exist

.. figure:: ../assets/images/model-error.png
  :alt: model-error
  :width: 70%
  :align: center

* routing strategy is incorrect

* no ready pods

Gateway ReferenceGrant Issue
---------------------------

When using multi-node inference with RayClusterFleet (as described in the `multi-node inference guide <https://aibrix.readthedocs.io/latest/features/multi-node-inference.html>`_), you might encounter a 500 error when accessing the model through the Envoy gateway, while direct access via port-forward works fine.

This issue occurs because the gateway (in aibrix-system namespace) needs explicit permission to access services in other namespaces (e.g., default namespace). To resolve this, you need to create a ReferenceGrant:

.. code-block:: yaml

    apiVersion: gateway.networking.k8s.io/v1beta1
    kind: ReferenceGrant
    metadata:
      name: allow-aibrix-gateway-to-access-services-route
      namespace: default
    spec:
      from:
      - group: gateway.networking.k8s.io
        kind: HTTPRoute
        namespace: aibrix-system
      to:
      - group: ""
        kind: Service

After applying this ReferenceGrant, the gateway should be able to properly route requests to your model service.

Note: This is typically only needed for multi-node deployments. Simple model deployments usually work without requiring this additional configuration.
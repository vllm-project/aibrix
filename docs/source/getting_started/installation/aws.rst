.. _aws:

=========================
Amazon Web Services (AWS)
=========================

Introduction
------------

An AIBrix cluster can either be deployed using the `AI on EKS <https://awslabs.github.io/ai-on-eks/>`_ project, which offers a simple deployment that will create a VPC, EKS cluster, and deploy AIBrix, or manually step-by-step.

AI on EKS
---------

`AI on EKS <https://awslabs.github.io/ai-on-eks/>`_ provides a one-line `deployment <https://awslabs.github.io/ai-on-eks/docs/infra/aibrix>`_ of AIBrix.

This deployment will create a VPC, subnets, EKS environment and deploy AIBrix.

AI on EKS also includes `inference charts <https://awslabs.github.io/ai-on-eks/docs/blueprints/inference/inference-charts>`_ that can deploy models to be served by AIBrix.

Manually
--------

Prerequisites
~~~~~~~~~~~~~

- `eksctl <https://eksctl.io/installation/>`_
- A quota of at least 1 GPU within your AWS project.

Steps
~~~~~

1. Create an eks cluster:

    .. code-block:: console

        eksctl create cluster --name aibrix --node-type=g5.4xlarge --nodes 2 --auto-kubeconfig

2. Clone AIBrix code repo ``git clone https://github.com/vllm-project/aibrix.git``.
3. Install AIBrix dependencies, CRDs, and core components:

   .. code-block:: bash

       kubectl apply -k config/dependency --server-side
       kubectl apply -k config/crd --server-side
       kubectl apply -k config/default
4. Wait for components to complete running.
5. Deploy a model by following the instructions in :doc:`../quickstart`.
6. Once the model is ready and running, you can test it by running:

    .. code-block:: bash

        LB_IP=$(kubectl get svc/envoy-aibrix-system-aibrix-eg-903790dc -n envoy-gateway-system -o=jsonpath='{.status.loadBalancer.ingress[0].hostname}')
        ENDPOINT="${LB_IP}:80"

        curl http://${ENDPOINT}/v1/chat/completions \
          -H "Content-Type: application/json" \
          -d '{
              "model": "deepseek-r1-distill-llama-8b",
              "messages": [
                  {"role": "system", "content": "You are a helpful assistant."},
                  {"role": "user", "content": "help me write a random generator in python"}
              ]
          }'

7. When you are finished testing and no longer want the resources, run ``eksctl delete cluster --name aibrix``.

AWS Neuron (Trainium) with Dynamic Resource Allocation
------------------------------------------------------

AIBrix supports disaggregated inference (prefill/decode separation) on AWS
Trainium (Trn2 / Trn3) using the ``pd`` routing algorithm. On Neuron, the KV
cache is transferred from prefill to decode over NIXL on the ``LIBFABRIC``
backend, which rides EFA (Elastic Fabric Adapter). This requires each serving
pod to be allocated **both** a set of NeuronCores **and** EFA devices — and,
critically, from the **same PCIe/NUMA group** so the fabric path between pods is
valid. The recommended way to request them together is Kubernetes
**Dynamic Resource Allocation (DRA)**.

Prerequisites
~~~~~~~~~~~~~

- An EKS nodegroup of ``trn2.48xlarge`` (or ``trn3-dev1.48xlarge``) with EFA
  enabled (``efaEnabled: true`` and ``privateNetworking: true`` in the eksctl
  nodegroup config). EFA capacity is typically obtained through an on-demand
  capacity reservation (ODCR).
- Two DRA drivers installed:

  - **Neuron DRA driver** — publishes ``neuron.aws.com`` devices. Install per the
    `Neuron DRA guide <https://awsdocs-neuron.readthedocs-hosted.com/en/latest/containers/neuron-dra.html>`_.
    Do not also run the Neuron device plugin on the same cluster.
  - **EFA DRA driver** (``aws-dranet``) — publishes ``efa.networking.k8s.aws``
    devices. Install per the
    `Amazon EKS DRA device-management guide <https://docs.aws.amazon.com/eks/latest/userguide/device-management-efa.html>`_.

Verify both device classes are discovered:

.. code-block:: bash

    kubectl get deviceclass                # neuron.aws.com + efa.networking.k8s.aws
    kubectl get resourceslice -o wide      # neuron + EFA devices per Neuron node

Allocate Neuron + EFA together with a ResourceClaimTemplate
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

A single claim requests both device classes and, via a ``constraints`` block,
pins them to the **same device group** — this is what keeps the NeuronCores and
EFA NICs on the same PCIe/NUMA locality so the NIXL/LIBFABRIC path between pods
is valid. Only the ``48xlarge`` Trn size is EFA-enabled.

Two ready-to-use templates are provided in
`samples/disaggregation/neuron <https://github.com/vllm-project/aibrix/tree/main/samples/disaggregation/neuron>`_:

- ``xl-lnc2-trn2-efa-rct.yaml`` — **half an instance**: 8 NeuronCores + 8 EFA
  from one ``devicegroup8``. A ``trn2.48xlarge`` has 16 Neuron devices in two
  such groups, so this template carves the node into two 8-device chunks — one
  per pod — letting a prefill and a decode pod co-reside on one node, each with
  an aligned, mutually-routable Neuron + EFA set.
- ``xxl-lnc2-trn2-efa-rct.yaml`` — **a full instance**: all 16 NeuronCores + 16
  EFA aligned on one ``devicegroup16`` (a single pod owns the whole node).

The essential shape (see the files for the full manifest):

.. code-block:: yaml

    kind: ResourceClaimTemplate
    metadata:
      name: xl-lnc2-trn2-efa
    spec:
      spec:
        devices:
          constraints:
          # Same PCIe/NUMA group for neurons AND efas — required for the NIXL EFA path.
          - matchAttribute: resource.aws.com/devicegroup8_id   # devicegroup16_id for xxl
            requests: [neurons, efas]
          requests:
          - name: neurons
            exactly:
              deviceClassName: neuron.aws.com
              count: 8                                          # 16 for xxl
              selectors:
              - cel: {expression: "device.attributes['neuron.aws.com'].instanceType == 'trn2.48xlarge'"}
          - name: efas
            exactly:
              deviceClassName: efa.networking.k8s.aws
              count: 8                                          # 16 for xxl
          ...

A pod references the template via ``resourceClaims`` and ``resources.claims``.
For Trn3, use the ``trn3-dev1.48xlarge`` selector.

.. tip::

    Validate the fabric before serving — run ``/opt/amazon/efa/bin/fi_pingpong -p efa``
    (server) in one pod and ``fi_pingpong -p efa <server-pod-ip>`` (client) in the
    other. A bandwidth table confirms EFA works pod-to-pod.

Once the prefill/decode Deployments are running with these claims and the
``role-name: prefill|decode`` labels, enable disaggregated routing on the gateway
with ``ROUTING_ALGORITHM=pd`` and deploy a model as in the steps above.

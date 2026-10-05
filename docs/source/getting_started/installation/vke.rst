.. _vke:

==============
Volcano Engine
==============

Introduction
------------

This doc deploys AIBrix in Volcano Engine Kubernetes Engine.

Steps
-----

AIBrix Installation
~~~~~~~~~~~~~~~~~~~

1. Assume you already have VKE cluster up and running
2. Install AIBrix on VKE

.. code-block:: console

    kubectl apply -k config/overlays/vke/dependency --server-side

    helm install aibrix dist/chart -f dist/chart/vke.yaml -n aibrix-system --create-namespace

3. Wait for components to complete running.

Download Model in TOS
~~~~~~~~~~~~~~~~~~~~~

Download models in TOS and create the credential in the cluster.

.. code-block:: console

    kubectl create secret generic tos-credential --from-literal=TOS_ACCESS_KEY=<YOUR_ACCESS_KEY> --from-literal=TOS_SECRET_KEY=<YOUR_SECRET_KEY>


Deploy base model
~~~~~~~~~~~~~~~~~

Save yaml as `model.yaml` and run `kubectl apply -f model.yaml`.

.. literalinclude:: ../../../../samples/quickstart/vke/model.yaml
   :language: yaml

Deploy Prefill-Decode (PD) Disaggregation Model
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

Save yaml as `pd-model.yaml` and run `kubectl apply -f pd-model.yaml`.

.. literalinclude:: ../../../../samples/quickstart/vke/pd-model.yaml
   :language: yaml


Inference
~~~~~~~~~

Once the model is ready and running, you can test it by running:

.. code-block:: bash

    LB_IP=$(kubectl get svc/envoy-aibrix-system-aibrix-eg-903790dc -n envoy-gateway-system -o=jsonpath='{.status.loadBalancer.ingress[0].ip}')
    ENDPOINT="${LB_IP}:80"

    curl http://${ENDPOINT}/v1/chat/completions \
      -H "Content-Type: application/json" \
      -H "routing-strategy: random" \ # change to `pd` if you deployed in disaggregation mode
      -d '{
          "model": "deepseek-r1-distill-llama-8b",
          "messages": [
              {"role": "system", "content": "You are a helpful assistant."},
              {"role": "user", "content": "help me write a random generator in python"}
          ]
      }'

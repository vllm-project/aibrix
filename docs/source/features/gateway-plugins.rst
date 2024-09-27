.. _gateway:

===============
Gateway Routing
===============

Gateway provides features such as user configuration, budgeting, dynamically routing user requests to respective model deployment and provides advanced routing strategies for hetrogenous GPU hardware.

Design
-----------------------------

TBD


Dynamic Routing
----------------------

Gateway dynamically creates a route for each model deployment without need for manual user configuration.
During requests, gateway uses model name from the header to route request to respective model deployment. 


.. code-block:: bash

    curl -v http://localhost:8888/v1/chat/completions \
    -H "user: your-user-name" \
    -H "model: your-model-name" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer any_key" \
    -d '{
        "model": "your-model-name",
        "messages": [{"role": "user", "content": "Say this is a test!"}],
        "temperature": 0.7
    }'


Routing Strategies
----------------------

Gateway supports two routing strategies right now.
1. least-request: routes request to a pod with least ongoing request.
2. throughput: routes request to a pod which has processed lowest tokens.

.. code-block:: bash

    curl -v http://localhost:8888/v1/chat/completions \
    -H "user: your-user-name" \
    -H "model: your-model-name" \
    -H "routing-strategy: least-request" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer any_key" \
    -d '{
        "model": "your-model-name",
        "messages": [{"role": "user", "content": "Say this is a test!"}],
        "temperature": 0.7
    }'
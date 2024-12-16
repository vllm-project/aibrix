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

    kubectl create -f config/dependency
    kubectl create -f config/default


If you want to clean up everything and reinstall the latest code

.. code-block:: bash

    kubectl delete -f config/default
    kubectl delete -f config/dependency

Mocked CPU App
--------------

In order to run the control plane and data plane e2e in development environments, we build a mocked app to mock a model server.
Now, it supports basic model inference, metrics and lora feature. Feel free to enrich the features. Check ``development`` folder for more details.


Test on GPU Cluster
-------------------

If you need to test the model in real GPU environment, we highly recommended `Lambda Labs <https://lambdalabs.com/>`_ platform to install and test kind based deployment.

.. attention::
    Kind itself doesn't support GPU yet. In order to use the kind version with GPU support, feel free to checkout `nvkind <https://github.com/klueska/nvkind>`_.

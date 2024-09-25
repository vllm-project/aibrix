.. _quickstart:

==========
Quickstart
==========


To get started with AIBrix, clone `aibrix/aibrix` repository, and install the manifests.

.. note::

    This is the latest version which is not very stable, take your risk.

.. code:: bash

    # Local Testing
    git clone https://github.com/aibrix/aibrix.git
    cd aibrix

    # Install component dependencies
    kubectl create -k config/dependency

    # Install aibrix components
    kubectl apply -k config/default


Install stable distribution.

.. code:: bash

    # Install component dependencies
    kubectl create -k "github.com/aibrix/aibrix/config/dependency?ref=v0.1.0-rc.1"

    # Install aibrix components
    kubectl apply -k "github.com/aibrix/aibrix/config/default?ref=v0.1.0-rc.1"

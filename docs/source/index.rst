Welcome to AIBrix
=================

.. image:: ./assets/logos/aibrix-logo-light.png
  :width: 60%
  :align: center
  :alt: AIBrix

AIBrix is the foundational building blocks for constructing your own GenAI inference infrastructure.
AIBrix offers a cloud-native solution tailored to meet the demands of enterprises aiming to deploy, manage, and scale LLMs efficiently.

Key features:

* High density Lora management
* Intelligent and LLM specific routing strategies
* LLM tailored pod autoscaler
* AI runtime sidecar (metrics merge, fast model downloading, admin operations)


Documentation
=============

.. toctree::
   :maxdepth: 1
   :caption: Getting Started

   getting_started/quickstart.rst
   getting_started/installation.rst


.. toctree::
   :maxdepth: 1
   :caption: Core Concepts

   designs/architecture.rst

.. toctree::
   :maxdepth: 1
   :caption: User Manuals

   features/autoscaling.rst
   features/lora-dynamic-loading.rst
   features/gateway-plugins.rst
   features/multi-node-inference.rst
   features/runtime.rst

.. toctree::
   :maxdepth: 1
   :caption: Development

   development/contribution.rst

.. toctree::
   :maxdepth: 1
   :caption: Community

   community/community.rst

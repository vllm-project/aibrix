Welcome to AIBrix
=================

.. image:: ./assets/logos/aibrix-logo-light.png
  :width: 60%
  :align: center
  :alt: AIBrix

AIBrix is an open-source initiative designed to provide essential building blocks to construct scalable GenAI inference infrastructure. 
AIBrix delivers a cloud-native solution optimized for deploying, managing, and scaling large language model (LLM) inference, tailored specifically to enterprise needs.

Key features:

- **LLM Gateway and Routing**: Efficiently manage and direct traffic across multiple models and replicas.
- **High-Density LoRA Management**: Streamlined support for lightweight, low-rank adaptations of models.
- **Distributed Inference**: Scalable architecture to handle large workloads across multiple nodes.
- **LLM App-Tailored Autoscaler**: Dynamically scale inference resources based on real-time demand.
- **Unified AI Runtime**: A versatile sidecar enabling metric standardization, model downloading, and management.
- **GPU Hardware Failure Detection (TBD)**: Proactive detection of GPU hardware issues.
- **Benchmark Tool (TBD)**: A tool for measuring inference performance and resource efficiency.

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

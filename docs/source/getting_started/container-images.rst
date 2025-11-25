.. _container-images:

=======================
AIBrix Container Images
=======================

Overview
--------

AIBrix provides enhanced container images for **vLLM** and **SGLang** that include additional capabilities for distributed inference and KV cache disaggregation:

- **aibrix_kvcache** - Built from source for KV cache disaggregation support
- **nixl + nixl-cu12** - UCX-based high-performance networking libraries for RDMA
- **UCX tooling** - Pre-installed debugging and performance testing utilities

Image Naming Convention
-----------------------

AIBrix images extend upstream inference engines with additional capabilities:

.. list-table:: Upstream vs. AIBrix Images
   :header-rows: 1
   :widths: 40 40 20

   * - Upstream Image
     - AIBrix Enhanced Image
     - Use Case
   * - ``vllm/vllm-openai:v0.10.2``
     - ``aibrix/vllm-openai:v0.10.2-aibrix-v0.5.0-nixl-0.7.1-20251123``
     - vLLM + KVCache + RDMA
   * - ``lmsysorg/sglang:v0.5.5.post3``
     - ``aibrix/sglang:v0.5.5.post3-aibrix-v0.5.0-nixl-0.7.1-20251123``
     - SGLang + KVCache + RDMA

When to Use AIBrix Images
--------------------------

Use **AIBrix-enhanced images** when you need:

- **KV Cache Offloading**: KV cache offload to Host memory or remote storage
- **Prefill-Decode Disaggregation**: Separate prefill and decode workloads via NIXL

Use **upstream images** for:

- Standard single-node inference without disaggregation
- Development and testing without specialized networking

Compatibility Matrix
--------------------

The following table shows tested component versions for AIBrix v0.5.0:

.. list-table:: Component Versions
   :header-rows: 1
   :widths: 25 20 20 35

   * - Component
     - vLLM Image
     - SGLang Image
     - Notes
   * - Engine Version
     - v0.10.2
     - v0.5.5.post3
     - Stable inference engines
   * - CUDA Version
     - 12.8
     - 12.9
     - CUDA toolkit version
   * - PyTorch Version
     - 2.8
     - 2.9
     - Auto-detected from base image
   * - AIBrix KVCache
     - v0.5.0
     - v0.5.0
     - KV cache disaggregation support
   * - NIXL Version
     - 0.7.1
     - 0.7.1
     - UCX-based RDMA networking
   * - UCX Version
     - 1.19.0
     - 1.19.0
     - Unified Communication X

.. note::
   PyTorch version is automatically extracted from the upstream base image to ensure compatibility.
   AIBrix KVCache is built against the exact PyTorch version from the base image.

Released Images (v0.5.0)
------------------------

The following pre-built images are available for immediate use:

**vLLM Image:**

.. code-block:: bash

   docker pull aibrix/vllm-openai:v0.10.2-aibrix-v0.5.0-nixl-0.7.1-20251123

**SGLang Image:**

.. code-block:: bash

   docker pull aibrix/sglang:v0.5.5.post3-aibrix-v0.5.0-nixl-0.7.1-20251123

Building Custom Images
-----------------------

For detailed build instructions and troubleshooting, see `build/container/README.md <https://github.com/vllm-project/aibrix/blob/main/build/container/README.md>`_.

Version History
---------------

v0.5.0
~~~~~~

- **vLLM**: v0.10.2 with CUDA 12.8, PyTorch 2.8
- **SGLang**: v0.5.5.post3 with CUDA 12.9, PyTorch 2.9
- **AIBrix KVCache**: v0.5.0
- **NIXL**: 0.7.1
- **UCX**: 1.19.0

Features:

- Full KV cache offloading support
- RDMA networking for distributed inference
- Prefill-Decode disaggregation support

Troubleshooting
---------------

Performance Issues
~~~~~~~~~~~~~~~~~~

For RDMA networking issues:

1. Verify RDMA devices are available: ``ibv_devices``
2. Check UCX configuration: ``ucx_info -d``
3. Test RDMA bandwidth: ``ib_write_bw``
4. Ensure security policies allow RDMA access

For debugging utilities included in the image, run:

.. code-block:: bash

   kubectl exec -it <pod-name> -- ucx_info -d
   kubectl exec -it <pod-name> -- ibv_devices
   kubectl exec -it <pod-name> -- ib_write_bw

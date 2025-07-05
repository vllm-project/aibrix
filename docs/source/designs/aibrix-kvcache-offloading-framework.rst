.. _aibrix_kvcache-offloading-framework:

===================================
AIBrix KVCache Offloading Framework
===================================

The rising demand for large language models has intensified the need for efficient memory management and caching to optimize inference performance and reduce costs. In multi-round use cases like chatbots and agent-based systems, overlapping token sequences lead to redundant computations during the prefill phase, wasting resources and limiting throughput.

Many inference engines, such as `vLLM <https://github.com/vllm-project/vllm>`_, use built-in KV caching to mitigate this issue, leveraging idle HBM and DRAM. However, single-node KV caches face key limitations: constrained memory capacity, engine-specific storage that prevents sharing across instances, and difficulty supporting scenarios like KV migration and prefill-decode disaggregation.

With AIBrix v0.3.0, we introduce a **production-ready KVCache Offloading Framework**, which enables efficient memory tiering and low-overhead cross-engine reuse. By default, the framework leverages **L1 DRAM-based caching**, which already provides significant performance improvements by offloading GPU memory pressure without incurring high latency. For scenarios requiring **multi-node sharing or larger-scale reuse**, AIBrix allows users to optionally enable **L2 remote caching**, unlocking the benefits of a distributed KV cache layer.

.. figure:: ../assets/images/aibrix-kvcache-offloading-arch-overview.png
  :alt: aibrix-kvcache-offloading-arch-overview
  :width: 80%
  :align: center

**Figure 1. AIBrix KVCache Offloading Framework**

As shown in Figure 1, on the data plane, it integrates tightly with inference engines (e.g., vLLM) via *AIBrix Offloading Connector*, which employs optimized CUDA kernels to significantly accelerate data movement between GPU and CPU. For memory scalability, its multi-tiered cache manager dynamically balances workloads across storage layers, alleviating GPU memory capacity limits while minimizing latency penalties. The framework supports pluggable eviction policies (e.g., LRU, `S3FIFO <https://blog.jasony.me/system/cache/2023/08/01/s3fifo>`_) and diverse backend storage options (e.g., `InfiniStore <https://github.com/bytedance/InfiniStore>`_), enabling selective KV cache offloading to reduce network and PCIe contention. Crucially, its cache placement module can coordinate with the centralized distributed KV cache cluster manager to maximize global KV cache utilization. This enables cross-engine KV reuse and ensures cluster-wide resource efficiency, transforming isolated KV cache instances into a scalable, shared KV cache infrastructure.

L1 Engine DRAM Cache Management
-------------------------------

The growing demands of modern models and increasing context lengths in LLM inference have led to KV caches consuming progressively more GPU memory, pushing against the hardware limits of even the most advanced GPUs. Recent systems like `Dynamo <https://github.com/ai-dynamo/dynamo>`_, `LMCache <https://github.com/LMCache/LMCache>`_, and `MoonCake <https://github.com/kvcache-ai/Mooncake>`_ have developed solutions that offload KV cache to external memory hierarchies, spanning from CPU memory to SSDs. :ref:`kvcache-offloading` supports the offloading of KV cache to CPU memory as well by only enabling its DRAM-backed ``L1Cache``. While this approach does not enable KV cache sharing across multiple engines, it eliminates the complexity of distributed KV cache setup and configuration. More importantly, by leveraging the significantly larger capacity of CPU memory, this method delivers substantial performance gains —- making it an ideal solution for use cases that prioritize scalable KV cache capacity over cross-engine KV reuse.


L2 Distributed KVCache and Cross-Engine KV Reuse
------------------------------------------------

The growing demand for large language models has significantly increased the need for expansive KV cache capacity. While CPU memory offloading effectively addresses moderate scaling needs, production environments handling massive-scale, dynamic workloads require even greater scalability -- particularly when memory needs exceed single-node capacities. To address this, AIBrix enables distributed KV cache services as its ``L2Cache`` backends, which can scale horizontally across multiple nodes to meet capacity demands.

In the meantime, as LLM deployments scale across multiple engines in the cluster, the redundancy of KV caches across engines introduces substantial inefficiencies. Repeated computations of common prompt prefixes waste GPU cycles and HBM bandwidth. AIBrix solves this challenge by enabling efficient **cross-engine KV reuse** through a high-performance, shared distributed KV cache, optimizing resource utilization at scale.

.. figure:: ../assets/images/aibrix-infinistore-arch-overview.png
  :alt: aibrix-infinistore-arch-overview
  :width: 100%
  :align: center


Adding New KVCache Backends
---------------------------

New KVCache backends can be easily added by implementing the ``Connector`` interface:

.. code-block:: python
  :linenos:

  @dataclass
  class ConnectorFeature:
      """The features of the kv cache connector.
      Args:
          mput_mget: Whether the kv cache connector supports mput/mget
          prefetch: Whether the kv cache connector supports prefetch.
          rdma: Whether the kv cache connector supports RDMA.
      """
  
      mput_mget: bool = False
      prefetch: bool = False
      rdma: bool = False
  
  
  @dataclass
  class ConnectorRegisterDescriptor:
      """The register descriptor"""
  
      pass
  
  
  class Connector(Generic[K, V]):
      """Connector interface."""
  
      @classmethod
      @abstractmethod
      def from_envs(cls, conn_id: str, executor: Executor, **kwargs):
          """Create a connector from environment variables."""
          raise NotImplementedError
  
      @property
      @abstractmethod
      def name(self) -> str:
          raise NotImplementedError
  
      @property
      @abstractmethod
      def feature(self) -> ConnectorFeature:
          """Get the feature of the connector.
          Returns:
              The feature of the kv cache service.
          """
          raise NotImplementedError
  
      @abstractmethod
      def open(self) -> Status:
          """Open a connection."""
          raise NotImplementedError
  
      @abstractmethod
      def close(self) -> Status:
          """Close a connection."""
          raise NotImplementedError
  
      async def prefetch(self, keys: Sequence[K]) -> None:
          """Prefetch a list of keys.
          Args:
              keys: The keys of the kv tensors.
          """
          pass
  
      @abstractmethod
      async def exists(self, key: K) -> Status:
          """Check if key is in the store."""
          raise NotImplementedError
  
      @abstractmethod
      async def get(self, key: K, mr: MemoryRegion) -> Status:
          """Get a value.
          Args:
              key: The key of the kv tensor.
              mr: The memory region to place the fetched kv tensor.
          Returns:
              The status of the get operation.
          """
          raise NotImplementedError
  
      @abstractmethod
      async def put(self, key: K, mr: MemoryRegion) -> Status:
          """Put a key value pair.
          Args:
              key: The key of the kv cache.
              mr: The memory region holding the kv tensors.
          Returns:
              The status of the put operation.
          """
          raise NotImplementedError
  
      def register_slabs(self, slabs: List[torch.Tensor]) -> Status:
          """Register slabs with backend-specific register function.
          Args:
              slabs: slabs to be registered.
          Returns:
              Status of the register operation.
          """
          raise NotImplementedError
  
      def get_batches(
          self,
          keys: Sequence[K],
          mrs: Sequence[MemoryRegion],
          batch_size: int,
      ) -> Sequence[Sequence[Tuple[K, MemoryRegion]]]:
          """Get a list of key MR batches that is used for mput and mget
          operations.
  
          Args:
              keys: The keys of the kv tensors.
              mrs: Memory regions holding the kv tensors.
              batch_size: The maximum number of key MR pairs in a batch.
          Returns:
              List of key MR batches.
          """
          raise NotImplementedError
  
      async def mget(
          self, keys: Sequence[K], mrs: Sequence[MemoryRegion]
      ) -> Sequence[Status]:
          """MGet a list of values. This function is optional and only connectors
          have mput_mget feature enabled can implement this function.
          Args:
              keys: The keys of the kv tensors.
              mrs: Memory regions to hold the fetched kv tensors.
          Returns:
              List of statuses.
          """
          raise NotImplementedError
  
      async def mput(
          self, keys: Sequence[K], mrs: Sequence[MemoryRegion]
      ) -> Sequence[Status]:
          """MPut a list of key value pairs. This function is optional and only
          connectors have mput_mget feature enabled can implement this function.
          Args:
              keys: The keys of the kv tensors.
              mrs: Memory regions holding the kv tensors.
          Returns:
              List of statuses.
          """
          raise NotImplementedError
  
      @abstractmethod
      async def delete(self, key: K) -> Status:
          """Delete a key.
          Args:
              key: The key of the kv cache.
          Returns:
              The status of the delete operation.
          """
          raise NotImplementedError
  
Please refer to the `existing connectors <https://github.com/vllm-project/aibrix/tree/main/python/aibrix_kvcache/aibrix_kvcache/l2/connectors>`_ for more details.
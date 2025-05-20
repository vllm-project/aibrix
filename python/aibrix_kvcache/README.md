# AIBrix KV Cache Offloading Framework for Cross-Engine KV Reuse
AIBrix KV cache offloading framework provides several common functionalities for cross-engine KV reuse use cases:

**Tensor Parallelism Aware Management**: When inference engine (e.g., vLLM) uses tensor parallelism, each participating engine instance fetches KV tensors independently from the cache backend. In case of cache misses, before proceeding with prefill computation, participants must align the potentially different number of KV tensors fetched from the external KV cache service to ensure a consistent view .

**Embedded Cache w/ CPU Memory**: To meet performance requirements, it's common to have a small CPU memory-based cache embedded in the engine to avoid frequently accessing remote cache backends.

**Selective KV Cache Offloading**: Enables fine-grained control over offloading strategies and thus is crucial in optimizing performance across diverse deployment environments:
1. Many cloud providers and companies deploy lower-end GPU instances without high-speed interconnects like RDMA, suited for tasks related to 7B/8B models running on 24/32GiB GPU cards. In these setups, GPUs within the same instance (typically 8-16 GPUs) share a single VPC NIC, leading to significant network bandwidth contention. Selective KV cache offloading (e.g., only offloading KV tensors identified by the employed eviction policy as hot rather than offloading all KV tensors) helps mitigate this issue by reducing unnecessary data transfers and conserving limited network bandwidth.
2. Even in high-performance environments with RDMA-equipped GPUs, selective KV cache offloading can enhance efficiency by limiting the PCIe bandwidth consumed by remote data movement. While RDMA enables low-latency, high-bandwidth communication, remote data access still incurs higher latency than local memory access. By leveraging selective KV offloading, the framework reduces the frequency of remote data transfers, preserving PCIe bandwidth and ensuring that local memory access remains the preferred data pathway.
To achieve selective KV cache offloading, we introduce an eviction policy layer that can be extended and customized with advanced offloading strategies to determine which KV tensors should be offloaded. Within this layer, multiple callbacks are available to support different offloading modes, including offloading all KV tensors, only hot KV tensors, or only cold KV tensors, with the definition of "hot" and "cold" being determined by the specific eviction policy in use. In this initial PR, the framework will provide built-in support for LRU, FIFO, and S3FIFO eviction policies.

## Quick Start
### Installation
AIBrix KV cache offloading framework can be installed by `pip`.

```sh
pip install aibrix-kvcache
```

## Contributing
We welcome contributions from the community! Check out our [contributing guidelines](https://github.com/vllm-project/aibrix/blob/main/CONTRIBUTING.md) to see how you can make a difference.

### Build from source

```bash
# This may take several minutes
pip install -e .
```

### Lint, Format and Type Check

Before contribute your code, please run the following commands to ensure that your code passes the tests and linting checks.

```bash
# install basic dependencies
poetry install --no-root --with dev
# or, install basic dependencies + torch
poetry install --no-root --with dev --extras "torch"
# or, install basic dependencies + infinistore (which will also install torch since infinistore depends on torch)
poetry install --no-root --with dev --extras "infinistore"
# or, install basic dependencies + all extras
poetry install --no-root --with dev --all-extras

# linting, formatting and type checking
bash ./scripts/format.sh
```

## License

AI Runtime is licensed under the [APACHE License](https://github.com/vllm-project/aibrix/LICENSE.md).

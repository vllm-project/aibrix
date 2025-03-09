# AIBrix

Welcome to AIBrix, an open-source initiative designed to provide essential building blocks to construct scalable GenAI inference infrastructure. AIBrix delivers a cloud-native solution optimized for deploying, managing, and scaling large language model (LLM) inference, tailored specifically to enterprise needs.

## Key Features

The initial release includes the following key features:

- **LLM Gateway and Routing**: Efficiently manage and direct traffic across multiple models and replicas.
- **High-Density LoRA Management**: Streamlined support for lightweight, low-rank adaptations of models.
- **Distributed Inference**: Scalable architecture to handle large workloads across multiple nodes.
- **LLM App-Tailored Autoscaler**: Dynamically scale inference resources based on real-time demand.
- **Unified AI Runtime**: A versatile sidecar enabling metric standardization, model downloading, and management.
- **Distributed KV Cache**: Enables high-capacity, cross-engine KV reuse.
- **Cost-efficient Heterogeneous Serving**: Enables mixed GPU inference to reduce costs with SLO guarantees.
- **GPU Hardware Failure Detection (TBD)**: Proactive detection of GPU hardware issues.

## Architecture

![aibrix-architecture-v1](docs/source/assets/images/aibrix-architecture-v1.jpeg)


## Quick Start

To get started with AIBrix, clone this repository and follow the setup instructions in the documentation. Our comprehensive guide will help you configure and deploy your first LLM infrastructure seamlessly.

```shell
# Local Testing
git clone https://github.com/aibrix/aibrix.git
cd aibrix

# Install nightly aibrix dependencies
kubectl create -k config/dependency

# Install nightly aibrix components
kubectl create -k config/default
```

Install stable distribution
```shell
# Install component dependencies
kubectl create -k "github.com/aibrix/aibrix/config/dependency?ref=v0.2.1"

# Install aibrix components
kubectl create -k "github.com/aibrix/aibrix/config/overlays/release?ref=v0.2.1"
```

## Documentation

For detailed documentation on installation, configuration, and usage, please visit our [documentation page](https://aibrix-aibrix.readthedocs-hosted.com/en/latest/).

## Contributing

We welcome contributions from the community! Check out our [contributing guidelines](https://github.com/aibrix/aibrix/CONTRIBUTING.md) to see how you can make a difference.

Slack Channel: https://vllm-dev.slack.com/archives/C07QP347J4D

## License

AIBrix is licensed under the [APACHE License](https://github.com/aibrix/aibrix/LICENSE.md).

## Support

If you have any questions or encounter any issues, please submit an issue on our [GitHub issues page](https://github.com/aibrix/aibrix/issues).

Thank you for choosing AIBrix for your GenAI infrastructure needs!

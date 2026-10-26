# AIBrix

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-Apache_2.0-blue.svg" alt="License"></a>
  <a href="https://aibrix.readthedocs.io/latest/"><img src="https://img.shields.io/badge/docs-latest-brightgreen.svg" alt="Documentation"></a>
  <a href="https://github.com/vllm-project/aibrix/releases/latest"><img src="https://img.shields.io/github/v/release/vllm-project/aibrix?label=release" alt="Release"></a>
  <a href="https://vllm-dev.slack.com/archives/C08EQ883CSV"><img src="https://img.shields.io/badge/slack-%23aibrix-4A154B.svg" alt="Slack"></a>
</p>

<p align="center">
  <a href="https://aibrix.readthedocs.io/latest/"><b>官方文档</b></a> ·
  <a href="https://aibrix.github.io/"><b>技术博客</b></a> ·
  <a href="https://arxiv.org/abs/2504.03648"><b>白皮书</b></a> ·
  <a href="https://x.com/vllm_project"><b>X</b></a> ·
  <a href="https://vllm-dev.slack.com/archives/C08EQ883CSV"><b>Slack 社区</b></a>
</p>

<p align="center">
  <a href="README.md"><b>English</b></a> · <b>简体中文</b>
</p>

AIBrix 是面向生产级大语言模型（LLM）推理的 Kubernetes 控制平面。您只需接入底层推理引擎（如 vLLM、SGLang 或 TensorRT-LLM），AIBrix 即可基于实时的引擎运行状态智能路由每个请求、根据推理核心指标执行弹性扩缩容，并统一管理基础模型、LoRA 适配器以及分布式 KV 缓存（KV Cache）。

当您需要同时运行大量模型或多个副本、混合调度异构 GPU，或者严格保障推理成本与延迟目标（SLO）时，AIBrix 是您的理想基础设施选择。

## 核心特性 (Features)

| 核心能力 | 功能说明 |
| --- | --- |
| [网关与智能路由 (Gateway and routing)](https://aibrix.readthedocs.io/latest/features/gateway-routing/gateway-plugins.html) | 提供 OpenAI 兼容的 API 接口。根据实时引擎状态（包括前缀缓存 Prefix-Cache 命中率、当前负载以及[预填充/解码分离 (Prefill/Decode)](https://aibrix.readthedocs.io/latest/features/gateway-routing/pd-disaggregation.html) 拓扑）动态选择最优 Pod，并为智能体（Agentic）工作负载提供会话亲和性（Session Affinity）支持。 |
| [多引擎统一接入 (Multi-engine)](https://aibrix.readthedocs.io/latest/features/model-serving/multi-engine.html) | 在同一个网关背后统一托管并提供 vLLM、SGLang 与 TensorRT-LLM 推理服务。 |
| [LoRA 动态加载 (LoRA)](https://aibrix.readthedocs.io/latest/features/model-serving/lora-dynamic-loading.html) | 将大量低秩适配器（LoRA Adapters）高密度打包并动态挂载至共享的基础模型 Pod 上。 |
| [模型生命周期管理 (Model lifecycle)](https://aibrix.readthedocs.io/latest/features/model-serving/model-claim.html) | 通过 `ModelClaim` 声明式管理模型，并借助 `ModelWarmup` [预加载容器镜像与模型权重](https://aibrix.readthedocs.io/latest/features/model-serving/model-warmup.html)，大幅提速扩容冷启动过程。 |
| [推理感知弹性伸缩 (Autoscaling)](https://aibrix.readthedocs.io/latest/features/scaling-performance/autoscaling/autoscaling.html) | 基于队列深度（Queue Depth）、KV Cache 利用率及 Token 吞吐量等底层引擎实时指标自动扩缩容实例副本。 |
| [分布式多机推理 (Distributed inference)](https://aibrix.readthedocs.io/latest/features/model-serving/multi-node-inference.html) | 借助 `StormService` 运行多节点、多角色（含 Prefill/Decode 分离架构）的大规模分布式推理部署。 |
| [分布式 KV 缓存 (Distributed KV cache)](https://aibrix.readthedocs.io/latest/features/scaling-performance/kvcache-offloading.html) | 支持 KV Cache 卸载（Offloading）并在不同推理引擎实例之间高效复用。 |
| [异构 GPU 混合调度 (Heterogeneous GPUs)](https://aibrix.readthedocs.io/latest/features/model-serving/heterogeneous-gpu.html) | 支持混合编排不同型号的 GPU，同时精准兼顾成本优化与服务质量目标（SLO）（实验性特性）。 |
| [推理运行时边车 (Runtime)](https://aibrix.readthedocs.io/latest/features/model-serving/runtime.html) | 以 Sidecar 容器形式负责拉取模型权重、动态加载与卸载 LoRA 适配器，并标准化统一各推理引擎的监控指标。 |

## 系统架构 (Architecture)

![AIBrix 架构图：包含将请求路由至引擎 Pod 的数据平面，以及负责模型生命周期、预热、多节点服务、弹性伸缩与可观测性的 Kubernetes 控制平面](docs/source/assets/images/aibrix-architecture-v2.png)

客户端请求首先到达兼容 OpenAI 协议的网关，并由数据平面直接转发至最优的推理引擎 Pod；与此同时，Kubernetes 控制器（控制平面）负责决策集群的目标运行状态——涵盖模型生命周期、冷启动预热、多节点拓扑编排、自动扩缩容以及全方位可观测性。详情请参阅[架构设计指南](https://aibrix.readthedocs.io/latest/designs/architecture.html)以深入了解数据平面与控制平面的协同机制。

## 快速开始 (Quick start)

以下命令会将 AIBrix（包括其依赖的 Envoy Gateway）安装到您当前 `kubectl` 上下文所指向的 Kubernetes 集群中。依赖包中同时包含了 KubeRay（仅在使用基于 Ray 的多节点分布式推理时需要）。

以下脚本默认安装最新正式版 [v0.8.0](https://github.com/vllm-project/aibrix/releases/tag/v0.8.0)。如需安装其他版本，请修改 `VERSION` 变量。

### 安装正式发布版本 (Install a release)

```shell
VERSION=v0.8.0

# 安装依赖组件（Envoy Gateway、KubeRay 等）
kubectl apply -f "https://github.com/vllm-project/aibrix/releases/download/${VERSION}/aibrix-dependency-${VERSION}.yaml" --server-side

# 独立安装 CRD，确保卸载 Operator 时不会误删用户的自定义资源
kubectl apply -f "https://github.com/vllm-project/aibrix/releases/download/${VERSION}/aibrix-core-crds-${VERSION}.yaml" --server-side

# 安装控制器、网关插件及配套支撑服务
kubectl apply -f "https://github.com/vllm-project/aibrix/releases/download/${VERSION}/aibrix-core-${VERSION}.yaml"

kubectl get pods -n aibrix-system
```

当所有 Pod 就绪后，即可部署模型并调用网关接口。[快速入门指南 (Quickstart)](https://aibrix.readthedocs.io/latest/getting_started/quickstart.html) 提供了基础模型部署清单（Manifest）以及兼容 OpenAI 格式的 `curl` 测试命令。如需使用 Helm 安装，请参阅[安装指南](https://aibrix.readthedocs.io/latest/getting_started/installation/installation.html)。

### 从源码仓库安装 (Install from this checkout)

`config/default` 中的镜像默认使用 `nightly` 标签。

```shell
git clone https://github.com/vllm-project/aibrix.git
cd aibrix

kubectl apply -k config/dependency --server-side
kubectl apply -k config/crd --server-side
kubectl apply -k config/default
```

## 文档导航 (Documentation)

完整文档托管于 [aibrix.readthedocs.io](https://aibrix.readthedocs.io/latest/)。

| 您的目标场景 | 推荐阅读入口 |
| --- | --- |
| 部署并运行第一个模型 | 先阅读[架构概览 (Overview)](https://aibrix.readthedocs.io/latest/getting_started/overview.html)，随后参考[快速入门 (Quickstart)](https://aibrix.readthedocs.io/latest/getting_started/quickstart.html) |
| 降低现有推理部署的运行成本 | [自动扩缩容 (Autoscaling)](https://aibrix.readthedocs.io/latest/features/scaling-performance/autoscaling/autoscaling.html)、[LoRA 动态加载](https://aibrix.readthedocs.io/latest/features/model-serving/lora-dynamic-loading.html)、[KV 缓存卸载](https://aibrix.readthedocs.io/latest/features/scaling-performance/kvcache-offloading.html)、[可观测性 (Observability)](https://aibrix.readthedocs.io/latest/production/observability.html) |
| 达成大规模吞吐或低延迟目标 | [预填充/解码分离 (Prefill/decode)](https://aibrix.readthedocs.io/latest/features/gateway-routing/pd-disaggregation.html)、[多节点分布式推理](https://aibrix.readthedocs.io/latest/features/model-serving/multi-node-inference.html)、[异构 GPU 调度](https://aibrix.readthedocs.io/latest/features/model-serving/heterogeneous-gpu.html) |

## 最新动态 (News)

### 版本发布 (Releases)

- **[2026-10-09]** [v0.8.0](https://github.com/vllm-project/aibrix/releases/tag/v0.8.0)
- **[2026-06-16]** [v0.7.0](https://github.com/vllm-project/aibrix/releases/tag/v0.7.0) · [发布博客](https://aibrix.github.io/posts/2026-06-16-v0.7.0-release/)
- **[2026-03-05]** [v0.6.0](https://github.com/vllm-project/aibrix/releases/tag/v0.6.0) · [发布博客](https://aibrix.github.io/posts/2026-03-03-v0.6.0-release/)
- **[2025-11-10]** [v0.5.0](https://github.com/vllm-project/aibrix/releases/tag/v0.5.0) · [发布博客](https://aibrix.github.io/posts/2025-11-10-v0.5.0-release/)

<details>
<summary>历史版本 (Older releases)</summary>

- **[2025-08-05]** [v0.4.0](https://github.com/vllm-project/aibrix/releases/tag/v0.4.0) · [发布博客](https://aibrix.github.io/posts/2025-08-04-v0.4.0-release/)
- **[2025-05-21]** [v0.3.0](https://github.com/vllm-project/aibrix/releases/tag/v0.3.0) · [发布博客](https://aibrix.github.io/posts/2025-05-21-v0.3.0-release/)
- **[2025-03-09]** [v0.2.1](https://github.com/vllm-project/aibrix/releases/tag/v0.2.1) 新增 DeepSeek-R1 全量权重部署支持与网关稳定性修复 · [技术博客](https://aibrix.github.io/posts/2025-03-10-deepseek-r1/)
- **[2025-02-19]** [v0.2.0](https://github.com/vllm-project/aibrix/releases/tag/v0.2.0) · [发布博客](https://aibrix.github.io/posts/2025-02-05-v0.2.0-release/)
- **[2024-11-13]** [v0.1.0](https://github.com/vllm-project/aibrix/releases/tag/v0.1.0) · [发布博客](https://aibrix.github.io/posts/2024-11-12-v0.1.0-release/)

</details>

### 演讲与分享 (Talks)

- **[2025-11-12]** KubeCon 北美主旨演讲 (Keynote)：[《Kubernetes 原生生成式 AI 推理基础设施》](https://www.youtube.com/watch?v=7KHenRXNGAw&t=875s)
- **[2025-06-10]** KubeCon 中国技术分享：[《面向 vLLM 的高性价比可扩展 Kubernetes 控制平面》](https://kccncchn2025.sched.com/event/1x5im/introducing-aibrix-cost-effective-and-scalable-kubernetes-control-plane-for-vllm-jiaxin-shan-liguang-xie-bytedance)
- **[2025-04-04]** KubeCon 欧洲主旨演讲（与 Google 联合发表）：[《Kubernetes 中的 LLM 感知负载均衡》](https://kccnceu2025.sched.com/event/1txC7/keynote-llm-aware-load-balancing-in-kubernetes-a-new-era-of-efficiency-clayton-coleman-distinguished-engineer-google-jiaxin-shan-software-engineer-bytedance)
- **[2025-03-30]** ASPLOS'25 研讨会：[《面向系统研究的大规模 LLM 推理基础设施》](https://docs.google.com/presentation/d/1YDVsPFTIgGXnROGaJ1VKuDDAB4T5fzpE/edit)

## 开源社区 (Community)

非常欢迎参与项目贡献！请阅读[贡献指南](./CONTRIBUTING.md)，加入 vLLM Slack 工作区的 [#aibrix 频道](https://vllm-dev.slack.com/archives/C08EQ883CSV)参与讨论，或发送邮件至 [maintainers@aibrix.ai](mailto:maintainers@aibrix.ai)。

缺陷反馈与功能请求请提交至 [GitHub Issues](https://github.com/vllm-project/aibrix/issues)。AIBrix 基于 [Apache 2.0 许可证](LICENSE)开源。

---

> _注：本简体中文文档由社区志愿者翻译维护。若与英文原版存在差异，请以 [README.md](README.md) 为准。_

## PS Performance Benchmark

This setup is used to reproduce the PS benchmark. Although the benchmark is carefully crafted to highlight the strengths of the PS use case—with configurations heavily skewed in its favor—we still run it against the AIBrix setup for comparison.
However, please note that this is a narrow and selective case, not representative of a comprehensive or production-grade workload. It only covers one specific execution path.

> Note: we will publish a comprehensive benchmark scripts and datasets soon.

## Manifest Preparation

We already generated the experiment Yaml, If you like to generate from scratch, follow the steps below.

> Note:
>
> 1. The scripts has been verified to be working on VKE, If you need to run on other clouds, you still need to make some changes.
> 2. Due to testing environments difference, default yaml use `aibrix-container-registry-cn-beijing.cr.volces.com`,
you can remove this prefix to directly fetch images from dockerhub.


| Experiment Name           | Gateway     | Routing Algorithm   | vLLM Version            | KV Cache                    | Explanation                                                                          |
|---------------------------|-------------|---------------------|-------------------------|-----------------------------|--------------------------------------------------------------------------------------|
| aibrix_kvcache_0.6.1.yaml | AIBrix      | Kubernetes          | 0.6.1-aibrix-optimized  | AIBrix client + distributed | AIBrix deployment with KV cache enabled; used to test distributed KV cache behavior. |
| aibrix_naive.yaml         | AIBrix      | Kubernetes          | 0.7.0-upstream          | w/o                         | AIBrix baseline setup without optimizations; used as a naive comparison point.       |
| k8s_stack.yaml            | K8S Service | Kubernetes          | 0.7.0-upstream          | w/o                         | General Kubernetes stack configuration for running inference workloads. K8s Service  |
| ps_k8s_stack.yaml         | PS Router   | Session cache aware | 0.7.0-upstream          | w/o                         | Helm-rendered YAML for the Production Stack setup; includes Router, not just k8s svc |
| ps_stack.yaml             | PS Router   | Session cache aware | 0.7.0-lmcache-optimized | LM Cache Client lib         | PS full Setup with session aware router and LMCache.                                 |


### AIBrix control plane

Please follow the [installation guidance](https://aibrix.readthedocs.io/latest/getting_started/installation/installation.html) to install AIBrix.

> Note: for production setup, we recommend 2C8G resources for gateway-plugin and envoyproxy pod.

You can make the resource changes in following way:
```bash
kubectl apply -k config/overlays/release
```

### Generate All YAML Files from Scratch

- Create raw yaml files
   - PS yaml is generated from helm chart.
   - AIBrix yaml is from official documentation
- Modify YAMLs Based on Environment Needs
 - Node Affinity: Adjust for clusters that have other GPUs (e.g., test environments).
 - Model Persistence & Downloading: Update volume settings and model download steps (e.g., for TOS & VKE usage).
 - Parameters: Tune engine-related settings and resources such as `--max-model-len`, `VLLM_RPC_TIMEOUT`, `memory limits`, etc.
- Ensure Completeness: Each YAML file should include all necessary components for deployment.

Feel free to use text diff to compare the differences.

> note: raw yaml source

Scripts to render PS yaml, please check [source codes](https://github.com/vllm-project/production-stack/tree/main/benchmarks/multi-round-qa) here for stack.yaml and naive.yaml

```
helm template vllm vllm/vllm-stack -f stack.yaml > ps_stack.yaml
helm template vllm vllm/vllm-stack -f naive.yaml > ps_k8s_stack.yaml
```
AIBrix-related YAMLs can be found at: https://aibrix.readthedocs.io/latest/features/distributed-kv-cache.html

## Benchmark

### Client setup

It's recommended to run the benchmark directly within the same inference cluster instead of using `port-forward`, as the latter may lead to frequent errors such as:

```bash
E0322 23:28:41.254372   62759 portforward.go:351] error creating error stream for port 30080 -> 8000: Timeout occurred
```

```
kubectl apply -f client.yaml
```

Copy the benchmark scripts and setup the client pod.

```bash
# setup environments
sudo apt update && sudo apt install vim curl -y

# install benchmark dependencies.
# append `-i https://pypi.tuna.tsinghua.edu.cn/simple` in CN env.
pip3 install openai pandas tqdm

# Prepare the benchmark codes.
# https://raw.githubusercontent.com/vllm-project/production-stack/refs/heads/main/benchmarks/multi-round-qa/multi-round-qa.py
# https://raw.githubusercontent.com/vllm-project/production-stack/refs/heads/main/benchmarks/multi-round-qa/utils.py
# https://raw.githubusercontent.com/vllm-project/production-stack/refs/heads/main/benchmarks/multi-round-qa/run.sh
vim multi-round-qa.py
vim utils.py
vim run.sh
```

> Note: in run.sh, use wildcast `*naive*`
> ```bash
> if [[ "$KEY" == "*naive*" ]]; then
>    QPS_VALUES=(0.1 0.5 0.9 1.3 1.7 2.1 2.5 2.9 3.3 3.7 4.1)
> else
>    QPS_VALUES=(4.1 3.7 3.3 2.9 2.5 2.1 1.7 1.3 0.9 0.5 0.1)
> fi
> ```

### Sample requests

1. For each experiment, make sure to delete all existing YAMLs and recreate them from scratch.
2. Some YAML files do not define readiness probes. Even if the pod status is marked as `Ready`, the application inside may not be fully initialized. Always wait until the application is actually ready before proceeding.
3. Run a sample curl command to verify that the service has started successfully.


```bash
# only kubernetes naive use k8s service for engine
curl -v http://vllm-engine-service:80/v1/completions \
-H "Content-Type: application/json" \
-d '{
    "model": "meta-llama/Llama-3.1-8B-Instruct",
    "prompt": "San Francisco is a",
    "max_tokens": 128,
    "temperature": 0
}'

# ps router is the entrypoint for ps related experiments
curl -v http://vllm-router-service:80/v1/completions \
-H "Content-Type: application/json" \
-d '{
"model": "meta-llama/Llama-3.1-8B-Instruct",
"prompt": "San Francisco is a",
"max_tokens": 128,
"temperature": 0
}'

# aibrix gateway is the entrypoint for aibrix related experiments,
# note the model name is different.
curl -v http://envoy-aibrix-system-aibrix-eg-903790dc.envoy-gateway-system:80/v1/completions \
-H "Content-Type: application/json" \
-d '{
"model": "llama3-1-8b",
"prompt": "San Francisco is a",
"max_tokens": 128,
"temperature": 0
}'
```

### Benchmark

It's time to run benchmark results now.

```bash
bash run.sh meta-llama/Llama-3.1-8B-Instruct http://vllm-engine-service:80/v1/ naive
bash run.sh meta-llama/Llama-3.1-8B-Instruct http://vllm-router-service:80/v1/ ps_naive
bash run.sh meta-llama/Llama-3.1-8B-Instruct http://vllm-router-service:80/v1/ ps_stack_high_low
bash run.sh meta-llama/Llama-3.1-8B-Instruct http://vllm-router-service:80/v1/ ps_stack_low_high
bash run.sh llama3-1-8b http://envoy-aibrix-system-aibrix-eg-903790dc.envoy-gateway-system:80/v1/ aibrix_naive_7
bash run.sh llama3-1-8b http://envoy-aibrix-system-aibrix-eg-903790dc.envoy-gateway-system:80/v1/ aibrix_high_low
```

> Note:high_low means use QPS high to low, this is a trick in PS run.sh, it tried to show better results in Low QPS.
> While, for ps_stack_low_high, you need to modify the scripts a little bit. Do not forget to change it back in later experiments

```bash
> if [[ "$KEY" == "*naive*" ]]; then
>    QPS_VALUES=(0.1 0.5 0.9 1.3 1.7 2.1 2.5 2.9 3.3 3.7 4.1)
> else
>    QPS_VALUES=(4.1 3.7 3.3 2.9 2.5 2.1 1.7 1.3 0.9 0.5 0.1)
> fi
>
```

Collect results from the client pod template.

```bash
# inside the pod
mkdir benchmark_output
mv *.csv benchmark_output
tar -cvf benchmark_output.tar benchmark_output/

# outside pod
kubectl cp benchmark-client:/home/ray/benchmark_output.tar /tmp/benchmark_output.tar
```

## Plot the results

Put `plot.py` under the within the same result of benchmark output folder and run `python plot.py`.
remember to change the experiment name and file mappings in `experiments` list.

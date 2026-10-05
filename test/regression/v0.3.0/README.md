## PS Performance Benchmark Regression Test for v0.3.0

We prefer to use AIBrix benchmark for performance testing. Since we spend some efforts in v0.2.0 release to reproduce this benchmark, we just use it for regression testing.

## Manifest Preparation

We already generated the experiment Yaml, If you like to generate from scratch, follow the steps below.

> Note:
> 1. The scripts have been verified to be working on VKE, If you need to run on other clouds, you still need to make some changes.
> 2. Due to testing environments difference, default yaml use `aibrix-cn-beijing.cr.volces.com`,
you can remove this prefix to directly fetch images from dockerhub.


| Experiment Name                | Gateway     | Routing Algorithm    | vLLM Version            | KV Cache            | Explanation                                                                         |
|--------------------------------|-------------|----------------------|-------------------------|---------------------|-------------------------------------------------------------------------------------|
| k8s_stack.yaml                 | K8S Service | Kubernetes           | 0.8.5-upstream          | w/o                 | General Kubernetes stack configuration for running inference workloads. K8s Service |
| ps_stack.yaml                  | PS Router   | Session cache aware  | 0.8.5-lmcache-optimized | LM Cache Dram       | PS full Setup with session aware router and LMCache.                                |
| aibrix_naive.yaml              | AIBrix      | Kubernetes HTTPRoute | 0.8.5-upstream          | w/o                 | AIBrix baseline setup without optimizations; used as a naive comparison point.      |
| aibrix_naive_prefix_cache.yaml | AIBrix      | Prefix Cache         | 0.8.5-upstream          | w/o                 | AIBrix baseline with routing algorithm, used to compare prefix cache effectiveness. |
| aibrix_kvcache_dram.yaml       | AIBrix      | Prefix Cache         | 0.8.5-aibrix-optimized  | AIBrix KVCache Dram | AIBrix deployment with KV cache L1 (Dram) Cache enabled.                            |
| aibrix_kvcache_external.yaml   | AIBrix      | Prefix Cache         | 0.8.5-aibrix-optimized  | InfiniStore         | AIBrix deployment with KV cache L2 (Infinistore) Enabled.                           |


### AIBrix control plane

Please follow the [installation guidance](https://aibrix.readthedocs.io/latest/getting_started/installation/installation.html) to install AIBrix.

> Note: for production setup, we recommend 2C8G resources for gateway-plugin and envoyproxy pod.

You can make the resource changes in following way:
```bash
# update envoy gateway              
kubectl edit deployment envoy-aibrix-system-aibrix-eg-903790dc -n envoy-gateway-system
# update gateway plugin
kubectl edit deployment aibrix-gateway-plugins -n aibrix-system

# update code
            requests:
              cpu: 2
              memory: 8Gi
            limits:
              cpu: 2
              memory: 8Gi
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

Scripts to render PS yaml, please check [source codes](https://github.com/vllm-project/production-stack/tree/main/benchmarks/multi-round-qa) here for stack.yaml and naive.yaml

```
helm template vllm vllm/vllm-stack -f lmcache_helm_stack.yaml > ps_stack.yaml
helm template vllm vllm/vllm-stack -f lmcache_helm_naive.yaml > ps_k8s_stack.yaml
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

Copy the benchmark scripts and setup the client pod inside the pod.

```bash
# setup environments
sudo apt update && sudo apt install vim curl -y

# install benchmark dependencies.
# append `-i https://pypi.tuna.tsinghua.edu.cn/simple` in CN env.
pip3 install openai pandas tqdm -i https://mirrors.ivolces.com/pypi/simple/

# Prepare the benchmark codes.
# https://raw.githubusercontent.com/vllm-project/production-stack/refs/heads/main/benchmarks/multi-round-qa/multi-round-qa.py
# https://raw.githubusercontent.com/vllm-project/production-stack/refs/heads/main/benchmarks/multi-round-qa/utils.py
# https://raw.githubusercontent.com/vllm-project/production-stack/refs/heads/main/benchmarks/multi-round-qa/run.sh
vim multi-round-qa.py
vim utils.py
vim run.sh
chmod +x run.sh
```
> Note: sometimes, content paste inside the pod may be blocked. in that case, you can download scripts locally and cp into container.
>
> ```
> ➜  /tmp kubectl cp run.sh benchmark-client:/home/ray/
> ➜  /tmp kubectl cp utils.py benchmark-client:/home/ray/
> ➜  /tmp kubectl cp multi-round-qa.py benchmark-client:/home/ray/
> ```

In run.sh, use wildcast `*naive*` for some naive testing case.

> Note: this is a trick in PS run.sh, it tried to show better results in Low QPS.
> ```bash
> if [[ $KEY =~ naive ]]; then
>    QPS_VALUES=(0.1 0.5 0.9 1.3 1.7 2.1 2.5 2.9 3.3 3.7 4.1)
> else
>    QPS_VALUES=(4.1 3.7 3.3 2.9 2.5 2.1 1.7 1.3 0.9 0.5 0.1)
> fi
> ```


### Send sample requests

1. For each experiment, make sure to delete all existing YAMLs and recreate them from scratch.
2. If the test needs routing algorithm, it's better to update `kubectl edit deployment aibrix-gateway-plugins -n aibrix-system` and delete pod to each round to clean up the cache information.
    ```yaml
            - name: ROUTING_ALGORITHM
              value: prefix-cache
    ```
3. Before starting the test:
   - Some YAML files do not define readiness probes. Even if the pod status is marked as `Ready`, the application inside may not be fully initialized. Always wait until the application is actually ready before proceeding. 
   - Run a sample `curl` command to verify that the service has started successfully.


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
"prompt": "San Francisco is a city known for its rich history, vibrant culture, and stunning geography. Nestled between the Pacific Ocean and the San Francisco Bay, this iconic city has played a pivotal role in American history, from the Gold Rush era to the rise of the tech industry in Silicon Valley.The city’s defining feature is perhaps the Golden Gate Bridge, an engineering marvel and a global symbol of San Francisco. But beyond its landmarks, San Francisco embodies a unique blend of diversity and innovation. Its neighborhoods—from the artistic Mission District to the upscale Pacific Heights—each have their own character, representing a microcosm of the city’s larger spirit.Throughout the decades, San Francisco has attracted dreamers, entrepreneurs, artists, and activists. During the 1960s, it was the heart of the counterculture movement. In the 1980s and 90s, it became a focal point for the LGBTQ+ rights movement. More recently, it has been both celebrated and criticized as the epicenter of the tech boom, with companies like Twitter, Uber, and Airbnb calling it home.But the story of San Francisco is also one of tension and transformation. The influx of wealth and talent has created stark economic disparities, housing shortages, and debates over the city’s identity. The contrast between historic Victorians and gleaming glass towers mirrors the ongoing struggle between preservation and progress. The city is also known for its climate, famously described as “the coldest winter I ever spent was a summer in San Francisco,” a quote often (incorrectly) attributed to Mark Twain. Its microclimates mean you can experience fog in the Sunset district while the Mission basks",
"max_tokens": 128,
"temperature": 0
}'
```

### Benchmark

It's time to run benchmark results now.

```bash
./run.sh meta-llama/Llama-3.1-8B-Instruct http://vllm-engine-service:80/v1/ naive
./run.sh meta-llama/Llama-3.1-8B-Instruct http://vllm-router-service:80/v1/ ps_stack
./run.sh llama3-1-8b http://envoy-aibrix-system-aibrix-eg-903790dc.envoy-gateway-system:80/v1/ aibrix_naive
./run.sh llama3-1-8b http://envoy-aibrix-system-aibrix-eg-903790dc.envoy-gateway-system:80/v1/ aibrix_naive_prefix_routing
./run.sh llama3-1-8b http://envoy-aibrix-system-aibrix-eg-903790dc.envoy-gateway-system:80/v1/ aibrix_kvcache_dram
./run.sh llama3-1-8b http://envoy-aibrix-system-aibrix-eg-903790dc.envoy-gateway-system:80/v1/ aibrix_kvcache_external
```

we can use `nohup command > xxx.log 2>&1 &`  to run job in background to prevent potential issues.

Once all benchmark is done, use following command to collect results from the client pod template.

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

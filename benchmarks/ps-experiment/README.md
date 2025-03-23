## Experiment to run performance benchmark against other solutions



## Manifest

## oPtion 1. pS stack
```bash
helm template vllm vllm/vllm-stack -f stack.yaml > ps_stack.yaml

```

## Option 2. ps router + vLLM

```dockerfile
helm template vllm vllm/vllm-stack -f naive.yaml > ps_k8s_stack.yaml
```

## Option 3. kubernetes service + vLLM

```dockerfile

```

## Option 4. AIBrix + vLLM 0.6.1

```dockerfile

```


## workload

## Option 1. PS Stack

```bash
kubectl port-forward svc/vllm-router-service 30080:80
bash run.sh meta-llama/Llama-3.1-8B-Instruct http://localhost:30080/v1/ stack
```

## Option 2. PS Router + vLLM

```bash
kubectl port-forward svc/vllm-router-service 30080:80
bash run.sh meta-llama/Llama-3.1-8B-Instruct http://localhost:30080/v1/ native
```

## Option 3. AIBrix + vLLM 0.6.1

```bash
bash run.sh llama3-1-8b http://localhost:8888/v1/ aibrix
```


## Modifications

1. change to 
          - "--max-model-len"
          - "32000"

2. only ps stack has 
```
          - name: VLLM_RPC_TIMEOUT
            value: "1000000"
```

3. kv cache cpu to 16 CPU
   
   ```
   cpu: 4000m
   ```

4. add aibrix resources
```dockerfile
              cpu: "10"
              memory: "150G"
```

5. initContainer
6. affinity
7. other arguments


```
curl -v http://localhost:30080/v1/completions \
    -H "Content-Type: application/json" \
    -d '{
        "model": "meta-llama/Llama-3.1-8B-Instruct",
        "prompt": "San Francisco is a",
        "max_tokens": 128,
        "temperature": 0
    }'
```

```dockerfile
E0322 23:28:41.254372   62759 portforward.go:351] error creating error stream for port 30080 -> 8000: Timeout occurred
```

##
curl -v http://vllm-engine-service:80/v1/completions \
-H "Content-Type: application/json" \
-d '{
    "model": "meta-llama/Llama-3.1-8B-Instruct",
    "prompt": "San Francisco is a",
    "max_tokens": 128,
    "temperature": 0
}'

curl -v http://vllm-router-service:80/v1/completions \
-H "Content-Type: application/json" \
-d '{
"model": "meta-llama/Llama-3.1-8B-Instruct",
"prompt": "San Francisco is a",
"max_tokens": 128,
"temperature": 0
}'

curl -v http://envoy-aibrix-system-aibrix-eg-903790dc.envoy-gateway-system:80/v1/completions \
-H "Content-Type: application/json" \
-d '{
"model": "llama3-1-8b",
"prompt": "San Francisco is a",
"max_tokens": 128,
"temperature": 0
}'


```dockerfile

```
now, it looks much better


```dockerfile
bash run.sh meta-llama/Llama-3.1-8B-Instruct http://vllm-engine-service:80/v1/ naive
bash run.sh meta-llama/Llama-3.1-8B-Instruct http://vllm-router-service:80/v1/ ps_naive
bash run.sh meta-llama/Llama-3.1-8B-Instruct http://vllm-router-service:80/v1/ ps_stack_high_low # remember to delete and recreate
bash run.sh llama3-1-8b http://envoy-aibrix-system-aibrix-eg-903790dc.envoy-gateway-system:80/v1/ aibrix_naive_7
bash run.sh llama3-1-8b http://envoy-aibrix-system-aibrix-eg-903790dc.envoy-gateway-system:80/v1/ aibrix_high_low
bash run.sh llama3-1-8b http://envoy-aibrix-system-aibrix-eg-903790dc.envoy-gateway-system:80/v1/ aibrix_low_high
```


kubectl cp benchmark-client:/home/ray/benchmark_output.tar ~/workspace/aibrix/benchmark_output.tar


```dockerfile
mkdir benchmark_output
mv *.csv benchmark_output
tar -cvf benchmark_output.tar benchmark_output/
```
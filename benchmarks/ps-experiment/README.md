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
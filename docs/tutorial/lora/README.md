# Lora Model Adapter Example 

## Development testing


## Experiments

1. Builder mocked base model image
```dockerfile
docker build -t aibrix/vllm:v0.1.0 -f Dockerfile .
```

2. Deploy mocked model image
```shell
kubectl apply -f deployment.yaml
```

3. Load models

Ssh into the pod and run following commands.

```
curl -X POST http://localhost:8000/v1/load_lora_adapter \
     -H "Content-Type: application/json" \
     -d '{"lora_name": "text2sql-lora-1", "lora_path": "meta-llama/Llama-2-7b-hf"}'
```

4. Unload Model

```shell
curl -X POST http://localhost:8000/v1/unload_lora_adapter \
     -H "Content-Type: application/json" \
     -d '{"lora_name": "text2sql-lora-1"}'
```

Verified! The model is loaded and unloaded successfully and pod annotations are updated successfully.

5. Deploy the controller and apply the `model_adapter.yaml`

```
kubectl apply -f model_adapter.yaml
```

# Lora Model Adapter Example 

## Development testing


## Experiments

1. Builder mocked base model image
```dockerfile
docker build -t aibrix/vllm-mock:nightly -f Dockerfile .
```

2. Deploy mocked model image
```shell
kubectl create -k config/mock
```

3. Load models

Ssh into the pod and run following commands.

```
curl -X POST http://localhost:8000/v1/load_lora_adapter \
     -H "Content-Type: application/json" \
     -d '{"lora_name": "text2sql-lora-1", "lora_path": "yard1/llama-2-7b-sql-lora-test"}'
```

```
# check available models
curl http://localhost:8000/v1/models | jq .
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
kubectl apply -f development/tutorials/lora/model_adapter.yaml
```


## Test python app separately

```shell
curl http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -d '{
     "model": "text2sql-lora-1",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'
```

```shell
curl https://localhost:8000/v1/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -d '{
    "model": "text2sql-lora-1",
    "prompt": "Say this is a test",
    "max_tokens": 7,
    "temperature": 0
  }'
```

# request via gateway without routing strategy
```shell
curl -v http://localhost:8888/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any_key" \
  -d '{
     "model": "text2sql-lora-1",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'
```

# request via gateway with routing strategy
```shell
curl -v http://localhost:8888/v1/chat/completions \
  -H "user: your-user-name" \
  -H "model: text2sql-lora-1" \
  -H "routing-strategy: least-request" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any_key" \
  -d '{
     "model": "text2sql-lora-1",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'
```
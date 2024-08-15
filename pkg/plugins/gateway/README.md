

# Install backed storage for persist rpm/tpm configuration
kubectl apply -f redis.yaml

# Add rpm/tpm config 
kubectl exec -it redis-master-<pod-name> -- redis-cli

set aibrix:<user-name>_TPM_LIMIT 100
set aibrix:<user-name>_RPM_LIMIT 10

# Install extension proc
make build && make apply

# Test requests
```shell
curl -v http://localhost:8888/v1/chat/completions \
  -H "user: varun" \
  -H "model: llama2-70b" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any_key" \
  -d '{
     "model": "llama2-70b",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'
```
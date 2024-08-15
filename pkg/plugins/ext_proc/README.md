

kubectl apply -f redis.yaml


kubectl exec -it redis-master-767bcb955d-z6rd8 -- redis-cli

set varun_TPM_LIMIT 100
set varun_RPM_LIMIT 10

```shell
curl http://localhost:8888/v1/chat/completions \
  -H "user: varun" \
  -H "model: llama2-70b" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any_key" \
  -d '{
     "model": "llama2-70b",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'

curl http://localhost:8888/v1/chat/completions \
  -H "x-envoy-original-dst-host: 172.18.0.4:8000" \
  -H "user: varun" \
  -H "model: llama2-70b" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any_key" \
  -d '{
     "model": "llama2-70b",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'


curl http://localhost:8888/v1/chat/completions \
  -H "target-pod: 172.18.0.4:8000" \
  -H "user: varun" \
  -H "model: llama2-70b" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any_key" \
  -d '{
     "model": "llama2-70b",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'

curl http://localhost:8888/v1/chat/completions \
  -H "target-pod: 10.244.1.3" \
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
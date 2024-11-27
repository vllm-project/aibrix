# vLLM application simulator

## Run locally

Ensure that you have Python 3.10 installed on your system. Refer https://www.bitecode.dev/p/installing-python-the-bare-minimum
Create a virtual environment using venv module using python3.10 -m venv .venv
Activate the virtual environment using source .venv/bin/activate
Install the dependencies using python -m pip install -r requirements.txt
Run python app.py to start the server.
Run deactivate to deactivate the virtual environment

## Run in kubernetes

1. Build simulated base model image
```dockerfile
docker build -t aibrix/vllm-simulator:nightly -f Dockerfile .

# If you are using Docker-Desktop on Mac, Kubernetes shares the local image repository with Docker.
# Therefore, the following command is not necessary.
kind load docker-image aibrix/vllm-simulator:nightly
```

2. Deploy simulated model image
```shell
kubectl apply -f docs/development/simulator/deployment.yaml
kubectl -n aibrix-system port-forward svc/llama2-7b 8000:8000 1>/dev/null 2>&1 &
```

## Test python app separately

```shell
curl http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any_key" \
  -d '{
     "model": "llama2-7b",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }'
```

```shell
kubectl delete -f docs/development/simulator/deployment.yaml
```

## Test with envoy gateway

Add User:


Port forward to the User and Envoy service:
```shell
kubectl -n aibrix-system port-forward svc/aibrix-gateway-users 8090:8090 1>/dev/null 2>&1 &
kubectl -n envoy-gateway-system port-forward service/envoy-aibrix-system-aibrix-eg-903790dc 8888:80 1>/dev/null 2>&1 &
```

Add User
```shell
curl http://localhost:8090/CreateUser \
  -H "Content-Type: application/json" \
  -d '{"name": "your-user-name","rpm": 100,"tpm": 1000}'
```

Test request (ensure header model name matches with deployment's model name for routing)
```shell
curl -v http://localhost:8888/v1/chat/completions \
  -H "user: your-user-name" \
  -H "model: llama2-7b" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any_key" \
  -d '{
     "model": "llama2-7b",
     "messages": [{"role": "user", "content": "Say this is a test!"}],
     "temperature": 0.7
   }' &
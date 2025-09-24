# vLLM P/D Disaggregation Examples

Note:
- The examples in this directory are for demonstration purposes only. Feel free to use your own model path, rdma network, image etc instead.
- Routing solution in the examples uses vllm upstream sample router `disagg_proxy_server.py` images. AIBrix Envoy Routing examples will be updated shortly.
- In order to use this example, you need to build your own vLLM image with Nixl. Guidance is provided.
- AIBrix storm service support replica mode and pool mode, please refer to [AIBrix storm service]( mode, please refer to [AIBrix storm service](https://aibrix.readthedocs.io/latest/designs/aibrix-stormservice.html) for more details.
- `vke.volcengine.com/rdma`, `k8s.volcengine.com/pod-networks` and `NCCL_IB_GID_INDEX` are specific to Volcano Engine Cloud. Feel free to customize it for your own cluster.

## Configuration

### Build vLLM images with Nixl

```Dockerfile
FROM vllm/vllm-openai:v0.9.2

RUN pip install nixl==0.4.1 
```

```bash
docker build -t aibrix/vllm-openai:v0.9.2-cu128-nixl-v0.4.1
```

> Note: sample router has been included in the image. We do not need additional steps to build it. 

## Start the Router

Currently, the router is very simple and it relies on user to pass the prefill and decode IPs.
Launch the router `kubectl apply -f router.yaml`, ssh to the pod and launch the process.

Copy `disagg_proxy_server.py` into the container.

```bash
python3 disagg_proxy_server.py \
    --host localhost \
    --port 8000 \
    --prefiller-host 192.168.0.125,192.168.0.127 \
    --prefiller-port 8000 \
    --decoder-host 192.168.0.129,192.168.0.149 \
    --decoder-port 8000
```

### Run Query Example inside the Router

```bash
curl http://localhost:8000/v1/chat/completions \
-H "Content-Type: application/json" \
-d '{
    "model": "qwen3-8B",
    "messages": [
        {"role": "system", "content": "You are a helpful assistant."},
        {"role": "user", "content": "help me write a random generator in python"}
    ]
}'
```

```bash
curl http://localhost:8000/v1/completions \
    -H "Content-Type: application/json" \
    -d '{
        "model": "qwen3-8B",
        "prompt": "San Francisco is a",
        "max_tokens": 128,
        "temperature": 0
    }'
```

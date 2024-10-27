from flask import Flask, request, Response, jsonify
import time
from random import randint
import os
try:
    from kubernetes import client, config
except Exception as e:
    print(f"Failed to import kubernetes, skip: {e}")

app = Flask(__name__)
v1 = None

# Global storage for overridden values
overrides = {}

MODEL_NAME = 'llama2-70b'
DEPLOYMENT_NAME = os.getenv('DEPLOYMENT_NAME', 'llama2-70b')
NAMESPACE = os.getenv('NAMESPACE', 'aibrix-system')
DEFAULT_REPLICAS = int(os.getenv('DEFAULT_REPLICAS', '1'))

models = [
    {
        "id": "meta-llama/Llama-2-7b-hf",
        "object": "model",
        "created": 1715644056,
        "owned_by": "vllm",
        "root": "meta-llama/Llama-2-7b-hf",
        "parent": None,
        "permission": [
            {
                "id": "modelperm-cb1adf4457b2417e8c7770aadcffe4cc",
                "object": "model_permission",
                "created": 1715644056,
                "allow_create_engine": False,
                "allow_sampling": True,
                "allow_logprobs": True,
                "allow_search_indices": False,
                "allow_view": True,
                "allow_fine_tuning": False,
                "organization": "*",
                "group": None,
                "is_blocking": False
            }
        ]
    },
    {
        "id": "startup-default-lora",
        "object": "model",
        "created": 1715644056,
        "owned_by": "vllm",
        "root": "meta-llama/Llama-2-7b-hf",
        "parent": None,
        "permission": [
            {
                "id": "modelperm-6a01d79e4d0e452b94d52d2c2e8c8562",
                "object": "model_permission",
                "created": 1715644056,
                "allow_create_engine": False,
                "allow_sampling": True,
                "allow_logprobs": True,
                "allow_search_indices": False,
                "allow_view": True,
                "allow_fine_tuning": False,
                "organization": "*",
                "group": None,
                "is_blocking": False
            }
        ]
    }
]

@app.route('/v1/models', methods=['GET'])
def get_models():
    return jsonify({
        "object": "list",
        "data": models
    })

@app.route('/v1/load_lora_adapter', methods=['POST'])
def load_model():
    lora_name = request.json.get('lora_name')
    # Check if the model already exists
    if any(model['id'] == lora_name for model in models):
        return jsonify({"status": "success", "message": "Model already loaded"}), 200

    new_model = {
        'id': lora_name,
        'created': int(time.time()),
        'object': "model",
        'owned_by': "vllm",
        'parent': None,
        'root': request.json.get('lora_path')
    }

    models.append(new_model)
    return jsonify({"status": "success", "message": "Model loaded successfully"}), 200


@app.route('/v1/unload_lora_adapter', methods=['POST'])
def unload_model():
    model_id = request.json.get('lora_name')
    global models
    models = [model for model in models if model['id'] != model_id]
    return jsonify({"status": "success", "message": "Model unloaded successfully"}), 200


@app.route('/v1/completions', methods=['POST'])
def completion():
    prompt = request.json.get('prompt')
    model = request.json.get('model')
    if not prompt or not model:
        return jsonify({"status": "error", "message": "Prompt and model are required"}), 400

    prompt_tokens = randint(1, 100)
    completion_tokens = randint(1, 100)
    
    # Simulated response
    response = {
        "id": "cmpl-uqkvlQyYK7bGYrRHQ0eXlWi7",
        "object": "text_completion",
        "created": 1589478378,
        "model": model,
        "system_fingerprint": "fp_44709d6fcb",
        "choices": [
            {
                "text": f"This is indeed a test from model {model}!",
                "index": 0,
                "logprobs": None,
                "finish_reason": "length"
            }
        ],
        "usage": {
            "prompt_tokens": prompt_tokens,
            "completion_tokens": completion_tokens,
            "total_tokens": prompt_tokens+completion_tokens
        }
    }
    return jsonify(response), 200


@app.route('/v1/chat/completions', methods=['POST'])
def chat_completions():
    messages = request.json.get('messages')
    model = request.json.get('model')
    if not messages or not model:
        return jsonify({"status": "error", "message": "Messages and model are required"}), 400

    prompt_tokens = randint(1, 100)
    completion_tokens = randint(1, 100)
    
    # Simulated response
    response = {
        "id": "chatcmpl-abc123",
        "object": "chat.completion",
        "created": 1677858242,
        "model": model,
        "usage": {
            "prompt_tokens": prompt_tokens,
            "completion_tokens": completion_tokens,
            "total_tokens": prompt_tokens+completion_tokens
        },
        "choices": [
            {
                "message": {
                    "role": "assistant",
                    "content": f"\n\nThis is a test from{model}!"
                },
                "logprobs": None,
                "finish_reason": "stop",
                "index": 0
            }
        ]
    }
    return jsonify(response), 200

@app.route('/set_metrics', methods=['POST'])
def set_metrics():
    global overrides
    # Get JSON data from the request
    data = request.json
    if data:
        # Update overrides with new key-value pairs
        overrides.update(data)
        return {"status": "success", "message": "Overrides updated"}, 200
    else:
        return {"status": "error", "message": "No data provided"}, 400

@app.route('/metrics')
def metrics():
    # get deployment information
    try:
        apps_v1 = client.AppsV1Api()
        resp = apps_v1.read_namespaced_deployment(DEPLOYMENT_NAME, NAMESPACE)
        replicas = resp.spec.replicas if resp.spec.replicas is not None else 1
    except Exception as e:
        print(f"Failed to get deployment information: {DEPLOYMENT_NAME=} {NAMESPACE=} {e=}, set replicas to {DEFAULT_REPLICAS}")
        replicas = DEFAULT_REPLICAS

    # a reasonable mock total value
    total = overrides.get("total", 100.0)
    model_name = overrides.get("model_name", MODEL_NAME)
    # calculate metrics with potential overrides
    success_total = overrides.get("success_total", total / replicas)
    avg_prompt_throughput = overrides.get("avg_prompt_throughput", total / replicas if replicas > 0 else 0)
    avg_generation_throughput = overrides.get("avg_generation_throughput", total / replicas if replicas > 0 else 0)
    running = overrides.get("running", randint(1, 100))
    waiting = overrides.get("waiting", randint(1, 100))
    swapped = overrides.get("swapped", randint(1, 100))
    max_running_capacity = 100
    gpu_cache_usage_perc = overrides.get("gpu_cache_usage_perc", min(100.0, (running / max_running_capacity) * 100))

    # construct Prometheus-style Metrics
    metrics_output = f"""# HELP vllm:request_success_total Count of successfully processed requests.
# TYPE vllm:request_success_total counter
vllm:request_success_total{{finished_reason="stop",model_name="{model_name}"}} {success_total}
# HELP vllm:num_requests_running Number of requests currently running on GPU.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{{model_name="{model_name}"}} {running}
# HELP vllm:num_requests_swapped Number of requests swapped to CPU.
# TYPE vllm:num_requests_swapped gauge
vllm:num_requests_swapped{{model_name="{model_name}"}} {swapped}
# HELP vllm:num_requests_waiting Number of requests waiting to be processed.
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{{model_name="{model_name}"}} {waiting}
# HELP vllm:avg_prompt_throughput_toks_per_s Average prefill throughput in tokens/s.
# TYPE vllm:avg_prompt_throughput_toks_per_s gauge
vllm:avg_prompt_throughput_toks_per_s{{model_name="{model_name}"}} {avg_prompt_throughput}
# HELP vllm:avg_generation_throughput_toks_per_s Average generation throughput in tokens/s.
# TYPE vllm:avg_generation_throughput_toks_per_s gauge
vllm:avg_generation_throughput_toks_per_s{{model_name="{model_name}"}} {avg_generation_throughput}
# HELP vllm:gpu_cache_usage_perc GPU KV-cache usage. 1 means 100 percent usage.
# TYPE vllm:gpu_cache_usage_perc gauge
vllm:gpu_cache_usage_perc{{model_name="model_name"}} {gpu_cache_usage_perc}
"""
    return Response(metrics_output, mimetype='text/plain')

if __name__ == '__main__':
    try:
        # config.load_kube_config()
        config.load_incluster_config()
    except Exception as e:
        print(f"Failed to load k8s config: {e}")

    print(f"Starting app. DEPLOYMENT_NAME: {DEPLOYMENT_NAME}, NAMESPACE: {NAMESPACE}")
    app.run(host='0.0.0.0', port=8000)

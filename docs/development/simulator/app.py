import logging
import os
import sys
import time
from datetime import datetime
from random import randint

from flask import Flask, Response, jsonify, request

try:
    from kubernetes import client, config
except Exception as e:
    print(f"Failed to import kubernetes, skip: {e}")

from simulator import Simulator
from transformers import AutoTokenizer
from vidur.config import SimulationConfig
from vidur.config_optimizer.config_explorer.config import ModelConfig
from vidur.entities import Request

MODEL_NAME = os.getenv('MODEL_NAME', 'llama2-70b')
DEPLOYMENT_NAME = os.getenv('DEPLOYMENT_NAME', 'llama2-70b')
NAMESPACE = os.getenv('NAMESPACE', 'default')
DEFAULT_REPLICAS = int(os.getenv('DEFAULT_REPLICAS', '1'))

# Load the tokenizer for your model
tokenizer = AutoTokenizer.from_pretrained(
    'bert-base-uncased',
    model_max_length=16384, # Suppress warning
    clean_up_tokenization_spaces=True)  

app = Flask(__name__)
modelMaps = {
    "llama2-7b": "meta-llama/Llama-2-7b-hf",
    "llama2-70b": "meta-llama/Llama-2-70b-hf"
}
sys.argv.append(f"--replica_config_model_name={modelMaps.get(MODEL_NAME, MODEL_NAME)}")
simulator_config: SimulationConfig = SimulationConfig.create_from_cli_args()
simulator = Simulator(simulator_config)
v1 = None

# Global storage for overridden values
overrides = {}

logger = logging.getLogger(__name__)

def get_token_count(text):
    try:
        # Encode the text
        encoded_input = tokenizer(text)

        # Get the number of tokens
        return len(encoded_input['input_ids'])
    except Exception as e:
        logger.error(f"Failed to get number of tokens: {e}")

    return 1

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
    try:
        prompt = request.json.get('prompt')
        model = request.json.get('model')
        max_tokens = request.json.get('max_tokens')
        if not prompt or not model:
            return jsonify({"status": "error", "message": "Prompt and model are required"}), 400
        
        arrived_at = datetime.now().timestamp()
        input_tokens = get_token_count(prompt)
        output_tokens = max_tokens if max_tokens else randint(10, 500)
        arrived_next = request.json.get('next_in')
        if not arrived_next:
            arrived_next = 0.0
        else:
            arrived_next += arrived_at

        start = datetime.now().timestamp()
        latency = simulator.execute(Request(arrived_at, input_tokens, output_tokens, arrived_next=arrived_next))

        # Simulated response
        response = {
            "id": "cmpl-uqkvlQyYK7bGYrRHQ0eXlWi7",
            "object": "text_completion",
            "created": int(arrived_at),
            "model": model,
            "system_fingerprint": "fp_44709d6fcb",
            "choices": [
                {
                    "text": f"This is simulated message from {model}!",
                    "index": 0,
                    "logprobs": None,
                    "finish_reason": "length"
                }
            ],
            "usage": {
                "prompt_tokens": input_tokens,
                "completion_tokens": output_tokens,
                "total_tokens": input_tokens + output_tokens,
                "time": latency
            }
        }
        overhead = datetime.now().timestamp()-start
        if latency > overhead:
            time.sleep(latency-overhead)
        else:
            logger.warning(f"Latency is less than overhead: L{latency} - O{overhead}")
        
        return jsonify(response), 200
    except Exception as e:
        import traceback
        traceback.print_exc()


@app.route('/v1/chat/completions', methods=['POST'])
def chat_completions():
    messages = request.json.get('messages')
    model = request.json.get('model')
    max_tokens = request.json.get('max_tokens')
    if not messages or not model:
        return jsonify({"status": "error", "message": "Messages and model are required"}), 400
    
    arrived_at = datetime.now().timestamp()
    input_tokens = sum(get_token_count(message["content"]) for message in messages)
    output_tokens = max_tokens if max_tokens else randint(10, 500)
    arrived_next = request.json.get('next_in')
    if not arrived_next:
        arrived_next = 0.0
    else:
        arrived_next += arrived_at

    start = datetime.now().timestamp()
    latency = simulator.execute(Request(arrived_at, input_tokens, output_tokens, arrived_next=arrived_next))

    # Simulated response
    response = {
        "id": "chatcmpl-abc123",
        "object": "chat.completion",
        "created": int(arrived_at),
        "model": model,
        "usage": {
            "prompt_tokens": input_tokens,
            "completion_tokens": output_tokens,
            "total_tokens": input_tokens + output_tokens,
            "time": latency
        },
        "choices": [
            {
                "message": {
                    "role": "assistant",
                    "content": f"\n\nThis is simulated message from {model}!"
                },
                "logprobs": None,
                "finish_reason": "stop",
                "index": 0
            }
        ]
    }
    overhead = datetime.now().timestamp()-start
    if latency > overhead:
        time.sleep(latency-overhead)
    else:
        logger.warning(f"Latency is less than overhead: L{latency} - O{overhead}")
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
    total = overrides.get("total", 0)
    model_name = overrides.get("model_name", MODEL_NAME)
    # calculate metrics with potential overrides
    success_total = overrides.get("success_total", total / replicas)
    avg_prompt_throughput = overrides.get("avg_prompt_throughput", total / replicas if replicas > 0 else 0)
    avg_generation_throughput = overrides.get("avg_generation_throughput", total / replicas if replicas > 0 else 0)
    running = overrides.get("running", 0)
    waiting = overrides.get("waiting", 0)
    swapped = overrides.get("swapped", 0)
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
    logging.basicConfig(level=logging.DEBUG)
    logging.getLogger("kubernetes.client.rest").setLevel(logging.ERROR)  # Suppress kubenetes logs

    print(f"Starting app. DEPLOYMENT_NAME: {DEPLOYMENT_NAME}, NAMESPACE: {NAMESPACE}, MODEL: {MODEL_NAME}")
   
    thread = simulator.start()

    import sys
    if '--time_limit' not in sys.argv:
        try:
            # config.load_kube_config()
            config.load_incluster_config()
        except Exception as e:
            print(f"Failed to load k8s config: {e}")

        # Perform profiling and skip actual run
        app.run(host='0.0.0.0', port=8000)

    # latency = simulator.execute(Request(0, 25, 100))
    # print(f"request latency: {latency}")

    simulator.stop()

    thread.join()

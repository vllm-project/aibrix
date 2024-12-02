from flask import Flask, request, Response, jsonify
from werkzeug import serving
import random
import re
import time
from random import randint
import os

try:
    from kubernetes import client, config
except Exception as e:
    print(f"Failed to import kubernetes, skip: {e}")

# Global storage for overridden values
overrides = {}

MODEL_NAME = 'llama2-70b'
DEPLOYMENT_NAME = os.getenv('DEPLOYMENT_NAME', 'llama2-70b')
NAMESPACE = os.getenv('POD_NAMESPACE', 'default')
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


# Note: this is to supress /metrics logs, gateway sends request to pods to scrape
# the metrics and results in lots of meaningless requests that we do not want to log.
def disable_endpoint_logs():
    """Disable logs for requests to specific endpoints."""
    disabled_endpoints = ('/', '/healthz', '/metrics')
    parent_log_request = serving.WSGIRequestHandler.log_request

    def log_request(self, *args, **kwargs):
        if not any(re.match(f"{de}$", self.path) for de in disabled_endpoints):
            parent_log_request(self, *args, **kwargs)

    serving.WSGIRequestHandler.log_request = log_request


app = Flask(__name__)
disable_endpoint_logs()


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
            "total_tokens": prompt_tokens + completion_tokens
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
            "total_tokens": prompt_tokens + completion_tokens
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


# Initialize global state to keep track of metrics data
metrics_state = {}


def generate_histogram_metric(metric_name, description, model_name, buckets, new_requests, help_header=True):
    """
    Generate Prometheus histogram metrics with dynamically updated bucket values.

    Args:
        metric_name (str): Name of the metric.
        description (str): Metric description.
        model_name (str): Model name.
        buckets (list): List of bucket boundaries.
        new_requests (dict): Dictionary with new requests to update bucket values.
        help_header: the flag to include HELP Header

    Returns:
        str: Prometheus-formatted histogram metric.
    """
    global metrics_state

    # Initialize state if not already present
    if metric_name not in metrics_state:
        metrics_state[metric_name] = {
            "buckets": {bucket: 0 for bucket in buckets},  # Bucket values
            "total_sum": 0,  # Total sum of all values
            "total_count": 0  # Total count of all events
        }

    # Retrieve current metric state
    current_state = metrics_state[metric_name]

    # Update buckets and ensure cumulative nature
    for bucket in buckets:
        if bucket in new_requests:
            # Add new requests for this bucket
            current_state["buckets"][bucket] += new_requests[bucket]

        # Ensure cumulative updates for histogram buckets
        if bucket != buckets[0]:  # Skip the first bucket
            current_state["buckets"][bucket] = max(
                current_state["buckets"][bucket],
                current_state["buckets"][buckets[buckets.index(bucket) - 1]]
            )

    # Update total_count and total_sum
    current_state["total_count"] = current_state["buckets"][buckets[-1]]  # `+Inf` bucket is the total count
    current_state["total_sum"] += sum(
        float(bucket) * value for bucket, value in new_requests.items() if bucket != "+Inf"
    )

    # Generate Prometheus bucket strings
    bucket_strings = "\n".join(
        [f'vllm:{metric_name}_bucket{{le="{bucket}",model_name="{model_name}"}} {current_state["buckets"][bucket]}'
         for bucket in buckets]
    )

    # Return formatted histogram metric
    histogram_template = """
# HELP vllm:{metric_name} {description}
# TYPE vllm:{metric_name} histogram
vllm:{metric_name}_sum{{model_name="{model_name}"}} {value}
{buckets}
vllm:{metric_name}_count{{model_name="{model_name}"}} {count}
""" if help_header else """
vllm:{metric_name}_sum{{model_name="{model_name}"}} {value}
{buckets}
vllm:{metric_name}_count{{model_name="{model_name}"}} {count}
"""

    return histogram_template.format(
        metric_name=metric_name,
        description=description,
        model_name=model_name,
        value=current_state["total_sum"],
        buckets=bucket_strings,
        count=current_state["total_count"]
    )


def generate_counter_gauge_metric(metric_name, metric_type, description, model_name, value, help_header=True):
    """
    Generates a Prometheus metric string for counter or gauge.

    Args:
        metric_name (str): The name of the metric.
        metric_type (str): The type of the metric ('counter' or 'gauge').
        description (str): The HELP description of the metric.
        model_name (str): The name of the model.
        value (float): The value of the metric.
        help_header: the flag to include HELP Header

    Returns:
        str: A formatted Prometheus metric string.
    """
    counter_gauge_template = """
# HELP vllm:{metric_name} {description}
# TYPE vllm:{metric_name} {metric_type}
vllm:{metric_name}{{model_name="{model_name}"}} {value}
""" if help_header else """
vllm:{metric_name}{{model_name="{model_name}"}} {value}
"""

    return counter_gauge_template.format(
        metric_name=metric_name,
        metric_type=metric_type,
        description=description,
        model_name=model_name,
        value=value
    )


@app.route('/metrics')
def metrics():
    # get deployment information
    try:
        apps_v1 = client.AppsV1Api()
        resp = apps_v1.read_namespaced_deployment(DEPLOYMENT_NAME, NAMESPACE)
        replicas = resp.spec.replicas if resp.spec.replicas is not None else 1
    except Exception as e:
        print(f"Failed to get deployment information: {DEPLOYMENT_NAME=} {NAMESPACE=} error={str(e)}")
        print(f"Due to the failure, replicas {DEFAULT_REPLICAS} will be used to calculate metrics")
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

    # Define metrics and their attributes
    simple_metrics = [
        {
            "name": "request_success_total",
            "type": "counter",
            "description": "Count of successfully processed requests.",
            "value": overrides.get("success_total", success_total)
        },
        {
            "name": "num_requests_running",
            "type": "gauge",
            "description": "Number of requests currently running on GPU.",
            "value": overrides.get("running", running)
        },
        {
            "name": "num_requests_swapped",
            "type": "gauge",
            "description": "Number of requests swapped to CPU.",
            "value": overrides.get("swapped", swapped)
        },
        {
            "name": "num_requests_waiting",
            "type": "gauge",
            "description": "Number of requests waiting to be processed.",
            "value": overrides.get("waiting", waiting)
        },
        {
            "name": "avg_prompt_throughput_toks_per_s",
            "type": "gauge",
            "description": "Average prefill throughput in tokens/s.",
            "value": overrides.get("avg_prompt_throughput", avg_prompt_throughput)
        },
        {
            "name": "avg_generation_throughput_toks_per_s",
            "type": "gauge",
            "description": "Average generation throughput in tokens/s.",
            "value": overrides.get("avg_generation_throughput", avg_generation_throughput)
        },
        {
            "name": "gpu_cache_usage_perc",
            "type": "gauge",
            "description": "GPU KV-cache usage. 1 means 100 percent usage.",
            "value": overrides.get(
                "gpu_cache_usage_perc", gpu_cache_usage_perc
            )
        },
    ]

    # Generate all metrics
    metrics_output = ""
    for metric in simple_metrics:
        metrics_output += generate_counter_gauge_metric(metric["name"], metric["type"], metric["description"],
                                                        model_name, metric["value"])
        metrics_output += generate_counter_gauge_metric(metric["name"], metric["type"], metric["description"],
                                                        "lora-1", metric["value"], help_header=False)

    histogram_metrics = [
        {
            "name": "iteration_tokens_total",
            "type": "histogram",
            "description": "Histogram of number of tokens per engine_step.",
            "buckets": ["1.0", "8.0", "16.0", "32.0", "64.0", "128.0", "256.0",
                        "512.0", "1024.0", "2048.0", "4096.0", "8192.0", "+Inf"]
        },
        {
            "name": "time_to_first_token_seconds",
            "type": "histogram",
            "description": "Histogram of time to first token in seconds.",
            "buckets": ["0.001", "0.005", "0.01", "0.02", "0.04", "0.06",
                        "0.08", "0.1", "0.25", "0.5", "+Inf"]
        },
        {
            "name": "time_per_output_token_seconds",
            "type": "histogram",
            "description": "Histogram of time per output token in seconds.",
            "buckets": ["0.01", "0.025", "0.05", "0.075", "0.1", "0.15",
                        "0.2", "0.3", "0.4", "+Inf"]
        },
        {
            "name": "e2e_request_latency_seconds",
            "type": "histogram",
            "description": "Histogram of end to end request latency in seconds.",
            "buckets": ["0.3", "0.5", "0.8", "1.0", "1.5", "2.0", "5.0", "+Inf"]
        },
        {
            "name": "request_queue_time_seconds",
            "type": "histogram",
            "description": "Histogram of time spent in WAITING phase for request.",
            "buckets": ["0.3", "0.5", "0.8", "1.0", "1.5", "2.0", "5.0", "+Inf"]
        },
        {
            "name": "request_inference_time_seconds",
            "type": "histogram",
            "description": "Histogram of time spent in RUNNING phase for request.",
            "buckets": ["0.3", "0.5", "0.8", "1.0", "1.5", "2.0", "5.0", "+Inf"]
        },
        {
            "name": "request_decode_time_seconds",
            "type": "histogram",
            "description": "Histogram of time spent in DECODE phase for request.",
            "buckets": ["0.3", "0.5", "0.8", "1.0", "1.5", "2.0", "5.0", "+Inf"]
        },
        {
            "name": "request_prefill_time_seconds",
            "type": "histogram",
            "description": "Histogram of time spent in PREFILL phase for request.",
            "buckets": ["0.3", "0.5", "0.8", "1.0", "1.5", "2.0", "5.0", "+Inf"]
        },
    ]

    # Generate metrics output
    histogram_metrics_output = ""
    for metric in histogram_metrics:
        # Simulate random new requests for the metric
        new_requests = {bucket: random.randint(0, 5) for bucket in metric["buckets"]}
        histogram_metrics_output += generate_histogram_metric(
            metric_name=metric["name"],
            description=metric["description"],
            model_name=model_name,
            buckets=metric["buckets"],
            new_requests=new_requests
        )
        new_requests = {bucket: random.randint(0, 5) for bucket in metric["buckets"]}
        histogram_metrics_output += generate_histogram_metric(
            metric_name=metric["name"],
            description=metric["description"],
            model_name="lora-1",
            buckets=metric["buckets"],
            new_requests=new_requests,
            help_header=False
        )

    return Response(metrics_output  + histogram_metrics_output, mimetype='text/plain')


if __name__ == '__main__':
    try:
        # config.load_kube_config()
        config.load_incluster_config()
    except Exception as e:
        print(f"Failed to load k8s config: {e}")

    print(f"Starting app. DEPLOYMENT_NAME: {DEPLOYMENT_NAME}, NAMESPACE: {NAMESPACE}")
    app.run(host='0.0.0.0', port=8000)

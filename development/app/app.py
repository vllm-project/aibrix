from flask import Flask, request, Response, jsonify
from flask_httpauth import HTTPTokenAuth
from functools import wraps
from werkzeug import serving
import random
import re
import logging
import sys
import time
from datetime import datetime
from random import randint
import os
import json
from typing import Optional

try:
    from kubernetes import client, config
except Exception as e:
    print(f"Failed to import kubernetes, skip: {e}")

from simulator import Simulator
from vidur.config import SimulationConfig
from vidur.entities import Request

# Global storage for overridden values
overrides = {}

MODEL_NAME = os.getenv('MODEL_NAME', 'llama2-70b')
DEPLOYMENT_NAME = os.getenv('DEPLOYMENT_NAME', 'llama2-70b')
NAMESPACE = os.getenv('POD_NAMESPACE', 'default')
DEFAULT_REPLICAS = int(os.getenv('DEFAULT_REPLICAS', '1'))
SIMULATION = os.getenv('SIMULATION', 'disabled')

modelMaps = {
    "llama2-7b": "meta-llama/Llama-2-7b-hf",
    "llama2-70b": "meta-llama/Llama-2-70b-hf"
}

# Polifill the necessary arguments.
if "--replica_config_device" not in sys.argv:
    sys.argv.append("--replica_config_device")
    sys.argv.append(SIMULATION)
if "--replica_config_model_name" not in sys.argv:
    sys.argv.append("--replica_config_model_name")
    sys.argv.append(modelMaps.get(MODEL_NAME, MODEL_NAME))

tokenizer = None
simulator: Optional[Simulator] = None

# Extract the api_key argument and prepare for authentication
api_key = None
try:
    index = sys.argv.index("--api_key")
    if index + 1 < len(sys.argv):
        api_key = sys.argv[index + 1]
except ValueError:
    pass

auth = HTTPTokenAuth(scheme='Bearer')


@auth.verify_token
def verify_token(token):
    if api_key is None:
        return True
    return token == api_key


@auth.error_handler
def auth_error(status):
    return jsonify({"error": "Unauthorized"}), 401


logger = logging.getLogger(__name__)


def read_configs(file_path):
    """
    Reads a JSON file that store sensitive information.
    """
    try:
        with open(file_path, "r") as f:
            data = json.load(f)
            if not isinstance(data, dict):
                raise Exception("invalid config format, dict expected.")
            return data
    except Exception as e:
        print(f"Error reading JSON file: {e}")
        return {}


configs = read_configs("config.json")
HUGGINGFACE_TOKEN = configs.get("huggingface_token", "your huggingface token")


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
@auth.login_required
def get_models():
    return jsonify({
        "object": "list",
        "data": models
    })


@app.route('/v1/load_lora_adapter', methods=['POST'])
@auth.login_required
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
@auth.login_required
def unload_model():
    model_id = request.json.get('lora_name')
    global models
    models = [model for model in models if model['id'] != model_id]
    return jsonify({"status": "success", "message": "Model unloaded successfully"}), 200


@app.route('/v1/completions', methods=['POST'])
@auth.login_required
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
        latency = 0.0
        if simulator is not None:
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
        overhead = datetime.now().timestamp() - start
        if latency > overhead:
            time.sleep(latency - overhead)
        elif latency > 0.0:
            logger.warning(f"Latency is less than overhead: L{latency} - O{overhead}")

        return jsonify(response), 200
    except Exception as e:
        err = {
            "error": {
                "message": f"The server had an error while processing your request: {e}",
                "type": "server_error"
            }
        }
        return jsonify(err), 500


@app.route('/v1/chat/completions', methods=['POST'])
@auth.login_required
def chat_completions():
    try:
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
        latency = 0.0
        if simulator is not None:
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
        overhead = datetime.now().timestamp() - start
        if latency > overhead:
            time.sleep(latency - overhead)
        else:
            logger.warning(f"Latency is less than overhead: L{latency} - O{overhead}")

        return jsonify(response), 200
    except Exception as e:
        err = {
            "error": {
                "message": f"The server had an error while processing your request: {e}",
                "type": "server_error"
            }
        }
        return jsonify(err), 500


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
    prompt_tokens_total = overrides.get("prompt_tokens_total", randint(100, 1024) * success_total)
    generation_tokens_total = overrides.get("generation_tokens_total", randint(100, 1024) * success_total)
    running = overrides.get("running", randint(1, 100))
    cpu_running = overrides.get("cpu_running", randint(1, 100))
    waiting = overrides.get("waiting", randint(1, 100))
    swapped = overrides.get("swapped", randint(1, 100))
    max_running_capacity = 100
    gpu_cache_usage_perc = overrides.get("gpu_cache_usage_perc", min(100.0, (running / max_running_capacity) * 100))
    cpu_cache_usage_perc = overrides.get("cpu_cache_usage_perc", min(100.0, (cpu_running / max_running_capacity) * 100))

    # Define metrics and their attributes
    simple_metrics = [
        {
            "name": "prompt_tokens_total",
            "type": "counter",
            "description": "Count of prefill tokens processed.",
            "value": overrides.get("prompt_tokens_total", prompt_tokens_total)
        },
        {
            "name": "generation_tokens_total",
            "type": "counter",
            "description": "Count of generation tokens processed.",
            "value": overrides.get("generation_tokens_total", generation_tokens_total)
        },
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
        {
            "name": "cpu_cache_usage_perc",
            "type": "gauge",
            "description": "CPU KV-cache usage. 1 means 100 percent usage.",
            "value": overrides.get(
                "cpu_cache_usage_perc", cpu_cache_usage_perc
            )
        },
    ]

    # Generate all metrics
    metrics_output = """
# HELP vllm:lora_requests_info Running stats on lora requests.
# TYPE vllm:lora_requests_info gauge
vllm:lora_requests_info{max_lora="1",running_lora_adapters="text2sql-lora-1",waiting_lora_adapters=""} 1
"""
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
            "name": "request_prompt_tokens",
            "type": "histogram",
            "description": "Histogram of number of prefill tokens processed..",
            "buckets": ["1.0", "2.0", "5.0", "10.0", "20.0", "50.0",
                        "100.0", "200.0", "500.0", "1000.0", "2000.0",
                        "5000.0", "10000.0", "+Inf"]
        },
        {
            "name": "request_generation_tokens",
            "type": "histogram",
            "description": "Histogram of number of generation tokens processed..",
            "buckets": ["1.0", "2.0", "5.0", "10.0", "20.0", "50.0",
                        "100.0", "200.0", "500.0", "1000.0", "2000.0",
                        "5000.0", "10000.0", "+Inf"]
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

    return Response(metrics_output + histogram_metrics_output, mimetype='text/plain')


if __name__ == '__main__':
    logging.basicConfig(level=logging.DEBUG)
    logging.getLogger("kubernetes.client.rest").setLevel(logging.ERROR)  # Suppress kubenetes logs

    print(f"Starting app. DEPLOYMENT_NAME: {DEPLOYMENT_NAME}, NAMESPACE: {NAMESPACE}, MODEL: {MODEL_NAME}")

    # Extract gpu_device without call argparse
    gpu_device = "disabled"
    try:
        index = sys.argv.index("--replica_config_device")
        if index + 1 < len(sys.argv):
            gpu_device = sys.argv[index + 1]
    except ValueError:
        pass

    # Restore -h functionality
    if '-h' in sys.argv:
        SimulationConfig.create_from_cli_args()

    # Launch simulator
    if gpu_device != "disabled":
        # Load the tokenizer for your model
        from transformers import AutoTokenizer

        default_model = 'bert-base-uncased'
        try:
            # can we make this as an application argument.
            # no need to use such map, we can use huggingface id directly.
            token_model = modelMaps.get(MODEL_NAME, default_model)
            tokenizer = AutoTokenizer.from_pretrained(
                token_model,
                token=HUGGINGFACE_TOKEN,
                model_max_length=16384,  # Suppress warning
                clean_up_tokenization_spaces=True)
        except Exception as e:
            logger.error(f"Failed to initialize tokenizer, will use default tokenizer model: {e}")
            tokenizer = AutoTokenizer.from_pretrained(
                default_model,
                model_max_length=16384,  # Suppress warning
                clean_up_tokenization_spaces=True)

        # TODO: check whether able to use argparse to build SimulationConfig
        simulator = Simulator(SimulationConfig.create_from_cli_args())
        overrides = {
            "total": 100.0,
            "running": 0,
            "waiting": 0,
            "swapped": 0
        }

    thread = None
    if simulator is not None:
        # TODO: Move simulation to a separate workflow, independent of the main web service
        thread = simulator.start()

    # Perform profiling and skip actual run
    if '--time_limit' not in sys.argv:
        try:
            # config.load_kube_config()
            config.load_incluster_config()
        except Exception as e:
            print(f"Failed to load k8s config: {e}")

        app.run(host='0.0.0.0', port=8000)

    if simulator is not None:
        simulator.stop()

    if thread is not None:
        thread.join()

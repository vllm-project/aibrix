from flask import Flask, request, Response, jsonify
from flask_httpauth import HTTPTokenAuth
from functools import wraps
from werkzeug import serving
import base64
import random
import re
import logging
import struct
import sys
import time
from datetime import datetime
from random import randint
import os
import json
from typing import Optional

# Global storage for overridden values
overrides = {}

MODEL_NAME = os.getenv("MODEL_NAME", "llama2-7b")
DEPLOYMENT_NAME = os.getenv("DEPLOYMENT_NAME", "llama2-7b")
NAMESPACE = os.getenv("POD_NAMESPACE", "default")
DEFAULT_REPLICAS = int(os.getenv("DEFAULT_REPLICAS", "1"))
SIMULATION = os.getenv("SIMULATION", "disabled")
STANDALONE_MODE = os.getenv("STANDALONE_MODE", "false").lower() in ("true", "1", "yes")

# Optional kubernetes import (only needed in Kubernetes environment)
if not STANDALONE_MODE:
    try:
        from kubernetes import client, config
    except Exception as e:
        print(f"Failed to import kubernetes, skip: {e}")
        STANDALONE_MODE = True  # Fall back to standalone mode

# Optional simulator/vidur imports - only loaded when SIMULATION is enabled
if SIMULATION != "disabled":
    try:
        from simulator import Simulator
        from vidur.config import SimulationConfig
        from vidur.entities import Request as VidurRequest
    except ImportError as e:
        print(f"Simulator/vidur not available: {e}")
        SIMULATION = "disabled"
    except Exception as e:
        print(f"Error loading simulator: {e}")
        SIMULATION = "disabled"

modelMaps = {
    "llama2-7b": "meta-llama/Llama-2-7b-hf",
    "llama2-70b": "meta-llama/Llama-2-70b-hf",
}

# Polyfill the necessary arguments (only if simulator is enabled)
if SIMULATION != "disabled":
    if "--replica_config_device" not in sys.argv:
        sys.argv.append("--replica_config_device")
        sys.argv.append(SIMULATION)
    if "--replica_config_model_name" not in sys.argv:
        sys.argv.append("--replica_config_model_name")
        sys.argv.append(modelMaps.get(MODEL_NAME, MODEL_NAME))

tokenizer = None
simulator = None  # Optional[Simulator] when simulation is enabled

# Extract the api_key argument and prepare for authentication
api_key = None
try:
    index = sys.argv.index("--api_key")
    if index + 1 < len(sys.argv):
        api_key = sys.argv[index + 1]
except ValueError:
    pass

auth = HTTPTokenAuth(scheme="Bearer")


@auth.verify_token
def verify_token(token):
    if api_key is None:
        return True
    return token == api_key


@auth.error_handler
def auth_error(status):
    return (
        jsonify(
            {
                "error": {
                    "message": "Incorrect API key provided. You can find your API key at https://platform.openai.com/account/api-keys.",
                    "type": "authentication_error",
                    "param": None,
                    "code": "invalid_api_key",
                }
            }
        ),
        401,
    )


logger = logging.getLogger(__name__)


# =============================================================================
# HELPER FUNCTIONS
# =============================================================================

def create_error_response(message, error_type="invalid_request_error", param=None, code=None, status_code=400):
    """
    Create a consistent OpenAI-compatible error response.

    Args:
        message: Error message to display
        error_type: Type of error (invalid_request_error, api_error, authentication_error, etc.)
        param: The parameter that caused the error (if applicable)
        code: Error code (if applicable)
        status_code: HTTP status code

    Returns:
        Tuple of (response, status_code)
    """
    return (
        jsonify({
            "error": {
                "message": message,
                "type": error_type,
                "param": param,
                "code": code,
            }
        }),
        status_code,
    )


def create_vllm_error(message, error_type, status_code):
    """
    Create a vLLM-specific error response.

    Args:
        message: Error message to display
        error_type: Type of error (InvalidUserInput, NotFoundError, etc.)
        status_code: HTTP status code

    Returns:
        Tuple of (response, status_code)
    """
    return (
        jsonify({
            "error": {
                "message": message,
                "type": error_type,
                "param": None,
                "code": status_code,
            }
        }),
        status_code,
    )


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
        return len(encoded_input["input_ids"])
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
                "is_blocking": False,
            }
        ],
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
                "is_blocking": False,
            }
        ],
    },
]


# Note: this is to supress /metrics logs, gateway sends request to pods to scrape
# the metrics and results in lots of meaningless requests that we do not want to log.
def disable_endpoint_logs():
    """Disable logs for requests to specific endpoints."""
    disabled_endpoints = ("/", "/health", "/ready", "/metrics")
    parent_log_request = serving.WSGIRequestHandler.log_request

    def log_request(self, *args, **kwargs):
        if not any(re.match(f"{de}$", self.path) for de in disabled_endpoints):
            parent_log_request(self, *args, **kwargs)

    serving.WSGIRequestHandler.log_request = log_request


app = Flask(__name__)
disable_endpoint_logs()


# =============================================================================
# HEALTH & UTILITY ENDPOINTS
# =============================================================================

@app.route("/health", methods=["GET"])
def health():
    return {"status": "ok"}, 200


@app.route("/ready", methods=["GET"])
def ready():
    return {"status": "ready"}, 200


@app.route("/v1/models", methods=["GET"])
@auth.login_required
def get_models():
    return jsonify({"object": "list", "data": models})


# =============================================================================
# VLLM-SPECIFIC ENDPOINTS
# =============================================================================

@app.route("/v1/load_lora_adapter", methods=["POST"])
@auth.login_required
def load_lora_adapter():
    """
    Load a LoRA adapter. Matches vLLM error handling behavior.
    """
    data = request.json or {}
    lora_name = data.get("lora_name")
    lora_path = data.get("lora_path")

    # Validate required fields
    if not lora_name or not lora_path:
        return create_vllm_error(
            "Both 'lora_name' and 'lora_path' must be provided.",
            "InvalidUserInput",
            400,
        )

    # Check if adapter already loaded
    if any(model["id"] == lora_name for model in models):
        return create_vllm_error(
            f"The lora adapter '{lora_name}' has already been loaded.",
            "InvalidUserInput",
            400,
        )

    # Simulate path not found for paths containing "nonexistent" or "/invalid/"
    if "nonexistent" in lora_path.lower() or "/invalid/" in lora_path.lower():
        return create_vllm_error(
            f"Loading lora {lora_name} failed: No adapter found for {lora_path}",
            "NotFoundError",
            404,
        )

    new_model = {
        "id": lora_name,
        "created": int(time.time()),
        "object": "model",
        "owned_by": "vllm",
        "parent": None,
        "root": lora_path,
    }

    models.append(new_model)
    return jsonify({"status": "success", "message": "Model loaded successfully"}), 200


@app.route("/v1/unload_lora_adapter", methods=["POST"])
@auth.login_required
def unload_lora_adapter():
    """
    Unload a LoRA adapter. Matches vLLM error handling behavior.
    """
    global models
    data = request.json or {}
    lora_name = data.get("lora_name")

    # Validate required field
    if not lora_name:
        return create_vllm_error(
            "'lora_name' must be provided.",
            "InvalidUserInput",
            400,
        )

    # Check if adapter exists
    if not any(model["id"] == lora_name for model in models):
        return create_vllm_error(
            f"The lora adapter '{lora_name}' cannot be found.",
            "NotFoundError",
            400,
        )

    models = [model for model in models if model["id"] != lora_name]
    return jsonify({"status": "success", "message": "Model unloaded successfully"}), 200


# =============================================================================
# OPENAI-COMPATIBLE ENDPOINTS
# =============================================================================

@app.route("/v1/completions", methods=["POST"])
@auth.login_required
def completion():
    try:
        prompt = request.json.get("prompt")
        model = request.json.get("model")
        max_tokens = request.json.get("max_tokens")
        stream = request.json.get("stream", False)
        if not prompt or not model:
            return (
                jsonify(
                    {
                        "error": {
                            "message": "'prompt' and 'model' are required parameters",
                            "type": "invalid_request_error",
                            "param": "prompt" if not prompt else "model",
                            "code": None,
                        }
                    }
                ),
                400,
            )

        arrived_at = datetime.now().timestamp()
        input_tokens = get_token_count(prompt)
        output_tokens = max_tokens if max_tokens else randint(10, 500)
        arrived_next = request.json.get("next_in")
        if not arrived_next:
            arrived_next = 0.0
        else:
            arrived_next += arrived_at

        start = datetime.now().timestamp()
        latency = 0.0
        if simulator is not None:
            latency = simulator.execute(
                VidurRequest(
                    arrived_at, input_tokens, output_tokens, arrived_next=arrived_next
                )
            )

        overhead = datetime.now().timestamp() - start
        if latency > overhead:
            time.sleep(latency - overhead)
        elif latency > 0.0:
            logger.warning(f"Latency is less than overhead: L{latency} - O{overhead}")

        if stream:

            def generate():
                completion_id = "cmpl-" + "".join(
                    random.choices(
                        "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
                        k=20,
                    )
                )
                full_text = f"This is simulated message from {model}!"
                words = full_text.split()
                for i, word in enumerate(words):
                    chunk = {
                        "id": completion_id,
                        "object": "text_completion",
                        "created": int(arrived_at),
                        "model": model,
                        "system_fingerprint": "fp_44709d6fcb",
                        "choices": [
                            {
                                "text": word + (" " if i < len(words) - 1 else ""),
                                "index": 0,
                                "logprobs": None,
                                "finish_reason": None,
                            }
                        ],
                    }
                    yield f"data: {json.dumps(chunk)}\n\n"
                    time.sleep(0.05)

                final_chunk = {
                    "id": completion_id,
                    "object": "text_completion",
                    "created": int(arrived_at),
                    "model": model,
                    "system_fingerprint": "fp_44709d6fcb",
                    "choices": [
                        {
                            "text": "",
                            "index": 0,
                            "logprobs": None,
                            "finish_reason": "length",
                        }
                    ],
                    "usage": {
                        "prompt_tokens": input_tokens,
                        "completion_tokens": output_tokens,
                        "total_tokens": input_tokens + output_tokens,
                    },
                }
                yield f"data: {json.dumps(final_chunk)}\n\n"
                time.sleep(0.01)  # Small delay to ensure data is flushed
                yield "data: [DONE]\n\n"

            response = Response(generate(), mimetype="text/event-stream")
            response.headers['Cache-Control'] = 'no-cache'
            response.headers['X-Accel-Buffering'] = 'no'
            return response
        else:
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
                        "finish_reason": "length",
                    }
                ],
                "usage": {
                    "prompt_tokens": input_tokens,
                    "completion_tokens": output_tokens,
                    "total_tokens": input_tokens + output_tokens,
                },
            }
            return jsonify(response), 200
    except Exception as e:
        err = {
            "error": {
                "message": f"The server had an error while processing your request. Sorry about that!",
                "type": "api_error",
                "param": None,
                "code": None,
            }
        }
        return jsonify(err), 500


@app.route("/v1/chat/completions", methods=["POST"])
@auth.login_required
def chat_completions():
    try:
        messages = request.json.get("messages")
        model = request.json.get("model")
        max_tokens = request.json.get("max_tokens")
        stream = request.json.get("stream", False)
        stream_options = request.json.get("stream_options", {})
        include_usage = stream_options.get("include_usage", False) if stream else False
        if not messages or not model:
            return (
                jsonify(
                    {
                        "error": {
                            "message": "'messages' and 'model' are required parameters",
                            "type": "invalid_request_error",
                            "param": "messages" if not messages else "model",
                            "code": None,
                        }
                    }
                ),
                400,
            )

        arrived_at = datetime.now().timestamp()
        input_tokens = sum(get_token_count(message["content"]) for message in messages)
        output_tokens = max_tokens if max_tokens else randint(10, 500)
        arrived_next = request.json.get("next_in")
        if not arrived_next:
            arrived_next = 0.0
        else:
            arrived_next += arrived_at

        start = datetime.now().timestamp()
        latency = 0.0
        if simulator is not None:
            latency = simulator.execute(
                VidurRequest(
                    arrived_at, input_tokens, output_tokens, arrived_next=arrived_next
                )
            )

        overhead = datetime.now().timestamp() - start
        if latency > overhead:
            time.sleep(latency - overhead)
        else:
            logger.warning(f"Latency is less than overhead: L{latency} - O{overhead}")

        if stream:

            def generate():
                completion_id = "chatcmpl-" + "".join(
                    random.choices(
                        "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
                        k=20,
                    )
                )

                # First chunk with role
                role_chunk = {
                    "id": completion_id,
                    "object": "chat.completion.chunk",
                    "created": int(arrived_at),
                    "model": model,
                    "system_fingerprint": "fp_44709d6fcb",
                    "choices": [
                        {
                            "index": 0,
                            "delta": {"role": "assistant"},
                            "logprobs": None,
                            "finish_reason": None,
                        }
                    ],
                }
                yield f"data: {json.dumps(role_chunk)}\n\n"

                # Content chunks
                full_text = f"\n\nThis is simulated message from {model}!"
                words = full_text.split()
                for i, word in enumerate(words):
                    chunk = {
                        "id": completion_id,
                        "object": "chat.completion.chunk",
                        "created": int(arrived_at),
                        "model": model,
                        "system_fingerprint": "fp_44709d6fcb",
                        "choices": [
                            {
                                "index": 0,
                                "delta": {
                                    "content": word
                                    + (" " if i < len(words) - 1 else "")
                                },
                                "logprobs": None,
                                "finish_reason": None,
                            }
                        ],
                    }
                    yield f"data: {json.dumps(chunk)}\n\n"
                    time.sleep(0.05)

                # Final chunk with finish_reason
                final_chunk = {
                    "id": completion_id,
                    "object": "chat.completion.chunk",
                    "created": int(arrived_at),
                    "model": model,
                    "system_fingerprint": "fp_44709d6fcb",
                    "choices": [
                        {
                            "index": 0,
                            "delta": {},
                            "logprobs": None,
                            "finish_reason": "stop",
                        }
                    ],
                }
                yield f"data: {json.dumps(final_chunk)}\n\n"

                # Usage chunk (optional, included when stream_options.include_usage is true)
                if include_usage:
                    usage_chunk = {
                        "id": completion_id,
                        "object": "chat.completion.chunk",
                        "created": int(arrived_at),
                        "model": model,
                        "system_fingerprint": "fp_44709d6fcb",
                        "choices": [],
                        "usage": {
                            "prompt_tokens": input_tokens,
                            "completion_tokens": output_tokens,
                            "total_tokens": input_tokens + output_tokens,
                        },
                    }
                    yield f"data: {json.dumps(usage_chunk)}\n\n"

                # Stream termination
                time.sleep(0.01)  # Small delay to ensure data is flushed
                yield "data: [DONE]\n\n"

            response = Response(generate(), mimetype="text/event-stream")
            response.headers['Cache-Control'] = 'no-cache'
            response.headers['X-Accel-Buffering'] = 'no'
            return response
        else:
            response = {
                "id": "chatcmpl-abc123",
                "object": "chat.completion",
                "created": int(arrived_at),
                "model": model,
                "usage": {
                    "prompt_tokens": input_tokens,
                    "completion_tokens": output_tokens,
                    "total_tokens": input_tokens + output_tokens,
                },
                "choices": [
                    {
                        "message": {
                            "role": "assistant",
                            "content": f"\n\nThis is simulated message from {model}!",
                        },
                        "logprobs": None,
                        "finish_reason": "stop",
                        "index": 0,
                    }
                ],
            }
            return jsonify(response), 200
    except Exception as e:
        err = {
            "error": {
                "message": f"The server had an error while processing your request. Sorry about that!",
                "type": "api_error",
                "param": None,
                "code": None,
            }
        }
        return jsonify(err), 500


@app.route("/v1/audio/speech", methods=["POST"])
@auth.login_required
def audio_speech():
    """
    Simulates the OpenAI text-to-speech endpoint.
    Returns mock audio data.
    """
    try:
        model = request.json.get("model", "tts-1")
        input_text = request.json.get("input")
        voice = request.json.get("voice", "alloy")
        response_format = request.json.get("response_format", "mp3")
        speed = request.json.get("speed", 1.0)

        if not input_text:
            return create_error_response("'input' is a required parameter", param="input")

        # Validate voice
        valid_voices = ["alloy", "echo", "fable", "onyx", "nova", "shimmer"]
        if voice not in valid_voices:
            return create_error_response(
                f"Invalid voice '{voice}'. Must be one of: {', '.join(valid_voices)}",
                param="voice"
            )

        # Validate response format
        valid_formats = ["mp3", "opus", "aac", "flac", "wav", "pcm"]
        if response_format not in valid_formats:
            return create_error_response(
                f"Invalid response_format '{response_format}'. Must be one of: {', '.join(valid_formats)}",
                param="response_format"
            )

        # Simulate processing time based on text length
        time.sleep(0.1 + len(input_text) * 0.001)

        # Generate mock audio data (minimal valid audio bytes)
        # This is a minimal MP3 frame header for testing purposes
        mock_audio = bytes([
            0xFF, 0xFB, 0x90, 0x00,  # MP3 frame header
            0x00, 0x00, 0x00, 0x00,
            0x00, 0x00, 0x00, 0x00,
            0x00, 0x00, 0x00, 0x00,
        ])

        # Set content type based on format
        content_types = {
            "mp3": "audio/mpeg",
            "opus": "audio/opus",
            "aac": "audio/aac",
            "flac": "audio/flac",
            "wav": "audio/wav",
            "pcm": "audio/pcm",
        }

        return Response(
            mock_audio,
            mimetype=content_types.get(response_format, "audio/mpeg"),
            headers={
                "Content-Disposition": f"attachment; filename=speech.{response_format}"
            }
        )

    except Exception as e:
        logger.error(f"Error in audio speech endpoint: {e}")
        return create_error_response(
            "The server had an error while processing your request. Sorry about that!",
            error_type="api_error",
            status_code=500
        )


@app.route("/v1/audio/transcriptions", methods=["POST"])
@auth.login_required
def audio_transcriptions():
    """
    Simulates the OpenAI audio transcription endpoint.
    Accepts multipart/form-data with audio file and returns a mock transcription.
    """
    try:
        # Get form data
        model = request.form.get("model")
        language = request.form.get("language", "en")
        response_format = request.form.get("response_format", "json")
        stream = request.form.get("stream", "false").lower() in ("true", "1")

        # Get the uploaded file (optional for mock)
        audio_file = request.files.get("file")

        if not model:
            return (
                jsonify(
                    {
                        "error": {
                            "message": "'model' is a required parameter",
                            "type": "invalid_request_error",
                            "param": "model",
                            "code": None,
                        }
                    }
                ),
                400,
            )

        # Simulate processing time
        time.sleep(0.1)

        # Mock transcription text
        mock_text = f"This is a simulated transcription from {model}. The audio file was processed successfully."

        if stream:
            def generate():
                completion_id = "transcript-" + "".join(
                    random.choices(
                        "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
                        k=20,
                    )
                )
                words = mock_text.split()
                for i, word in enumerate(words):
                    chunk = {
                        "id": completion_id,
                        "object": "audio.transcription.chunk",
                        "text": word + (" " if i < len(words) - 1 else ""),
                    }
                    yield f"data: {json.dumps(chunk)}\n\n"
                    time.sleep(0.05)

                # Final chunk
                final_chunk = {
                    "id": completion_id,
                    "object": "audio.transcription",
                    "text": mock_text,
                }
                yield f"data: {json.dumps(final_chunk)}\n\n"
                yield "data: [DONE]\n\n"

            response = Response(generate(), mimetype="text/event-stream")
            response.headers['Cache-Control'] = 'no-cache'
            response.headers['X-Accel-Buffering'] = 'no'
            return response
        else:
            if response_format == "verbose_json":
                response = {
                    "task": "transcribe",
                    "language": language,
                    "duration": 5.5,
                    "text": mock_text,
                    "words": [
                        {"word": word, "start": i * 0.5, "end": (i + 1) * 0.5}
                        for i, word in enumerate(mock_text.split())
                    ],
                    "segments": [
                        {
                            "id": 0,
                            "seek": 0,
                            "start": 0.0,
                            "end": 5.5,
                            "text": mock_text,
                            "tokens": list(range(100)),
                            "temperature": 0.0,
                            "avg_logprob": -0.25,
                            "compression_ratio": 1.5,
                            "no_speech_prob": 0.01,
                        }
                    ],
                }
            elif response_format in ("text", "srt", "vtt"):
                return Response(mock_text, mimetype="text/plain")
            else:  # json (default)
                response = {"text": mock_text}

            return jsonify(response), 200

    except Exception as e:
        logger.error(f"Error in audio transcriptions endpoint: {e}")
        err = {
            "error": {
                "message": "The server had an error while processing your request. Sorry about that!",
                "type": "api_error",
                "param": None,
                "code": None,
            }
        }
        return jsonify(err), 500


@app.route("/v1/audio/translations", methods=["POST"])
@auth.login_required
def audio_translations():
    """
    Simulates the OpenAI audio translation endpoint.
    Accepts multipart/form-data with audio file and returns a mock translation to English.
    """
    try:
        # Get form data
        model = request.form.get("model")
        response_format = request.form.get("response_format", "json")
        stream = request.form.get("stream", "false").lower() in ("true", "1")

        # Get the uploaded file (optional for mock)
        audio_file = request.files.get("file")

        if not model:
            return (
                jsonify(
                    {
                        "error": {
                            "message": "'model' is a required parameter",
                            "type": "invalid_request_error",
                            "param": "model",
                            "code": None,
                        }
                    }
                ),
                400,
            )

        # Simulate processing time
        time.sleep(0.1)

        # Mock translation text (always to English)
        mock_text = f"This is a simulated English translation from {model}. The audio was translated successfully."

        if stream:
            def generate():
                completion_id = "translation-" + "".join(
                    random.choices(
                        "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
                        k=20,
                    )
                )
                words = mock_text.split()
                for i, word in enumerate(words):
                    chunk = {
                        "id": completion_id,
                        "object": "audio.translation.chunk",
                        "text": word + (" " if i < len(words) - 1 else ""),
                    }
                    yield f"data: {json.dumps(chunk)}\n\n"
                    time.sleep(0.05)

                # Final chunk
                final_chunk = {
                    "id": completion_id,
                    "object": "audio.translation",
                    "text": mock_text,
                }
                yield f"data: {json.dumps(final_chunk)}\n\n"
                yield "data: [DONE]\n\n"

            response = Response(generate(), mimetype="text/event-stream")
            response.headers['Cache-Control'] = 'no-cache'
            response.headers['X-Accel-Buffering'] = 'no'
            return response
        else:
            if response_format == "verbose_json":
                response = {
                    "task": "translate",
                    "language": "en",
                    "duration": 5.5,
                    "text": mock_text,
                    "segments": [
                        {
                            "id": 0,
                            "seek": 0,
                            "start": 0.0,
                            "end": 5.5,
                            "text": mock_text,
                            "tokens": list(range(100)),
                            "temperature": 0.0,
                            "avg_logprob": -0.25,
                            "compression_ratio": 1.5,
                            "no_speech_prob": 0.01,
                        }
                    ],
                }
            elif response_format in ("text", "srt", "vtt"):
                return Response(mock_text, mimetype="text/plain")
            else:  # json (default)
                response = {"text": mock_text}

            return jsonify(response), 200

    except Exception as e:
        logger.error(f"Error in audio translations endpoint: {e}")
        err = {
            "error": {
                "message": "The server had an error while processing your request. Sorry about that!",
                "type": "api_error",
                "param": None,
                "code": None,
            }
        }
        return jsonify(err), 500


@app.route("/v1/embeddings", methods=["POST"])
@auth.login_required
def embeddings():
    try:
        input_data = request.json.get("input")
        model = request.json.get("model")
        encoding_format = request.json.get("encoding_format", "float")
        dimensions = request.json.get("dimensions")
        user = request.json.get("user")

        if not input_data or not model:
            return (
                jsonify(
                    {
                        "error": {
                            "message": "'input' and 'model' are required parameters",
                            "type": "invalid_request_error",
                            "param": "input" if not input_data else "model",
                            "code": None,
                        }
                    }
                ),
                400,
            )

        # Convert single string input to list for uniform processing
        inputs = [input_data] if isinstance(input_data, str) else input_data

        # Validate input limits
        if len(inputs) > 2048:
            return (
                jsonify(
                    {
                        "error": {
                            "message": "The maximum number of inputs is 2048",
                            "type": "invalid_request_error",
                            "param": "input",
                            "code": None,
                        }
                    }
                ),
                400,
            )

        # Default dimensions based on model
        default_dimensions = {
            "text-embedding-3-small": 1536,
            "text-embedding-3-large": 3072,
            "text-embedding-ada-002": 1536,
        }
        embedding_dim = (
            dimensions if dimensions else default_dimensions.get(model, 1536)
        )

        # Generate embeddings
        embeddings_data = []
        total_tokens = 0

        for idx, text in enumerate(inputs):
            # Calculate token count
            if isinstance(text, str):
                tokens = get_token_count(text)
            elif isinstance(text, list):
                # Array of tokens (integers)
                tokens = len(text)
            elif isinstance(text, int):
                # Single token (from a token array)
                tokens = 1
            else:
                tokens = 1  # Fallback for unexpected types
            total_tokens += tokens

            # Validate token limit per input
            if tokens > 8192:
                return (
                    jsonify(
                        {
                            "error": {
                                "message": f"Input {idx} exceeds maximum token limit of 8192",
                                "type": "invalid_request_error",
                                "param": "input",
                                "code": None,
                            }
                        }
                    ),
                    400,
                )

            # Generate random embedding vector
            if encoding_format == "float":
                # Generate normalized random vector
                embedding = [random.uniform(-1, 1) for _ in range(embedding_dim)]
                # Normalize the vector
                magnitude = sum(x**2 for x in embedding) ** 0.5
                if magnitude > 0:
                    embedding = [x / magnitude for x in embedding]
                else:
                    # Extremely unlikely but handle zero vector case
                    embedding = [1.0 / (embedding_dim ** 0.5)] * embedding_dim
            elif encoding_format == "base64":
                # Generate random vector and encode as base64
                embedding_floats = [random.uniform(-1, 1) for _ in range(embedding_dim)]
                # Pack floats as bytes
                embedding_bytes = struct.pack(f"{embedding_dim}f", *embedding_floats)
                # Encode to base64
                embedding = base64.b64encode(embedding_bytes).decode("utf-8")
            else:
                return (
                    jsonify(
                        {
                            "error": {
                                "message": f"Invalid encoding_format: {encoding_format}. Must be 'float' or 'base64'",
                                "type": "invalid_request_error",
                                "param": "encoding_format",
                                "code": None,
                            }
                        }
                    ),
                    400,
                )

            embeddings_data.append(
                {"object": "embedding", "embedding": embedding, "index": idx}
            )

        response = {
            "object": "list",
            "data": embeddings_data,
            "model": model,
            "usage": {"prompt_tokens": total_tokens, "total_tokens": total_tokens},
        }

        return jsonify(response), 200

    except Exception as e:
        logger.error(f"Error in embeddings endpoint: {e}")
        err = {
            "error": {
                "message": "The server had an error while processing your request. Sorry about that!",
                "type": "api_error",
                "param": None,
                "code": None,
            }
        }
        return jsonify(err), 500


@app.route("/v1/images/generations", methods=["POST"])
@auth.login_required
def images_generations():
    """
    Simulates the OpenAI image generation endpoint.
    Returns mock image data in OpenAI format.
    """
    try:
        model = request.json.get("model", "dall-e-3")
        prompt = request.json.get("prompt")
        n = request.json.get("n", 1)
        size = request.json.get("size", "1024x1024")
        quality = request.json.get("quality", "standard")
        response_format = request.json.get("response_format", "url")
        style = request.json.get("style", "vivid")
        user = request.json.get("user")

        if not prompt:
            return create_error_response("'prompt' is a required parameter", param="prompt")

        # Simulate processing time
        time.sleep(0.2)

        created = int(datetime.now().timestamp())

        # Generate mock image data
        data = []
        for i in range(n):
            if response_format == "url":
                image_data = {
                    "url": f"https://mock-images.example.com/{model}/{created}_{i}.png",
                    "revised_prompt": f"Mock revised prompt for: {prompt[:50]}..."
                }
            else:  # b64_json
                # Generate a small mock base64 encoded PNG (1x1 pixel transparent)
                mock_b64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
                image_data = {
                    "b64_json": mock_b64,
                    "revised_prompt": f"Mock revised prompt for: {prompt[:50]}..."
                }
            data.append(image_data)

        response = {
            "created": created,
            "data": data,
        }

        return jsonify(response), 200

    except Exception as e:
        logger.error(f"Error in images generations endpoint: {e}")
        return create_error_response(
            "The server had an error while processing your request. Sorry about that!",
            error_type="api_error",
            status_code=500
        )


@app.route("/v1/video/generations", methods=["POST"])
@auth.login_required
def video_generations():
    """
    Simulates a video generation endpoint.
    Returns mock video data.
    """
    try:
        model = request.json.get("model", "video-model")
        prompt = request.json.get("prompt")
        duration = request.json.get("duration", 5)
        fps = request.json.get("fps", 24)
        size = request.json.get("size", "1280x720")

        if not prompt:
            return create_error_response("'prompt' is a required parameter", param="prompt")

        # Simulate processing time
        time.sleep(0.3)

        created = int(datetime.now().timestamp())

        response = {
            "id": f"video-{created}",
            "object": "video.generation",
            "created": created,
            "model": model,
            "data": [
                {
                    "url": f"https://mock-videos.example.com/{model}/{created}.mp4",
                    "duration": duration,
                    "fps": fps,
                    "size": size,
                    "revised_prompt": f"Mock revised prompt for: {prompt[:50]}..."
                }
            ],
        }

        return jsonify(response), 200

    except Exception as e:
        logger.error(f"Error in video generations endpoint: {e}")
        return create_error_response(
            "The server had an error while processing your request. Sorry about that!",
            error_type="api_error",
            status_code=500
        )


@app.route("/v1/rerank", methods=["POST"])
@auth.login_required
def rerank():
    """
    Simulates the rerank endpoint (vLLM/JinaAI format).
    Re-ranks documents based on relevance to a query.
    """
    try:
        model = request.json.get("model", "rerank-model")
        query = request.json.get("query")
        documents = request.json.get("documents", [])
        top_n = request.json.get("top_n", 0)

        if not query:
            return create_error_response("'query' is a required parameter", param="query")

        if not documents:
            return create_error_response("'documents' is a required parameter", param="documents")

        # Simulate processing time
        time.sleep(0.1)

        # Generate mock relevance scores
        results = []
        for idx, doc in enumerate(documents):
            # Generate a random relevance score between 0 and 1
            relevance_score = random.uniform(0.1, 0.95)
            results.append({
                "index": idx,
                "document": {"text": doc if isinstance(doc, str) else str(doc)},
                "relevance_score": round(relevance_score, 6)
            })

        # Sort by relevance score (descending)
        results.sort(key=lambda x: x["relevance_score"], reverse=True)

        # Apply top_n if specified
        if top_n > 0:
            results = results[:top_n]

        # Calculate token counts
        query_tokens = get_token_count(query) if tokenizer else len(query.split())
        doc_tokens = sum(
            get_token_count(d) if tokenizer else len(str(d).split())
            for d in documents
        )
        total_tokens = query_tokens + doc_tokens

        response = {
            "id": f"rerank-{int(datetime.now().timestamp())}",
            "model": model,
            "usage": {
                "prompt_tokens": total_tokens,
                "total_tokens": total_tokens
            },
            "results": results
        }

        return jsonify(response), 200

    except Exception as e:
        logger.error(f"Error in rerank endpoint: {e}")
        return create_error_response(
            "The server had an error while processing your request. Sorry about that!",
            error_type="api_error",
            status_code=500
        )


@app.route("/version", methods=["GET"])
def version():
    """
    Returns the version information (vLLM-compatible).
    """
    return jsonify({"version": "0.13.0-mock"})


@app.route("/tokenize", methods=["POST"])
@auth.login_required
def tokenize():
    """
    Simulates the tokenize endpoint.
    Returns token IDs for the given text.
    """
    try:
        data = request.json or {}
        model = data.get("model")
        prompt = data.get("prompt")
        add_special_tokens = data.get("add_special_tokens", True)

        if not prompt:
            return create_error_response("'prompt' is a required parameter", param="prompt")

        # Generate mock tokens
        if tokenizer is not None:
            encoded = tokenizer(prompt, add_special_tokens=add_special_tokens)
            tokens = encoded["input_ids"]
        else:
            # Mock tokenization: split by whitespace and assign sequential IDs
            words = prompt.split()
            tokens = list(range(100, 100 + len(words)))

        response = {
            "tokens": tokens,
            "count": len(tokens),
            "max_model_len": 16384,
        }

        return jsonify(response), 200

    except Exception as e:
        logger.error(f"Error in tokenize endpoint: {e}")
        return create_error_response(
            "The server had an error while processing your request. Sorry about that!",
            error_type="api_error",
            status_code=500
        )


@app.route("/detokenize", methods=["POST"])
@auth.login_required
def detokenize():
    """
    Simulates the detokenize endpoint.
    Returns text for the given token IDs.
    """
    try:
        data = request.json or {}
        model = data.get("model")
        tokens = data.get("tokens")

        if not tokens:
            return create_error_response("'tokens' is a required parameter", param="tokens")

        # Generate mock detokenization
        if tokenizer is not None:
            try:
                prompt = tokenizer.decode(tokens)
            except Exception:
                prompt = f"[Detokenized {len(tokens)} tokens]"
        else:
            prompt = f"[Mock detokenized text for {len(tokens)} tokens]"

        response = {
            "prompt": prompt,
        }

        return jsonify(response), 200

    except Exception as e:
        logger.error(f"Error in detokenize endpoint: {e}")
        return create_error_response(
            "The server had an error while processing your request. Sorry about that!",
            error_type="api_error",
            status_code=500
        )


@app.route("/load", methods=["GET"])
def load():
    """
    Returns the current server load metrics (vLLM-compatible).
    """
    server_load = overrides.get("server_load", random.randint(0, 10))
    return jsonify({"server_load": server_load})


@app.route("/ping", methods=["GET", "POST"])
def ping():
    """
    Simple health check endpoint (SageMaker-compatible).
    """
    return Response(status=200)


@app.route("/set_metrics", methods=["POST"])
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


def generate_histogram_metric(
    metric_name, description, model_name, buckets, new_requests, help_header=True
):
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
            "total_count": 0,  # Total count of all events
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
                current_state["buckets"][buckets[buckets.index(bucket) - 1]],
            )

    # Update total_count and total_sum
    current_state["total_count"] = current_state["buckets"][
        buckets[-1]
    ]  # `+Inf` bucket is the total count
    current_state["total_sum"] += sum(
        float(bucket) * value
        for bucket, value in new_requests.items()
        if bucket != "+Inf"
    )

    # Generate Prometheus bucket strings
    bucket_strings = "\n".join(
        [
            f'vllm:{metric_name}_bucket{{le="{bucket}",model_name="{model_name}"}} {current_state["buckets"][bucket]}'
            for bucket in buckets
        ]
    )

    # Return formatted histogram metric
    histogram_template = (
        """
# HELP vllm:{metric_name} {description}
# TYPE vllm:{metric_name} histogram
vllm:{metric_name}_sum{{model_name="{model_name}"}} {value}
{buckets}
vllm:{metric_name}_count{{model_name="{model_name}"}} {count}
"""
        if help_header
        else """
vllm:{metric_name}_sum{{model_name="{model_name}"}} {value}
{buckets}
vllm:{metric_name}_count{{model_name="{model_name}"}} {count}
"""
    )

    return histogram_template.format(
        metric_name=metric_name,
        description=description,
        model_name=model_name,
        value=current_state["total_sum"],
        buckets=bucket_strings,
        count=current_state["total_count"],
    )


def generate_counter_gauge_metric(
    metric_name, metric_type, description, model_name, value, help_header=True
):
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
    counter_gauge_template = (
        """
# HELP vllm:{metric_name} {description}
# TYPE vllm:{metric_name} {metric_type}
vllm:{metric_name}{{model_name="{model_name}"}} {value}
"""
        if help_header
        else """
vllm:{metric_name}{{model_name="{model_name}"}} {value}
"""
    )

    return counter_gauge_template.format(
        metric_name=metric_name,
        metric_type=metric_type,
        description=description,
        model_name=model_name,
        value=value,
    )


@app.route("/metrics")
def metrics():
    # get deployment information
    if STANDALONE_MODE:
        replicas = DEFAULT_REPLICAS
    else:
        try:
            apps_v1 = client.AppsV1Api()
            resp = apps_v1.read_namespaced_deployment(DEPLOYMENT_NAME, NAMESPACE)
            replicas = resp.spec.replicas if resp.spec.replicas is not None else 1
        except Exception as e:
            print(
                f"Failed to get deployment information: {DEPLOYMENT_NAME=} {NAMESPACE=} error={str(e)}"
            )
            print(
                f"Due to the failure, replicas {DEFAULT_REPLICAS} will be used to calculate metrics"
            )
            replicas = DEFAULT_REPLICAS

    # a reasonable mock total value
    total = overrides.get("total", 100.0)
    model_name = overrides.get("model_name", MODEL_NAME)
    # calculate metrics with potential overrides
    success_total = overrides.get("success_total", total / replicas)
    avg_prompt_throughput = overrides.get(
        "avg_prompt_throughput", total / replicas if replicas > 0 else 0
    )
    avg_generation_throughput = overrides.get(
        "avg_generation_throughput", total / replicas if replicas > 0 else 0
    )
    prompt_tokens_total = overrides.get(
        "prompt_tokens_total", randint(100, 1024) * success_total
    )
    generation_tokens_total = overrides.get(
        "generation_tokens_total", randint(100, 1024) * success_total
    )
    running = overrides.get("running", randint(1, 100))
    cpu_running = overrides.get("cpu_running", randint(1, 100))
    waiting = overrides.get("waiting", randint(1, 100))
    swapped = overrides.get("swapped", randint(1, 100))
    max_running_capacity = 100
    gpu_cache_usage_perc = overrides.get(
        "gpu_cache_usage_perc", min(100.0, (running / max_running_capacity) * 100)
    )
    cpu_cache_usage_perc = overrides.get(
        "cpu_cache_usage_perc", min(100.0, (cpu_running / max_running_capacity) * 100)
    )

    # Define metrics and their attributes
    simple_metrics = [
        {
            "name": "prompt_tokens_total",
            "type": "counter",
            "description": "Count of prefill tokens processed.",
            "value": overrides.get("prompt_tokens_total", prompt_tokens_total),
        },
        {
            "name": "generation_tokens_total",
            "type": "counter",
            "description": "Count of generation tokens processed.",
            "value": overrides.get("generation_tokens_total", generation_tokens_total),
        },
        {
            "name": "request_success_total",
            "type": "counter",
            "description": "Count of successfully processed requests.",
            "value": overrides.get("success_total", success_total),
        },
        {
            "name": "num_requests_running",
            "type": "gauge",
            "description": "Number of requests currently running on GPU.",
            "value": overrides.get("running", running),
        },
        {
            "name": "num_requests_swapped",
            "type": "gauge",
            "description": "Number of requests swapped to CPU.",
            "value": overrides.get("swapped", swapped),
        },
        {
            "name": "num_requests_waiting",
            "type": "gauge",
            "description": "Number of requests waiting to be processed.",
            "value": overrides.get("waiting", waiting),
        },
        {
            "name": "avg_prompt_throughput_toks_per_s",
            "type": "gauge",
            "description": "Average prefill throughput in tokens/s.",
            "value": overrides.get("avg_prompt_throughput", avg_prompt_throughput),
        },
        {
            "name": "avg_generation_throughput_toks_per_s",
            "type": "gauge",
            "description": "Average generation throughput in tokens/s.",
            "value": overrides.get(
                "avg_generation_throughput", avg_generation_throughput
            ),
        },
        {
            "name": "gpu_cache_usage_perc",
            "type": "gauge",
            "description": "GPU KV-cache usage. 1 means 100 percent usage.",
            "value": overrides.get("gpu_cache_usage_perc", gpu_cache_usage_perc),
        },
        {
            "name": "cpu_cache_usage_perc",
            "type": "gauge",
            "description": "CPU KV-cache usage. 1 means 100 percent usage.",
            "value": overrides.get("cpu_cache_usage_perc", cpu_cache_usage_perc),
        },
    ]

    # Generate all metrics
    metrics_output = ""
    for metric in simple_metrics:
        metrics_output += generate_counter_gauge_metric(
            metric["name"],
            metric["type"],
            metric["description"],
            model_name,
            metric["value"],
        )
        metrics_output += generate_counter_gauge_metric(
            metric["name"],
            metric["type"],
            metric["description"],
            "text2sql-lora-2",
            metric["value"],
            help_header=False,
        )

    metrics_output += """
# HELP vllm:lora_requests_info Running stats on lora requests.
# TYPE vllm:lora_requests_info gauge
vllm:lora_requests_info{max_lora="1",running_lora_adapters="text2sql-lora-2",waiting_lora_adapters=""} 1
"""

    histogram_metrics = [
        {
            "name": "iteration_tokens_total",
            "type": "histogram",
            "description": "Histogram of number of tokens per engine_step.",
            "buckets": [
                "1.0",
                "8.0",
                "16.0",
                "32.0",
                "64.0",
                "128.0",
                "256.0",
                "512.0",
                "1024.0",
                "2048.0",
                "4096.0",
                "8192.0",
                "+Inf",
            ],
        },
        {
            "name": "time_to_first_token_seconds",
            "type": "histogram",
            "description": "Histogram of time to first token in seconds.",
            "buckets": [
                "0.001",
                "0.005",
                "0.01",
                "0.02",
                "0.04",
                "0.06",
                "0.08",
                "0.1",
                "0.25",
                "0.5",
                "+Inf",
            ],
        },
        {
            "name": "time_per_output_token_seconds",
            "type": "histogram",
            "description": "Histogram of time per output token in seconds.",
            "buckets": [
                "0.01",
                "0.025",
                "0.05",
                "0.075",
                "0.1",
                "0.15",
                "0.2",
                "0.3",
                "0.4",
                "+Inf",
            ],
        },
        {
            "name": "request_prompt_tokens",
            "type": "histogram",
            "description": "Histogram of number of prefill tokens processed..",
            "buckets": [
                "1.0",
                "2.0",
                "5.0",
                "10.0",
                "20.0",
                "50.0",
                "100.0",
                "200.0",
                "500.0",
                "1000.0",
                "2000.0",
                "5000.0",
                "10000.0",
                "+Inf",
            ],
        },
        {
            "name": "request_generation_tokens",
            "type": "histogram",
            "description": "Histogram of number of generation tokens processed..",
            "buckets": [
                "1.0",
                "2.0",
                "5.0",
                "10.0",
                "20.0",
                "50.0",
                "100.0",
                "200.0",
                "500.0",
                "1000.0",
                "2000.0",
                "5000.0",
                "10000.0",
                "+Inf",
            ],
        },
        {
            "name": "e2e_request_latency_seconds",
            "type": "histogram",
            "description": "Histogram of end to end request latency in seconds.",
            "buckets": ["0.3", "0.5", "0.8", "1.0", "1.5", "2.0", "5.0", "+Inf"],
        },
        {
            "name": "request_queue_time_seconds",
            "type": "histogram",
            "description": "Histogram of time spent in WAITING phase for request.",
            "buckets": ["0.3", "0.5", "0.8", "1.0", "1.5", "2.0", "5.0", "+Inf"],
        },
        {
            "name": "request_inference_time_seconds",
            "type": "histogram",
            "description": "Histogram of time spent in RUNNING phase for request.",
            "buckets": ["0.3", "0.5", "0.8", "1.0", "1.5", "2.0", "5.0", "+Inf"],
        },
        {
            "name": "request_decode_time_seconds",
            "type": "histogram",
            "description": "Histogram of time spent in DECODE phase for request.",
            "buckets": ["0.3", "0.5", "0.8", "1.0", "1.5", "2.0", "5.0", "+Inf"],
        },
        {
            "name": "request_prefill_time_seconds",
            "type": "histogram",
            "description": "Histogram of time spent in PREFILL phase for request.",
            "buckets": ["0.3", "0.5", "0.8", "1.0", "1.5", "2.0", "5.0", "+Inf"],
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
            new_requests=new_requests,
        )
        new_requests = {bucket: random.randint(0, 5) for bucket in metric["buckets"]}
        histogram_metrics_output += generate_histogram_metric(
            metric_name=metric["name"],
            description=metric["description"],
            model_name="text2sql-lora-2",
            buckets=metric["buckets"],
            new_requests=new_requests,
            help_header=False,
        )

    return Response(metrics_output + histogram_metrics_output, mimetype="text/plain")


if __name__ == "__main__":
    logging.basicConfig(level=logging.DEBUG)
    logging.getLogger("kubernetes.client.rest").setLevel(
        logging.ERROR
    )  # Suppress kubenetes logs

    print(
        f"Starting app. DEPLOYMENT_NAME: {DEPLOYMENT_NAME}, NAMESPACE: {NAMESPACE}, MODEL: {MODEL_NAME}"
    )

    # Extract gpu_device without call argparse
    gpu_device = "disabled"
    try:
        index = sys.argv.index("--replica_config_device")
        if index + 1 < len(sys.argv):
            gpu_device = sys.argv[index + 1]
    except ValueError:
        pass

    # Restore -h functionality
    if "-h" in sys.argv:
        SimulationConfig.create_from_cli_args()

    # Launch simulator
    if gpu_device != "disabled":
        # Load the tokenizer for your model
        from transformers import AutoTokenizer

        default_model = "bert-base-uncased"
        try:
            # can we make this as an application argument.
            # no need to use such map, we can use huggingface id directly.
            token_model = modelMaps.get(MODEL_NAME, default_model)
            tokenizer = AutoTokenizer.from_pretrained(
                token_model,
                token=HUGGINGFACE_TOKEN,
                model_max_length=16384,  # Suppress warning
                clean_up_tokenization_spaces=True,
            )
        except Exception as e:
            logger.error(
                f"Failed to initialize tokenizer, will use default tokenizer model: {e}"
            )
            tokenizer = AutoTokenizer.from_pretrained(
                default_model,
                model_max_length=16384,  # Suppress warning
                clean_up_tokenization_spaces=True,
            )

        # TODO: check whether able to use argparse to build SimulationConfig
        simulator = Simulator(SimulationConfig.create_from_cli_args())
        overrides = {"total": 100.0, "running": 0, "waiting": 0, "swapped": 0}

    thread = None
    if simulator is not None:
        # TODO: Move simulation to a separate workflow, independent of the main web service
        thread = simulator.start()

    # Perform profiling and skip actual run
    if "--time_limit" not in sys.argv:
        if not STANDALONE_MODE:
            try:
                # config.load_kube_config()
                config.load_incluster_config()
            except Exception as e:
                print(f"Failed to load k8s config: {e}")

        app.run(host="0.0.0.0", port=8000)

    if simulator is not None:
        simulator.stop()

    if thread is not None:
        thread.join()

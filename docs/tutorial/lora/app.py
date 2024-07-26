from flask import Flask, request, jsonify
import time

app = Flask(__name__)

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
    new_model = {}
    new_model['id'] = request.json.get('lora_name')
    new_model['created'] = int(time.time())
    new_model['object'] = "model"
    new_model['owned_by'] = "vllm"
    new_model['parent'] = None
    new_model['root'] = request.json.get('lora_path')
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
            "prompt_tokens": 5,
            "completion_tokens": 7,
            "total_tokens": 12
        }
    }
    return jsonify(response), 200


@app.route('/v1/chat/completions', methods=['POST'])
def chat_completions():
    messages = request.json.get('messages')
    model = request.json.get('model')
    if not messages or not model:
        return jsonify({"status": "error", "message": "Messages and model are required"}), 400

    # Simulated response
    response = {
        "id": "chatcmpl-abc123",
        "object": "chat.completion",
        "created": 1677858242,
        "model": model,
        "usage": {
            "prompt_tokens": 13,
            "completion_tokens": 7,
            "total_tokens": 20
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


if __name__ == '__main__':
    app.run(host='0.0.0.0', port=8000)

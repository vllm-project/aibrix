from flask import Flask, request, jsonify
import time

app = Flask(__name__)

@app.route('/v1/completions', methods=['POST'])
def completion():
    prompt = request.json.get('prompt')
    model = request.json.get('model')
    if not prompt or not model:
        return jsonify({"status": "error", "message": "Prompt and model are required"}), 400
    
    if model != "m2": 
        return jsonify({"status": "error", "message": "incorrect model name"}), 500

    # Simulated response
    response = {
        "id": "cmpl-uqkvlQyYK7bGYrRHQ0eXlWi7",
        "object": "text_completion",
        "created": 1589478378,
        "model": model,
        "system_fingerprint": "fp_44709d6fcb",
        "choices": [
            {
                "text": f"Request for M2. This is indeed a test from model {model}!",
                "index": 0,
                "logprobs": None,
                "finish_reason": "length"
            }
        ],
        "usage": {
            "prompt_tokens": 11,
            "completion_tokens": 22,
            "total_tokens": 33
        }
    }
    return jsonify(response), 200


@app.route('/v1/chat/completions', methods=['POST'])
def chat_completions():
    messages = request.json.get('messages')
    model = request.json.get('model')
    if not messages or not model:
        return jsonify({"status": "error", "message": "Messages and model are required"}), 400

    if model != "m2": 
        return jsonify({"status": "error", "message": "incorrect model name"}), 500
    
    # Simulated response
    response = {
        "id": "chatcmpl-abc123",
        "object": "chat.completion",
        "created": 1677858242,
        "model": model,
        "usage": {
            "prompt_tokens": 30,
            "completion_tokens": 15,
            "total_tokens": 45
        },
        "choices": [
            {
                "message": {
                    "role": "assistant",
                    "content": f"\n\nRequest for M2. This is a test from{model}!"
                },
                "logprobs": None,
                "finish_reason": "stop",
                "index": 0
            }
        ]
    }
    return jsonify(response), 200


if __name__ == '__main__':
    app.run(host='0.0.0.0', port=16000)

import base64
import json
import requests
import argparse
import mimetypes
import os

def encode_file_to_base64(file_path):
    """
    Encode an image or video file to a base64 string with proper MIME type prefix.
    """
    mime_type, _ = mimetypes.guess_type(file_path)
    if mime_type is None:
        # Fallback if unknown
        if file_path.lower().endswith((".mp4", ".mov", ".avi")):
            mime_type = "video/mp4"
        else:
            mime_type = "image/jpeg"

    with open(file_path, "rb") as f:
        encoded_bytes = base64.b64encode(f.read())
    return f"data:{mime_type};base64,{encoded_bytes.decode('utf-8')}"

def create_request_payload(text_prompt, file_base64, file_type="image"):
    """
    Create JSON payload for the API request.
    file_type: "image" or "video" (affects the type in JSON)
    """
    content_type = "image_url" if file_type == "image" else "video_url"
    payload = {
        "model": "qwen-vl",
        "messages": [
            {
                "role": "user",
                "content": [
                    {"type": "text", "text": text_prompt},
                    {"type": content_type, content_type: {"url": file_base64}}
                ]
            }
        ]
    }
    return payload

def send_request(api_url, payload):
    """Send POST request to the API."""
    headers = {"Content-Type": "application/json"}
    response = requests.post(api_url, headers=headers, data=json.dumps(payload))
    return response

def main():
    parser = argparse.ArgumentParser(description="Send an image or video encoded as base64 to OpenAI-like API")
    parser.add_argument("file", help="Path to the image or video file")
    parser.add_argument("--text", default="What are in these files?", help="Text prompt")
    parser.add_argument("--url", default="http://localhost:8888/v1/chat/completions", help="API endpoint URL")
    args = parser.parse_args()

    file_path = args.file
    if not os.path.exists(file_path):
        raise FileNotFoundError(f"File not found: {file_path}")

    # Determine if image or video
    ext = os.path.splitext(file_path)[1].lower()
    if ext in [".mp4", ".mov", ".avi", ".mkv"]:
        file_type = "video"
    else:
        file_type = "image"

    file_base64 = encode_file_to_base64(file_path)
    payload = create_request_payload(args.text, file_base64, file_type=file_type)
    response = send_request(args.url, payload)

    print("Status code:", response.status_code)
    print("Response:", response.text)

if __name__ == "__main__":
    main()

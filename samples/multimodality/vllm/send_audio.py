import os
import sys
import requests
import argparse


def send_audio_request(file_path, model, mode, url, language=None, response_format=None):
    """Send audio file to transcription or translation endpoint using multipart/form-data."""
    if mode == "transcribe":
        endpoint = f"{url}/v1/audio/transcriptions"
    else:
        endpoint = f"{url}/v1/audio/translations"

    with open(file_path, "rb") as f:
        files = {"file": (os.path.basename(file_path), f)}
        data = {"model": model}
        if response_format:
            data["response_format"] = response_format
        if mode == "transcribe" and language:
            data["language"] = language

        response = requests.post(endpoint, files=files, data=data)

    return response


def main():
    parser = argparse.ArgumentParser(
        description="Send an audio file to the AIBrix audio transcription or translation endpoint"
    )
    parser.add_argument("file", help="Path to the audio file (e.g. audio.mp3, audio.wav)")
    parser.add_argument(
        "--mode",
        choices=["transcribe", "translate"],
        default="transcribe",
        help="Operation mode: 'transcribe' (speech-to-text) or 'translate' (speech-to-English text)",
    )
    parser.add_argument("--model", required=True, help="Model name (e.g. qwen-audio-7b)")
    parser.add_argument(
        "--language",
        help="Source language code (e.g. zh, en, ja). Transcription only; ignored for translation.",
    )
    parser.add_argument(
        "--response-format",
        dest="response_format",
        default="json",
        help="Response format: json or text (default: json)",
    )
    parser.add_argument(
        "--url",
        default="http://localhost:8888",
        help="Base URL of the AIBrix gateway (default: http://localhost:8888)",
    )
    args = parser.parse_args()

    if not os.path.exists(args.file):
        print(f"Error: file not found: {args.file}", file=sys.stderr)
        sys.exit(1)

    response = send_audio_request(
        file_path=args.file,
        model=args.model,
        mode=args.mode,
        url=args.url,
        language=args.language,
        response_format=args.response_format,
    )

    print("Status code:", response.status_code)
    print("Response:", response.text)


if __name__ == "__main__":
    main()

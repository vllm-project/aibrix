#!/usr/bin/env python3
"""
Test script using OpenAI SDK to validate mocked app endpoints.
Tests both regular and streaming responses for OpenAI-compatible endpoints.

Usage:
    # First start the mocked app:
    STANDALONE_MODE=true python app.py

    # Then run this test:
    python test_openai_endpoints.py

    # Or run with custom base URL:
    python test_openai_endpoints.py --base-url http://localhost:8000/v1
"""
import argparse
import sys
import time
from typing import Optional

try:
    from openai import OpenAI
    from openai import APIError, APIConnectionError, BadRequestError
except ImportError:
    print("OpenAI SDK not installed. Run: pip install openai")
    sys.exit(1)


class TestResult:
    def __init__(self):
        self.passed = 0
        self.failed = 0
        self.errors = []

    def add_pass(self, name: str):
        self.passed += 1
        print(f"  ✓ PASS: {name}")

    def add_fail(self, name: str, error: str):
        self.failed += 1
        self.errors.append((name, error))
        print(f"  ✗ FAIL: {name}")
        print(f"         Error: {error}")

    def summary(self):
        total = self.passed + self.failed
        print(f"\n{'='*60}")
        print(f"SUMMARY: {self.passed}/{total} passed, {self.failed} failed")
        if self.errors:
            print(f"\nFailed tests:")
            for name, error in self.errors:
                print(f"  - {name}: {error}")
        print(f"{'='*60}\n")
        return self.failed == 0


def create_client(base_url: str, api_key: str = "test-key") -> OpenAI:
    """Create OpenAI client pointing to mocked server."""
    return OpenAI(
        base_url=base_url,
        api_key=api_key,
    )


def test_models(client: OpenAI, result: TestResult):
    """Test /v1/models endpoint."""
    print("\n--- Testing /v1/models ---")
    try:
        models = client.models.list()
        if models.data and len(models.data) > 0:
            result.add_pass("List models")
            print(f"         Found {len(models.data)} models: {[m.id for m in models.data]}")
        else:
            result.add_fail("List models", "No models returned")
    except Exception as e:
        result.add_fail("List models", str(e))


def test_chat_completions(client: OpenAI, result: TestResult):
    """Test /v1/chat/completions endpoint."""
    print("\n--- Testing /v1/chat/completions ---")

    # Test non-streaming
    try:
        response = client.chat.completions.create(
            model="meta-llama/Llama-2-7b-hf",
            messages=[
                {"role": "system", "content": "You are a helpful assistant."},
                {"role": "user", "content": "Hello!"}
            ],
            max_tokens=50
        )
        if response.choices and response.choices[0].message.content:
            result.add_pass("Chat completions (non-streaming)")
            print(f"         Response: {response.choices[0].message.content[:50]}...")
            print(f"         Usage: {response.usage}")
        else:
            result.add_fail("Chat completions (non-streaming)", "Empty response")
    except Exception as e:
        result.add_fail("Chat completions (non-streaming)", str(e))

    # Test streaming
    try:
        stream = client.chat.completions.create(
            model="meta-llama/Llama-2-7b-hf",
            messages=[{"role": "user", "content": "Hello!"}],
            stream=True
        )
        chunks = []
        content = ""
        for chunk in stream:
            chunks.append(chunk)
            if chunk.choices and chunk.choices[0].delta.content:
                content += chunk.choices[0].delta.content

        if len(chunks) > 1 and content:
            result.add_pass("Chat completions (streaming)")
            print(f"         Received {len(chunks)} chunks")
            print(f"         Content: {content[:50]}...")
        else:
            result.add_fail("Chat completions (streaming)", f"Only {len(chunks)} chunks received")
    except Exception as e:
        result.add_fail("Chat completions (streaming)", str(e))

    # Test streaming with usage
    try:
        stream = client.chat.completions.create(
            model="meta-llama/Llama-2-7b-hf",
            messages=[{"role": "user", "content": "Hello!"}],
            stream=True,
            stream_options={"include_usage": True}
        )
        chunks = list(stream)
        # Check if last chunk has usage info
        has_usage = any(hasattr(c, 'usage') and c.usage is not None for c in chunks)
        if has_usage:
            result.add_pass("Chat completions (streaming with usage)")
        else:
            result.add_fail("Chat completions (streaming with usage)", "No usage in stream")
    except Exception as e:
        result.add_fail("Chat completions (streaming with usage)", str(e))

    # Test error case - missing model
    try:
        client.chat.completions.create(
            model="",
            messages=[{"role": "user", "content": "Hello!"}]
        )
        result.add_fail("Chat completions (missing model)", "Should have raised error")
    except BadRequestError:
        result.add_pass("Chat completions (error handling - missing model)")
    except Exception as e:
        result.add_fail("Chat completions (error handling - missing model)", str(e))


def test_completions(client: OpenAI, result: TestResult):
    """Test /v1/completions endpoint."""
    print("\n--- Testing /v1/completions ---")

    # Test non-streaming
    try:
        response = client.completions.create(
            model="meta-llama/Llama-2-7b-hf",
            prompt="Once upon a time",
            max_tokens=50
        )
        if response.choices and response.choices[0].text:
            result.add_pass("Completions (non-streaming)")
            print(f"         Response: {response.choices[0].text[:50]}...")
        else:
            result.add_fail("Completions (non-streaming)", "Empty response")
    except Exception as e:
        result.add_fail("Completions (non-streaming)", str(e))

    # Test streaming
    try:
        stream = client.completions.create(
            model="meta-llama/Llama-2-7b-hf",
            prompt="Once upon a time",
            stream=True
        )
        chunks = []
        text = ""
        for chunk in stream:
            chunks.append(chunk)
            if chunk.choices and chunk.choices[0].text:
                text += chunk.choices[0].text

        if len(chunks) > 1:
            result.add_pass("Completions (streaming)")
            print(f"         Received {len(chunks)} chunks")
            print(f"         Text: {text[:50]}...")
        else:
            result.add_fail("Completions (streaming)", f"Only {len(chunks)} chunks received")
    except Exception as e:
        result.add_fail("Completions (streaming)", str(e))


def test_embeddings(client: OpenAI, result: TestResult):
    """Test /v1/embeddings endpoint."""
    print("\n--- Testing /v1/embeddings ---")

    # Test single input
    try:
        response = client.embeddings.create(
            model="text-embedding-ada-002",
            input="Hello world"
        )
        if response.data and len(response.data[0].embedding) > 0:
            result.add_pass("Embeddings (single input)")
            print(f"         Embedding dimension: {len(response.data[0].embedding)}")
            print(f"         Usage: {response.usage}")
        else:
            result.add_fail("Embeddings (single input)", "Empty embedding")
    except Exception as e:
        result.add_fail("Embeddings (single input)", str(e))

    # Test multiple inputs
    try:
        response = client.embeddings.create(
            model="text-embedding-ada-002",
            input=["Hello", "World", "Test"]
        )
        if response.data and len(response.data) == 3:
            result.add_pass("Embeddings (multiple inputs)")
            print(f"         Got {len(response.data)} embeddings")
        else:
            result.add_fail("Embeddings (multiple inputs)", f"Expected 3, got {len(response.data)}")
    except Exception as e:
        result.add_fail("Embeddings (multiple inputs)", str(e))


def test_audio_speech(client: OpenAI, result: TestResult):
    """Test /v1/audio/speech endpoint (TTS)."""
    print("\n--- Testing /v1/audio/speech ---")

    try:
        response = client.audio.speech.create(
            model="tts-1",
            voice="alloy",
            input="Hello, this is a test."
        )
        # Response should be audio bytes
        content = response.read()
        if len(content) > 0:
            result.add_pass("Audio speech (TTS)")
            print(f"         Audio size: {len(content)} bytes")
        else:
            result.add_fail("Audio speech (TTS)", "Empty audio response")
    except Exception as e:
        result.add_fail("Audio speech (TTS)", str(e))


def test_images(client: OpenAI, result: TestResult):
    """Test /v1/images/generations endpoint."""
    print("\n--- Testing /v1/images/generations ---")

    try:
        response = client.images.generate(
            model="dall-e-3",
            prompt="A cute cat",
            n=1,
            size="1024x1024"
        )
        if response.data and len(response.data) > 0:
            result.add_pass("Image generation")
            print(f"         Generated {len(response.data)} image(s)")
            if response.data[0].url:
                print(f"         URL: {response.data[0].url[:50]}...")
        else:
            result.add_fail("Image generation", "No images returned")
    except Exception as e:
        result.add_fail("Image generation", str(e))


def test_audio_transcription(client: OpenAI, result: TestResult):
    """Test /v1/audio/transcriptions endpoint."""
    print("\n--- Testing /v1/audio/transcriptions ---")

    try:
        # Create a minimal audio file for testing
        import tempfile
        import os

        # Create a minimal WAV file header (44 bytes) + silence
        wav_header = bytes([
            0x52, 0x49, 0x46, 0x46,  # "RIFF"
            0x24, 0x00, 0x00, 0x00,  # File size - 8
            0x57, 0x41, 0x56, 0x45,  # "WAVE"
            0x66, 0x6D, 0x74, 0x20,  # "fmt "
            0x10, 0x00, 0x00, 0x00,  # Subchunk1Size (16)
            0x01, 0x00,              # AudioFormat (PCM)
            0x01, 0x00,              # NumChannels (1)
            0x44, 0xAC, 0x00, 0x00,  # SampleRate (44100)
            0x88, 0x58, 0x01, 0x00,  # ByteRate
            0x02, 0x00,              # BlockAlign
            0x10, 0x00,              # BitsPerSample (16)
            0x64, 0x61, 0x74, 0x61,  # "data"
            0x00, 0x00, 0x00, 0x00,  # Subchunk2Size
        ])

        with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as f:
            f.write(wav_header)
            temp_path = f.name

        try:
            with open(temp_path, "rb") as audio_file:
                response = client.audio.transcriptions.create(
                    model="whisper-1",
                    file=audio_file
                )
            if response.text:
                result.add_pass("Audio transcription")
                print(f"         Transcription: {response.text[:50]}...")
            else:
                result.add_fail("Audio transcription", "Empty transcription")
        finally:
            os.unlink(temp_path)
    except Exception as e:
        result.add_fail("Audio transcription", str(e))


def test_connection(base_url: str) -> bool:
    """Test if the server is reachable."""
    import urllib.request
    import urllib.error

    health_url = base_url.replace("/v1", "") + "/health"
    try:
        with urllib.request.urlopen(health_url, timeout=5) as response:
            return response.status == 200
    except (urllib.error.URLError, urllib.error.HTTPError):
        return False


def main():
    parser = argparse.ArgumentParser(description="Test mocked app with OpenAI SDK")
    parser.add_argument(
        "--base-url",
        default="http://localhost:8000/v1",
        help="Base URL of the mocked server (default: http://localhost:8000/v1)"
    )
    parser.add_argument(
        "--api-key",
        default="test-key",
        help="API key for authentication (default: test-key)"
    )
    parser.add_argument(
        "--skip-connection-check",
        action="store_true",
        help="Skip checking if server is running"
    )
    args = parser.parse_args()

    print(f"\n{'='*60}")
    print("OPENAI SDK COMPATIBILITY TESTS")
    print(f"{'='*60}")
    print(f"Base URL: {args.base_url}")

    # Check if server is running
    if not args.skip_connection_check:
        print("\nChecking server connection...")
        if not test_connection(args.base_url):
            print(f"\n✗ Cannot connect to server at {args.base_url}")
            print("  Please start the mocked app first:")
            print("  STANDALONE_MODE=true python app.py")
            return 1

    # Create client
    client = create_client(args.base_url, args.api_key)
    result = TestResult()

    # Run tests
    test_models(client, result)
    test_chat_completions(client, result)
    test_completions(client, result)
    test_embeddings(client, result)
    test_audio_speech(client, result)
    test_images(client, result)
    test_audio_transcription(client, result)

    # Print summary
    success = result.summary()
    return 0 if success else 1


if __name__ == "__main__":
    sys.exit(main())

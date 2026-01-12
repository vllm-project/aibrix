#!/usr/bin/env python3
"""
Test script for vLLM-specific endpoints using HTTP client.
Tests endpoints that are not part of the OpenAI SDK.

Usage:
    # First start the mocked app:
    STANDALONE_MODE=true python app.py

    # Then run this test:
    python test_vllm_endpoints.py

    # Or run with custom base URL:
    python test_vllm_endpoints.py --base-url http://localhost:8000
"""
import argparse
import sys
import json
import urllib.request
import urllib.error
from typing import Optional


class TestResult:
    def __init__(self):
        self.passed = 0
        self.failed = 0
        self.errors = []

    def add_pass(self, name: str, details: str = ""):
        self.passed += 1
        print(f"  ✓ PASS: {name}")
        if details:
            print(f"         {details}")

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


def make_request(
    base_url: str,
    path: str,
    method: str = "GET",
    data: Optional[dict] = None,
    api_key: str = "test-key",
) -> tuple:
    """
    Make an HTTP request and return (status_code, response_data).
    """
    url = f"{base_url}{path}"
    headers = {
        "Content-Type": "application/json",
        "Authorization": f"Bearer {api_key}",
    }

    req_data = json.dumps(data).encode("utf-8") if data else None
    request = urllib.request.Request(url, data=req_data, headers=headers, method=method)

    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            body = response.read().decode("utf-8")
            try:
                return response.status, json.loads(body)
            except json.JSONDecodeError:
                return response.status, body
    except urllib.error.HTTPError as e:
        body = e.read().decode("utf-8")
        try:
            return e.code, json.loads(body)
        except json.JSONDecodeError:
            return e.code, body
    except urllib.error.URLError as e:
        return None, str(e)


def test_health_endpoints(base_url: str, result: TestResult):
    """Test health and utility endpoints."""
    print("\n--- Testing Health/Utility Endpoints ---")

    # Health check
    status, data = make_request(base_url, "/health")
    if status == 200:
        result.add_pass("Health check", f"Response: {data}")
    else:
        result.add_fail("Health check", f"Status {status}: {data}")

    # Ready check
    status, data = make_request(base_url, "/ready")
    if status == 200:
        result.add_pass("Ready check", f"Response: {data}")
    else:
        result.add_fail("Ready check", f"Status {status}: {data}")

    # Ping
    status, data = make_request(base_url, "/ping")
    if status == 200:
        result.add_pass("Ping check")
    else:
        result.add_fail("Ping check", f"Status {status}: {data}")

    # Version
    status, data = make_request(base_url, "/version")
    if status == 200 and "version" in data:
        result.add_pass("Version info", f"Version: {data.get('version')}")
    else:
        result.add_fail("Version info", f"Status {status}: {data}")


def test_lora_adapters(base_url: str, result: TestResult):
    """Test LoRA adapter management endpoints."""
    print("\n--- Testing LoRA Adapter Endpoints (Success Cases) ---")

    lora_name = "test-lora-adapter"
    lora_path = "/path/to/adapter"

    # Load LoRA adapter
    status, data = make_request(
        base_url,
        "/v1/load_lora_adapter",
        method="POST",
        data={"lora_name": lora_name, "lora_path": lora_path},
    )
    if status == 200:
        result.add_pass("Load LoRA adapter", f"Response: {data}")
    else:
        result.add_fail("Load LoRA adapter", f"Status {status}: {data}")

    # Verify LoRA adapter in models list
    status, data = make_request(base_url, "/v1/models")
    if status == 200:
        model_ids = [m["id"] for m in data.get("data", [])]
        if lora_name in model_ids:
            result.add_pass("LoRA adapter in models list", f"Models: {model_ids}")
        else:
            result.add_fail("LoRA adapter in models list", f"Adapter not found in {model_ids}")
    else:
        result.add_fail("LoRA adapter in models list", f"Status {status}: {data}")

    # Unload LoRA adapter
    status, data = make_request(
        base_url,
        "/v1/unload_lora_adapter",
        method="POST",
        data={"lora_name": lora_name},
    )
    if status == 200:
        result.add_pass("Unload LoRA adapter", f"Response: {data}")
    else:
        result.add_fail("Unload LoRA adapter", f"Status {status}: {data}")

    # Verify LoRA adapter removed from models list
    status, data = make_request(base_url, "/v1/models")
    if status == 200:
        model_ids = [m["id"] for m in data.get("data", [])]
        if lora_name not in model_ids:
            result.add_pass("LoRA adapter removed from models", f"Models: {model_ids}")
        else:
            result.add_fail("LoRA adapter removed from models", f"Adapter still in {model_ids}")
    else:
        result.add_fail("LoRA adapter removed from models", f"Status {status}: {data}")


def test_lora_adapters_errors(base_url: str, result: TestResult):
    """Test LoRA adapter error handling."""
    print("\n--- Testing LoRA Adapter Endpoints (Error Cases) ---")

    # Error: Missing lora_name
    status, data = make_request(
        base_url,
        "/v1/load_lora_adapter",
        method="POST",
        data={"lora_path": "/path/to/adapter"},
    )
    if status == 400:
        result.add_pass("Load LoRA (error: missing lora_name)")
    else:
        result.add_fail("Load LoRA (error: missing lora_name)", f"Expected 400, got {status}")

    # Error: Missing lora_path
    status, data = make_request(
        base_url,
        "/v1/load_lora_adapter",
        method="POST",
        data={"lora_name": "test-adapter"},
    )
    if status == 400:
        result.add_pass("Load LoRA (error: missing lora_path)")
    else:
        result.add_fail("Load LoRA (error: missing lora_path)", f"Expected 400, got {status}")

    # Error: Missing both fields
    status, data = make_request(
        base_url,
        "/v1/load_lora_adapter",
        method="POST",
        data={},
    )
    if status == 400:
        result.add_pass("Load LoRA (error: missing both fields)")
    else:
        result.add_fail("Load LoRA (error: missing both fields)", f"Expected 400, got {status}")

    # Load an adapter first for duplicate test
    make_request(
        base_url,
        "/v1/load_lora_adapter",
        method="POST",
        data={"lora_name": "duplicate-test", "lora_path": "/path/to/adapter"},
    )

    # Error: Adapter already loaded
    status, data = make_request(
        base_url,
        "/v1/load_lora_adapter",
        method="POST",
        data={"lora_name": "duplicate-test", "lora_path": "/path/to/adapter"},
    )
    if status == 400:
        result.add_pass("Load LoRA (error: already loaded)")
    else:
        result.add_fail("Load LoRA (error: already loaded)", f"Expected 400, got {status}")

    # Clean up
    make_request(
        base_url,
        "/v1/unload_lora_adapter",
        method="POST",
        data={"lora_name": "duplicate-test"},
    )

    # Error: Adapter path not found (404)
    status, data = make_request(
        base_url,
        "/v1/load_lora_adapter",
        method="POST",
        data={"lora_name": "notfound-adapter", "lora_path": "/nonexistent/path"},
    )
    if status == 404:
        result.add_pass("Load LoRA (error: path not found)")
    else:
        result.add_fail("Load LoRA (error: path not found)", f"Expected 404, got {status}")

    # Error: Unload missing lora_name
    status, data = make_request(
        base_url,
        "/v1/unload_lora_adapter",
        method="POST",
        data={},
    )
    if status == 400:
        result.add_pass("Unload LoRA (error: missing lora_name)")
    else:
        result.add_fail("Unload LoRA (error: missing lora_name)", f"Expected 400, got {status}")

    # Error: Unload non-existent adapter
    status, data = make_request(
        base_url,
        "/v1/unload_lora_adapter",
        method="POST",
        data={"lora_name": "non-existent-adapter"},
    )
    if status == 400:
        result.add_pass("Unload LoRA (error: adapter not found)")
    else:
        result.add_fail("Unload LoRA (error: adapter not found)", f"Expected 400, got {status}")


def test_tokenization(base_url: str, result: TestResult):
    """Test tokenization endpoints."""
    print("\n--- Testing Tokenization Endpoints ---")

    # Tokenize
    status, data = make_request(
        base_url,
        "/tokenize",
        method="POST",
        data={"prompt": "Hello world, this is a test."},
    )
    if status == 200 and "tokens" in data:
        result.add_pass("Tokenize", f"Token count: {data.get('count')}, Tokens: {data.get('tokens')[:5]}...")
    else:
        result.add_fail("Tokenize", f"Status {status}: {data}")

    # Tokenize - missing prompt
    status, data = make_request(
        base_url,
        "/tokenize",
        method="POST",
        data={},
    )
    if status == 400:
        result.add_pass("Tokenize (error: missing prompt)")
    else:
        result.add_fail("Tokenize (error: missing prompt)", f"Expected 400, got {status}")

    # Detokenize
    status, data = make_request(
        base_url,
        "/detokenize",
        method="POST",
        data={"tokens": [100, 101, 102, 103, 104]},
    )
    if status == 200 and "prompt" in data:
        result.add_pass("Detokenize", f"Result: {data.get('prompt')}")
    else:
        result.add_fail("Detokenize", f"Status {status}: {data}")

    # Detokenize - missing tokens
    status, data = make_request(
        base_url,
        "/detokenize",
        method="POST",
        data={},
    )
    if status == 400:
        result.add_pass("Detokenize (error: missing tokens)")
    else:
        result.add_fail("Detokenize (error: missing tokens)", f"Expected 400, got {status}")


def test_server_load(base_url: str, result: TestResult):
    """Test server load endpoint."""
    print("\n--- Testing Server Load Endpoint ---")

    status, data = make_request(base_url, "/load")
    if status == 200 and "server_load" in data:
        result.add_pass("Server load", f"Load: {data.get('server_load')}")
    else:
        result.add_fail("Server load", f"Status {status}: {data}")


def test_metrics(base_url: str, result: TestResult):
    """Test metrics endpoint."""
    print("\n--- Testing Metrics Endpoint ---")

    status, data = make_request(base_url, "/metrics")
    if status == 200 and isinstance(data, str) and "vllm:" in data:
        lines = data.strip().split("\n")
        result.add_pass("Metrics", f"Got {len(lines)} lines of Prometheus metrics")
    else:
        result.add_fail("Metrics", f"Status {status}, unexpected response format")


def test_connection(base_url: str) -> bool:
    """Test if the server is reachable."""
    try:
        request = urllib.request.Request(f"{base_url}/health", method="GET")
        with urllib.request.urlopen(request, timeout=5) as response:
            return response.status == 200
    except (urllib.error.URLError, urllib.error.HTTPError):
        return False


def main():
    parser = argparse.ArgumentParser(description="Test vLLM-specific endpoints")
    parser.add_argument(
        "--base-url",
        default="http://localhost:8000",
        help="Base URL of the mocked server (default: http://localhost:8000)",
    )
    parser.add_argument(
        "--api-key",
        default="test-key",
        help="API key for authentication (default: test-key)",
    )
    parser.add_argument(
        "--skip-connection-check",
        action="store_true",
        help="Skip checking if server is running",
    )
    args = parser.parse_args()

    print(f"\n{'='*60}")
    print("VLLM-SPECIFIC ENDPOINT TESTS")
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
        print("  ✓ Server is reachable")

    result = TestResult()

    # Run tests
    test_health_endpoints(args.base_url, result)
    test_lora_adapters(args.base_url, result)
    test_lora_adapters_errors(args.base_url, result)
    test_tokenization(args.base_url, result)
    test_server_load(args.base_url, result)
    test_metrics(args.base_url, result)

    # Print summary
    success = result.summary()
    return 0 if success else 1


if __name__ == "__main__":
    sys.exit(main())

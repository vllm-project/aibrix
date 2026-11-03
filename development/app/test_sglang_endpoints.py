#!/usr/bin/env python3
"""
Test script for SGLang-specific endpoints using HTTP client.
Tests endpoints that only an SGLang engine serves, so they are kept apart from
the vLLM endpoint checks.

Usage:
    # First start the mocked app:
    STANDALONE_MODE=true python app.py

    # Then run this test:
    python test_sglang_endpoints.py

    # Or run with custom base URL:
    python test_sglang_endpoints.py --base-url http://localhost:8000
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
            print("\nFailed tests:")
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


def test_decisions(base_url: str, result: TestResult, api_key: str):
    """Test the SGLang decisions endpoint."""
    print("\n--- Testing Decisions Endpoint ---")

    questions = [
        {
            "id": "pick",
            "type": "choice",
            "question": "Which replica?",
            "options": [{"name": "a"}, {"name": "b"}],
        }
    ]

    # Decisions - string input
    status, data = make_request(
        base_url,
        "/v1/decisions",
        method="POST",
        data={"model": "test-model", "input": "Route this request.", "questions": questions},
        api_key=api_key,
    )
    if status == 200 and data.get("object") == "decisions" and "pick" in data.get("answers", {}):
        usage = data.get("usage", {})
        if usage.get("total_tokens") == usage.get("prompt_tokens"):
            result.add_pass("Decisions (string input)", f"Prompt tokens: {usage.get('prompt_tokens')}")
        else:
            result.add_fail("Decisions (string input)", f"total_tokens != prompt_tokens: {usage}")
    else:
        result.add_fail("Decisions (string input)", f"Status {status}: {data}")

    # Decisions - object input
    status, data = make_request(
        base_url,
        "/v1/decisions",
        method="POST",
        data={"model": "test-model", "input": {"load": [1, 2]}, "questions": questions},
        api_key=api_key,
    )
    if status == 200 and "answers" in data:
        result.add_pass("Decisions (object input)")
    else:
        result.add_fail("Decisions (object input)", f"Status {status}: {data}")

    # Decisions - missing questions
    status, data = make_request(
        base_url,
        "/v1/decisions",
        method="POST",
        data={"model": "test-model", "input": "Route this request."},
        api_key=api_key,
    )
    if status == 400:
        result.add_pass("Decisions (error: missing questions)")
    else:
        result.add_fail("Decisions (error: missing questions)", f"Expected 400, got {status}")

    # Decisions - missing input
    status, data = make_request(
        base_url,
        "/v1/decisions",
        method="POST",
        data={"model": "test-model", "questions": questions},
        api_key=api_key,
    )
    if status == 400:
        result.add_pass("Decisions (error: missing input)")
    else:
        result.add_fail("Decisions (error: missing input)", f"Expected 400, got {status}")


def test_systemone(base_url: str, result: TestResult, api_key: str):
    """Test the SGLang System One endpoint."""
    print("\n--- Testing System One Endpoint ---")

    questions = {
        "team": {
            "type": "choice",
            "instructions": "Which team?",
            "criteria": {"billing": None, "support": "Bugs or integration problems"},
        },
        "urgent": {"type": "noul", "instructions": "The customer needs an answer today."},
    }

    # System One - string state
    status, data = make_request(
        base_url,
        "/v1/systemone",
        method="POST",
        data={"model": "test-model", "state": "Stripe keeps failing.", "questions": questions},
        api_key=api_key,
    )
    if status == 200 and set(data.get("answers", {})) == {"team", "urgent"}:
        usage = data.get("usage", {})
        # The engine reports input and output tokens and no total; the gateway derives it.
        if usage.get("input_tokens", 0) > 0 and "total_tokens" not in usage:
            result.add_pass("System One (string state)", f"Input tokens: {usage.get('input_tokens')}")
        else:
            result.add_fail("System One (string state)", f"Unexpected usage: {usage}")
    else:
        result.add_fail("System One (string state)", f"Status {status}: {data}")

    # System One - object state
    status, data = make_request(
        base_url,
        "/v1/systemone",
        method="POST",
        data={"model": "test-model", "state": {"load": [1, 2]}, "questions": questions},
        api_key=api_key,
    )
    if status == 200 and "answers" in data:
        result.add_pass("System One (object state)")
    else:
        result.add_fail("System One (object state)", f"Status {status}: {data}")

    # System One - empty questions (the mock mirrors SGLang's 422 for a schema error)
    status, data = make_request(
        base_url,
        "/v1/systemone",
        method="POST",
        data={"model": "test-model", "state": "Stripe keeps failing.", "questions": {}},
        api_key=api_key,
    )
    if status == 422:
        result.add_pass("System One (error: empty questions)")
    else:
        result.add_fail("System One (error: empty questions)", f"Expected 422, got {status}")

    # System One - missing state
    status, data = make_request(
        base_url,
        "/v1/systemone",
        method="POST",
        data={"model": "test-model", "questions": questions},
        api_key=api_key,
    )
    if status == 422:
        result.add_pass("System One (error: missing state)")
    else:
        result.add_fail("System One (error: missing state)", f"Expected 422, got {status}")


def test_connection(base_url: str) -> bool:
    """Test if the server is reachable."""
    try:
        request = urllib.request.Request(f"{base_url}/health", method="GET")
        with urllib.request.urlopen(request, timeout=5) as response:
            return response.status == 200
    except (urllib.error.URLError, urllib.error.HTTPError):
        return False


def main():
    parser = argparse.ArgumentParser(description="Test SGLang-specific endpoints")
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
    print("SGLANG-SPECIFIC ENDPOINT TESTS")
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
    test_decisions(args.base_url, result, args.api_key)
    test_systemone(args.base_url, result, args.api_key)

    # Print summary
    success = result.summary()
    return 0 if success else 1


if __name__ == "__main__":
    sys.exit(main())

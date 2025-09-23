#!/usr/bin/env python3
"""
Demo script showing how to run S3 tests.

This script demonstrates how to set up environment variables and run S3 tests.
"""

import os
import subprocess
import sys


def demo_s3_test_setup():
    """Show example S3 test configuration."""
    print("=== AIBrix Storage S3 Test Configuration Demo ===\n")

    print("1. Check current AWS configuration:")
    aws_access_key = os.getenv("AWS_ACCESS_KEY_ID", "Not set")
    aws_secret_key = os.getenv("AWS_SECRET_ACCESS_KEY", "Not set")
    aws_region = os.getenv("AWS_DEFAULT_REGION", "Not set")
    test_bucket = os.getenv("AIBRIX_TEST_S3_BUCKET", "Not set")

    print(
        f"   AWS_ACCESS_KEY_ID: {aws_access_key[:10]}..."
        if aws_access_key != "Not set"
        else f"   AWS_ACCESS_KEY_ID: {aws_access_key}"
    )
    print(
        f"   AWS_SECRET_ACCESS_KEY: {'[HIDDEN]' if aws_secret_key != 'Not set' else 'Not set'}"
    )
    print(f"   AWS_DEFAULT_REGION: {aws_region}")
    print(f"   AIBRIX_TEST_S3_BUCKET: {test_bucket}\n")

    print("2. To configure S3 tests, run these commands:")
    print("   export AWS_ACCESS_KEY_ID='your-access-key-id'")
    print("   export AWS_SECRET_ACCESS_KEY='your-secret-access-key'")
    print("   export AWS_DEFAULT_REGION='us-east-1'")
    print("   export AIBRIX_TEST_S3_BUCKET='your-test-bucket-name'\n")

    print("3. Alternative: Configure using AWS CLI:")
    print("   aws configure")
    print("   export AIBRIX_TEST_S3_BUCKET='your-test-bucket-name'\n")

    # Check if we can run S3 tests
    s3_ready = all(
        [
            aws_access_key != "Not set",
            aws_secret_key != "Not set",
            test_bucket != "Not set",
        ]
    ) or (
        os.path.exists(os.path.expanduser("~/.aws/credentials"))
        and test_bucket != "Not set"
    )

    if s3_ready:
        print("✅ S3 tests are configured and ready to run!")
        print("   Run: pytest tests/storage/ -v")
    else:
        print("❌ S3 tests are not configured (will be skipped)")
        print("   Only local storage tests will run")

    print("\n4. Example test run:")
    return s3_ready


def run_demo_test():
    """Run a subset of storage tests to demonstrate functionality."""
    print("\n=== Running Storage Test Demo ===\n")

    try:
        # Run just the base storage tests
        result = subprocess.run(
            [
                sys.executable,
                "-m",
                "pytest",
                "tests/storage/test_base_storage.py::TestBaseStorageFunctionality::test_put_and_get_string",
                "-v",
            ],
            capture_output=True,
            text=True,
            cwd=".",
        )

        print(result.stdout)
        if result.stderr:
            print("STDERR:", result.stderr)

        return result.returncode == 0
    except Exception as e:
        print(f"Error running demo test: {e}")
        return False


if __name__ == "__main__":
    s3_configured = demo_s3_test_setup()

    print("\n" + "=" * 60)

    if len(sys.argv) > 1 and sys.argv[1] == "--run-demo":
        success = run_demo_test()
        if success:
            print("\n✅ Demo test completed successfully!")
        else:
            print("\n❌ Demo test failed")
            sys.exit(1)
    else:
        print("To run a demo test, use: python test_s3_demo.py --run-demo")
        print("To run all tests, use: pytest tests/storage/ -v")

# Copyright 2024 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# 	http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import os
import tempfile
from pathlib import Path
from typing import Iterator

import pytest

from aibrix.storage import LocalStorage, S3Storage, TOSStorage


@pytest.fixture
def temp_dir() -> Iterator[Path]:
    """Create a temporary directory for local storage tests."""
    with tempfile.TemporaryDirectory() as tmp_dir:
        yield Path(tmp_dir)


@pytest.fixture
def local_storage(temp_dir: Path) -> LocalStorage:
    """Create a LocalStorage instance for testing."""
    # Add test-specific configuration with reasonable multipart threshold
    return LocalStorage(str(temp_dir))


@pytest.fixture
def s3_config() -> dict:
    """S3 configuration from environment or AWS config."""
    # Check for AWS credentials
    aws_access_key_id = os.getenv("AWS_ACCESS_KEY_ID")
    aws_secret_access_key = os.getenv("AWS_SECRET_ACCESS_KEY")
    aws_region = os.getenv("AWS_DEFAULT_REGION", "us-east-1")

    # Check for test bucket
    test_bucket = os.getenv("AIBRIX_TEST_S3_BUCKET")

    # Try to get credentials from AWS config files
    aws_config_dir = Path.home() / ".aws"
    has_aws_config = (
        aws_config_dir.exists() and (aws_config_dir / "credentials").exists()
    )

    return {
        "has_credentials": bool(aws_access_key_id and aws_secret_access_key)
        or has_aws_config,
        "bucket_name": test_bucket,
        "region_name": aws_region,
        "aws_access_key_id": aws_access_key_id,
        "aws_secret_access_key": aws_secret_access_key,
    }


@pytest.fixture
def s3_storage(s3_config: dict) -> S3Storage:
    """Create an S3Storage instance for testing (if configured)."""
    if not s3_config["has_credentials"]:
        pytest.skip("S3 credentials not available")

    if not s3_config["bucket_name"]:
        pytest.skip("AIBRIX_TEST_S3_BUCKET environment variable not set")

    # Filter out None values
    kwargs = {
        k: v for k, v in s3_config.items() if v is not None and k != "has_credentials"
    }

    return S3Storage(**kwargs)


@pytest.fixture
def tos_config() -> dict:
    """TOS configuration from environment variables."""
    # TOS credentials and configuration
    tos_access_key = os.getenv("TOS_ACCESS_KEY")
    tos_secret_key = os.getenv("TOS_SECRET_KEY")
    tos_endpoint = os.getenv("TOS_ENDPOINT")
    tos_region = os.getenv("TOS_REGION")
    tos_bucket = os.getenv("AIBRIX_TEST_TOS_BUCKET")

    return {
        "has_credentials": bool(
            tos_access_key and tos_secret_key and tos_endpoint and tos_region
        ),
        "bucket_name": tos_bucket,
        "access_key": tos_access_key,
        "secret_key": tos_secret_key,
        "endpoint": tos_endpoint,
        "region": tos_region,
    }


@pytest.fixture
def tos_storage(tos_config: dict) -> TOSStorage:
    """Create a TOSStorage instance for testing (if configured)."""
    if not tos_config["has_credentials"]:
        pytest.skip("TOS credentials not available")

    if not tos_config["bucket_name"]:
        pytest.skip("AIBRIX_TEST_TOS_BUCKET environment variable not set")

    # Filter out None values and has_credentials flag
    kwargs = {
        k: v for k, v in tos_config.items() if v is not None and k != "has_credentials"
    }

    return TOSStorage(**kwargs)

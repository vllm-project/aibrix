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

"""
Tests for the secret_gen module.
"""

import base64
import os
from unittest.mock import Mock, patch

import pytest

from aibrix.metadata.secret_gen import SecretGenerator


class TestSecretGenerator:
    """Test cases for SecretGenerator class."""

    def test_init(self):
        """Test SecretGenerator initialization."""
        generator = SecretGenerator(namespace="test-namespace")
        assert generator.namespace == "test-namespace"
        assert generator.core_v1 is not None
        assert generator.setting_dir.name == "setting"

    def test_encode_data(self):
        """Test base64 encoding of secret data."""
        generator = SecretGenerator()

        data = {
            "key1": "value1",
            "key2": "value2",
            "key3": None,  # Should be filtered out
        }

        encoded = generator._encode_data(data)

        assert "key1" in encoded
        assert "key2" in encoded
        assert "key3" not in encoded

        # Verify base64 encoding
        assert encoded["key1"] == base64.b64encode("value1".encode()).decode()
        assert encoded["key2"] == base64.b64encode("value2".encode()).decode()

    @patch("aibrix.metadata.secret_gen.open")
    @patch("aibrix.metadata.secret_gen.yaml.safe_load")
    def test_load_template(self, mock_yaml_load, mock_open):
        """Test loading secret templates."""
        generator = SecretGenerator()

        mock_template = {"apiVersion": "v1", "kind": "Secret"}
        mock_yaml_load.return_value = mock_template

        result = generator._load_template("test_template.yaml")

        assert result == mock_template
        mock_open.assert_called_once()
        mock_yaml_load.assert_called_once()

    @patch("aibrix.metadata.secret_gen.boto3.Session")
    def test_get_s3_credentials_success(self, mock_session):
        """Test successful S3 credentials retrieval."""
        generator = SecretGenerator()

        # Mock boto3 session and credentials
        mock_credentials = Mock()
        mock_credentials.access_key = "test_access_key"
        mock_credentials.secret_key = "test_secret_key"

        mock_session_instance = Mock()
        mock_session_instance.get_credentials.return_value = mock_credentials
        mock_session_instance.region_name = "us-west-2"
        mock_session.return_value = mock_session_instance

        credentials = generator._get_s3_credentials()

        assert credentials["access_key"] == "test_access_key"
        assert credentials["secret_key"] == "test_secret_key"
        assert credentials["region"] == "us-west-2"

    @patch("aibrix.metadata.secret_gen.boto3.Session")
    def test_get_s3_credentials_no_credentials(self, mock_session):
        """Test S3 credentials retrieval when no credentials found."""
        generator = SecretGenerator()

        mock_session_instance = Mock()
        mock_session_instance.get_credentials.return_value = None
        mock_session.return_value = mock_session_instance

        with pytest.raises(RuntimeError, match="No AWS credentials found"):
            generator._get_s3_credentials()

    @patch.dict(
        os.environ,
        {
            "TOS_ACCESS_KEY": "tos_access",
            "TOS_SECRET_KEY": "tos_secret",
            "TOS_ENDPOINT": "https://tos.example.com",
            "TOS_REGION": "us-east-1",
        },
    )
    def test_get_tos_credentials_success(self):
        """Test successful TOS credentials retrieval."""
        generator = SecretGenerator()

        credentials = generator._get_tos_credentials()

        assert credentials["access_key"] == "tos_access"
        assert credentials["secret_key"] == "tos_secret"
        assert credentials["endpoint"] == "https://tos.example.com"
        assert credentials["region"] == "us-east-1"

    @patch.dict(
        os.environ,
        {
            "TOS_ACCESS_KEY": "tos_access",
            # Missing other required variables
        },
        clear=True,
    )
    def test_get_tos_credentials_missing_vars(self):
        """Test TOS credentials retrieval with missing environment variables."""
        generator = SecretGenerator()

        with pytest.raises(RuntimeError, match="Missing TOS environment variables"):
            generator._get_tos_credentials()

    @patch("aibrix.metadata.secret_gen.client.CoreV1Api")
    def test_secret_exists_true(self, mock_core_v1_class):
        """Test secret_exists when secret exists."""
        mock_core_v1 = Mock()
        mock_core_v1_class.return_value = mock_core_v1

        generator = SecretGenerator()
        generator.core_v1 = mock_core_v1

        # Mock successful read
        mock_core_v1.read_namespaced_secret.return_value = Mock()

        result = generator.secret_exists("test-secret")

        assert result is True
        mock_core_v1.read_namespaced_secret.assert_called_once_with(
            name="test-secret", namespace="default"
        )

    @patch("aibrix.metadata.secret_gen.client.CoreV1Api")
    def test_secret_exists_false(self, mock_core_v1_class):
        """Test secret_exists when secret doesn't exist."""
        from kubernetes import client

        mock_core_v1 = Mock()
        mock_core_v1_class.return_value = mock_core_v1

        generator = SecretGenerator()
        generator.core_v1 = mock_core_v1

        # Mock 404 exception
        mock_exception = client.ApiException(status=404)
        mock_core_v1.read_namespaced_secret.side_effect = mock_exception

        result = generator.secret_exists("test-secret")

        assert result is False

    @patch("aibrix.metadata.secret_gen.client.CoreV1Api")
    def test_delete_secret_success(self, mock_core_v1_class):
        """Test successful secret deletion."""
        mock_core_v1 = Mock()
        mock_core_v1_class.return_value = mock_core_v1

        generator = SecretGenerator()
        generator.core_v1 = mock_core_v1

        result = generator.delete_secret("test-secret")

        assert result is True
        mock_core_v1.delete_namespaced_secret.assert_called_once_with(
            name="test-secret", namespace="default"
        )

    @patch("aibrix.metadata.secret_gen.client.CoreV1Api")
    def test_delete_secret_not_found(self, mock_core_v1_class):
        """Test secret deletion when secret doesn't exist."""
        from kubernetes import client

        mock_core_v1 = Mock()
        mock_core_v1_class.return_value = mock_core_v1

        generator = SecretGenerator()
        generator.core_v1 = mock_core_v1

        # Mock 404 exception
        mock_exception = client.ApiException(status=404)
        mock_core_v1.delete_namespaced_secret.side_effect = mock_exception

        result = generator.delete_secret("test-secret")

        assert result is False

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
Kubernetes secret generator for S3 and TOS credentials.

This module provides utilities to generate and apply Kubernetes secrets
for S3 and TOS storage backends using their respective credential sources.
"""

import base64
import os
from pathlib import Path
from typing import Dict, Optional

import boto3
import yaml
from kubernetes import client

from aibrix.logger import init_logger

logger = init_logger(__name__)


class SecretGenerator:
    """Generator for Kubernetes secrets with storage credentials."""

    def __init__(self, namespace: str = "default"):
        """
        Initialize the secret generator.

        Args:
            namespace: Kubernetes namespace to create secrets in
        """
        self.namespace = namespace
        self.core_v1 = client.CoreV1Api()
        self.setting_dir = Path(__file__).parent / "setting"

    def _encode_data(self, data: Dict[str, str]) -> Dict[str, str]:
        """
        Base64 encode secret data for Kubernetes.

        Args:
            data: Dictionary of key-value pairs to encode

        Returns:
            Dictionary with base64 encoded values
        """
        return {
            key: base64.b64encode(value.encode()).decode()
            for key, value in data.items()
            if value is not None
        }

    def _load_template(self, template_name: str) -> Dict:
        """
        Load a secret template from the setting directory.

        Args:
            template_name: Name of the template file

        Returns:
            Parsed YAML template
        """
        template_path = self.setting_dir / template_name
        with open(template_path, "r") as f:
            return yaml.safe_load(f)

    def _get_s3_credentials(self) -> Dict[str, str]:
        """
        Get S3 credentials using boto3.

        Returns:
            Dictionary with S3 credentials

        Raises:
            RuntimeError: If credentials cannot be obtained
        """
        try:
            session = boto3.Session()
            credentials = session.get_credentials()

            if not credentials:
                raise RuntimeError("No AWS credentials found")

            access_key = credentials.access_key
            secret_key = credentials.secret_key
            region = session.region_name or "us-east-1"

            if not access_key or not secret_key:
                raise RuntimeError("AWS credentials incomplete")

            return {
                "access_key": access_key,
                "secret_key": secret_key,
                "region": region,
            }

        except Exception as e:
            raise RuntimeError(f"Failed to get S3 credentials: {e}")

    def _get_tos_credentials(self) -> Dict[str, str]:
        """
        Get TOS credentials from environment variables.

        Returns:
            Dictionary with TOS credentials

        Raises:
            RuntimeError: If required environment variables are not set
        """
        tos_access_key = os.getenv("TOS_ACCESS_KEY")
        tos_secret_key = os.getenv("TOS_SECRET_KEY")
        tos_endpoint = os.getenv("TOS_ENDPOINT")
        tos_region = os.getenv("TOS_REGION")

        if not all([tos_access_key, tos_secret_key, tos_endpoint, tos_region]):
            missing = [
                var
                for var, val in [
                    ("TOS_ACCESS_KEY", tos_access_key),
                    ("TOS_SECRET_KEY", tos_secret_key),
                    ("TOS_ENDPOINT", tos_endpoint),
                    ("TOS_REGION", tos_region),
                ]
                if not val
            ]
            raise RuntimeError(
                f"Missing TOS environment variables: {', '.join(missing)}"
            )

        # Type assertions after None check
        assert tos_access_key is not None
        assert tos_secret_key is not None
        assert tos_endpoint is not None
        assert tos_region is not None

        return {
            "access_key": tos_access_key,
            "secret_key": tos_secret_key,
            "endpoint": tos_endpoint,
            "region": tos_region,
        }

    def create_s3_secret(
        self, bucket_name: Optional[str] = None, secret_name: Optional[str] = None
    ) -> str:
        """
        Create a Kubernetes secret with S3 credentials.

        Args:
            bucket_name: S3 bucket name (optional)
            secret_name: Custom secret name (optional, uses template default)

        Returns:
            Name of the created secret

        Raises:
            RuntimeError: If secret creation fails
        """
        try:
            # Load template
            template = self._load_template("s3_secret_template.yaml")

            # Get S3 credentials
            credentials = self._get_s3_credentials()

            # Prepare secret data
            secret_data = {
                "access-key-id": credentials["access_key"],
                "secret-access-key": credentials["secret_key"],
                "region": credentials["region"],
            }

            if bucket_name:
                secret_data["bucket-name"] = bucket_name

            # Update template
            if secret_name:
                template["metadata"]["name"] = secret_name
            template["metadata"]["namespace"] = self.namespace
            template["data"] = self._encode_data(secret_data)

            # Create Kubernetes secret object
            secret = client.V1Secret(
                metadata=client.V1ObjectMeta(
                    name=template["metadata"]["name"], namespace=self.namespace
                ),
                data=template["data"],
                type=template["type"],
            )

            # Apply to cluster
            secret_name = template["metadata"]["name"]
            assert secret_name is not None, "Secret name must be set in template"

            # Delete existing secret if it exists
            try:
                self.core_v1.delete_namespaced_secret(
                    name=secret_name, namespace=self.namespace
                )
                logger.info(f"Deleted existing S3 secret: {secret_name}")
            except client.ApiException as e:
                if e.status != 404:
                    raise

            # Create the secret
            self.core_v1.create_namespaced_secret(namespace=self.namespace, body=secret)
            logger.info(
                f"Created S3 secret: {secret_name} in namespace: {self.namespace}"
            )

            return secret_name

        except Exception as e:
            raise RuntimeError(f"Failed to create S3 secret: {e}")

    def create_tos_secret(
        self, bucket_name: Optional[str] = None, secret_name: Optional[str] = None
    ) -> str:
        """
        Create a Kubernetes secret with TOS credentials.

        Args:
            bucket_name: TOS bucket name (optional)
            secret_name: Custom secret name (optional, uses template default)

        Returns:
            Name of the created secret

        Raises:
            RuntimeError: If secret creation fails
        """
        try:
            # Load template
            template = self._load_template("tos_secret_template.yaml")

            # Get TOS credentials
            credentials = self._get_tos_credentials()

            # Prepare secret data
            secret_data = {
                "access-key": credentials["access_key"],
                "secret-key": credentials["secret_key"],
                "endpoint": credentials["endpoint"],
                "region": credentials["region"],
            }

            if bucket_name:
                secret_data["bucket-name"] = bucket_name

            # Update template
            if secret_name:
                template["metadata"]["name"] = secret_name
            template["metadata"]["namespace"] = self.namespace
            template["data"] = self._encode_data(secret_data)

            # Create Kubernetes secret object
            secret = client.V1Secret(
                metadata=client.V1ObjectMeta(
                    name=template["metadata"]["name"], namespace=self.namespace
                ),
                data=template["data"],
                type=template["type"],
            )

            # Apply to cluster
            secret_name = template["metadata"]["name"]
            assert secret_name is not None, "Secret name must be set in template"

            # Delete existing secret if it exists
            try:
                self.core_v1.delete_namespaced_secret(
                    name=secret_name, namespace=self.namespace
                )
                logger.info(f"Deleted existing TOS secret: {secret_name}")
            except client.ApiException as e:
                if e.status != 404:
                    raise

            # Create the secret
            self.core_v1.create_namespaced_secret(namespace=self.namespace, body=secret)
            logger.info(
                f"Created TOS secret: {secret_name} in namespace: {self.namespace}"
            )

            return secret_name

        except Exception as e:
            raise RuntimeError(f"Failed to create TOS secret: {e}")

    def delete_secret(self, secret_name: str) -> bool:
        """
        Delete a Kubernetes secret.

        Args:
            secret_name: Name of the secret to delete

        Returns:
            True if deleted successfully, False if not found

        Raises:
            RuntimeError: If deletion fails for reasons other than not found
        """
        try:
            self.core_v1.delete_namespaced_secret(
                name=secret_name, namespace=self.namespace
            )
            logger.info(
                f"Deleted secret: {secret_name} from namespace: {self.namespace}"
            )
            return True

        except client.ApiException as e:
            if e.status == 404:
                logger.warning(
                    f"Secret {secret_name} not found in namespace {self.namespace}"
                )
                return False
            else:
                raise RuntimeError(f"Failed to delete secret {secret_name}: {e}")

    def secret_exists(self, secret_name: str) -> bool:
        """
        Check if a secret exists in the namespace.

        Args:
            secret_name: Name of the secret to check

        Returns:
            True if secret exists, False otherwise
        """
        try:
            self.core_v1.read_namespaced_secret(
                name=secret_name, namespace=self.namespace
            )
            return True
        except client.ApiException as e:
            if e.status == 404:
                return False
            else:
                raise RuntimeError(f"Failed to check secret {secret_name}: {e}")


def create_s3_secret(
    namespace: str = "default",
    bucket_name: Optional[str] = None,
    secret_name: Optional[str] = None,
) -> str:
    """
    Convenience function to create an S3 secret.

    Args:
        namespace: Kubernetes namespace
        bucket_name: S3 bucket name (optional)
        secret_name: Custom secret name (optional)

    Returns:
        Name of the created secret
    """
    generator = SecretGenerator(namespace)
    return generator.create_s3_secret(bucket_name, secret_name)


def create_tos_secret(
    namespace: str = "default",
    bucket_name: Optional[str] = None,
    secret_name: Optional[str] = None,
) -> str:
    """
    Convenience function to create a TOS secret.

    Args:
        namespace: Kubernetes namespace
        bucket_name: TOS bucket name (optional)
        secret_name: Custom secret name (optional)

    Returns:
        Name of the created secret
    """
    generator = SecretGenerator(namespace)
    return generator.create_tos_secret(bucket_name, secret_name)


def delete_secret(secret_name: str, namespace: str = "default") -> bool:
    """
    Convenience function to delete a secret.

    Args:
        secret_name: Name of the secret to delete
        namespace: Kubernetes namespace

    Returns:
        True if deleted successfully, False if not found
    """
    generator = SecretGenerator(namespace)
    return generator.delete_secret(secret_name)

# Copyright 2025 The Aibrix Team.
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
Tests for the metadata users API.

Test Coverage:
- User Pydantic model validation
- User CRUD API: Redis-backed user management with rate limiting
"""

import os
from unittest.mock import AsyncMock

import pytest
from fastapi.testclient import TestClient

# Set required environment variable before importing
os.environ.setdefault("SECRET_KEY", "test-secret-key-for-testing")

# Try importing, skip tests if dependencies missing
try:
    from aibrix.metadata.api.v1.users import User
    from aibrix.metadata.app import build_app

    DEPENDENCIES_AVAILABLE = True
except ModuleNotFoundError as e:
    DEPENDENCIES_AVAILABLE = False
    SKIP_REASON = f"Missing dependency: {e}"

pytestmark = pytest.mark.skipif(
    not DEPENDENCIES_AVAILABLE,
    reason="Dependencies not available. Run: poetry install --with dev",
)


class TestUserModel:
    """Tests for User Pydantic model."""

    def test_user_valid(self):
        """Test creating valid user."""
        user = User(name="test-user", rpm=100, tpm=1000)
        assert user.name == "test-user"
        assert user.rpm == 100
        assert user.tpm == 1000

    def test_user_defaults(self):
        """Test user default values."""
        user = User(name="test-user")
        assert user.name == "test-user"
        assert user.rpm == 0
        assert user.tpm == 0

    def test_user_negative_rpm_validation(self):
        """Test rpm validation rejects negative values."""
        with pytest.raises(ValueError, match="rpm and tpm must be non-negative"):
            User(name="test-user", rpm=-1)

    def test_user_negative_tpm_validation(self):
        """Test tpm validation rejects negative values."""
        with pytest.raises(ValueError, match="rpm and tpm must be non-negative"):
            User(name="test-user", tpm=-1)


class TestUsersAPI:
    """Integration tests for Users CRUD API."""

    @pytest.fixture
    def mock_redis(self):
        """Mock Redis client."""
        mock = AsyncMock()
        return mock

    @pytest.fixture
    def app_with_redis(self, mock_redis):
        """Build app with mocked Redis."""
        from argparse import Namespace

        args = Namespace(
            enable_fastapi_docs=False,
            disable_batch_api=True,
            disable_file_api=True,
            enable_k8s_job=False,
            e2e_test=False,
        )
        app = build_app(args)
        app.state.redis_client = mock_redis
        return app

    def test_create_user(self, app_with_redis, mock_redis):
        """Test creating a new user."""
        client = TestClient(app_with_redis)
        mock_redis.exists.return_value = 0  # User doesn't exist
        mock_redis.set.return_value = True

        user_data = {"name": "test-user", "rpm": 100, "tpm": 1000}

        response = client.post("/CreateUser", json=user_data)
        assert response.status_code == 200

        data = response.json()
        assert "user" in data
        assert data["user"]["name"] == "test-user"
        assert data["user"]["rpm"] == 100
        assert data["user"]["tpm"] == 1000
        assert "Created User" in data["message"]

        # Verify Redis was called correctly
        mock_redis.exists.assert_called_once_with("aibrix-users/test-user")
        mock_redis.set.assert_called_once()

    def test_create_user_already_exists(self, app_with_redis, mock_redis):
        """Test creating user that already exists returns success with existing user."""
        client = TestClient(app_with_redis)
        mock_redis.exists.return_value = 1  # User exists

        user_data = {"name": "test-user", "rpm": 100, "tpm": 1000}

        response = client.post("/CreateUser", json=user_data)
        assert response.status_code == 200
        data = response.json()
        assert "exists" in data["message"]

    def test_read_user(self, app_with_redis, mock_redis):
        """Test reading a user."""
        client = TestClient(app_with_redis)
        user_data = {"name": "test-user", "rpm": 100, "tpm": 1000}
        user = User(**user_data)
        mock_redis.get.return_value = user.model_dump_json().encode()

        response = client.post("/ReadUser", json=user_data)
        assert response.status_code == 200

        data = response.json()
        assert "user" in data
        assert data["user"]["name"] == "test-user"
        assert data["user"]["rpm"] == 100
        assert data["user"]["tpm"] == 1000

    def test_read_user_not_found(self, app_with_redis, mock_redis):
        """Test reading non-existent user returns 404."""
        client = TestClient(app_with_redis)
        mock_redis.get.return_value = None

        response = client.post("/ReadUser", json={"name": "nonexistent"})
        assert response.status_code == 404
        assert "does not exist" in response.json()["detail"]

    def test_update_user(self, app_with_redis, mock_redis):
        """Test updating a user."""
        client = TestClient(app_with_redis)
        mock_redis.exists.return_value = 1  # User exists
        mock_redis.set.return_value = True

        updated_data = {"name": "test-user", "rpm": 200, "tpm": 2000}

        response = client.post("/UpdateUser", json=updated_data)
        assert response.status_code == 200

        data = response.json()
        assert "user" in data
        assert data["user"]["name"] == "test-user"
        assert data["user"]["rpm"] == 200
        assert data["user"]["tpm"] == 2000
        assert "Updated User" in data["message"]

    def test_update_user_not_found(self, app_with_redis, mock_redis):
        """Test updating non-existent user returns 404."""
        client = TestClient(app_with_redis)
        mock_redis.exists.return_value = 0  # User doesn't exist

        user_data = {"name": "nonexistent", "rpm": 100, "tpm": 1000}

        response = client.post("/UpdateUser", json=user_data)
        assert response.status_code == 404
        assert "does not exist" in response.json()["detail"]

    def test_delete_user(self, app_with_redis, mock_redis):
        """Test deleting a user."""
        client = TestClient(app_with_redis)
        mock_redis.exists.return_value = 1  # User exists
        mock_redis.delete.return_value = 1

        response = client.post("/DeleteUser", json={"name": "test-user"})
        assert response.status_code == 200

        data = response.json()
        assert "Deleted User" in data["message"]

    def test_delete_user_not_found(self, app_with_redis, mock_redis):
        """Test deleting non-existent user returns 404."""
        client = TestClient(app_with_redis)
        mock_redis.exists.return_value = 0  # User doesn't exist

        response = client.post("/DeleteUser", json={"name": "nonexistent"})
        assert response.status_code == 404
        assert "does not exist" in response.json()["detail"]

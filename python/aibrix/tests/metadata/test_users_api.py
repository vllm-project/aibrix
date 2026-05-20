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
- User CRUD API: MetadataStore-backed user management with rate limiting
- MockMetadataStore helper for test isolation
"""

import os

import pytest
from fastapi.testclient import TestClient

# Set required environment variable before importing
os.environ.setdefault("SECRET_KEY", "test-secret-key-for-testing")

# Try importing, skip tests if dependencies missing
try:
    from aibrix.metadata.api.v1.users import User
    from aibrix.metadata.app import build_app
    from aibrix.metadata.store import MetadataStore

    DEPENDENCIES_AVAILABLE = True
except ModuleNotFoundError as e:
    DEPENDENCIES_AVAILABLE = False
    SKIP_REASON = f"Missing dependency: {e}"

pytestmark = pytest.mark.skipif(
    not DEPENDENCIES_AVAILABLE,
    reason="Dependencies not available. Run: poetry install --with dev",
)


class MockMetadataStore(MetadataStore):
    """In-memory MetadataStore for testing."""

    def __init__(self):
        self._data: dict[str, bytes] = {}

    async def get(self, key: str):
        return self._data.get(key)

    async def set(self, key: str, value) -> bool:
        self._data[key] = value if isinstance(value, bytes) else value.encode()
        return True

    async def exists(self, key: str) -> bool:
        return key in self._data

    async def delete(self, key: str) -> bool:
        if key in self._data:
            del self._data[key]
            return True
        return False

    async def ping(self) -> bool:
        return True

    async def close(self) -> None:
        pass


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
    """Integration tests for Users CRUD API using MetadataStore abstraction."""

    @pytest.fixture
    def mock_store(self):
        """Create a MockMetadataStore instance for testing."""
        return MockMetadataStore()

    @pytest.fixture
    def app_with_store(self, mock_store):
        """Build app with mocked MetadataStore."""
        from argparse import Namespace

        args = Namespace(
            enable_fastapi_docs=False,
            disable_batch_api=True,
            disable_file_api=True,
            enable_k8s_job=False,
            disable_inference_endpoint=True,
        )
        app = build_app(args)
        app.state.metadata_store = mock_store
        # Also set redis_client for backward compatibility (via .client if needed)
        app.state.redis_client = None
        return app

    def test_create_user(self, app_with_store, mock_store):
        """Test creating a new user."""
        client = TestClient(app_with_store)

        user_data = {"name": "test-user", "rpm": 100, "tpm": 1000}

        response = client.post("/CreateUser", json=user_data)
        assert response.status_code == 200

        data = response.json()
        assert "user" in data
        assert data["user"]["name"] == "test-user"
        assert data["user"]["rpm"] == 100
        assert data["user"]["tpm"] == 1000
        assert "Created User" in data["message"]

        # Verify store contains the user
        assert "aibrix-users/test-user" in mock_store._data

    def test_create_user_already_exists(self, app_with_store, mock_store):
        """Test creating user that already exists returns success with existing user."""
        client = TestClient(app_with_store)

        # Pre-populate store with existing user
        user = User(name="test-user", rpm=50, tpm=500)
        mock_store._data["aibrix-users/test-user"] = user.model_dump_json().encode()

        user_data = {"name": "test-user", "rpm": 100, "tpm": 1000}

        response = client.post("/CreateUser", json=user_data)
        assert response.status_code == 200
        data = response.json()
        assert "exists" in data["message"]

    def test_read_user(self, app_with_store, mock_store):
        """Test reading a user."""
        client = TestClient(app_with_store)
        user_data = {"name": "test-user", "rpm": 100, "tpm": 1000}
        user = User(**user_data)

        # Pre-populate store
        mock_store._data["aibrix-users/test-user"] = user.model_dump_json().encode()

        response = client.post("/ReadUser", json=user_data)
        assert response.status_code == 200

        data = response.json()
        assert "user" in data
        assert data["user"]["name"] == "test-user"
        assert data["user"]["rpm"] == 100
        assert data["user"]["tpm"] == 1000

    def test_read_user_not_found(self, app_with_store, mock_store):
        """Test reading non-existent user returns 404."""
        client = TestClient(app_with_store)

        response = client.post("/ReadUser", json={"name": "nonexistent"})
        assert response.status_code == 404
        assert "does not exist" in response.json()["error"]["message"]

    def test_update_user(self, app_with_store, mock_store):
        """Test updating a user."""
        client = TestClient(app_with_store)

        # Pre-populate store with existing user
        user = User(name="test-user", rpm=100, tpm=1000)
        mock_store._data["aibrix-users/test-user"] = user.model_dump_json().encode()

        updated_data = {"name": "test-user", "rpm": 200, "tpm": 2000}

        response = client.post("/UpdateUser", json=updated_data)
        assert response.status_code == 200

        data = response.json()
        assert "user" in data
        assert data["user"]["name"] == "test-user"
        assert data["user"]["rpm"] == 200
        assert data["user"]["tpm"] == 2000
        assert "Updated User" in data["message"]

    def test_update_user_not_found(self, app_with_store, mock_store):
        """Test updating non-existent user returns 404."""
        client = TestClient(app_with_store)

        user_data = {"name": "nonexistent", "rpm": 100, "tpm": 1000}

        response = client.post("/UpdateUser", json=user_data)
        assert response.status_code == 404
        assert "does not exist" in response.json()["error"]["message"]

    def test_delete_user(self, app_with_store, mock_store):
        """Test deleting a user."""
        client = TestClient(app_with_store)

        # Pre-populate store with existing user
        user = User(name="test-user", rpm=100, tpm=1000)
        mock_store._data["aibrix-users/test-user"] = user.model_dump_json().encode()

        response = client.post("/DeleteUser", json={"name": "test-user"})
        assert response.status_code == 200

        data = response.json()
        assert "Deleted User" in data["message"]
        # Verify user was removed from store
        assert "aibrix-users/test-user" not in mock_store._data

    def test_delete_user_not_found(self, app_with_store, mock_store):
        """Test deleting non-existent user returns 404."""
        client = TestClient(app_with_store)

        response = client.post("/DeleteUser", json={"name": "nonexistent"})
        assert response.status_code == 404
        assert "does not exist" in response.json()["error"]["message"]

"""In-memory user and token store for simple auth."""

from __future__ import annotations

import uuid

from models.schemas import User


class AuthStore:
    """Simple in-memory user store with token-based sessions."""

    def __init__(self) -> None:
        self._users_by_name: dict[str, User] = {}
        self._tokens: dict[str, User] = {}  # token -> User

    def login(self, name: str) -> tuple[User, str]:
        """Create or retrieve user by name, return (user, token)."""
        name = name.strip()
        if name in self._users_by_name:
            user = self._users_by_name[name]
        else:
            user = User(name=name)
            self._users_by_name[name] = user

        token = str(uuid.uuid4())
        self._tokens[token] = user
        return user, token

    def get_user_by_token(self, token: str) -> User | None:
        return self._tokens.get(token)

    def logout(self, token: str) -> bool:
        return self._tokens.pop(token, None) is not None


auth_store = AuthStore()

"""Shared state for the mock SGLang bootstrap handoff."""

from hashlib import sha256
import threading
import time


def handoff_key(request_id, bootstrap_room):
    if not request_id or bootstrap_room is None:
        raise ValueError("request_id and bootstrap_room are required")
    identity = f"{request_id}:{bootstrap_room}".encode("utf-8")
    return sha256(identity).hexdigest()


class InMemorySGLangHandoffStore:
    """Thread-safe store used by standalone mock tests and local processes."""

    def __init__(self):
        self._failures = {}
        self._lock = threading.Lock()

    def mark_failure(self, request_id, bootstrap_room, error):
        key = handoff_key(request_id, bootstrap_room)
        if not error:
            raise ValueError("error is required")
        with self._lock:
            self._failures[key] = str(error)

    def get_failure(self, request_id, bootstrap_room):
        key = handoff_key(request_id, bootstrap_room)
        with self._lock:
            return self._failures.get(key)


class KubernetesSGLangHandoffStore:
    """ConfigMap-backed store shared by mock pods in one namespace."""

    def __init__(self, api, namespace):
        self._api = api
        self._namespace = namespace

    @staticmethod
    def _name(key):
        return f"aibrix-sglang-handoff-{key[:32]}"

    def mark_failure(self, request_id, bootstrap_room, error):
        key = handoff_key(request_id, bootstrap_room)
        if not error:
            raise ValueError("error is required")
        name = self._name(key)
        body = {
            "apiVersion": "v1",
            "kind": "ConfigMap",
            "metadata": {
                "name": name,
                "labels": {"app.kubernetes.io/managed-by": "aibrix-vllm-mock"},
            },
            "data": {"status": "failed", "error": str(error)},
        }
        for attempt in range(3):
            try:
                self._api.create_namespaced_config_map(self._namespace, body)
                return
            except Exception as exc:
                if getattr(exc, "status", None) == 409:
                    try:
                        self._api.patch_namespaced_config_map(
                            name,
                            self._namespace,
                            {"data": body["data"]},
                        )
                        return
                    except Exception:
                        if attempt == 2:
                            raise
                elif attempt == 2:
                    raise
            time.sleep(0.05 * (attempt + 1))

    def get_failure(self, request_id, bootstrap_room):
        key = handoff_key(request_id, bootstrap_room)
        name = self._name(key)
        try:
            config_map = self._api.read_namespaced_config_map(
                name, self._namespace
            )
        except Exception as exc:
            if getattr(exc, "status", None) == 404:
                return None
            raise
        data = getattr(config_map, "data", None) or {}
        if data.get("status") != "failed":
            return None
        try:
            self._api.delete_namespaced_config_map(name, self._namespace)
        except Exception:
            pass
        return data.get("error") or "prefill handoff failed"

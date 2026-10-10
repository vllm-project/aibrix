import pytest

from sglang_handoff import InMemorySGLangHandoffStore, handoff_key
from sglang_handoff import KubernetesSGLangHandoffStore


class FakeKubernetesError(Exception):
    def __init__(self, status):
        self.status = status


class FakeConfigMap:
    def __init__(self, data):
        self.data = data


class FakeConfigMapAPI:
    def __init__(self, *, exists=False, create_error=None):
        self.exists = exists
        self.create_error = create_error
        self.data = None
        self.patched = False
        self.deleted = False

    def create_namespaced_config_map(self, namespace, body):
        if self.create_error is not None:
            raise self.create_error
        if self.exists:
            raise FakeKubernetesError(409)
        self.exists = True
        self.data = body["data"]

    def patch_namespaced_config_map(self, name, namespace, body):
        self.patched = True
        self.data = body["data"]

    def read_namespaced_config_map(self, name, namespace):
        if not self.exists:
            raise FakeKubernetesError(404)
        return FakeConfigMap(self.data)

    def delete_namespaced_config_map(self, name, namespace):
        self.deleted = True
        self.exists = False


def test_handoff_key_is_stable_for_request_and_bootstrap_room():
    assert handoff_key("request-1", 123) == handoff_key("request-1", 123)
    assert handoff_key("request-1", 123) != handoff_key("request-2", 123)
    assert handoff_key("request-1", 123) != handoff_key("request-1", 124)


def test_in_memory_store_records_and_reads_prefill_failure():
    store = InMemorySGLangHandoffStore()

    assert store.get_failure("request-1", 123) is None

    store.mark_failure("request-1", 123, "prefill handoff failed")

    assert store.get_failure("request-1", 123) == "prefill handoff failed"


def test_in_memory_store_rejects_empty_handoff_identity():
    store = InMemorySGLangHandoffStore()

    with pytest.raises(ValueError):
        store.mark_failure("", 123, "prefill handoff failed")
    with pytest.raises(ValueError):
        store.get_failure("request-1", None)


def test_kubernetes_store_creates_reads_and_deletes_failure_config_map():
    api = FakeConfigMapAPI()
    store = KubernetesSGLangHandoffStore(api, "default")

    store.mark_failure("request-1", 123, "prefill handoff failed")

    assert store.get_failure("request-1", 123) == "prefill handoff failed"
    assert api.deleted


def test_kubernetes_store_patches_existing_failure_config_map():
    api = FakeConfigMapAPI(exists=True)
    store = KubernetesSGLangHandoffStore(api, "default")

    store.mark_failure("request-1", 123, "prefill handoff failed")

    assert api.patched


def test_kubernetes_store_propagates_create_errors():
    api = FakeConfigMapAPI(create_error=RuntimeError("API unavailable"))
    store = KubernetesSGLangHandoffStore(api, "default")

    with pytest.raises(RuntimeError, match="API unavailable"):
        store.mark_failure("request-1", 123, "prefill handoff failed")

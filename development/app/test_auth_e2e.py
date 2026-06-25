import importlib.util
import json
import sys
import threading
import urllib.error
import urllib.request
from pathlib import Path

from werkzeug.serving import make_server


APP_DIR = Path(__file__).parent
APP_PATH = APP_DIR / "app.py"
CHAT_COMPLETIONS_PAYLOAD = {
    "model": "llama2-7b",
    "messages": [{"role": "user", "content": "hello"}],
}


def load_mock_app(monkeypatch, argv):
    module_name = "aibrix_mock_app_under_test"
    monkeypatch.chdir(APP_DIR)
    monkeypatch.setattr(sys, "argv", argv)
    monkeypatch.setenv("STANDALONE_MODE", "true")
    monkeypatch.setenv("SIMULATION", "disabled")
    monkeypatch.setenv("MOCK_REQUEST_DURATION_SECONDS", "0")
    sys.modules.pop(module_name, None)

    spec = importlib.util.spec_from_file_location(module_name, APP_PATH)
    module = importlib.util.module_from_spec(spec)
    sys.modules[module_name] = module
    spec.loader.exec_module(module)
    return module.app


class LiveServer:
    def __init__(self, app):
        self.server = make_server("127.0.0.1", 0, app)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    @property
    def base_url(self):
        return f"http://127.0.0.1:{self.server.server_port}"

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, exc_type, exc, tb):
        self.server.shutdown()
        self.thread.join(timeout=5)
        self.server.server_close()


def post_chat_completion(base_url, token=None):
    headers = {"Content-Type": "application/json"}
    if token is not None:
        headers["Authorization"] = f"Bearer {token}"

    request = urllib.request.Request(
        f"{base_url}/v1/chat/completions",
        data=json.dumps(CHAT_COMPLETIONS_PAYLOAD).encode("utf-8"),
        headers=headers,
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=5) as response:
            return response.status, json.loads(response.read().decode("utf-8"))
    except urllib.error.HTTPError as error:
        return error.code, json.loads(error.read().decode("utf-8"))


def test_chat_completions_allows_missing_authorization_without_api_key(monkeypatch):
    app = load_mock_app(monkeypatch, ["app.py"])

    with LiveServer(app) as server:
        status, body = post_chat_completion(server.base_url)

    assert status == 200
    assert body["choices"][0]["message"]["content"]


def test_chat_completions_requires_matching_authorization_with_api_key(monkeypatch):
    app = load_mock_app(monkeypatch, ["app.py", "--api_key", "secret"])

    with LiveServer(app) as server:
        missing_status, missing_body = post_chat_completion(server.base_url)
        wrong_status, wrong_body = post_chat_completion(server.base_url, token="wrong")
        correct_status, correct_body = post_chat_completion(server.base_url, token="secret")

    assert missing_status == 401
    assert missing_body["error"]["code"] == "invalid_api_key"
    assert wrong_status == 401
    assert wrong_body["error"]["code"] == "invalid_api_key"
    assert correct_status == 200
    assert correct_body["choices"][0]["message"]["content"]


def test_chat_completions_requires_matching_authorization_with_api_key_alias(monkeypatch):
    app = load_mock_app(monkeypatch, ["app.py", "--api-key", "secret"])

    with LiveServer(app) as server:
        missing_status, missing_body = post_chat_completion(server.base_url)
        correct_status, correct_body = post_chat_completion(server.base_url, token="secret")

    assert missing_status == 401
    assert missing_body["error"]["code"] == "invalid_api_key"
    assert correct_status == 200
    assert correct_body["choices"][0]["message"]["content"]

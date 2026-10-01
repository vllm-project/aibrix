import importlib.util
import json
import sys
import time
from pathlib import Path


APP_DIR = Path(__file__).parent
APP_PATH = APP_DIR / "app.py"
SCALING_ERROR = "The model is currently scaling. Please try again later."


def load_mock_module(monkeypatch, api_key=None):
    module_name = "aibrix_mock_app_elastic_ep_under_test"
    monkeypatch.chdir(APP_DIR)
    monkeypatch.setenv("STANDALONE_MODE", "true")
    monkeypatch.setenv("SIMULATION", "disabled")
    monkeypatch.setenv("MOCK_REQUEST_DURATION_SECONDS", "0")
    if api_key is None:
        monkeypatch.setattr(sys, "argv", ["app.py"])
    else:
        monkeypatch.setattr(sys, "argv", ["app.py", "--api_key", api_key])
    sys.modules.pop(module_name, None)

    spec = importlib.util.spec_from_file_location(module_name, APP_PATH)
    module = importlib.util.module_from_spec(spec)
    sys.modules[module_name] = module
    spec.loader.exec_module(module)
    return module


def post_json(client, path, payload):
    return client.post(
        path,
        data=json.dumps(payload),
        headers={"Content-Type": "application/json"},
    )


def test_is_scaling_elastic_ep_reports_idle_by_default(monkeypatch):
    module = load_mock_module(monkeypatch)
    client = module.app.test_client()

    response = client.post("/is_scaling_elastic_ep")

    assert response.status_code == 200
    assert response.get_json() == {"is_scaling_elastic_ep": False}


def test_debug_elastic_ep_drives_the_scaling_state(monkeypatch):
    module = load_mock_module(monkeypatch)
    client = module.app.test_client()

    idle = client.get("/debug/elastic_ep")
    assert idle.status_code == 200
    assert idle.get_json() == {"is_scaling_elastic_ep": False}

    started = post_json(client, "/debug/elastic_ep", {"scaling": True})
    assert started.status_code == 200
    assert started.get_json() == {"status": "success", "is_scaling_elastic_ep": True}

    probe = client.post("/is_scaling_elastic_ep")
    assert probe.status_code == 503
    assert probe.get_json() == {"error": SCALING_ERROR}

    cleared = post_json(client, "/debug/elastic_ep", {"scaling": False})
    assert cleared.status_code == 200
    assert cleared.get_json() == {"status": "success", "is_scaling_elastic_ep": False}

    probe = client.post("/is_scaling_elastic_ep")
    assert probe.status_code == 200
    assert probe.get_json() == {"is_scaling_elastic_ep": False}


def test_scaling_window_blocks_engine_requests_but_not_the_debug_surface(monkeypatch):
    module = load_mock_module(monkeypatch)
    client = module.app.test_client()
    post_json(client, "/debug/elastic_ep", {"scaling": True})

    health = client.get("/health")
    assert health.status_code == 503
    assert health.get_json() == {"error": SCALING_ERROR}

    completion = post_json(
        client,
        "/v1/chat/completions",
        {"model": "llama2-7b", "messages": [{"role": "user", "content": "hello"}]},
    )
    assert completion.status_code == 503
    assert completion.get_json() == {"error": SCALING_ERROR}

    debug = client.get("/debug/elastic_ep")
    assert debug.status_code == 200
    assert debug.get_json() == {"is_scaling_elastic_ep": True}


def test_scaling_window_expires_after_its_duration(monkeypatch):
    module = load_mock_module(monkeypatch)
    client = module.app.test_client()
    post_json(client, "/debug/elastic_ep", {"scaling": True, "duration_seconds": 1.0})

    assert client.post("/is_scaling_elastic_ep").status_code == 503

    time.sleep(1.5)
    response = client.post("/is_scaling_elastic_ep")
    assert response.status_code == 200
    assert response.get_json() == {"is_scaling_elastic_ep": False}


def test_debug_elastic_ep_rejects_invalid_payloads(monkeypatch):
    module = load_mock_module(monkeypatch)
    client = module.app.test_client()

    assert client.post("/debug/elastic_ep").status_code == 400
    assert post_json(client, "/debug/elastic_ep", {}).status_code == 400
    assert post_json(client, "/debug/elastic_ep", {"scaling": "yes"}).status_code == 400
    invalid_duration = post_json(
        client, "/debug/elastic_ep", {"scaling": True, "duration_seconds": 0}
    )
    assert invalid_duration.status_code == 400

    probe = client.post("/is_scaling_elastic_ep")
    assert probe.status_code == 200
    assert probe.get_json() == {"is_scaling_elastic_ep": False}


def test_probe_stays_reachable_when_api_key_is_set(monkeypatch):
    module = load_mock_module(monkeypatch, api_key="secret")
    client = module.app.test_client()

    probe = client.post("/is_scaling_elastic_ep")
    assert probe.status_code == 200

    guarded = post_json(
        client,
        "/v1/chat/completions",
        {"model": "llama2-7b", "messages": [{"role": "user", "content": "hello"}]},
    )
    assert guarded.status_code == 401

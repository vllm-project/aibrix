# Copyright 2026 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import hashlib
import json
import os
import subprocess
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from types import SimpleNamespace
from urllib.parse import unquote

import pytest
import requests
from huggingface_hub import constants, file_download, hf_api, hf_hub_download

from aibrix.downloader.hf_cache import discover_cached_hf_artifacts
from aibrix.downloader.model_cache import prepare_model_cache
from aibrix.runtime.model_runtime import _resolve_model_path

FILES = {
    "config.json": b'{"model_type":"qwen2"}',
    "tokenizer_config.json": b'{"tokenizer_class":"Qwen2Tokenizer"}',
    "tokenizer.json": b'{"version":"1.0"}',
    "model.safetensors": b"complete-model-weights",
}
COMMIT = "a" * 40


@pytest.fixture
def hub(monkeypatch):
    state = SimpleNamespace(payloads=[], fail=False)

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_HEAD(self):
            self.respond(False)

        def do_GET(self):
            self.respond(True)

        def respond(self, body):
            path = unquote(self.path.split("?", 1)[0])
            if path.startswith("/api/models/Org/Qwen/revision/"):
                data = json.dumps(
                    {
                        "id": "Org/Qwen",
                        "sha": COMMIT,
                        "siblings": [{"rfilename": name} for name in FILES],
                    }
                ).encode()
                self.send_response(200)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                if body:
                    self.wfile.write(data)
                return
            name = path.rsplit("/", 1)[-1]
            if name not in FILES:
                self.send_error(404)
                return
            if body and state.fail and name == "model.safetensors":
                self.send_error(500, "interrupted fixture transfer")
                return
            data = FILES[name]
            self.send_response(200)
            self.send_header("Content-Length", str(len(data)))
            self.send_header("ETag", '"' + hashlib.sha256(data).hexdigest() + '"')
            self.send_header("X-Repo-Commit", COMMIT)
            self.end_headers()
            if body:
                state.payloads.append(name)
                self.wfile.write(data)

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    state.endpoint = f"http://127.0.0.1:{server.server_port}"
    original_request = requests.Session.request

    def local_only(session, method, url, *args, **kwargs):
        assert url.startswith(state.endpoint + "/"), "test attempted a nonlocal request"
        return original_request(session, method, url, *args, **kwargs)

    monkeypatch.setattr(requests.Session, "request", local_only)
    monkeypatch.setattr(constants, "ENDPOINT", state.endpoint)
    monkeypatch.setattr(hf_api, "ENDPOINT", state.endpoint)
    monkeypatch.setattr(
        file_download,
        "HUGGINGFACE_CO_URL_TEMPLATE",
        state.endpoint + "/{repo_id}/resolve/{revision}/{filename}",
    )
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield state
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


def test_prepare_is_reused_by_unchanged_native_repo_loader(hub, tmp_path):
    prepared = prepare_model_cache("huggingface://Org/Qwen", str(tmp_path))
    assert prepared.name == COMMIT
    assert sorted(hub.payloads) == sorted(FILES)
    assert (tmp_path / "models--Org--Qwen/refs/main").read_text() == COMMIT
    # This is the exact repo id returned by the unmodified runtime resolver.
    repo = _resolve_model_path("huggingface://Org/Qwen")
    assert repo == "Org/Qwen"
    for name, data in FILES.items():
        file = hf_hub_download(repo, name, cache_dir=str(tmp_path))
        assert Path(file).read_bytes() == data
    assert sorted(hub.payloads) == sorted(FILES), "native consumer repeated payload GET"
    assert "huggingface://Org/Qwen" in discover_cached_hf_artifacts(tmp_path)


def test_concurrent_native_preparers_share_client_locks(hub, tmp_path):
    with ThreadPoolExecutor(max_workers=2) as pool:
        paths = list(
            pool.map(
                lambda _: prepare_model_cache("hf://Org/Qwen", str(tmp_path)), range(2)
            )
        )
    assert paths[0] == paths[1]
    assert sorted(hub.payloads) == sorted(FILES)


def test_interrupted_native_download_is_not_advertised_and_recovers(hub, tmp_path):
    hub.fail = True
    with pytest.raises(Exception):
        prepare_model_cache("hf://Org/Qwen", str(tmp_path))
    assert discover_cached_hf_artifacts(tmp_path) == []
    hub.fail = False
    result = prepare_model_cache("hf://Org/Qwen", str(tmp_path))
    assert (result / "model.safetensors").read_bytes() == FILES["model.safetensors"]
    assert "hf://Org/Qwen" in discover_cached_hf_artifacts(tmp_path)


def test_prepare_cli_uses_native_cache_without_a_revision_argument(hub, tmp_path):
    env = dict(os.environ, HF_ENDPOINT=hub.endpoint, HF_HUB_DISABLE_TELEMETRY="1")
    result = subprocess.run(
        [
            sys.executable,
            "-m",
            "aibrix.downloader.model_cache",
            "--model-uri",
            "hf://Org/Qwen",
            "--cache-dir",
            str(tmp_path),
        ],
        env=env,
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert result.returncode == 0, result.stderr
    assert result.stdout.strip().endswith(COMMIT)
    assert "hf://Org/Qwen" in discover_cached_hf_artifacts(tmp_path)


def test_prepare_cli_rejects_unknown_scheme(tmp_path):
    result = subprocess.run(
        [
            sys.executable,
            "-m",
            "aibrix.downloader.model_cache",
            "--model-uri",
            "azure://bucket/model",
            "--cache-dir",
            str(tmp_path),
        ],
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert result.returncode != 0
    assert not list(tmp_path.iterdir())


def test_prepare_job_and_native_consumer_process_share_weight_download(hub, tmp_path):
    env = dict(
        os.environ,
        HF_ENDPOINT=hub.endpoint,
        HF_HUB_DISABLE_TELEMETRY="1",
        HF_HUB_DISABLE_IMPLICIT_TOKEN="1",
    )
    prepare = [
        sys.executable,
        "-m",
        "aibrix.downloader.model_cache",
        "--model-uri",
        "hf://Org/Qwen",
        "--cache-dir",
        str(tmp_path),
    ]
    consume = [
        sys.executable,
        "-c",
        "from huggingface_hub import hf_hub_download; import sys; print(hf_hub_download('Org/Qwen', 'model.safetensors', cache_dir=sys.argv[1]))",
        str(tmp_path),
    ]
    with ThreadPoolExecutor(max_workers=2) as pool:
        outcomes = list(
            pool.map(
                lambda cmd: subprocess.run(
                    cmd, env=env, capture_output=True, text=True, timeout=30
                ),
                [prepare, consume],
            )
        )
    assert all(result.returncode == 0 for result in outcomes), [
        r.stderr for r in outcomes
    ]
    assert hub.payloads.count("model.safetensors") == 1
    assert "hf://Org/Qwen" in discover_cached_hf_artifacts(tmp_path)


def test_prepared_files_reach_existing_runtime_snapshot_http_field(
    hub, tmp_path, monkeypatch
):
    from fastapi import FastAPI
    from fastapi.testclient import TestClient

    from aibrix import envs
    from aibrix.app import runtime_lifespan
    from aibrix.runtime import model_runtime
    from aibrix.runtime.model_runtime_api import model_runtime_router

    prepare_model_cache("hf://Org/Qwen", str(tmp_path))
    monkeypatch.setattr(constants, "HF_HUB_CACHE", str(tmp_path))
    monkeypatch.setattr(envs, "DOWNLOADER_LOCAL_DIR", str(tmp_path / "objects"))
    monkeypatch.setenv("AIBRIX_WEIGHT_CACHE_DIR", str(tmp_path / "markers"))
    agent = model_runtime.ModelRuntime(model_runtime.MockEngineLauncher())
    monkeypatch.setattr(model_runtime, "_AGENT", agent)
    app = FastAPI(lifespan=runtime_lifespan)
    app.include_router(model_runtime_router)
    with TestClient(app) as client:
        deadline = time.monotonic() + 3
        while time.monotonic() < deadline:
            response = client.get("/v1/runtime/snapshot")
            assert response.status_code == 200
            if "hf://Org/Qwen" in response.json()["cached_artifacts"]:
                break
            time.sleep(0.005)
        else:
            pytest.fail("prepared files never appeared in the unchanged snapshot field")
    assert sorted(hub.payloads) == sorted(FILES), "discovery must not download remotely"

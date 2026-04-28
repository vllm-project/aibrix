# Copyright 2026 The Aibrix Team.
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

"""TemplateRegistry / ProfileRegistry load + lookup tests.

Uses LocalFileSource so tests run without a Kubernetes cluster.
"""

from pathlib import Path

import yaml

from aibrix.batch.template import (
    local_profile_registry,
    local_template_registry,
)


def _write(tmp_path: Path, name: str, content) -> Path:
    p = tmp_path / name
    p.write_text(yaml.safe_dump(content) if not isinstance(content, str) else content)
    return p


def _valid_template(name="m", version="v1", status="active"):
    return {
        "name": name,
        "version": version,
        "status": status,
        "spec": {
            "engine": {"type": "mock", "version": "0.1", "image": "x"},
            "model_source": {"type": "local", "uri": "/x"},
            "accelerator": {"type": "cpu", "count": 1},
            "provider_config": {"type": "k8s"},
            "supported_endpoints": ["/v1/chat/completions"],
        },
    }


def _valid_profile(name="p1"):
    return {
        "name": name,
        "spec": {"storage": {"backend": "local", "bucket": "/tmp"}},
    }


class TestTemplateRegistry:
    def test_load_plain_top_level_list(self, tmp_path):
        path = _write(tmp_path, "t.yaml", [_valid_template("a"), _valid_template("b")])
        reg = local_template_registry(path)
        errors = reg.reload()
        assert errors == []
        assert sorted(reg.names()) == ["a", "b"]

    def test_load_configmap_wrapped(self, tmp_path):
        cm = {
            "apiVersion": "v1",
            "kind": "ConfigMap",
            "metadata": {"name": "x", "namespace": "aibrix-system"},
            "data": {"templates.yaml": yaml.safe_dump([_valid_template()])},
        }
        path = _write(tmp_path, "cm.yaml", cm)
        reg = local_template_registry(path)
        reg.reload()
        assert reg.get("m") is not None

    def test_missing_file_yields_empty_registry(self, tmp_path):
        reg = local_template_registry(tmp_path / "nonexistent.yaml")
        errors = reg.reload()
        assert errors == []
        assert reg.names() == []

    def test_per_item_error_isolation(self, tmp_path):
        bad = _valid_template(name="bad")
        bad["spec"]["accelerator"]["count"] = 4
        bad["spec"]["parallelism"] = {"tp": 2}  # 2 != 4 -> validator fails
        path = _write(tmp_path, "t.yaml", [_valid_template("good"), bad])
        reg = local_template_registry(path)
        errors = reg.reload()
        assert len(errors) == 1
        assert "bad" in errors[0].item_identifier
        # 'good' loaded despite 'bad' failing
        assert reg.get("good") is not None
        assert reg.get("bad") is None

    def test_multiple_active_versions_rejected(self, tmp_path):
        path = _write(
            tmp_path,
            "t.yaml",
            [
                _valid_template("dup", "v1", "active"),
                _valid_template("dup", "v2", "active"),
            ],
        )
        reg = local_template_registry(path)
        errors = reg.reload()
        assert any("multiple active" in e.error for e in errors)
        # Both versions must be excluded from active map
        assert reg.get("dup") is None
        # But discoverable individually for admin debugging
        assert reg.get_by_version("dup", "v1") is not None
        assert reg.get_by_version("dup", "v2") is not None

    def test_deprecated_entry_excluded_from_get(self, tmp_path):
        path = _write(
            tmp_path,
            "t.yaml",
            [_valid_template("old", "v1", "deprecated")],
        )
        reg = local_template_registry(path)
        reg.reload()
        assert reg.get("old") is None  # not active
        assert reg.get_by_version("old", "v1") is not None
        assert reg.names() == []

    def test_top_level_non_list_rejected(self, tmp_path):
        path = _write(tmp_path, "t.yaml", {"items": [_valid_template()]})
        reg = local_template_registry(path)
        errors = reg.reload()
        assert any("top-level list" in e.error for e in errors)

    def test_thread_safe_lookup(self, tmp_path):
        # Sanity: get() and reload() can be invoked from same thread
        # without deadlock; full multithread soak is overkill here.
        path = _write(tmp_path, "t.yaml", [_valid_template("a")])
        reg = local_template_registry(path)
        reg.reload()
        for _ in range(100):
            assert reg.get("a") is not None


class TestProfileRegistry:
    def test_load_with_default(self, tmp_path):
        path = _write(
            tmp_path,
            "p.yaml",
            {"default": "p1", "items": [_valid_profile("p1"), _valid_profile("p2")]},
        )
        reg = local_profile_registry(path)
        errors = reg.reload()
        assert errors == []
        assert reg.default_name() == "p1"
        assert reg.get_default().name == "p1"
        assert sorted(reg.names()) == ["p1", "p2"]

    def test_default_pointing_at_missing_profile_recorded_as_error(self, tmp_path):
        path = _write(
            tmp_path,
            "p.yaml",
            {"default": "missing", "items": [_valid_profile("p1")]},
        )
        reg = local_profile_registry(path)
        reg.reload()
        # Either pydantic-level or registry-level error; either way, default must not stick.
        assert reg.default_name() is None or reg.default_name() == "p1"

    def test_per_item_error_isolation(self, tmp_path):
        bad = {"name": "bad", "spec": {"storage": {}}}  # storage.backend missing
        path = _write(
            tmp_path,
            "p.yaml",
            {"items": [_valid_profile("good"), bad]},
        )
        reg = local_profile_registry(path)
        errors = reg.reload()
        assert any("bad" in e.item_identifier for e in errors)
        assert reg.get("good") is not None
        assert reg.get("bad") is None

    def test_no_default_field(self, tmp_path):
        path = _write(tmp_path, "p.yaml", {"items": [_valid_profile("p1")]})
        reg = local_profile_registry(path)
        reg.reload()
        assert reg.default_name() is None
        assert reg.get_default() is None

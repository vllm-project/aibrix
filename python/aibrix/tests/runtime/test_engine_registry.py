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

"""Tests for the crash-safe local runtime engine registry."""

from aibrix.runtime.engine_registry import EngineRegistry


def _engine_record(model_name="qwen3-0.6b"):
    return {
        "model_name": model_name,
        "port": 8100,
        "ipc_name": "kvc_qwen3-0-6b",
        "pid": 1234,
        "pid_start_time": "123456",
        "engine": "vllm",
        "artifact_url": "hf://Qwen/Qwen3-0.6B",
        "engine_config": {"--max-model-len": "2048"},
        "additional_config": {"VLLM_LOGGING_LEVEL": "WARNING"},
        "claim_ref": {"name": "qwen", "namespace": "default", "uid": "uid-1"},
        "phase": "active",
        "restart_count": 2,
        "last_error": "previous launch timed out",
        "last_transition": "2026-07-14T00:00:00+00:00",
        "next_restart_at": None,
    }


def test_registry_round_trips_complete_engine_metadata(tmp_path):
    path = tmp_path / "run" / "engines.json"
    records = [_engine_record()]

    EngineRegistry(path).save(records)

    assert EngineRegistry(path).load() == records


def test_registry_ignores_corrupt_file_without_destroying_it(tmp_path, caplog):
    path = tmp_path / "engines.json"
    path.write_text("{broken json", encoding="utf-8")

    assert EngineRegistry(path).load() == []

    assert path.read_text(encoding="utf-8") == "{broken json"
    assert "could not load engine registry" in caplog.text


def test_registry_ignores_non_utf8_file_without_destroying_it(tmp_path):
    path = tmp_path / "engines.json"
    path.write_bytes(b"\xff\xfe")

    assert EngineRegistry(path).load() == []

    assert path.read_bytes() == b"\xff\xfe"


def test_registry_replaces_previous_complete_state(tmp_path):
    path = tmp_path / "engines.json"
    registry = EngineRegistry(path)
    registry.save([_engine_record("first")])

    replacement = [_engine_record("second")]
    registry.save(replacement)

    assert registry.load() == replacement

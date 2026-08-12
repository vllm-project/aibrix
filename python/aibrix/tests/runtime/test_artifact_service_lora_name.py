# Copyright 2024 The Aibrix Team.
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

import os

import pytest

from aibrix.runtime.artifact_service import ArtifactDelegationService


@pytest.fixture
def service(tmp_path):
    return ArtifactDelegationService(local_dir=str(tmp_path))


class TestLoraNameValidation:
    """Reject lora_name shapes that escape local_dir."""

    @pytest.mark.parametrize(
        "name",
        [
            "../etc",
            "../../etc/passwd",
            "..",
            "a/../b",
            "valid_but_then/../escape",
            "/abs/path",
            "name/with/slash",
            "name\\with\\backslash",
            "name\x00null",
            ".hidden",
            "-leading-dash",
            "",
            "a" * 129,
        ],
    )
    def test_rejects_malicious_names(self, service, name):
        with pytest.raises(ValueError):
            service._get_local_path_for_adapter(name)

    @pytest.mark.parametrize(
        "name",
        [
            "valid_adapter",
            "adapter-v2",
            "Adapter.1",
            "abc123",
            "a",
            "A" * 128,
        ],
    )
    def test_accepts_valid_names(self, service, name):
        path = service._get_local_path_for_adapter(name)
        assert path.endswith(os.sep + name)

    def test_symlink_traversal_blocked(self, service, tmp_path):
        outside = tmp_path.parent / "outside_target"
        outside.mkdir(exist_ok=True)
        link = tmp_path / "linked"
        try:
            os.symlink(outside, link)
        except (OSError, NotImplementedError):
            pytest.skip("symlinks unavailable on this platform")
        with pytest.raises(ValueError):
            service._get_local_path_for_adapter("linked")

    def test_symlink_resolving_to_base_blocked(self, service, tmp_path):
        """Regression: a name whose resolved path equals local_dir itself
        must be rejected so cleanup never targets the artifacts root."""
        link = tmp_path / "selfloop"
        try:
            os.symlink(tmp_path, link)
        except (OSError, NotImplementedError):
            pytest.skip("symlinks unavailable on this platform")
        with pytest.raises(ValueError):
            service._get_local_path_for_adapter("selfloop")

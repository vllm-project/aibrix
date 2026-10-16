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

"""Tests for the always-allow file suffix mechanism.

When DOWNLOADER_ALLOW_FILE_SUFFIX is set (e.g. "json, safetensors" as
recommended in the docs and samples), small runtime-critical files such as
remote-code files (``*.py``), tokenizer vocabularies (``*.txt`` / ``*.model`` /
``*.tiktoken``) and standalone chat templates (``*.jinja``) are silently
dropped, breaking trust_remote_code models (see issue #834) and models whose
chat template only lives in ``chat_template.jinja``.

These files are KB~MB scale, so they should always be downloaded regardless of
the user-specified suffix filter, which is meant to exclude large weights.
"""

from pathlib import Path
from typing import List, Optional
from unittest.mock import patch

import pytest

from aibrix import envs
from aibrix.downloader.base import BaseDownloader, DownloadExtraConfig
from aibrix.downloader.huggingface import HuggingFaceDownloader

# A representative repo layout, modeled after XHToken/Spark-X2.5-4B and
# deepseek-ai/DeepSeek-R1 (issue #834).
REPO_FILES = [
    "config.json",
    "generation_config.json",
    "model.safetensors.index.json",
    "model-00001-of-00002.safetensors",
    "model-00002-of-00002.safetensors",
    "modeling_spark.py",  # remote code (auto_map)
    "configuration_spark.py",  # remote code (auto_map)
    "merges.txt",  # BPE tokenizer merges
    "tokenizer.model",  # sentencepiece tokenizer
    "tokenizer.tiktoken",  # tiktoken vocab
    "chat_template.jinja",  # standalone chat template
    "README.md",
    "LICENSE",
    "large_weights.bin",  # should still be filtered out
    "calib/activations.npy",  # must NOT match the always-allow "py" suffix
    "pytorch_model",  # extensionless; must NOT match the always-allow "model"
]


class FakeDownloader(BaseDownloader):
    """Minimal S3/TOS-like downloader that records the filtered file list."""

    downloaded: List[str] = []

    def _valid_config(self):
        pass

    def _is_directory(self) -> bool:
        return True

    def _directory_list(self, path: str) -> List[str]:
        return list(REPO_FILES)

    def _support_range_download(self) -> bool:
        return False

    def download(
        self,
        local_path: Path,
        bucket_path: str,
        bucket_name: Optional[str] = None,
        enable_range: bool = True,
    ):
        FakeDownloader.downloaded.append(bucket_path)


@pytest.fixture(autouse=True)
def reset_fake_downloader():
    FakeDownloader.downloaded = []
    yield


def make_fake_downloader(
    allow_file_suffix: Optional[List[str]] = None,
    always_allow_file_suffix: Optional[List[str]] = None,
) -> FakeDownloader:
    config = DownloadExtraConfig(
        allow_file_suffix=allow_file_suffix,
        always_allow_file_suffix=always_allow_file_suffix,
    )
    return FakeDownloader(
        model_uri="s3://bucket/model",
        model_name="model",
        bucket_path="model",
        bucket_name="bucket",
        download_extra_config=config,
    )


def make_hf_downloader(
    allow_file_suffix: Optional[List[str]] = None,
    always_allow_file_suffix: Optional[List[str]] = None,
) -> HuggingFaceDownloader:
    config = DownloadExtraConfig(
        allow_file_suffix=allow_file_suffix,
        always_allow_file_suffix=always_allow_file_suffix,
    )
    with patch("aibrix.downloader.huggingface.HfApi") as mock_hf_api:
        mock_hf_api.return_value.repo_exists.return_value = True
        return HuggingFaceDownloader(
            model_uri="XHToken/Spark-X2.5-4B",
            model_name="spark",
            download_extra_config=config,
        )


class TestAlwaysAllowEnvDefault:
    def test_env_has_always_allow_suffix_default(self):
        assert envs.DOWNLOADER_ALWAYS_ALLOW_FILE_SUFFIX is not None
        for suffix in ("py", "txt", "jinja", "model", "tiktoken"):
            assert suffix in envs.DOWNLOADER_ALWAYS_ALLOW_FILE_SUFFIX


class TestBaseDownloaderFilter:
    def test_no_filter_downloads_everything(self, monkeypatch):
        monkeypatch.setattr(envs, "DOWNLOADER_ALLOW_FILE_SUFFIX", None)
        downloader = make_fake_downloader(allow_file_suffix=None)
        downloader.download_directory(local_path=Path("/tmp/model"))
        assert sorted(FakeDownloader.downloaded) == sorted(REPO_FILES)

    def test_suffix_filter_keeps_small_runtime_files(self, monkeypatch):
        # Docs/samples recommended config: "json, safetensors"
        monkeypatch.setattr(envs, "DOWNLOADER_ALLOW_FILE_SUFFIX", None)
        downloader = make_fake_downloader(allow_file_suffix=["json", "safetensors"])
        downloader.download_directory(local_path=Path("/tmp/model"))
        downloaded = set(FakeDownloader.downloaded)

        # user suffixes still work
        assert "config.json" in downloaded
        assert "model-00001-of-00002.safetensors" in downloaded
        # remote code (issue #834) is kept
        assert "modeling_spark.py" in downloaded
        assert "configuration_spark.py" in downloaded
        # tokenizer files are kept
        assert "merges.txt" in downloaded
        assert "tokenizer.model" in downloaded
        assert "tokenizer.tiktoken" in downloaded
        # chat template is kept
        assert "chat_template.jinja" in downloaded
        # non-essential files are still filtered out
        assert "README.md" not in downloaded
        assert "LICENSE" not in downloaded
        assert "large_weights.bin" not in downloaded

    def test_always_allow_can_be_disabled_via_config(self, monkeypatch):
        monkeypatch.setattr(envs, "DOWNLOADER_ALLOW_FILE_SUFFIX", None)
        downloader = make_fake_downloader(
            allow_file_suffix=["json", "safetensors"],
            always_allow_file_suffix=[],
        )
        downloader.download_directory(local_path=Path("/tmp/model"))
        downloaded = set(FakeDownloader.downloaded)
        assert "modeling_spark.py" not in downloaded
        assert "config.json" in downloaded

    def test_always_allow_can_be_overridden_via_config(self, monkeypatch):
        monkeypatch.setattr(envs, "DOWNLOADER_ALLOW_FILE_SUFFIX", None)
        downloader = make_fake_downloader(
            allow_file_suffix=["json"],
            always_allow_file_suffix=["gguf"],
        )
        downloader.download_directory(local_path=Path("/tmp/model"))
        downloaded = set(FakeDownloader.downloaded)
        assert "modeling_spark.py" not in downloaded
        assert "config.json" in downloaded

    def test_env_always_allow_override(self, monkeypatch):
        monkeypatch.setattr(envs, "DOWNLOADER_ALLOW_FILE_SUFFIX", None)
        monkeypatch.setattr(envs, "DOWNLOADER_ALWAYS_ALLOW_FILE_SUFFIX", ["py"])
        downloader = make_fake_downloader(allow_file_suffix=["json", "safetensors"])
        downloader.download_directory(local_path=Path("/tmp/model"))
        downloaded = set(FakeDownloader.downloaded)
        assert "modeling_spark.py" in downloaded
        assert "merges.txt" not in downloaded
        assert "chat_template.jinja" not in downloaded

    def test_env_allow_suffix_with_always_allow(self, monkeypatch):
        # Config via env vars instead of extra config, as in the sample YAMLs.
        monkeypatch.setattr(envs, "DOWNLOADER_ALLOW_FILE_SUFFIX", ["json"])
        monkeypatch.setattr(envs, "DOWNLOADER_ALWAYS_ALLOW_FILE_SUFFIX", ["py"])
        downloader = make_fake_downloader()
        downloader.download_directory(local_path=Path("/tmp/model"))
        downloaded = set(FakeDownloader.downloaded)
        assert "config.json" in downloaded
        assert "modeling_spark.py" in downloaded
        assert "merges.txt" not in downloaded

    def test_empty_string_env_does_not_match_everything(self, monkeypatch):
        # DOWNLOADER_ALWAYS_ALLOW_FILE_SUFFIX="" parses to [""], and
        # "".endswith("") is True for every file — make sure the empty
        # string is ignored rather than disabling the whole filter.
        monkeypatch.setattr(envs, "DOWNLOADER_ALLOW_FILE_SUFFIX", None)
        monkeypatch.setattr(envs, "DOWNLOADER_ALWAYS_ALLOW_FILE_SUFFIX", [""])
        downloader = make_fake_downloader(allow_file_suffix=["json"])
        downloader.download_directory(local_path=Path("/tmp/model"))
        downloaded = set(FakeDownloader.downloaded)
        assert downloaded == {
            "config.json",
            "generation_config.json",
            "model.safetensors.index.json",
        }


class TestHuggingFaceAllowPatterns:
    def test_no_filter_no_patterns(self):
        downloader = make_hf_downloader(allow_file_suffix=None)
        assert downloader.allow_patterns is None

    def test_patterns_include_always_allow_suffixes(self):
        downloader = make_hf_downloader(allow_file_suffix=["json", "safetensors"])
        assert downloader.allow_patterns is not None
        for pattern in (
            "*.json",
            "*.safetensors",
            "*.py",
            "*.txt",
            "*.jinja",
            "*.model",
            "*.tiktoken",
        ):
            assert pattern in downloader.allow_patterns

    def test_patterns_disabled_when_always_allow_empty(self):
        downloader = make_hf_downloader(
            allow_file_suffix=["json", "safetensors"],
            always_allow_file_suffix=[],
        )
        assert downloader.allow_patterns == ["*.json", "*.safetensors"]

    def test_patterns_no_duplicates(self):
        downloader = make_hf_downloader(
            allow_file_suffix=["json", "py"],
        )
        assert downloader.allow_patterns.count("*.py") == 1

    def test_leading_dot_suffixes_normalized(self):
        # Users may write ".json" instead of "json"; without normalization
        # the HF pattern becomes "*..json" and silently matches nothing.
        downloader = make_hf_downloader(
            allow_file_suffix=[".json", " .safetensors "],
            always_allow_file_suffix=[".py"],
        )
        assert downloader.allow_patterns is not None
        assert "*.json" in downloader.allow_patterns
        assert "*.safetensors" in downloader.allow_patterns
        assert "*.py" in downloader.allow_patterns
        assert not any(".." in p for p in downloader.allow_patterns)

    def test_leading_dot_suffixes_dedup_with_bare(self):
        downloader = make_hf_downloader(
            allow_file_suffix=["json", ".json"],
            always_allow_file_suffix=[],
        )
        assert downloader.allow_patterns == ["*.json"]


class TestLeadingDotNormalizationBase:
    def test_dotted_suffix_filter_keeps_runtime_files(self, monkeypatch):
        # S3/TOS path: dotted suffixes must also go through normalization
        # and keep merging the always-allow defaults.
        monkeypatch.setattr(envs, "DOWNLOADER_ALLOW_FILE_SUFFIX", None)
        downloader = make_fake_downloader(allow_file_suffix=[".json"])
        downloader.download_directory(local_path=Path("/tmp/model"))
        downloaded = set(FakeDownloader.downloaded)
        assert "config.json" in downloaded
        assert "modeling_spark.py" in downloaded
        assert "large_weights.bin" not in downloaded


class TestAlwaysAllowDotBoundaryOnBase:
    """The S3/TOS path matches with a bare ``endswith``. Always-allow suffixes
    must be matched at the dot boundary (``"." + suffix``) so defaults like
    ``py``/``model`` do not over-match ``*.npy`` arrays or extensionless
    ``*_model`` files, which is what the HF ``*.{suffix}`` patterns already
    do. See the review on #3119.
    """

    def test_always_allow_does_not_overmatch_on_s3_path(self, monkeypatch):
        monkeypatch.setattr(envs, "DOWNLOADER_ALLOW_FILE_SUFFIX", None)
        downloader = make_fake_downloader(allow_file_suffix=["json", "safetensors"])
        downloader.download_directory(local_path=Path("/tmp/model"))
        downloaded = set(FakeDownloader.downloaded)
        # dot-boundary matches are kept
        assert "modeling_spark.py" in downloaded
        assert "tokenizer.model" in downloaded
        # over-matches are rejected
        assert "calib/activations.npy" not in downloaded  # "py" vs ".npy"
        assert "pytorch_model" not in downloaded  # "model" vs extensionless

    def test_user_suffixes_keep_bare_endswith(self, monkeypatch):
        # User-specified suffixes keep the historical bare ``endswith``
        # semantics: some users pass whole-name suffixes such as
        # ``tokenizer.json``, and a forced dot boundary would break them.
        monkeypatch.setattr(envs, "DOWNLOADER_ALLOW_FILE_SUFFIX", None)
        downloader = make_fake_downloader(
            allow_file_suffix=["npy"],
            always_allow_file_suffix=[],
        )
        downloader.download_directory(local_path=Path("/tmp/model"))
        assert FakeDownloader.downloaded == ["calib/activations.npy"]

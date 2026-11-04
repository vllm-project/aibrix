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

"""Native Hub cache preparation and conservative, local-only discovery."""

import threading
from pathlib import Path
from typing import List, Optional

from huggingface_hub import constants, scan_cache_dir, snapshot_download
from huggingface_hub.utils import validate_repo_id

from aibrix.downloader.model_cache import model_files

# Hub 0.24.x uses a process-global tqdm lock that races between concurrent
# snapshot_download calls. This guard protects that Python state only; Hub
# file locks still coordinate independent prepare Jobs and engine processes.
_snapshot_lock = threading.Lock()


def default_hf_cache_dir() -> Path:
    # Use the client's own environment-variable precedence, not AIBrix markers.
    return Path(constants.HF_HUB_CACHE)


def prepare_hf_cache(
    model_uri: str, cache_dir: Optional[str] = None, revision: Optional[str] = None
) -> Path:
    repo = model_uri.split("://", 1)[-1]
    validate_repo_id(repo)
    root = Path(cache_dir) if cache_dir is not None else default_hf_cache_dir()
    with _snapshot_lock:
        path = Path(snapshot_download(repo, cache_dir=str(root), revision=revision))
    model_files(path, path.parent.parent)
    return path


def discover_cached_hf_artifacts(root: Path) -> List[str]:
    artifacts = set()
    if not root.is_dir():
        return []
    for repo in scan_cache_dir(root).repos:
        if (
            repo.repo_type != "model"
            or repo.repo_path.resolve().parent != root.resolve()
        ):
            continue
        for revision in repo.revisions:
            if "main" not in revision.refs:
                continue
            try:
                model_files(revision.snapshot_path, repo.repo_path)
            except (OSError, ValueError, TypeError, KeyError, RuntimeError):
                continue
            artifacts.update(
                [repo.repo_id, "hf://" + repo.repo_id, "huggingface://" + repo.repo_id]
            )
    return sorted(artifacts)

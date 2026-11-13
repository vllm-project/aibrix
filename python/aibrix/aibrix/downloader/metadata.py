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

"""Generic download results, registered separately from backend cache files."""

import hashlib
import json
import os
import uuid
from pathlib import Path, PurePosixPath
from typing import TYPE_CHECKING, Any, Dict, List, Optional, Tuple
from urllib.parse import urlsplit

from aibrix import envs
from aibrix.downloader.utils import infer_model_name

if TYPE_CHECKING:
    from aibrix.downloader.base import BaseDownloader, DownloadExtraConfig

MAX_METADATA_BYTES = 2 * 1024 * 1024


def metadata_directory() -> Optional[Path]:
    directory = os.getenv("AIBRIX_MODEL_CACHE_METADATA_DIR")
    return Path(directory).expanduser().absolute() if directory else None


def normalize_source(uri: str) -> Tuple[str, str]:
    if not isinstance(uri, str):
        raise ValueError("Model source must be a string")
    parsed = urlsplit(uri)
    if parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise ValueError(
            "Model source must not contain credentials or query parameters"
        )
    if parsed.scheme in ("", "hf", "huggingface"):
        from huggingface_hub.utils import validate_repo_id

        repo = uri.split("://", 1)[-1]
        validate_repo_id(repo)
        return "huggingface", "huggingface://" + repo
    if (
        parsed.scheme not in {"s3", "tos", "gcs", "gs"}
        or not parsed.netloc
        or not parsed.path.strip("/")
        or ".." in parsed.path.split("/")
    ):
        raise ValueError("Expected an HF repo or S3/TOS/GCS source")
    backend = "gcs" if parsed.scheme == "gs" else parsed.scheme
    return backend, parsed._replace(scheme=backend).geturl().rstrip("/")


def _context(backend: str, config: Optional["DownloadExtraConfig"] = None) -> str:
    def configured(name, default):
        return getattr(config, name, None) or default

    if backend == "huggingface":
        values = [
            configured(
                "hf_endpoint", envs.DOWNLOADER_HF_ENDPOINT or "https://huggingface.co"
            )
        ]
    elif backend == "s3":
        values = [
            configured("endpoint", envs.DOWNLOADER_AWS_ENDPOINT_URL),
            configured("region", envs.DOWNLOADER_AWS_REGION),
        ]
    elif backend == "tos":
        values = [
            configured("endpoint", envs.DOWNLOADER_TOS_ENDPOINT),
            configured("region", envs.DOWNLOADER_TOS_REGION),
            envs.DOWNLOADER_TOS_VERSION,
        ]
    else:
        values = [os.getenv("STORAGE_EMULATOR_HOST")]
    # Source location matters; credentials never belong in published metadata.
    return hashlib.sha256(json.dumps(values).encode()).hexdigest()


def _local_path(root: Path, relative: str) -> Path:
    if (
        not isinstance(relative, str)
        or not relative
        or PurePosixPath(relative).is_absolute()
        or ".." in PurePosixPath(relative).parts
    ):
        raise ValueError("Unsafe cache-relative path")
    path = root / relative
    if not path.resolve().is_relative_to(root.resolve()):
        raise ValueError("Cache path escapes its root")
    return path


def _fingerprint(path: Path) -> List[int]:
    if not path.is_file() or not os.access(path, os.R_OK):
        raise ValueError("Downloaded file is missing or unreadable")
    stat = path.stat()
    return [stat.st_size, stat.st_mtime_ns, stat.st_ino]


def publish_download_metadata(
    downloader: "BaseDownloader",
    model_path: Path,
    cache_root: Path,
    layout: str = "directory",
) -> None:
    directory = metadata_directory()
    if directory is None:
        if downloader.download_extra_config.prepare_cache:
            raise ValueError("--prepare-cache requires AIBRIX_MODEL_CACHE_METADATA_DIR")
        return
    backend, source = normalize_source(downloader.model_uri)
    root, model_path = cache_root.absolute(), model_path.absolute()
    relative = model_path.relative_to(root).as_posix()
    _local_path(root, relative)
    files, cache_hint = downloader._metadata_files(model_path, layout)
    if not files:
        raise ValueError("Downloaded result contains no files")
    fingerprints = {}
    for file in files:
        name = file.relative_to(root).as_posix()
        fingerprints[name] = _fingerprint(_local_path(root, name))
    context = _context(backend, downloader.download_extra_config)
    data = {
        "schema": 1,
        "cache_hint": cache_hint,
        "complete": downloader.effective_allow_file_suffix is None,
        "source": source,
        "backend": backend,
        "layout": layout,
        "relative_path": relative,
        "source_context": context,
        "files": fingerprints,
    }
    encoded = json.dumps(data)
    if len(encoded.encode()) > MAX_METADATA_BYTES:
        raise ValueError("Download metadata exceeds size limit")
    key = hashlib.sha256(
        json.dumps([source, context, layout, relative]).encode()
    ).hexdigest()
    directory.mkdir(parents=True, exist_ok=True)
    temporary = directory / (uuid.uuid4().hex + ".tmp")
    try:
        temporary.write_text(encoded)
        temporary.replace(directory / (key + ".json"))
    finally:
        temporary.unlink(missing_ok=True)


def _registered_artifacts(data: Dict[str, Any]) -> List[str]:
    backend, source = normalize_source(data["source"])
    if (
        data.get("schema") != 1
        or data.get("cache_hint") is not True
        or data.get("complete") is not True
        or data.get("backend") != backend
        or data.get("source_context") != _context(backend)
    ):
        return []
    layout, relative = data["layout"], data["relative_path"]
    if backend == "huggingface":
        # Plain HF local_dir outputs are valid generic results, but the unchanged
        # repo-id loader cannot use them as native Hub snapshots.
        if layout != "hf_hub":
            return []
        from huggingface_hub import constants

        root = Path(constants.HF_HUB_CACHE)
        repo = source.split("://", 1)[1]
        parts = PurePosixPath(relative).parts
        if len(parts) != 3 or parts[:2] != (
            "models--" + repo.replace("/", "--"),
            "snapshots",
        ):
            return []
        aliases = [repo, "hf://" + repo, source]
    else:
        if layout != "directory" or relative != infer_model_name(source):
            return []
        root = Path(envs.DOWNLOADER_LOCAL_DIR)
        aliases = [source, source + "/"]
        if backend == "gcs":
            aliases += [
                source.replace("gcs://", "gs://", 1),
                source.replace("gcs://", "gs://", 1) + "/",
            ]
    model_path = _local_path(root, relative)
    if not model_path.is_dir():
        return []
    files = data["files"]
    if not isinstance(files, dict) or not files:
        return []
    payload_seen = False
    for name, expected in files.items():
        file = _local_path(root, name)
        payload_seen = payload_seen or file.is_relative_to(model_path)
        if _fingerprint(file) != expected:
            return []
    return aliases if payload_seen else []


def discover_download_metadata(directory: Optional[Path] = None) -> List[str]:
    directory = directory if directory is not None else metadata_directory()
    if directory is None or not directory.is_dir():
        return []
    artifacts = set()
    for record in directory.glob("*.json"):
        try:
            if record.is_symlink() or record.stat().st_size > MAX_METADATA_BYTES:
                continue
            data = json.loads(record.read_text())
            if isinstance(data, dict):
                artifacts.update(_registered_artifacts(data))
        except (OSError, ValueError, KeyError, TypeError, RuntimeError):
            continue
    return sorted(artifacts)

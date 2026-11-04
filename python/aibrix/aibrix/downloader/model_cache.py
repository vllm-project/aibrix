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

"""Prepare existing downloader caches; receipts are hints, never load authority."""

import argparse
import hashlib
import json
import os
import uuid
from pathlib import Path, PurePosixPath
from typing import Any, Dict, List, Optional
from urllib.parse import urlsplit

from filelock import FileLock

from aibrix import envs
from aibrix.downloader.utils import infer_model_name, meta_file

OBJECT_SCHEMES = {"s3", "tos", "gcs", "gs"}
MAX_METADATA_BYTES = 2 * 1024 * 1024


def readable_file(path: Path, boundary: Path, *, allow_empty: bool = False) -> bool:
    """Check the consumer's view, including symlink targets and permissions."""
    try:
        return (
            path.resolve().is_relative_to(boundary.resolve())
            and path.is_file()
            and (allow_empty or path.stat().st_size > 0)
            and os.access(path, os.R_OK)
        )
    except (OSError, ValueError, RuntimeError):
        return False


def read_json(path: Path, boundary: Path) -> Dict[str, Any]:
    if not readable_file(path, boundary) or path.stat().st_size > MAX_METADATA_BYTES:
        raise ValueError("Missing or oversized cache metadata")
    data = json.loads(path.read_text())
    if not isinstance(data, dict):
        raise ValueError("Cache metadata must be an object")
    return data


def model_files(directory: Path, boundary: Path) -> List[Path]:
    """Recognize common Transformers layouts without reading weight contents.

    This is a conservative locality hint, not an engine compatibility or
    cryptographic integrity check. Unrecognized layouts get no new hint.
    """
    config = read_json(directory / "config.json", boundary)
    if not isinstance(config.get("model_type"), str) or not config["model_type"]:
        raise ValueError("Missing model type")
    read_json(directory / "tokenizer_config.json", boundary)
    files = [directory / "config.json", directory / "tokenizer_config.json"]
    for names in [
        ["tokenizer.json"],
        ["tokenizer.model"],
        ["spiece.model"],
        ["vocab.json", "merges.txt"],
        ["vocab.txt"],
    ]:
        if all(readable_file(directory / name, boundary) for name in names):
            files.extend(directory / name for name in names)
            break
    else:
        raise ValueError("Missing tokenizer files")
    for filename in ["model.safetensors", "pytorch_model.bin"]:
        index = directory / (filename + ".index.json")
        if index.exists() or index.is_symlink():
            mapping = read_json(index, boundary).get("weight_map")
            if not isinstance(mapping, dict) or not mapping:
                raise ValueError("Invalid weight index")
            shards = set()
            for name in mapping.values():
                if (
                    not isinstance(name, str)
                    or PurePosixPath(name).name != name
                    or not name.endswith((".safetensors", ".bin"))
                ):
                    raise ValueError("Unsafe shard path")
                shards.add(directory / name)
            if not all(readable_file(path, boundary) for path in shards):
                raise ValueError("Missing weight shard")
            return files + [index] + sorted(shards)
        if readable_file(directory / filename, boundary):
            return files + [directory / filename]
    raise ValueError("Missing supported weights")


def object_uri(uri: str) -> str:
    if not isinstance(uri, str):
        raise ValueError("Model URI must be a string")
    parsed = urlsplit(uri)
    if (
        parsed.scheme not in OBJECT_SCHEMES
        or not parsed.netloc
        or parsed.username
        or parsed.password
        or parsed.query
        or parsed.fragment
        or not parsed.path.strip("/")
        or any(part in (".", "..") for part in parsed.path.split("/"))
    ):
        raise ValueError(
            "Expected an object-store model prefix without credentials or traversal"
        )
    return (
        parsed._replace(scheme="gcs" if parsed.scheme == "gs" else parsed.scheme)
        .geturl()
        .rstrip("/")
    )


def source_context(scheme: str) -> str:
    # Endpoints/regions distinguish stores with identical bucket/key names.
    # Never persist authentication material or a possibly credentialed endpoint.
    if scheme == "s3":
        values = [envs.DOWNLOADER_AWS_ENDPOINT_URL, envs.DOWNLOADER_AWS_REGION]
    elif scheme == "tos":
        values = [
            envs.DOWNLOADER_TOS_ENDPOINT,
            envs.DOWNLOADER_TOS_REGION,
            envs.DOWNLOADER_TOS_VERSION,
        ]
    else:
        values = [os.getenv("STORAGE_EMULATOR_HOST")]
    return hashlib.sha256(json.dumps(values).encode()).hexdigest()


def fingerprint(path: Path) -> List[int]:
    stat = path.stat()
    return [stat.st_size, stat.st_mtime_ns, stat.st_ino]


def _receipt_artifacts(receipt: Path, root: Path) -> List[str]:
    data = read_json(receipt, root)
    uri = object_uri(data["uri"])
    scheme = urlsplit(uri).scheme
    if data.get("schema") != 1 or data.get("source_context") != source_context(scheme):
        return []
    name = infer_model_name(uri)
    if data.get("model_dir") != name:
        return []
    directory = root / name
    if directory.resolve().parent != root.resolve():
        return []
    required = model_files(directory, root)
    files = data.get("files")
    if not isinstance(files, dict) or not files:
        return []
    if not all(str(path.relative_to(directory)) in files for path in required):
        return []
    for relative, recorded in files.items():
        if (
            not isinstance(relative, str)
            or PurePosixPath(relative).is_absolute()
            or ".." in PurePosixPath(relative).parts
        ):
            return []
        path = directory / relative
        if (
            not readable_file(path, directory, allow_empty=True)
            or fingerprint(path) != recorded
        ):
            return []
    aliases = [uri, uri + "/"]
    if scheme in ("gs", "gcs"):
        suffix = uri.split("://", 1)[1]
        aliases = [
            prefix + suffix + ending
            for prefix in ("gs://", "gcs://")
            for ending in ("", "/")
        ]
    return aliases


def discover_prepared_artifacts(root: Path) -> List[str]:
    artifacts = set()
    try:
        for receipt in (root / ".aibrix/prepared").glob("*.json"):
            try:
                artifacts.update(_receipt_artifacts(receipt, root))
            except (OSError, ValueError, KeyError, TypeError, RuntimeError):
                continue
    except OSError:
        return []
    return sorted(artifacts)


def prepare_model_cache(
    model_uri: str, cache_dir: Optional[str] = None, revision: Optional[str] = None
) -> Path:
    """Prepare the exact layout used by the unchanged runtime loading path."""
    scheme = urlsplit(model_uri).scheme
    if scheme in ("", "hf", "huggingface"):
        from aibrix.downloader.hf_cache import prepare_hf_cache

        return prepare_hf_cache(model_uri, cache_dir, revision)
    uri = object_uri(model_uri)
    if revision is not None:
        raise ValueError(
            "Object-store versions belong in the model prefix, not --revision"
        )
    from aibrix.downloader import download_model

    root = Path(cache_dir or envs.DOWNLOADER_LOCAL_DIR).expanduser().absolute()
    name = infer_model_name(uri)
    destination = root / name
    if destination.resolve().parent != root.resolve():
        raise ValueError("Model directory must remain inside the cache")
    receipt_dir = root / ".aibrix/prepared"
    if any(
        path.is_symlink()
        for path in [root / ".aibrix", receipt_dir, root / ".aibrix/locks"]
    ):
        raise ValueError("Cache metadata directories must not be symbolic links")
    receipt_dir.mkdir(parents=True, exist_ok=True)
    key = hashlib.sha256(uri.encode()).hexdigest()
    lock_dir = root / ".aibrix/locks"
    lock_dir.mkdir(parents=True, exist_ok=True)
    # Existing downloaders choose the prefix basename. Coordinate preparers by
    # that destination, not URL, and refuse a second source claiming its files.
    with FileLock(
        str(lock_dir / (hashlib.sha256(name.encode()).hexdigest() + ".lock"))
    ):
        for existing in receipt_dir.glob("*.json"):
            try:
                data = read_json(existing, root)
                if (
                    data.get("model_dir") == name
                    and data.get("uri") != uri
                    and _receipt_artifacts(existing, root)
                ):
                    raise FileExistsError(
                        "Different model sources share a destination; use separate cache roots"
                    )
            except (ValueError, KeyError, TypeError):
                continue
        receipt = receipt_dir / (key + ".json")
        receipt.unlink(missing_ok=True)
        path = download_model(model_uri, local_path=str(root))
        model_files(path, root)
        records = {}
        source = "gcs" if scheme == "gs" else scheme
        for file in path.rglob("*"):
            relative = file.relative_to(path)
            if ".cache" in relative.parts or not file.is_file():
                continue
            metadata = meta_file(path, str(relative), source)
            for candidate in (file, metadata):
                if not readable_file(candidate, path, allow_empty=candidate == file):
                    raise ValueError("Incomplete or escaped download")
                records[str(candidate.relative_to(path.absolute()))] = fingerprint(
                    candidate
                )
        data = {
            "schema": 1,
            "uri": uri,
            "source_context": source_context(scheme),
            "model_dir": name,
            "files": records,
        }
        serialized = json.dumps(data)
        if len(serialized.encode()) > MAX_METADATA_BYTES:
            raise ValueError("Preparation receipt exceeds the metadata size limit")
        temporary = receipt_dir / (uuid.uuid4().hex + ".tmp")
        try:
            temporary.write_text(serialized)
            temporary.replace(receipt)
        finally:
            temporary.unlink(missing_ok=True)
        return path


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Prepare HF, S3, TOS or GCS model caches for unchanged ModelClaims"
    )
    parser.add_argument(
        "--model-uri",
        required=True,
        help="HF repo id / hf:// URL, s3://, tos://, gcs:// or gs:// model prefix",
    )
    parser.add_argument(
        "--cache-dir",
        help="Native HF hub cache or object downloader root; defaults to the existing backend configuration",
    )
    parser.add_argument(
        "--revision",
        help="Optional HF reference; omitted uses the existing default reference",
    )
    args = parser.parse_args()
    try:
        path = prepare_model_cache(args.model_uri, args.cache_dir, args.revision)
    except Exception as exc:
        parser.exit(1, f"Model preparation failed: {exc}\n")
    print(str(path))


if __name__ == "__main__":
    main()

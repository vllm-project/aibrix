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
from pathlib import Path
from typing import Union

from aibrix import envs
# Use DOWNLOAD_CACHE_DIR as UPLOAD_CACHE_DIR since it's not defined separately
from aibrix.config import DOWNLOAD_CACHE_DIR as UPLOAD_CACHE_DIR
from aibrix.logger import init_logger
from aibrix.downloader.entity import RemoteSource

logger = init_logger(__name__)


def meta_file(local_path: Union[Path, str], file_name: str, source: str) -> Path:
    """Get the path to the metadata file for a given upload."""
    return (
        Path(local_path)
        .joinpath(UPLOAD_CACHE_DIR % source)
        .joinpath(f"{file_name}.metadata")
        .absolute()
    )


def save_meta_data(file_path: Union[Path, str], etag: str):
    """Save metadata for an uploaded file."""
    Path(file_path).parent.mkdir(parents=True, exist_ok=True)
    with open(file_path, "w") as f:
        f.write(etag)


def load_meta_data(file_path: Union[Path, str]):
    """Load metadata for an uploaded file."""
    if Path(file_path).exists():
        with open(file_path, "r") as f:
            return f.read()
    return None


def check_file_need_upload(
    local_file: Union[Path, str],
    meta_file: Union[Path, str],
    remote_file_exists: bool,
    force_upload: bool = False,
) -> bool:
    """Check if a file needs to be uploaded.

    Args:
        local_file: Path to the local file
        meta_file: Path to the metadata file
        remote_file_exists: Whether the remote file exists
        force_upload: Whether to force upload regardless of existing files

    Returns:
        True if the file needs to be uploaded, False otherwise
    """
    # If force upload is enabled, always upload
    if force_upload:
        return True

    # If the local file doesn't exist, we can't upload it
    if not Path(local_file).exists():
        return False

    # If the remote file doesn't exist, we need to upload
    if not remote_file_exists:
        return True

    # If metadata doesn't exist, we need to upload
    if not Path(meta_file).exists():
        return True

    # If all checks pass, we don't need to upload
    logger.debug(f"File {Path(local_file).name} does not need to be uploaded")
    return False


def need_to_upload(
    local_file: Union[Path, str],
    meta_data_file: Union[Path, str],
    remote_file_exists: bool,
) -> bool:
    """Determine if a file needs to be uploaded based on configuration and file state."""
    _file_name = Path(local_file).name
    force_upload = getattr(envs, "UPLOADER_FORCE_UPLOAD", False)
    
    if check_file_need_upload(local_file, meta_data_file, remote_file_exists, force_upload):
        if force_upload:
            logger.info(f"Forcing upload of file {_file_name}")
        else:
            logger.info(f"File {_file_name} needs to be uploaded")
        return True
    else:
        logger.info(f"File {_file_name} exists remotely and locally, skipping upload.")
        return False


def infer_model_name(uri: str):
    """Infer the model name from a URI."""
    if uri is None or uri == "":
        raise ValueError("Model uri is empty.")

    return uri.strip().strip("/").split("/")[-1]


def parse_s3_uri(uri: str) -> tuple[str, str]:
    """Parse an S3 URI into bucket name and path."""
    from urllib.parse import urlparse
    
    parsed = urlparse(uri)
    if parsed.scheme != "s3":
        raise ValueError(f"Invalid S3 URI: {uri}")
    
    bucket_name = parsed.netloc
    path = parsed.path.lstrip("/")
    
    return bucket_name, path


def ensure_directory_exists(directory: Union[Path, str]):
    """Ensure that a directory exists, creating it if necessary."""
    directory_path = Path(directory)
    if not directory_path.exists():
        directory_path.mkdir(parents=True, exist_ok=True)
        logger.debug(f"Created directory: {directory_path}")
    elif not directory_path.is_dir():
        raise ValueError(f"Path exists but is not a directory: {directory_path}")


def get_file_size(file_path: Union[Path, str]) -> int:
    """Get the size of a file in bytes."""
    return os.path.getsize(file_path)


def get_file_etag(file_path: Union[Path, str]) -> str:
    """Calculate an ETag-like hash for a file."""
    import hashlib
    
    file_path = Path(file_path)
    if not file_path.exists():
        raise ValueError(f"File does not exist: {file_path}")
    
    md5 = hashlib.md5()
    with open(file_path, "rb") as f:
        # Read in chunks to handle large files
        for chunk in iter(lambda: f.read(8 * 1024 * 1024), b""):
            md5.update(chunk)
    
    return md5.hexdigest()


def filter_files_by_suffix(files: list[str], allowed_suffixes: list[str]) -> list[str]:
    """Filter files by their suffixes."""
    if not allowed_suffixes:
        return files
    
    filtered = []
    for file in files:
        if any(file.endswith(suffix) for suffix in allowed_suffixes):
            filtered.append(file)
    
    return filtered
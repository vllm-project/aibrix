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
import contextlib
import tempfile
from pathlib import Path
from typing import Generator, List, Tuple

from filelock import FileLock

from aibrix.config import DOWNLOAD_CACHE_DIR
from aibrix.downloader.entity import (
    DownloadModel,
    FileDownloadStatus,
    ModelDownloadStatus,
    RemoteSource,
    get_local_download_paths,
)


@contextlib.contextmanager
def prepare_model_dir(
    local_path: Path,
    model_name: str,
    source: RemoteSource,
    files_with_status: List[Tuple[str, FileDownloadStatus]],
) -> Generator:
    model_base_dir = local_path.joinpath(model_name)
    cache_sub_dir = (DOWNLOAD_CACHE_DIR % source.value).strip("/")
    model_cache_dir = model_base_dir.joinpath(cache_sub_dir)

    model_base_dir.mkdir(parents=True, exist_ok=True)
    model_cache_dir.mkdir(parents=True, exist_ok=True)

    def _parepare_file(
        model_base_dir: Path,
        cache_dir: Path,
        file_name: str,
        status: FileDownloadStatus,
    ):
        file_path = model_base_dir.joinpath(file_name)
        lock_path = cache_dir.joinpath(f"{file_name}.lock")
        meta_path = cache_dir.joinpath(f"{file_name}.metadata")

        # Nested file names (e.g. "LLM/config.json") need their parent dirs.
        file_path.parent.mkdir(parents=True, exist_ok=True)
        lock_path.parent.mkdir(parents=True, exist_ok=True)
        meta_path.parent.mkdir(parents=True, exist_ok=True)

        if status == FileDownloadStatus.DOWNLOADED:
            file_path.touch()
            lock_path.touch()
            meta_path.touch()
        elif status == FileDownloadStatus.DOWNLOADING:
            lock_path.touch()
        elif status == FileDownloadStatus.NO_OPERATION:
            lock_path.touch()

    # acquire the donwloading lock
    all_locks = []
    for file_name, status in files_with_status:
        _parepare_file(model_base_dir, model_cache_dir, file_name, status)
        if status == FileDownloadStatus.DOWNLOADING:
            lock_path = model_cache_dir.joinpath(f"{file_name}.lock")
            lock = FileLock(lock_path)
            lock.acquire(blocking=False)
            all_locks.append(lock)

    yield

    # release all the locks
    for lock in all_locks:
        lock.release(force=True)


def test_prepare_model_dir():
    with tempfile.TemporaryDirectory() as local_dir:
        local_path = Path(local_dir)
        model_name = "model/name"
        source = RemoteSource.S3
        files_with_status = [
            ("file1", FileDownloadStatus.DOWNLOADED),
            ("file2", FileDownloadStatus.DOWNLOADING),
            ("file3", FileDownloadStatus.NO_OPERATION),
        ]
        with prepare_model_dir(local_path, model_name, source, files_with_status):
            model_base_dir = local_path.joinpath(model_name)
            # file1 and file1.lock, file1.metadata should be exist
            assert len(list(model_base_dir.glob("file1"))) == 1
            assert len(list(model_base_dir.glob("**/file1.lock"))) == 1
            assert len(list(model_base_dir.glob("**/file1.metadata"))) == 1

            # file2.lock should be exist
            assert len(list(model_base_dir.glob("file2"))) == 0
            assert len(list(model_base_dir.glob("**/file2.lock"))) == 1
            assert len(list(model_base_dir.glob("**/file2.metadata"))) == 0

            # file3.lock should be exist
            assert len(list(model_base_dir.glob("file3"))) == 0
            assert len(list(model_base_dir.glob("**/file3.lock"))) == 1
            assert len(list(model_base_dir.glob("**/file2.metadata"))) == 0


def test_get_local_download_paths():
    with tempfile.TemporaryDirectory() as local_dir:
        local_path = Path(local_dir)
        model_name = "model/name"
        source = RemoteSource.S3
        files_with_status = [
            ("file1", FileDownloadStatus.DOWNLOADED),
            ("file2", FileDownloadStatus.DOWNLOADING),
            ("file3", FileDownloadStatus.NO_OPERATION),
        ]
        model_base_dir = local_path.joinpath(model_name)
        with prepare_model_dir(local_path, model_name, source, files_with_status):
            for filename, status in files_with_status:
                download_file = get_local_download_paths(
                    model_base_dir=model_base_dir,
                    filename=filename,
                    source=source,
                )
                assert download_file.status == status, f"{filename} status not match"


def test_infer_from_local_path():
    with tempfile.TemporaryDirectory() as local_dir:
        local_path = Path(local_dir)
        # S3 model with 3 files
        s3_model_name = "s3_model/name"
        s3_source = RemoteSource.S3
        s3_files_with_status = [
            ("s3_file1", FileDownloadStatus.DOWNLOADED),
            ("s3_file2", FileDownloadStatus.DOWNLOADING),
            ("s3_file3", FileDownloadStatus.NO_OPERATION),
        ]
        # TOS model with 2 files
        tos_model_name = "tos_model_name"
        tos_source = RemoteSource.TOS
        tos_files_with_status = [
            ("tos_file1", FileDownloadStatus.DOWNLOADED),
            ("tos_file2", FileDownloadStatus.DOWNLOADED),
        ]
        # HuggingFace with 1 file
        hf_model_name = "hf/model_name"
        hf_source = RemoteSource.HUGGINGFACE
        hf_files_with_status = [
            ("hf_file1", FileDownloadStatus.DOWNLOADED),
        ]
        with (
            prepare_model_dir(
                local_path, s3_model_name, s3_source, s3_files_with_status
            ),
            prepare_model_dir(
                local_path, tos_model_name, tos_source, tos_files_with_status
            ),
            prepare_model_dir(
                local_path, hf_model_name, hf_source, hf_files_with_status
            ),
        ):
            download_models = DownloadModel.infer_from_local_path(local_path)
            assert len(download_models) == 3
            for download_model in download_models:
                if download_model.model_name == s3_model_name:
                    assert download_model.model_source == s3_source
                    assert len(download_model.download_files) == 3
                elif download_model.model_name == tos_model_name:
                    assert download_model.model_source == tos_source
                    assert len(download_model.download_files) == 2
                elif download_model.model_name == hf_model_name:
                    assert download_model.model_source == hf_source
                    assert len(download_model.download_files) == 1
                else:
                    raise ValueError(f"Unknown model name {download_model.model_name}")


def test_download_model_status():
    # All the file with downloaded will be model downloaded
    with tempfile.TemporaryDirectory() as local_dir:
        local_path = Path(local_dir)
        model_name = "model/name"
        source = RemoteSource.S3
        files_with_status = [
            ("file1", FileDownloadStatus.DOWNLOADED),
            ("file2", FileDownloadStatus.DOWNLOADED),
            ("file3", FileDownloadStatus.DOWNLOADED),
        ]
        with prepare_model_dir(local_path, model_name, source, files_with_status):
            download_model = DownloadModel.infer_from_local_path(local_path)[0]
            assert download_model.status == ModelDownloadStatus.DOWNLOADED

    # The model will only be in the NO_OPERATION state
    # if the file status is only in the DOWNLOADED or NO_OPERATION state
    with tempfile.TemporaryDirectory() as local_dir:
        local_path = Path(local_dir)
        model_name = "model/name"
        source = RemoteSource.S3
        files_with_status = [
            ("file1", FileDownloadStatus.DOWNLOADED),
            ("file2", FileDownloadStatus.NO_OPERATION),
            ("file3", FileDownloadStatus.DOWNLOADED),
        ]
        with prepare_model_dir(local_path, model_name, source, files_with_status):
            download_model = DownloadModel.infer_from_local_path(local_path)[0]
            assert download_model.status == ModelDownloadStatus.NO_OPERATION


def test_infer_from_model_path_uses_metadata(tmp_path: Path):
    # Ensure infer_from_model_path picks up files when only .metadata exists
    src = RemoteSource.S3
    model_name = "model"
    model_base_dir = tmp_path.joinpath(model_name)
    cache_sub_dir = (DOWNLOAD_CACHE_DIR % src.value).strip("/")
    cache_dir = model_base_dir.joinpath(cache_sub_dir)
    cache_dir.mkdir(parents=True, exist_ok=True)

    # Create only metadata (no .lock and no actual file) to simulate completed state
    meta_path = cache_dir.joinpath("weights.bin.metadata")
    meta_path.write_text("etag")

    model = DownloadModel.infer_from_model_path(
        local_path=tmp_path, model_name=model_name, source=src
    )
    assert model is not None
    names = {df.file_path.name for df in model.download_files}
    assert "weights.bin" in names


NESTED_DOWNLOADED_FILES = [
    ("config.json", FileDownloadStatus.DOWNLOADED),
    ("LLM/config.json", FileDownloadStatus.DOWNLOADED),
    ("a/b/weights.safetensors", FileDownloadStatus.DOWNLOADED),
]


def test_infer_from_model_path_tracks_nested_files(tmp_path: Path):
    src = RemoteSource.HUGGINGFACE
    model_name = "org/model"
    with prepare_model_dir(tmp_path, model_name, src, NESTED_DOWNLOADED_FILES):
        model = DownloadModel.infer_from_model_path(tmp_path, model_name, src)
        assert model is not None
        model_base_dir = tmp_path.joinpath(model_name)
        expected = {
            model_base_dir.joinpath(name) for name, _ in NESTED_DOWNLOADED_FILES
        }
        assert {df.file_path for df in model.download_files} == expected
        assert len(model.download_files) == len(NESTED_DOWNLOADED_FILES)
        assert model.status == ModelDownloadStatus.DOWNLOADED


def test_nested_file_downloading_marks_model_downloading(tmp_path: Path):
    src = RemoteSource.S3
    model_name = "model"
    files_with_status = [
        ("config.json", FileDownloadStatus.DOWNLOADED),
        ("LLM/config.json", FileDownloadStatus.DOWNLOADED),
        ("a/b/weights.safetensors", FileDownloadStatus.DOWNLOADING),
    ]
    with prepare_model_dir(tmp_path, model_name, src, files_with_status):
        model = DownloadModel.infer_from_model_path(tmp_path, model_name, src)
        assert model is not None
        assert model.status == ModelDownloadStatus.DOWNLOADING


def test_nested_file_missing_data_is_not_downloaded(tmp_path: Path):
    src = RemoteSource.TOS
    model_name = "model"
    with prepare_model_dir(tmp_path, model_name, src, NESTED_DOWNLOADED_FILES):
        model_base_dir = tmp_path.joinpath(model_name)
        model_base_dir.joinpath("LLM/config.json").unlink()
        model = DownloadModel.infer_from_model_path(tmp_path, model_name, src)
        assert model is not None
        assert model.status != ModelDownloadStatus.DOWNLOADED
        assert model.status == ModelDownloadStatus.NO_OPERATION


def test_infer_from_local_path_tracks_nested_files(tmp_path: Path):
    src = RemoteSource.HUGGINGFACE
    model_name = "org/model"
    with prepare_model_dir(tmp_path, model_name, src, NESTED_DOWNLOADED_FILES):
        models = DownloadModel.infer_from_local_path(tmp_path)
        assert len(models) == 1
        model = models[0]
        assert model.model_name == model_name
        assert model.model_source == src
        assert len(model.download_files) == len(NESTED_DOWNLOADED_FILES)
        paths = {
            df.file_path.relative_to(model.model_root_path).as_posix()
            for df in model.download_files
        }
        assert paths == {name for name, _ in NESTED_DOWNLOADED_FILES}


def test_nested_metadata_without_lock_is_tracked(tmp_path: Path):
    src = RemoteSource.HUGGINGFACE
    model_name = "org/model"
    model_base_dir = tmp_path.joinpath(model_name)
    cache_dir = model_base_dir.joinpath((DOWNLOAD_CACHE_DIR % src.value).strip("/"))
    # Built by hand: prepare_model_dir cannot express "metadata without lock".
    data_path = model_base_dir.joinpath("LLM/config.json")
    meta_path = cache_dir.joinpath("LLM/config.json.metadata")
    for path in (data_path, meta_path):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.touch()

    model = DownloadModel.infer_from_model_path(tmp_path, model_name, src)
    assert model is not None
    assert len(model.download_files) == 1
    download_file = model.download_files[0]
    assert download_file.file_path == data_path
    assert download_file.lock_path == cache_dir.joinpath("LLM/config.json.lock")
    assert download_file.metadata_path == meta_path
    assert model.status == ModelDownloadStatus.DOWNLOADED


def test_incidental_cache_entries_are_ignored(tmp_path: Path):
    src = RemoteSource.HUGGINGFACE
    model_name = "org/model"
    files = [
        ("config.json", FileDownloadStatus.DOWNLOADED),
        ("LLM/config.json", FileDownloadStatus.DOWNLOADED),
    ]
    with prepare_model_dir(tmp_path, model_name, src, files):
        model_base_dir = tmp_path.joinpath(model_name)
        cache_dir = model_base_dir.joinpath((DOWNLOAD_CACHE_DIR % src.value).strip("/"))
        # Files huggingface_hub leaves in the cache that are not lock/metadata.
        cache_dir.joinpath("LLM/config.json.0123abcd.incomplete").touch()
        cache_dir.joinpath(".gitignore").touch()
        # Repo folders named like the suffixes yield cache *directories*.
        cache_dir.joinpath("LLM/assets.metadata").mkdir()
        cache_dir.joinpath("LLM/other.lock").mkdir()

        model = DownloadModel.infer_from_model_path(tmp_path, model_name, src)
        assert model is not None
        assert {df.file_path for df in model.download_files} == {
            model_base_dir.joinpath(name) for name, _ in files
        }
        assert model.status == ModelDownloadStatus.DOWNLOADED

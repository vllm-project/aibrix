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

"""Shared helpers for metadata HTTP API tests.

These tests exercise the FastAPI routes in ``aibrix/metadata/api/v1/``
through ``TestClient``, with all storage backed by local in-process
implementations. K8s and kopf are disabled so the suite runs without
any external infrastructure.
"""

import argparse
import sys
import tempfile
from pathlib import Path

import pytest

from aibrix.metadata.app import build_app
from aibrix.metadata.setting import settings
from aibrix.storage import StorageType

ROOT = Path(__file__).resolve().parents[2]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))


@pytest.fixture(autouse=True)
def _isolated_local_storage(monkeypatch):
    """Per-test isolated STORAGE_LOCAL_PATH.

    Without this, multiple tests share the cwd-relative ``.storage``
    directory the storage factory picks by default, so list endpoints
    see leftovers from other tests and assertions on file counts /
    cursor pagination become flaky.

    Autouse keeps every test in this package self-contained — opt-in
    fixtures that forget to reach for tmpdir would silently rejoin the
    shared pool.
    """
    with tempfile.TemporaryDirectory(prefix="aibrix-test-storage-") as tmp:
        monkeypatch.setenv("STORAGE_LOCAL_PATH", tmp)
        yield


def create_test_app(disable_batch_api: bool = False, disable_file_api: bool = False):
    """Build a metadata FastAPI app for in-process TestClient tests.

    Adapted from ``tests/batch/conftest.py:create_test_app``. We keep
    a separate copy here so that ``tests/metadata/`` does not have a
    reverse dependency on ``tests/batch/``. The metadata HTTP layer
    can be exercised entirely against the local storage backend, so
    the k8s / kopf / template-registry plumbing of the batch helper
    is intentionally dropped.
    """
    old_storage = settings.STORAGE_TYPE
    old_metastore = settings.METASTORE_TYPE
    settings.STORAGE_TYPE = StorageType.LOCAL
    settings.METASTORE_TYPE = StorageType.LOCAL
    try:
        app = build_app(
            argparse.Namespace(
                host=None,
                port=8090,
                enable_fastapi_docs=False,
                disable_batch_api=disable_batch_api,
                disable_file_api=disable_file_api,
                enable_k8s_job=False,
                enable_mongo_job=False,
                k8s_job_patch=None,
                dry_run=True,
            )
        )
    finally:
        settings.STORAGE_TYPE = old_storage
        settings.METASTORE_TYPE = old_metastore
    return app

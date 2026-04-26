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

"""Generate batch-worker env vars from BatchProfile.storage and metastore.

Replaces the static k8s_job_{s3,tos,redis}_patch.yaml files. Each
storage backend produces a list of env var entries (with optional
secretKeyRef references) that are injected into the batch-worker
container by the renderer.

Two emitter functions:

- :func:`build_storage_env` reads from per-batch ``BatchProfile.storage``;
  this is the file backend (S3 / TOS / GCS / etc.) that the worker
  reads input.jsonl from and writes output.jsonl to.
- :func:`build_metastore_env` reads from the process-global metastore
  type (``aibrix.batch.storage.batch_metastore.get_metastore_type``);
  this is the kv store the worker uses for transient request-level
  state. It keeps the metastore process-global rather than
  per-profile because its values were hard-coded in the legacy
  ``k8s_job_redis_patch.yaml``.

The output structure matches Kubernetes V1EnvVar dict shape so it
can be appended directly into the container env list.
"""

from typing import Any, Dict, List

from aibrix.batch.template import BatchProfile, StorageBackend
from aibrix.storage import StorageType


def build_storage_env(profile: BatchProfile) -> List[Dict[str, Any]]:
    """Return a list of env var dicts for the batch-worker container.

    The generation rule is per-backend; missing optional fields produce
    no env var (admins can supply via custom serve_args or directly via
    the worker's own configuration loader).

    Args:
        profile: BatchProfile providing storage configuration.

    Returns:
        List of K8s V1EnvVar-shaped dicts.
    """
    backend = profile.spec.storage.backend
    if backend == StorageBackend.S3:
        return _s3_env(profile)
    if backend == StorageBackend.TOS:
        return _tos_env(profile)
    if backend == StorageBackend.MINIO:
        return _minio_env(profile)
    if backend == StorageBackend.GCS:
        return _gcs_env(profile)
    if backend == StorageBackend.LOCAL:
        return _local_env(profile)
    return []


def _s3_env(profile: BatchProfile) -> List[Dict[str, Any]]:
    """Equivalent of the legacy k8s_job_s3_patch.yaml.

    All values come from the secret named in
    profile.spec.storage.credentials_secret_ref. The renderer trusts
    that the secret exists in the same namespace as the Job.
    """
    secret_ref = profile.spec.storage.credentials_secret_ref
    if not secret_ref:
        return []
    keys = ["access-key-id", "secret-access-key", "region", "bucket-name"]
    env_names = [
        "STORAGE_AWS_ACCESS_KEY_ID",
        "STORAGE_AWS_SECRET_ACCESS_KEY",
        "STORAGE_AWS_REGION",
        "STORAGE_AWS_BUCKET",
    ]
    return [
        _secret_env(env_name, secret_ref, key) for env_name, key in zip(env_names, keys)
    ]


def _tos_env(profile: BatchProfile) -> List[Dict[str, Any]]:
    """Equivalent of the legacy k8s_job_tos_patch.yaml."""
    secret_ref = profile.spec.storage.credentials_secret_ref
    if not secret_ref:
        return []
    pairs = [
        ("STORAGE_TOS_ACCESS_KEY", "access-key"),
        ("STORAGE_TOS_SECRET_KEY", "secret-key"),
        ("STORAGE_TOS_ENDPOINT", "endpoint"),
        ("STORAGE_TOS_REGION", "region"),
        ("STORAGE_TOS_BUCKET", "bucket-name"),
    ]
    return [_secret_env(env_name, secret_ref, key) for env_name, key in pairs]


def _minio_env(profile: BatchProfile) -> List[Dict[str, Any]]:
    """MinIO uses the S3 protocol; same env names as S3 plus an explicit endpoint URL.

    Keep it simple: emit the same vars as S3 plus
    STORAGE_AWS_ENDPOINT_URL from the profile's endpoint_url field
    (literal, not from a secret).
    """
    env = _s3_env(profile)
    if profile.spec.storage.endpoint_url:
        env.append(
            {
                "name": "STORAGE_AWS_ENDPOINT_URL",
                "value": profile.spec.storage.endpoint_url,
            }
        )
    return env


def _gcs_env(profile: BatchProfile) -> List[Dict[str, Any]]:
    """GCS via service-account JSON key.

    Conventions: the secret holds a single key 'service-account.json'
    that the worker loads via STORAGE_GCS_SERVICE_ACCOUNT_PATH plus
    STORAGE_GCS_BUCKET. Only emitted when secret_ref is set.
    """
    secret_ref = profile.spec.storage.credentials_secret_ref
    if not secret_ref:
        return []
    return [
        _secret_env("STORAGE_GCS_SERVICE_ACCOUNT", secret_ref, "service-account.json"),
        _secret_env("STORAGE_GCS_BUCKET", secret_ref, "bucket-name"),
    ]


def _local_env(profile: BatchProfile) -> List[Dict[str, Any]]:
    """Local-filesystem backend. Bucket field is interpreted as a directory path.

    No secret needed. Renderer is expected to ensure the path is
    mounted into the worker container by the admin (via custom
    PodSpec extensions or future schema fields).
    """
    return [
        {
            "name": "STORAGE_LOCAL_PATH",
            "value": profile.spec.storage.bucket,
        }
    ]


def _secret_env(name: str, secret_ref: str, key: str) -> Dict[str, Any]:
    return {
        "name": name,
        "valueFrom": {
            "secretKeyRef": {
                "name": secret_ref,
                "key": key,
            }
        },
    }


def build_metastore_env() -> List[Dict[str, Any]]:
    """Return env vars for the worker to reach the metastore.

    Reads the process-global metastore type (set via env vars at
    metadata-service startup) and emits the matching set of worker
    env vars. Mirrors the legacy ``k8s_job_redis_patch.yaml`` behavior:
    when the metastore is Redis, emit ``REDIS_HOST``,
    ``REDIS_PORT``, ``REDIS_DB``. The values are read from the same
    process env vars as the metadata service uses, so worker pods
    reach the same Redis instance.
    """
    # Lazy import to avoid module-load-time side effects in tests
    # that don't exercise the metastore.
    import aibrix.batch.storage.batch_metastore as metastore_module

    try:
        metastore_type = metastore_module.get_metastore_type()
    except Exception:
        # If metastore type can't be resolved, behave as if no metastore
        # patch was applied (legacy code logged a warning and skipped).
        return []

    if metastore_type == StorageType.REDIS:
        return _redis_env()
    return []


def _redis_env() -> List[Dict[str, Any]]:
    """Worker env vars for a Redis metastore.

    Values come from process env (``REDIS_HOST`` / ``REDIS_PORT`` /
    ``REDIS_DB``), with sensible legacy-compatible fallbacks. Worker
    pods inherit the metadata service's view of the Redis instance.
    """
    import os

    return [
        {
            "name": "REDIS_HOST",
            "value": os.environ.get(
                "REDIS_HOST",
                "aibrix-redis-master.aibrix-system.svc.cluster.local",
            ),
        },
        {"name": "REDIS_PORT", "value": os.environ.get("REDIS_PORT", "6379")},
        {"name": "REDIS_DB", "value": os.environ.get("REDIS_DB", "0")},
    ]

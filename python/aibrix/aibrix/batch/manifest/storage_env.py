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

"""Generate batch-worker storage/metastore env from the metadata service's config.

In ``KubernetesJob`` mode the worker self-hosts and does its own storage I/O: it
reads ``input.jsonl`` / writes ``output.jsonl`` against the *same* store the
metadata service uploaded the input to, and records request-level state in the
*same* metastore the metadata service finalizes the output from. So both env
sets mirror the metadata process's own configuration — the per-batch profile is
not consulted.

Worker pods often need a different *address* than the metadata service: when the
metadata service runs off-cluster and reaches storage/redis via a port-forwarded
``localhost`` endpoint, a pod would resolve that to its own loopback.
``WORKER_REDIS_HOST`` / ``WORKER_REDIS_PORT`` and ``WORKER_STORAGE_AWS_ENDPOINT_URL``
override the addresses with in-cluster-reachable ones; credentials / bucket /
region / db are inherited as-is.

The output structure matches the Kubernetes V1EnvVar dict shape so it can be
appended directly into the worker container's env list.
"""

import os
from typing import Any, Dict, List

from aibrix import envs
from aibrix.storage import StorageType


def build_storage_env() -> List[Dict[str, Any]]:
    """Worker file-storage env, mirroring the metadata service's storage.

    The metadata service uploads input files to *its* storage; the worker must
    read/write the same place. Credentials are passed as literal env values (the
    same ones the metadata process holds) — fine for dev; production runs the
    metadata service in-cluster so the addresses already resolve.
    ``WORKER_STORAGE_AWS_ENDPOINT_URL`` overrides the endpoint when the metadata
    service reaches storage via a port-forwarded ``localhost`` address that pods
    can't resolve.
    """
    # Resolve the effective storage type the way the metadata process does.
    storage_type = os.getenv("STORAGE_TYPE")
    if not storage_type or storage_type.lower() == StorageType.AUTO.value:
        storage_type = (
            StorageType.S3.value
            if envs.STORAGE_AWS_ACCESS_KEY_ID and envs.STORAGE_AWS_SECRET_ACCESS_KEY
            else StorageType.LOCAL.value
        )
    env: List[Dict[str, Any]] = [{"name": "STORAGE_TYPE", "value": storage_type}]
    if storage_type == StorageType.S3.value:
        passthrough = {
            "STORAGE_AWS_ACCESS_KEY_ID": envs.STORAGE_AWS_ACCESS_KEY_ID,
            "STORAGE_AWS_SECRET_ACCESS_KEY": envs.STORAGE_AWS_SECRET_ACCESS_KEY,
            "STORAGE_AWS_REGION": envs.STORAGE_AWS_REGION,
            "STORAGE_AWS_BUCKET": envs.STORAGE_AWS_BUCKET,
        }
        for name, val in passthrough.items():
            if val:
                env.append({"name": name, "value": val})
        endpoint = (
            os.getenv("WORKER_STORAGE_AWS_ENDPOINT_URL") or envs.STORAGE_AWS_ENDPOINT_URL
        )
        if endpoint:
            env.append({"name": "STORAGE_AWS_ENDPOINT_URL", "value": endpoint})
    return env


def build_metastore_env() -> List[Dict[str, Any]]:
    """Worker metastore env, mirroring the metadata service's metastore.

    The worker records request-level state the metadata service finalizes the
    output from, so it must share the same metastore. Inherits the
    process-global metastore type; ``WORKER_REDIS_HOST`` overrides the address
    for in-cluster reachability.
    """
    # Lazy import to avoid module-load-time side effects in tests that don't
    # exercise the metastore.
    import aibrix.batch.storage.batch_metastore as metastore_module

    try:
        metastore_type = metastore_module.get_metastore_type()
    except Exception:
        # If metastore type can't be resolved, inject nothing (worker falls
        # back to its own default).
        return []

    if metastore_type == StorageType.REDIS:
        return _redis_env()
    return []


def _redis_env() -> List[Dict[str, Any]]:
    """Worker env vars for a Redis metastore.

    ``WORKER_REDIS_HOST`` / ``WORKER_REDIS_PORT`` override the address injected
    into worker pods; without them, fall back to the metadata service's own
    ``REDIS_HOST`` / ``REDIS_PORT``.

    Production (metadata in-cluster): set only ``REDIS_HOST`` — workers and
    metadata share the same Service DNS name. Dev (metadata on host): set
    ``REDIS_HOST=localhost`` for the metadata process and
    ``WORKER_REDIS_HOST=<service-dns>`` for the workers.
    """
    worker_host = (
        os.getenv("WORKER_REDIS_HOST")
        or envs.STORAGE_REDIS_HOST
        or "aibrix-redis-master.aibrix-system.svc.cluster.local"
    )
    worker_port = os.getenv("WORKER_REDIS_PORT") or str(envs.STORAGE_REDIS_PORT)
    return [
        {"name": "REDIS_HOST", "value": worker_host},
        {"name": "REDIS_PORT", "value": worker_port},
        {"name": "REDIS_DB", "value": str(envs.STORAGE_REDIS_DB)},
        # Password cannot be passed in env; set it via a secret ref.
    ]

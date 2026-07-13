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

"""FastAPI router for model lifecycle endpoints on the aibrix-runtime sidecar.

The ModelClaim controller uses these endpoints to activate/deactivate full
model engine processes and list locally managed runtime models. The import
chain is intentionally light: only protocol models and the runtime
implementation.
"""

import logging

from fastapi import APIRouter
from fastapi.responses import JSONResponse

from aibrix.openapi.protocol import (
    ActivateRuntimeModelRequest,
    ActivateRuntimeModelResponse,
    DeactivateRuntimeModelRequest,
    ListRuntimeModelsResponse,
    RuntimeModelInfo,
)
from aibrix.runtime.model_runtime import get_model_runtime

logger = logging.getLogger(__name__)

model_runtime_router = APIRouter()


# sync def: activation does blocking weight-download I/O; run it in a threadpool.
@model_runtime_router.post("/v1/runtime/models/activate")
def activate_runtime_model(request: ActivateRuntimeModelRequest):
    """Activate a model as its own kvcached-enabled engine process on this pod."""
    try:
        inst = get_model_runtime().activate(
            model_name=request.model_name,
            artifact_url=request.artifact_url,
            engine=request.engine,
            port=request.port,
            ipc_name=request.ipc_name or "",
            engine_config=(
                request.engine_config.model_dump(exclude_none=True)
                if request.engine_config
                else None
            ),
            additional_config=request.additional_config,
        )
    except Exception as exc:  # surface activation failure to the controller
        logger.error(f"failed to activate model {request.model_name}: {exc}")
        return JSONResponse(
            status_code=500,
            content=ActivateRuntimeModelResponse(
                status="error", model_name=request.model_name, message=str(exc)
            ).model_dump(),
        )
    return JSONResponse(
        status_code=200,
        content=ActivateRuntimeModelResponse(
            status="success",
            model_name=inst.model_name,
            port=inst.port,
            ipc_name=inst.ipc_name,
        ).model_dump(),
    )


@model_runtime_router.post("/v1/runtime/models/deactivate")
async def deactivate_runtime_model(request: DeactivateRuntimeModelRequest):
    """Deactivate a model by stopping its engine process."""
    get_model_runtime().deactivate(request.model_name, mode=request.mode)
    return JSONResponse(content={"status": "success"}, status_code=200)


# sync def: instance_ready issues blocking HTTP probes; run it in a threadpool.
@model_runtime_router.get("/v1/runtime/models")
def list_runtime_models():
    """List models managed by this runtime sidecar, with KV accounting read
    live from the model's kvcached /dev/shm MemInfoStruct (zero when the
    segment is absent)."""
    from aibrix.runtime.model_runtime import instance_ready, read_kv_segment

    models = []
    for m in get_model_runtime().list_models():
        seg = read_kv_segment(m.ipc_name)
        total, used, prealloc = seg if seg else (0, 0, 0)
        models.append(
            RuntimeModelInfo(
                model_name=m.model_name,
                port=m.port,
                ipc_name=m.ipc_name,
                phase=m.phase,
                ready=instance_ready(m),
                kv_used_bytes=used + prealloc,
                kv_total_bytes=total,
            )
        )
    return JSONResponse(
        status_code=200,
        content=ListRuntimeModelsResponse(models=models).model_dump(),
    )

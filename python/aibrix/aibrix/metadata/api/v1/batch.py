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

from fastapi import APIRouter, Request
from fastapi.responses import JSONResponse

from aibrix.metadata.logger import init_logger

logger = init_logger(__name__)

router = APIRouter()


@router.post("/")
async def create_batch(request: Request):
    # job_controller: JobManager = request.app.state.job_controller

    # Simply return a 200 status for liveness check
    return JSONResponse(content={"status": "ok"}, status_code=200)


@router.get("/{batch_id}")
async def get_batch(request: Request, batch_id: str):
    # job_controller: JobManager = request.app.state.job_controller
    return JSONResponse(content={"status": "ok"}, status_code=200)


@router.post("/{batch_id}/cancel")
async def cancel_batch(request: Request, batch_id: str):
    # job_controller: JobManager = request.app.state.job_controller
    return JSONResponse(content={"status": "ok"}, status_code=200)


@router.get("/")
async def list_batch(request: Request):
    # job_controller: JobManager = request.app.state.job_controller
    return JSONResponse(content={"status": "ok"}, status_code=200)

# Copyright 2024-2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
#
# Redistribution and use in source and binary forms, with or without
# modification, are permitted provided that the following conditions
# are met:
#  * Redistributions of source code must retain the above copyright
#    notice, this list of conditions and the following disclaimer.
#  * Redistributions in binary form must reproduce the above copyright
#    notice, this list of conditions and the following disclaimer in the
#    documentation and/or other materials provided with the distribution.
#  * Neither the name of NVIDIA CORPORATION nor the names of its
#    contributors may be used to endorse or promote products derived
#    from this software without specific prior written permission.
#
# THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS ``AS IS'' AND ANY
# EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
# IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR
# PURPOSE ARE DISCLAIMED.  IN NO EVENT SHALL THE COPYRIGHT OWNER OR
# CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL,
# EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO,
# PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR
# PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY
# OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
# (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
# OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

from fastapi import APIRouter, Depends, HTTPException

from aibrix.openai_frontend.engine.engine import LLMEngine
from aibrix.openai_frontend.frontend.fastapi.errors import get_engine
from aibrix.openai_frontend.schemas.openai import ListModelsResponse, Model, ObjectType
from aibrix.openai_frontend.utils.utils import StatusCode

router = APIRouter()

OWNED_BY = "Inference Server"


@router.get(
    "/v1/models",
    response_model=ListModelsResponse,
    response_model_exclude_unset=True,
    tags=["Models"],
)
async def list_models(
    engine: LLMEngine = Depends(get_engine),
) -> ListModelsResponse:
    """
    Lists the currently available models, and provides basic information about each one such as the owner and availability.
    """
    return ListModelsResponse(object=ObjectType.list, data=await engine.models())


@router.get("/v1/models/{model_name}", response_model=Model, tags=["Models"])
async def retrieve_model(
    model_name: str, engine: LLMEngine = Depends(get_engine)
) -> Model:
    """
    Retrieves a model instance, providing basic information about the model such as the owner and permissioning.
    """
    for model in await engine.models():
        if model.id == model_name:
            return model

    raise HTTPException(
        status_code=StatusCode.NOT_FOUND, detail=f"Unknown model: {model_name}"
    )

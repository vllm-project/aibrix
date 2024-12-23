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

from http import HTTPStatus
from multiprocessing import Process
from pathlib import Path
from typing import Optional, Union

from aibrix import envs
from aibrix.common.errors import (
    InvalidArgumentError,
    ModelNotFoundError,
)
from aibrix.downloader import download_model
from aibrix.downloader.base import get_downloader
from aibrix.downloader.entity import (
    DownloadModel,
    ModelDownloadStatus,
)
from aibrix.openapi.protocol import (
    DownloadModelRequest,
    ErrorResponse,
    ListModelRequest,
    ListModelResponse,
    ModelStatusCard,
)


class ModelManager:
    @staticmethod
    async def model_download(
        request: DownloadModelRequest,
    ) -> Union[ErrorResponse, ModelStatusCard]:
        model_uri = request.model_uri
        model_name = request.model_name
        local_dir = request.local_dir or envs.DOWNLOADER_LOCAL_DIR
        download_extra_config = request.download_extra_config
        try:
            downloader = get_downloader(
                model_uri, model_name, download_extra_config, False
            )
        except InvalidArgumentError as e:
            return ErrorResponse(
                message=str(e),
                type="InvalidArgumentError",
                code=HTTPStatus.UNPROCESSABLE_ENTITY.value,
            )
        except ModelNotFoundError as e:
            return ErrorResponse(
                message=str(e),
                type="ModelNotFoundError",
                code=HTTPStatus.NOT_FOUND.value,
            )

        except Exception as e:
            return ErrorResponse(
                message=str(e),
                type="InternalError",
                code=HTTPStatus.INTERNAL_SERVER_ERROR.value,
            )

        # Infer the model_name from model_uri and source type
        model_name = downloader.model_name
        local_path = Path(local_dir)
        model = DownloadModel.infer_from_model_path(
            local_path=local_path,
            model_name=model_name,
            source=downloader.source,
        )

        model_path = local_path.joinpath(downloader.model_name_path)
        model_status = model.status if model else ModelDownloadStatus.NOT_EXIST
        if model_status in [
            ModelDownloadStatus.DOWNLOADED,
            ModelDownloadStatus.DOWNLOADING,
        ]:
            return ModelStatusCard(
                model_name=model_name,
                model_root_path=str(model_path),
                model_status=str(model_status),
                source=str(downloader.source),
            )
        else:
            # Start to download the model in background
            Process(
                target=download_model,
                args=(model_uri, local_dir, model_name, download_extra_config),
            ).start()
            return ModelStatusCard(
                model_name=model_name,
                model_root_path=str(model_path),
                model_status=str(ModelDownloadStatus.DOWNLOADING),
                source=str(downloader.source),
            )

    @staticmethod
    async def model_list(
        request: Optional[ListModelRequest],
    ) -> Union[ErrorResponse, ListModelResponse]:
        local_dir = envs.DOWNLOADER_LOCAL_DIR if request is None else request.local_dir
        local_path = Path(local_dir)
        models = DownloadModel.infer_from_local_path(local_path)
        cards = []
        cards = [
            ModelStatusCard(
                model_name=model.model_name,
                model_root_path=str(model.model_root_path),
                model_status=str(model.status),
                source=str(model.model_source),
            )
            for model in models
        ]
        response = ListModelResponse(data=cards)
        return response

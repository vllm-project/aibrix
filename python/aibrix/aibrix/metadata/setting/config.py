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
from typing import Optional

from aibrix.config import AIBrixSettings
from aibrix.storage.types import StorageType


class Settings(AIBrixSettings):
    # --- Application General Settings ---
    PROJECT_NAME: str = "AIBrix Extension API Server"
    PROJECT_VERSION: str = "1.0.0"
    API_V1_STR: str = "/v1"  # Base path for version 1 of your API

    # --- CORS (Cross-Origin Resource Sharing) Settings ---
    # List of origins that are allowed to make requests to your API
    # Example: ["http://localhost:3000", "https://your-frontend-domain.com"]
    # Use ["*"] for development, but specify exact origins in production.
    BACKEND_CORS_ORIGINS: list[str] = ["*"]

    # --- External Service URLs (if any) ---
    EXTERNAL_API_URL: Optional[str] = None  # Example: URL for an external microservice

    # --- File API settings ---
    STORAGE_TYPE: StorageType = StorageType.AUTO
    METASTORE_TYPE: StorageType = StorageType.AUTO
    MAX_FILE_SIZE: int = 100 * 1024 * 1024  # 100 MB in bytes


# Create an instance of the Settings class
# Pydantic will automatically try to load values from environment variables
# or the .env file based on the `model_config` defined above.
settings = Settings()  # type: ignore[call-arg]

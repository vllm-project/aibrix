from pydantic_settings import BaseSettings


class Settings(BaseSettings):
    # AIBrix gateway
    aibrix_gateway_url: str = "http://localhost:8888"
    api_key: str = ""

    # Provider selection per modality (default: openai)
    image_provider: str = "openai"
    audio_provider: str = "openai"
    video_provider: str = "openai"
    chat_provider: str = "openai"

    # Per-modality URL/key overrides (fallback to aibrix_gateway_url / api_key)
    image_api_url: str = ""
    image_api_key: str = ""
    audio_api_url: str = ""
    audio_api_key: str = ""
    video_api_url: str = ""
    video_api_key: str = ""

    # Model filtering — comma-separated allowlists (empty = show all)
    # MODELS_ALLOWLIST applies to all capabilities; per-capability overrides below
    models_allowlist: str = ""
    text_models_allowlist: str = ""
    image_models_allowlist: str = ""
    audio_models_allowlist: str = ""
    video_models_allowlist: str = ""

    # CORS
    cors_origins: str = "http://localhost:5173"

    # Auth
    auth_mode: str = "none"  # "none" or "simple"

    # App
    app_version: str = "0.1.0"

    model_config = {"env_prefix": "", "case_sensitive": False}

    def get_image_url(self) -> str:
        return self.image_api_url or self.aibrix_gateway_url

    def get_image_key(self) -> str:
        return self.image_api_key or self.api_key

    def get_audio_url(self) -> str:
        return self.audio_api_url or self.aibrix_gateway_url

    def get_audio_key(self) -> str:
        return self.audio_api_key or self.api_key

    def get_video_url(self) -> str:
        return self.video_api_url or self.aibrix_gateway_url

    def get_video_key(self) -> str:
        return self.video_api_key or self.api_key

    def get_chat_url(self) -> str:
        return self.aibrix_gateway_url

    def get_chat_key(self) -> str:
        return self.api_key


settings = Settings()

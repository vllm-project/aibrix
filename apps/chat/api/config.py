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

    # Per-service URL/key overrides (all fallback to aibrix_gateway_url / api_key)
    chat_api_url: str = ""
    chat_api_key: str = ""
    asr_api_url: str = ""
    asr_api_key: str = ""
    tts_api_url: str = ""
    tts_api_key: str = ""
    image_api_url: str = ""
    image_api_key: str = ""
    image_edit_api_url: str = ""
    image_edit_api_key: str = ""
    video_api_url: str = ""
    video_api_key: str = ""


    # Default model names per capability
    image_model: str = ""
    image_edit_model: str = ""
    asr_model: str = ""
    tts_model: str = ""
    tts_voice: str = ""  # Name of a supported speaker that can be found in the API response
    video_model: str = ""

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

    def get_chat_url(self) -> str:
        return self.chat_api_url or self.aibrix_gateway_url

    def get_chat_key(self) -> str:
        return self.chat_api_key or self.api_key

    def get_asr_url(self) -> str:
        return self.asr_api_url or self.aibrix_gateway_url

    def get_asr_key(self) -> str:
        return self.asr_api_key or self.api_key

    def get_tts_url(self) -> str:
        return self.tts_api_url or self.aibrix_gateway_url

    def get_tts_key(self) -> str:
        return self.tts_api_key or self.api_key

    def get_image_url(self) -> str:
        return self.image_api_url or self.aibrix_gateway_url

    def get_image_key(self) -> str:
        return self.image_api_key or self.api_key

    def get_image_edit_url(self) -> str:
        return self.image_edit_api_url or self.aibrix_gateway_url

    def get_image_edit_key(self) -> str:
        return self.image_edit_api_key or self.api_key

    def get_video_url(self) -> str:
        return self.video_api_url or self.aibrix_gateway_url

    def get_video_key(self) -> str:
        return self.video_api_key or self.api_key


settings = Settings()

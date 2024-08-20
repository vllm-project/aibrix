from typing import Optional
from ai_runtime.downloader.base import get_downloader


def download_model(model_uri: str, local_path: Optional[str] = None):
    """Download model from model_uri to local_path.

    Args:
        model_uri (str): model uri.
        local_path (str): local path to save model.
    """

    downloader = get_downloader(model_uri)
    return downloader.download_model(local_path)

__all__ = [
    "download_model", "get_downloader"
]

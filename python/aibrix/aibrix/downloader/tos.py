from pathlib import Path
from typing import List

from aibrix.downloader.base import BaseDownloader


class TOSDownloader(BaseDownloader):
    
    def __init__(self, model_uri):
        super().__init__(model_uri)

    def _check_config(self):
        pass
    
    def _is_directory(self) -> bool:
        """Check if model_uri is a directory."""
        return False
    
    def _directory_list(self, path: str) -> List[str]:
        return []
    
    def _support_range_download(self) -> bool:
        return True
    
    def download(self, path: str, local_path: Path, enable_range: bool = True):
        pass

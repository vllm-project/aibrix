
from concurrent.futures import ThreadPoolExecutor, wait
from pathlib import Path
import re
from abc import ABC, abstractmethod
from dataclasses import dataclass, field
import time
from typing import List, Optional, Tuple

from ai_runtime import envs, logger


@dataclass
class BaseDownloader(ABC):
    """Base class for downloader."""
    model_uri: str
    model_name: str
    required_envs: Tuple[str] = field(default_factory=tuple)
    optional_envs: Tuple[str] = field(default_factory=tuple)
    allow_file_suffix: Tuple[str] = field(default_factory=tuple)

    def __post_init__(self):
        # ensure downloader required envs are set
        self._check_config()

    @abstractmethod
    def _check_config(self):
        return NotImplementedError
    
    @abstractmethod
    def _is_directory(self) -> bool:
        """Check if model_uri is a directory."""
        return NotImplementedError
    
    @abstractmethod
    def _directory_list(self, path: str) -> List[str]:
        return NotImplementedError
    
    @abstractmethod
    def _support_range_download(self) -> bool:
        return NotImplementedError
    
    @abstractmethod
    def download(self, path: str, local_path: Path, enable_range: bool = True):
        return NotImplementedError
    
    def download_directory(self, local_path: Path):
        directory_list = self._directory_list(self.model_uri)
        if len(self.allow_file_suffix) == 0:
            logger.info("All files from {self.model_uri} will be downloaded.")
            filtered_files = directory_list
        else:
            filtered_files = [file for file in directory_list 
                              if any(file.endswith(suffix)
                                     for suffix in self.allow_file_suffix)]
        
        if not self._support_range_download():
            # download using multi threads
            st = time.perf_counter()
            num_threads = envs.DOWNLOADER_NUM_THREADS
            logger.info(f"Downloader {self.__class__.__name__} does not support "
                        f"range download, use {num_threads} threads to download.")

            executor = ThreadPoolExecutor(num_threads)
            futures = [executor.submit(self.download, 
                                       path=file, 
                                       local_path=local_path, 
                                       enable_range=False) 
                       for file in filtered_files]
            wait(futures)
            duration = time.perf_counter() - st
            logger.info(f"Downloader {self.__class__.__name__} download "
                        f"{len(filtered_files)} files from {self.model_uri} "
                        f"using {num_threads} threads, "
                        f"duration: {duration:.2f} seconds.")

        else:
            st = time.perf_counter()
            for file in filtered_files:
                # use range download to speedup download
                self.download(file, local_path, True)
            duration = time.perf_counter() - st
            logger.info(f"Downloader {self.__class__.__name__} download "
                        f"{len(filtered_files)} files from {self.model_uri} "
                        f"using range support methods, "
                        f"duration: {duration:.2f} seconds.")

    def download_model(self, local_path: Optional[str]):
        if local_path is None:
            local_path = envs.DOWNLOADER_LOCAL_DIR
            Path(local_path).mkdir(parents=True, exist_ok=True)

        # ensure model local path exists
        model_name = self.model_name.replace("/", "_")
        model_path = Path(local_path).joinpath(model_name)
        model_path.mkdir(parents=True, exist_ok=True)

        # TODO check local file exists

        if self._is_directory():
            self.download_directory(model_path)
        else:
            self.download(self.model_uri, model_path)
        return model_path
        
        

def get_downloader(model_uri: str) -> BaseDownloader:
    """Get downloader for model_uri."""
    if re.match(envs.DOWNLOADER_S3_REGEX, model_uri):
        from ai_runtime.downloader.s3 import S3Downloader
        return S3Downloader(model_uri)
    elif re.match(envs.DOWNLOADER_TOS_REGEX, model_uri):
        from ai_runtime.downloader.tos import TOSDownloader
        return TOSDownloader(model_uri)
    else:
        from ai_runtime.downloader.huggingface import HuggingFaceDownloader
        return HuggingFaceDownloader(model_uri)

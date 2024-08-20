import logging
import sys
from pathlib import Path
from logging import Logger
from logging.handlers import RotatingFileHandler


def _default_logging_basic_config() -> None:
    Path("/tmp/ai_runtime").mkdir(parents=True, exist_ok=True)
    logging.basicConfig(
        format="%(asctime)s - %(filename)s:%(lineno)d - %(funcName)s - %(levelname)s - %(message)s",
        datefmt="%Y-%m-%d %H:%M:%S %Z",
        handlers=[
            logging.StreamHandler(stream=sys.stdout),
            RotatingFileHandler(
                "/tmp/ai_runtime/python.log",
                maxBytes=10 * (2 ** 20),
                backupCount=10,
            ),
        ],
        level=logging.INFO,
    )

def init_logger(name: str) -> Logger:
    return logging.getLogger(name)

_default_logging_basic_config()
logger = init_logger(__name__)

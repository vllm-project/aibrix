# Copyright 2026 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import argparse
import json
from typing import Dict, Optional

from aibrix.downloader import download_model


def str_to_dict(s) -> Optional[Dict]:
    if s is None:
        return None
    try:
        return json.loads(s)
    except Exception as e:
        raise ValueError(f"Invalid json string {s}") from e


def main():
    parser = argparse.ArgumentParser(
        description="Download model from HuggingFace, AWS S3, TOS, or GCS"
    )
    parser.add_argument(
        "--model-uri",
        type=str,
        default="deepseek-ai/deepseek-coder-6.7b-instruct",
        help="model uri from different source, support HuggingFace, AWS S3, TOS, GCS",
    )
    parser.add_argument(
        "--local-dir",
        type=str,
        default=None,
        help="base dir of model files; native Hub cache root for HF --prepare-cache. Defaults to the backend configuration",
    )
    parser.add_argument(
        "--model-name",
        type=str,
        default=None,
        help="subdir of the base dir to save model files",
    )
    parser.add_argument(
        "--enable-progress-bar",
        action="store_true",
        default=False,
        help="Enable download progress bar during downloading from TOS or S3",
    )
    parser.add_argument(
        "--download-extra-config",
        type=str_to_dict,
        default=None,
        help="Extra config for download, like auth config, parallel config, etc.",
    )
    parser.add_argument(
        "--prepare-cache",
        action="store_true",
        help="Prepare loader-compatible cache and require unified download metadata (AIBRIX_MODEL_CACHE_METADATA_DIR)",
    )
    args = parser.parse_args()
    if args.prepare_cache:
        args.download_extra_config = dict(args.download_extra_config or {})
        args.download_extra_config["prepare_cache"] = True
    model_path = download_model(
        args.model_uri,
        args.local_dir,
        args.model_name,
        args.download_extra_config,
        args.enable_progress_bar,
    )
    # Print the resolved model path for easier scripting
    if model_path is not None:
        print(str(model_path))


if __name__ == "__main__":
    main()

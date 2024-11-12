import argparse

from aibrix.downloader import download_model


def main():
    parser = argparse.ArgumentParser(description="Download model from HuggingFace")
    parser.add_argument(
        "--model-uri",
        type=str,
        default="deepseek-ai/deepseek-coder-6.7b-instruct",
        required=True,
        help="model uri from different source, support HuggingFace, AWS S3, TOS",
    )
    parser.add_argument(
        "--local-dir",
        type=str,
        default=None,
        help="base dir of the model file. If not set, it will used with env `DOWNLOADER_LOCAL_DIR`",
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
    args = parser.parse_args()
    download_model(
        args.model_uri, args.local_dir, args.model_name, args.enable_progress_bar
    )


if __name__ == "__main__":
    main()

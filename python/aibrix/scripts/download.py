import argparse
from aibrix.downloader.base import get_downloader


def download_model(args: argparse.Namespace):
    downloader = get_downloader(args.model_uri)
    downloader.download_model(args.local_dir)


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
        help="dir to save model files",
    )
    download_model(parser.parse_args())


if __name__ == "__main__":
    main()

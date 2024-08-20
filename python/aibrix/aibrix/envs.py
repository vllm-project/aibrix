import os

# Model Download Related Config

# Downloader Default Directory
DOWNLOADER_LOCAL_DIR = os.getenv('DOWNLOADER_LOCAL_DIR', '"/tmp/aibrix/ai_runtime/models/"')
DOWNLOADER_NUM_THREADS = int(os.getenv('DOWNLOADER_NUM_THREADS', '3'))

# Downloader Regex
DOWNLOADER_S3_REGEX = r"^s3://"
DOWNLOADER_TOS_REGEX = r"https://(.+?).tos-(.+?).volces.com/(.+)"  # TBD

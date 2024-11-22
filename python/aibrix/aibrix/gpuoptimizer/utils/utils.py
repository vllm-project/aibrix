import logging
from urllib.parse import quote


class ExcludePathsFilter(logging.Filter):
    def __init__(self, exclude_paths):
        super().__init__()
        self.exclude_paths = [quote(exclude_path) for exclude_path in exclude_paths]

    def filter(self, record):
        # Check if the record is an access log and extract the path
        if hasattr(record, 'args') and len(record.args) >= 3:
            request_path = record.args[2]
            return not any(request_path.startswith(path) for path in self.exclude_paths)
        return True  # Allow other log records

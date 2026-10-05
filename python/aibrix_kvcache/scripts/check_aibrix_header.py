# Copyright 2024 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# 	http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# SPDX-License-Identifier: Apache-2.0

# Adapted from vLLM

import os
import sys

AIBRIX_HEADER = """# Copyright 2024 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# 	http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
"""
AIBRIX_HEADER_PREFIX = "# Copyright 2024 The Aibrix Team."


def check_aibrix_header(file_path):
    with open(file_path, encoding="UTF-8") as file:
        lines = file.readlines()
        if not lines:
            # Empty file like __init__.py
            return True
        for line in lines:
            if line.strip().startswith(AIBRIX_HEADER_PREFIX):
                return True
    return False


def add_header(file_path):
    with open(file_path, "r+", encoding="UTF-8") as file:
        lines = file.readlines()
        file.seek(0, 0)
        if lines and lines[0].startswith("#!"):
            file.write(lines[0])
            file.write(AIBRIX_HEADER + "\n")
            file.writelines(lines[1:])
        else:
            file.write(AIBRIX_HEADER + "\n")
            file.writelines(lines)


def main():
    files_with_missing_header = []
    for file_path in sys.argv[1:]:
        if not os.path.exists(file_path):
            continue
        if not check_aibrix_header(file_path):
            files_with_missing_header.append(file_path)

    if files_with_missing_header:
        print("The following files are missing the AIBrix header:")
        for file_path in files_with_missing_header:
            print(f"  {file_path}")
            add_header(file_path)

    sys.exit(1 if files_with_missing_header else 0)


if __name__ == "__main__":
    main()

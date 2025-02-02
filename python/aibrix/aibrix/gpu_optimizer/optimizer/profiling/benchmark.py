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

import os
import subprocess
import sys  # Import sys for command-line arguments


def main():
    script_path = os.path.join(os.path.dirname(__file__), "benchmark.sh")

    # Get command-line arguments *after* the script name
    args = sys.argv[1:]  # Slice to exclude the Python script name itself

    try:
        subprocess.run([script_path] + args, check=True)  # Pass args to the script
    except subprocess.CalledProcessError as e:
        print(f"Error running benchmark script: {e}")
        exit(1)
    except FileNotFoundError:
        print(f"Error: benchmark.sh not found at {script_path}")
        exit(1)
    except Exception as e:
        print(f"A general error occurred: {e}")
        exit(1)


if __name__ == "__main__":
    main()

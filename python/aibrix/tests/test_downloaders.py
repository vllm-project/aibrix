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

import ast
import unittest
from pathlib import Path

DOWNLOADERS_FILE = (
    Path(__file__).resolve().parents[1] / "aibrix" / "runtime" / "downloaders.py"
)


class TestDownloaders(unittest.TestCase):
    def test_downloaders_py_parses(self):
        source = DOWNLOADERS_FILE.read_text(encoding="utf-8")
        ast.parse(source)

    def test_download_is_async_download_sync_is_sync(self):
        source = DOWNLOADERS_FILE.read_text(encoding="utf-8")
        tree = ast.parse(source)

        expected = {
            "S3ArtifactDownloader": {"download": "async", "_download_sync": "sync"},
            "GCSArtifactDownloader": {"download": "async", "_download_sync": "sync"},
            "HuggingFaceArtifactDownloader": {
                "download": "async",
                "_download_sync": "sync",
            },
        }

        classes = {
            node.name: node for node in tree.body if isinstance(node, ast.ClassDef)
        }
        for class_name, func_expectations in expected.items():
            self.assertIn(class_name, classes)
            class_node = classes[class_name]
            funcs = {
                node.name: node
                for node in class_node.body
                if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))
            }

            for func_name, kind in func_expectations.items():
                self.assertIn(func_name, funcs)
                func_node = funcs[func_name]
                if kind == "async":
                    self.assertIsInstance(func_node, ast.AsyncFunctionDef)
                else:
                    self.assertIsInstance(func_node, ast.FunctionDef)
                    self.assertFalse(
                        any(isinstance(n, ast.Await) for n in ast.walk(func_node))
                    )

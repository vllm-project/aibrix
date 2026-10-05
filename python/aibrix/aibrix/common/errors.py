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


from typing import Optional


class InvalidArgumentError(ValueError):
    pass


class ArgNotCongiuredError(InvalidArgumentError):
    def __init__(self, arg_name: str, arg_source: Optional[str] = None):
        self.arg_name = arg_name
        self.message = f"Argument `{arg_name}` is not configured" + (
            f" please check {arg_source}" if arg_source else ""
        )
        super().__init__(self.message)

    def __str__(self):
        return self.message


class ArgNotFormatError(InvalidArgumentError):
    def __init__(self, arg_name: str, expected_format: str):
        self.arg_name = arg_name
        self.message = (
            f"Argument `{arg_name}` is not in the expected format: {expected_format}"
        )
        super().__init__(self.message)

    def __str__(self):
        return self.message


class ModelNotFoundError(Exception):
    def __init__(self, model_uri: str, detail_msg: Optional[str] = None):
        self.model_uri = model_uri
        self.message = f"Model not found at URI: {model_uri}" + (
            f"\nDetails: {detail_msg}" if detail_msg else ""
        )
        super().__init__(self.message)

    def __str__(self):
        return self.message

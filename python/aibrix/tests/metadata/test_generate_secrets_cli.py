# Copyright 2026 The Aibrix Team.
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

import sys
from types import SimpleNamespace
from unittest.mock import Mock

import pytest
from kubernetes import config

from scripts import generate_secrets


@pytest.mark.parametrize("command", ["s3", "tos", "delete", "list"])
@pytest.mark.parametrize(
    "before,after,expected_namespace",
    [
        ([], [], "default"),
        (["--namespace", "team-a"], [], "team-a"),
        (["-n", "team-a"], [], "team-a"),
        ([], ["--namespace", "team-b"], "team-b"),
        ([], ["-n", "team-b"], "team-b"),
        (["--namespace", "team-a"], ["--namespace", "team-b"], "team-b"),
        (["--namespace", "team-a"], ["--namespace", "default"], "default"),
    ],
    ids=[
        "default",
        "global_long",
        "global_short",
        "subcommand_long",
        "subcommand_short",
        "subcommand_override",
        "explicit_default",
    ],
)
def test_namespace_option_position(
    monkeypatch, command, before, after, expected_namespace
):
    generator = Mock()
    generator.create_s3_secret.return_value = "example-secret"
    generator.create_tos_secret.return_value = "example-secret"
    generator.delete_secret.return_value = True
    generator.core_v1.list_namespaced_secret.return_value = SimpleNamespace(items=[])
    factory = Mock(return_value=generator)
    load_config = Mock()
    monkeypatch.setattr(generate_secrets, "SecretGenerator", factory)
    monkeypatch.setattr(config, "load_kube_config", load_config)
    operation = [command]
    if command in ("s3", "tos"):
        operation += ["--bucket", "example-bucket"]
    elif command == "delete":
        operation += ["example-secret"]
    monkeypatch.setattr(
        sys, "argv", ["aibrix_gen_secrets", *before, *operation, *after]
    )

    generate_secrets.main()

    load_config.assert_called_once_with()
    factory.assert_called_once_with(namespace=expected_namespace)
    if command in ("s3", "tos"):
        method = getattr(generator, f"create_{command}_secret")
        method.assert_called_once_with(bucket_name="example-bucket", secret_name=None)
    elif command == "delete":
        generator.delete_secret.assert_called_once_with("example-secret")
    else:
        generator.core_v1.list_namespaced_secret.assert_called_once_with(
            namespace=expected_namespace
        )

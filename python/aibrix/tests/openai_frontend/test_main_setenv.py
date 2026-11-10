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

"""Observable CLI contracts for the shared --setenv argument."""

import os

import pytest

from aibrix.openai_frontend.main import parse_args


@pytest.mark.parametrize(
    "engine_args,engine",
    [
        ([], "vipe"),
        (["--engine", "vipe"], "vipe"),
        (["--engine", "proxy", "--proxy-engine-cmd", "true"], "proxy"),
    ],
)
def test_setenv_is_shared_and_can_follow_engine_arguments(
    monkeypatch, engine_args, engine
):
    monkeypatch.delenv("AIBRIX_TEST_SETENV", raising=False)
    args = parse_args([*engine_args, "--setenv", "aibrix-test-setenv", "on"])
    assert os.environ["AIBRIX_TEST_SETENV"] == "on"
    assert args.engine == engine
    if engine == "proxy":
        assert args.proxy_engine_cmd == ["true"]


def test_setenv_pairs_repeats_and_empty_values(monkeypatch):
    for name in ("AIBRIX_TEST_SETENV", "AIBRIX_TEST_EMPTY"):
        monkeypatch.delenv(name, raising=False)
    parse_args(
        [
            "--setenv",
            "aibrix-test-setenv",
            "first",
            "aibrix-test-empty",
            "",
            "--port",
            "9000",
            "--setenv",
            "AIBRIX_TEST_SETENV",
            "last",
        ]
    )
    assert os.environ["AIBRIX_TEST_SETENV"] == "last"
    assert os.environ["AIBRIX_TEST_EMPTY"] == ""


@pytest.mark.parametrize(
    "command,expected",
    [
        (
            "python -m vllm.entrypoints.openai.api_server",
            ["python", "-m", "vllm.entrypoints.openai.api_server"],
        ),
        (
            'vllm serve "model with spaces" --tensor-parallel-size 2',
            ["vllm", "serve", "model with spaces", "--tensor-parallel-size", "2"],
        ),
    ],
)
def test_quoted_engine_command_preserves_options_and_shared_setenv(
    monkeypatch, command, expected
):
    monkeypatch.delenv("AIBRIX_TEST_SETENV", raising=False)
    args = parse_args(
        [
            "--engine",
            "proxy",
            "--proxy-engine-cmd",
            command,
            "--setenv",
            "aibrix-test-setenv",
            "on",
        ]
    )
    assert args.proxy_engine_cmd == expected
    assert os.environ["AIBRIX_TEST_SETENV"] == "on"


@pytest.mark.parametrize(
    "flag,value",
    [("--proxy-credentials", value) for value in ("{", "[]", '{"https://cdn/*": []}')]
    + [("--proxy-video-output-timeout", value) for value in ("0", "-1", "nan", "inf")],
)
def test_invalid_proxy_configuration_is_argparse_error(flag, value, capsys):
    with pytest.raises(SystemExit) as exc:
        parse_args(["--engine", "proxy", "--proxy-engine-cmd", "true", flag, value])
    assert exc.value.code == 2
    assert flag in capsys.readouterr().err


def test_proxy_video_timeout_and_credentials_arguments():
    import asyncio

    from aibrix.openai_frontend.main import build_proxy_engine

    base = ["--engine", "proxy", "--proxy-engine-cmd", "true", "--no-proxy-fetch-media"]
    defaults = parse_args(base)
    assert defaults.proxy_video_output_timeout == 1800.0
    assert defaults.proxy_credentials == {}
    args = parse_args(
        [
            *base,
            "--proxy-video-output-timeout",
            "120",
            "--proxy-credentials",
            '{"https://cdn/*": {"headers": {"X-Test": "on"}}}',
        ]
    )
    engine = build_proxy_engine(args)
    try:
        assert engine._video_output_timeout == 120.0
        assert engine._credentials_map == args.proxy_credentials
    finally:
        asyncio.run(engine.aclose())


@pytest.mark.parametrize("command", ["", "python 'unterminated"])
def test_invalid_quoted_command_is_argparse_error(command):
    with pytest.raises(SystemExit) as exc:
        parse_args(["--engine", "proxy", "--proxy-engine-cmd", command])
    assert exc.value.code == 2


@pytest.mark.parametrize(
    "argv",
    [
        ["--setenv", "missing-value"],
        ["--setenv", "1bad", "value"],
        ["--setenv", "AIBRIX_TEST_SETENV", "--setenv", "value"],
    ],
)
def test_invalid_setenv_is_argparse_error(argv, capsys):
    with pytest.raises(SystemExit) as exc:
        parse_args(argv)
    assert exc.value.code == 2
    error = capsys.readouterr().err
    assert "unrecognized arguments" not in error
    assert (
        "--setenv expects KEY VALUE pairs" in error
        or "invalid environment variable name" in error
    )

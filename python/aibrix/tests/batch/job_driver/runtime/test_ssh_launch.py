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
import types
from unittest.mock import AsyncMock, Mock, patch

import pytest

from aibrix.batch.job_driver.runtime.runpod import RunPodRuntime
from aibrix.batch.job_driver.runtime.ssh_launch import (
    SSHConnInfo,
    SSHHandle,
    SSHLaunchRuntime,
    build_launch_command,
    parse_conn_info,
)


def test_ssh_launch_runtime_is_abstract():
    """The base must not be usable directly — only RunPod/LambdaCloud."""
    with pytest.raises(TypeError):
        SSHLaunchRuntime()


def _make_job(options: dict):
    """Minimal BatchJob stand-in exposing job.spec.aibrix.runtime.options."""
    runtime = type("R", (), {"options": options})
    aibrix = type("A", (), {"runtime": runtime})
    return type("Job", (), {"spec": type("S", (), {"aibrix": aibrix})})()


def test_parse_conn_info_reads_options():
    opts = {
        "host": "1.2.3.4",
        "ssh_port": 40022,
        "ssh_user": "root",
        "http_base_url": "https://pod123-8000.proxy.runpod.net",
        "model": "Qwen/Qwen2.5-0.5B-Instruct",
        "vllm_args": ["--max-model-len", "4096"],
    }
    info = parse_conn_info(opts)
    assert info.host == "1.2.3.4"
    assert info.ssh_port == 40022
    assert info.http_base_url == "https://pod123-8000.proxy.runpod.net"
    assert info.model == "Qwen/Qwen2.5-0.5B-Instruct"
    assert info.vllm_args == ["--max-model-len", "4096"]


def test_parse_conn_info_requires_host_http_base_url_and_model():
    with pytest.raises(ValueError):
        parse_conn_info({"ssh_port": 22})
    with pytest.raises(ValueError):
        parse_conn_info({"host": "h", "model": "m"})


def test_parse_conn_info_handles_null_optional_values():
    info = parse_conn_info(
        {
            "host": "h",
            "ssh_port": None,
            "http_base_url": "http://h:8000",
            "model": "m",
            "vllm_args": None,
        }
    )
    assert info.ssh_port == 22
    assert info.vllm_args == []


def test_build_launch_command_includes_model_and_port():
    info = SSHConnInfo(
        host="h",
        ssh_port=22,
        ssh_user="root",
        http_base_url="http://h:8000",
        model="Qwen/Qwen2.5-0.5B-Instruct",
        vllm_args=["--max-model-len", "4096"],
    )
    cmd = build_launch_command(info, log_path="/tmp/vllm.log")
    assert "vllm serve Qwen/Qwen2.5-0.5B-Instruct" in cmd
    assert "--port 8000" in cmd
    assert "--max-model-len 4096" in cmd
    assert "/tmp/vllm.log" in cmd
    assert "nohup" in cmd or "setsid" in cmd
    # ~/.local/bin must be on PATH so pip-user vllm (LambdaCloud) is found.
    assert ".local/bin" in cmd


def _handle():
    return SSHHandle(
        info=SSHConnInfo(
            host="h",
            ssh_port=22,
            ssh_user="root",
            http_base_url="http://h:8000",
            model="m",
        )
    )


@pytest.mark.asyncio
async def test_wait_ready_polls_until_200():
    rt = RunPodRuntime(ready_timeout_s=5, poll_interval_s=0.01)
    calls = {"n": 0}

    async def fake_get(url, *a, **k):
        calls["n"] += 1

        class R:
            status_code = 200 if calls["n"] >= 3 else 503

        return R()

    with patch("httpx.AsyncClient.get", new=AsyncMock(side_effect=fake_get)):
        await rt._wait_ready(_handle())
    assert calls["n"] >= 3


@pytest.mark.asyncio
async def test_wait_ready_times_out():
    rt = RunPodRuntime(ready_timeout_s=0.05, poll_interval_s=0.01)

    async def always_503(url, *a, **k):
        class R:
            status_code = 503

        return R()

    with patch("httpx.AsyncClient.get", new=AsyncMock(side_effect=always_503)):
        with pytest.raises(TimeoutError):
            await rt._wait_ready(_handle())


@pytest.mark.asyncio
async def test_provision_connects_and_launches(monkeypatch):
    rt = RunPodRuntime()
    fake_conn = Mock()
    fake_conn.run = AsyncMock()

    monkeypatch.setattr(rt, "_connect_ssh", AsyncMock(return_value=fake_conn))

    class Spec:
        class aibrix:
            class runtime:
                options = {
                    "host": "1.2.3.4",
                    "ssh_port": 40022,
                    "ssh_user": "root",
                    "http_base_url": "http://1.2.3.4:8000",
                    "model": "m",
                }

    class Job:
        spec = Spec()

    handle = await rt._provision(Job(), "job_1")
    assert handle.info.host == "1.2.3.4"
    fake_conn.run.assert_awaited_once()
    assert fake_conn.run.await_args.kwargs["check"] is True
    await rt._teardown(handle)
    fake_conn.close.assert_called()


@pytest.mark.asyncio
async def test_provision_retries_ssh_connect(monkeypatch):
    rt = RunPodRuntime(ssh_connect_timeout_s=1, ssh_connect_retry_interval_s=0.01)
    fake_conn = Mock()
    fake_conn.run = AsyncMock()
    connect = AsyncMock(side_effect=[OSError("not up yet"), fake_conn])
    monkeypatch.setitem(
        sys.modules,
        "asyncssh",
        types.SimpleNamespace(connect=connect, Error=Exception),
    )

    class Spec:
        class aibrix:
            class runtime:
                options = {
                    "host": "1.2.3.4",
                    "ssh_port": 40022,
                    "ssh_user": "root",
                    "http_base_url": "http://1.2.3.4:8000",
                    "model": "m",
                }

    class Job:
        spec = Spec()

    handle = await rt._provision(Job(), "job_1")
    assert handle.conn is fake_conn
    assert connect.await_count == 2
    assert connect.await_args.kwargs["timeout"] == 15.0


def test_runpod_runtime_is_ssh_launch():
    import aibrix.batch.job_driver.runtime.runpod  # noqa: F401
    from aibrix.batch.job_driver.runtime import create_runtime

    rt = create_runtime("RunPod", job=None, context=None, entity_manager=None)
    assert isinstance(rt, SSHLaunchRuntime)
    assert rt.provisions is True


# --- LambdaCloud bare-VM bootstrap ------------------------------------------


def _lambda_job(options_override=None):
    opts = {
        "host": "5.6.7.8",
        "ssh_port": 22,
        "http_base_url": "http://5.6.7.8:8000",
        "model": "Qwen/Qwen2.5-0.5B-Instruct",
    }
    if options_override:
        opts.update(options_override)
    return _make_job(opts)


def test_lambda_runtime_is_ssh_launch():
    import aibrix.batch.job_driver.runtime.lambda_cloud  # noqa: F401
    from aibrix.batch.job_driver.runtime import create_runtime

    rt = create_runtime("LambdaCloud", job=None, context=None, entity_manager=None)
    assert isinstance(rt, SSHLaunchRuntime)
    assert rt.provisions is True


def test_runpod_bootstrap_is_empty():
    # RunPod's vLLM Docker image has vLLM preinstalled: no bootstrap.
    rt = RunPodRuntime()
    info = SSHConnInfo(
        host="h",
        ssh_port=22,
        ssh_user="root",
        http_base_url="http://h:8000",
        model="m",
    )
    assert rt._bootstrap_commands(info) == []


def test_lambda_bootstrap_checks_docker():
    from aibrix.batch.job_driver.runtime.lambda_cloud import LambdaCloudRuntime

    rt = LambdaCloudRuntime()
    info = SSHConnInfo(
        host="h",
        ssh_port=22,
        ssh_user="ubuntu",
        http_base_url="http://h:8000",
        model="m",
    )
    cmds = rt._bootstrap_commands(info)
    assert len(cmds) == 1
    # Container approach: verify docker is usable; no pip install of vLLM.
    assert "docker info" in cmds[0]
    assert "pip install" not in cmds[0]


def test_lambda_launch_uses_docker_run_with_image_and_model():
    from aibrix.batch.job_driver.runtime.lambda_cloud import (
        CONTAINER_NAME,
        LambdaCloudRuntime,
    )

    rt = LambdaCloudRuntime()
    info = SSHConnInfo(
        host="h",
        ssh_port=22,
        ssh_user="ubuntu",
        http_base_url="http://h:8000",
        model="Qwen/Qwen2.5-0.5B-Instruct",
        image="vllm/vllm-openai:v0.8.5",
    )
    cmd = rt._launch_command(info)
    assert "docker run -d" in cmd
    assert "--gpus all" in cmd
    assert "127.0.0.1:8000:8000" in cmd  # bound to VM localhost (SSH tunnel)
    assert "vllm/vllm-openai:v0.8.5" in cmd  # template image
    assert "--model Qwen/Qwen2.5-0.5B-Instruct" in cmd
    assert f"--name {CONTAINER_NAME}" in cmd
    assert "pip install" not in cmd
    # teardown removes the container, not pkill.
    assert "docker rm -f" in rt._teardown_command()


@pytest.mark.asyncio
async def test_lambda_provision_checks_docker_before_run(monkeypatch):
    """LambdaCloud must verify docker (bootstrap) BEFORE the docker run launch."""
    from aibrix.batch.job_driver.runtime.lambda_cloud import LambdaCloudRuntime

    rt = LambdaCloudRuntime()
    issued: list[str] = []
    fake_conn = Mock()

    async def record_run(cmd, *a, **k):
        issued.append((cmd, k))

    fake_conn.run = AsyncMock(side_effect=record_run)
    # Lambda opens an SSH local port-forward for the data plane.
    fake_listener = Mock(get_port=Mock(return_value=54321))
    fake_conn.forward_local_port = AsyncMock(return_value=fake_listener)
    monkeypatch.setattr(rt, "_connect_ssh", AsyncMock(return_value=fake_conn))

    handle = await rt._provision(_lambda_job(), "job_lambda")

    # ssh_user defaults to ubuntu for the bare Lambda VM.
    assert handle.info.ssh_user == "ubuntu"
    # Two commands: docker precondition check, then docker run.
    assert len(issued) == 2
    bootstrap_cmd, bootstrap_kwargs = issued[0]
    launch_cmd, launch_kwargs = issued[1]
    assert "docker info" in bootstrap_cmd
    assert "docker run -d" in launch_cmd
    assert bootstrap_kwargs["check"] is True
    assert launch_kwargs["check"] is True
    # Ordering: the precondition check precedes the run.
    assert "docker run" not in bootstrap_cmd
    # Data plane goes through the SSH tunnel (local forward), not the public IP.
    fake_conn.forward_local_port.assert_awaited_once()
    assert handle.endpoint_url == "http://127.0.0.1:54321"
    assert handle.listener is fake_listener


@pytest.mark.asyncio
async def test_runpod_provision_no_tunnel(monkeypatch):
    """RunPod dispatches directly to its proxy URL — no SSH tunnel opened."""
    from aibrix.batch.job_driver.runtime.runpod import RunPodRuntime

    rt = RunPodRuntime()
    fake_conn = Mock()
    fake_conn.run = AsyncMock()
    fake_conn.forward_local_port = AsyncMock()
    monkeypatch.setattr(rt, "_connect_ssh", AsyncMock(return_value=fake_conn))

    job = _make_job(
        {
            "host": "1.2.3.4",
            "ssh_port": 22,
            "ssh_user": "root",
            "http_base_url": "https://pod-8000.proxy.runpod.net",
            "model": "m",
        }
    )
    handle = await rt._provision(job, "job_rp")
    fake_conn.forward_local_port.assert_not_awaited()
    assert handle.endpoint_url == "https://pod-8000.proxy.runpod.net"
    assert handle.listener is None


@pytest.mark.asyncio
async def test_runpod_provision_skips_bootstrap(monkeypatch):
    """RunPod must NOT issue any bootstrap command — only `vllm serve`."""
    from aibrix.batch.job_driver.runtime.runpod import RunPodRuntime

    rt = RunPodRuntime()
    issued: list[str] = []
    fake_conn = Mock()

    async def record_run(cmd, *a, **k):
        issued.append((cmd, k))

    fake_conn.run = AsyncMock(side_effect=record_run)
    monkeypatch.setattr(rt, "_connect_ssh", AsyncMock(return_value=fake_conn))

    await rt._provision(_lambda_job({"ssh_user": "root"}), "job_runpod")

    assert len(issued) == 1
    cmd, kwargs = issued[0]
    assert "vllm serve" in cmd
    assert "pip install" not in cmd
    assert kwargs["check"] is True


def test_parse_conn_info_default_ssh_user_override():
    # When options omit ssh_user, the runtime-provided default is used.
    info = parse_conn_info(
        {"host": "h", "http_base_url": "http://h:8000", "model": "m"},
        default_ssh_user="ubuntu",
    )
    assert info.ssh_user == "ubuntu"
    # An explicit option still wins over the default.
    info2 = parse_conn_info(
        {
            "host": "h",
            "http_base_url": "http://h:8000",
            "model": "m",
            "ssh_user": "root",
        },
        default_ssh_user="ubuntu",
    )
    assert info2.ssh_user == "root"

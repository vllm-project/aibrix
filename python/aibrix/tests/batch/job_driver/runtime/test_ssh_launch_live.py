# Copyright 2026 The Aibrix Team.
# Licensed under the Apache License, Version 2.0 (see repo LICENSE).
"""Live SSH-launch test against a REAL, already-provisioned RunPod box.

Provision the box first (Go manual test with RM_MANUAL_KEEP=1), then run this
with the box's connection info. Gated behind AIBRIX_LIVE_SSH=1 so it never runs
in CI. Env:
  AIBRIX_SSH_HOST, AIBRIX_SSH_PORT, AIBRIX_SSH_USER, AIBRIX_HTTP_BASE_URL,
  AIBRIX_BATCH_SSH_KEY_FILE, AIBRIX_MODEL (default Qwen/Qwen2.5-0.5B-Instruct)

The box itself is released by the operator / RM, not by this test.
"""

import os

import httpx
import pytest

pytestmark = pytest.mark.skipif(
    os.environ.get("AIBRIX_LIVE_SSH") != "1", reason="AIBRIX_LIVE_SSH != 1"
)


def _job():
    options = {
        "host": os.environ["AIBRIX_SSH_HOST"],
        "ssh_port": int(os.environ.get("AIBRIX_SSH_PORT", "22")),
        "ssh_user": os.environ.get("AIBRIX_SSH_USER", "root"),
        "http_base_url": os.environ["AIBRIX_HTTP_BASE_URL"],
        "model": os.environ.get("AIBRIX_MODEL", "Qwen/Qwen2.5-0.5B-Instruct"),
    }
    runtime = type("R", (), {"options": options})
    aibrix = type("A", (), {"runtime": runtime})
    return type("Job", (), {"spec": type("S", (), {"aibrix": aibrix})})()


@pytest.mark.asyncio
async def test_live_launch_wait_and_dispatch():
    from aibrix.batch.client import DispatchEngine, InferenceRequest
    from aibrix.batch.job_driver.runtime.runpod import RunPodRuntime

    rt = RunPodRuntime()
    handle = await rt._provision(_job(), "live_1")
    try:
        # Phase 5: SSH-launched vLLM becomes ready and serves the model.
        await rt._wait_ready(handle)
        async with httpx.AsyncClient(timeout=30) as c:
            r = await c.get(handle.info.http_base_url.rstrip("/") + "/v1/models")
        assert r.status_code == 200, r.text
        assert handle.info.model in r.text

        # Phase 6: dispatch a small batch through the same engine the driver uses.
        endpoint = await rt._connect(handle)
        engine = DispatchEngine(endpoint.source)
        prompts = [
            "Say hello.",
            "What is 2+2?",
            "Name a color.",
            "Translate 'cat' to French.",
            "Finish: the sky is",
        ]
        ok = 0
        for i, p in enumerate(prompts):
            resp = await engine.send_one(
                InferenceRequest(
                    path="/v1/chat/completions",
                    payload={
                        "model": handle.info.model,
                        "messages": [{"role": "user", "content": p}],
                        "max_tokens": 16,
                    },
                    ref=i,
                )
            )
            content = resp["choices"][0]["message"]["content"]
            assert content, f"empty completion for prompt {i}"
            ok += 1
        assert ok == len(prompts)
    finally:
        await rt._teardown(handle)

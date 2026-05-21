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

"""Smoke test for AIBrix's OpenAI-compatible Batch API.

Drives the full batch flow through the official ``openai`` Python SDK:

    upload input file → create batch → poll → download output →
    verify per-record custom_id round-trip → cleanup

A passing run also confirms wire compatibility, since every response
must deserialize through the SDK's pydantic models.

================================================================
Usage
================================================================

# Prerequisite: metadata service running at http://localhost:8090.
# For standalone mode (no real engine), see "What completes" below.

# Default (10 records, /v1/chat/completions):
poetry run python scripts/batch_api_smoke.py

# Larger / different endpoint:
poetry run python scripts/batch_api_smoke.py --num-records 50 --endpoint /v1/completions

# Reproducible payload:
poetry run python scripts/batch_api_smoke.py --seed 42

# Test the cancel path (skip output verification):
poetry run python scripts/batch_api_smoke.py --cancel

# Keep input/output files for inspection (skip final delete):
poetry run python scripts/batch_api_smoke.py --keep

# Patient mode for a real engine (default times out at 90s):
poetry run python scripts/batch_api_smoke.py --timeout 600 --poll-interval 5

# K8s deployment mode:
poetry run python scripts/batch_api_smoke.py --resource-type deployment -timeout 600 --poll-interval 5

================================================================
What completes
================================================================

* Standalone metadata + no INFERENCE_ENGINE_ENDPOINT: the BatchDriver
  uses the echo client; batches reach 'completed' in seconds and the
  output mirrors input bodies. Good for shape verification.
* Standalone metadata + INFERENCE_ENGINE_ENDPOINT set: BatchDriver
  proxies to the real engine; raise --timeout if the model is slow.
* K8s job mode: kopf spawns worker pods. This script doesn't care about
  the path — it only watches batch status — but completion depends on
  pod scheduling, image pull, and the engine. Use --timeout 600+.
* K8s deployment mode: job specific driver spawns long-running deployment pods,
  and drive job progressing. Set "--resource-type deployment" to enable.
  This script watches batch status — but completion depends on
  pod scheduling, image pull, and the engine. Use --timeout 600+.

================================================================
Exit codes
================================================================

0  All steps passed (including --cancel reaching a cancelled state).
1  Any step failed: validation rejected, batch reached failed/expired,
   poll timed out, output content mismatch, etc.
"""

from __future__ import annotations

import argparse
import io
import json
import random
import sys
import time
from typing import Iterable

from openai import APIStatusError, OpenAI

PROMPT_BANK: list[str] = [
    "Summarize the plot of Hamlet in two sentences.",
    "Explain backpropagation to a high-school student.",
    "Translate 'good morning' into Japanese, French, and Swahili.",
    "Write a haiku about a thunderstorm at midnight.",
    "List three signs that a sourdough starter is healthy.",
    "Compare REST and gRPC for streaming workloads.",
    "Suggest a vegetarian dinner that takes under 20 minutes.",
    "Outline the difference between TCP and QUIC at a high level.",
    "Give me a one-line bash command to find the largest file in a tree.",
    "Describe the taste of an under-ripe persimmon.",
    "What is the lambda calculus, in one paragraph?",
    "Recommend a debugging strategy for a flaky integration test.",
]

MODEL_BANK: list[str] = [
    "gpt-3.5-turbo",
    "gpt-4o-mini",
    "claude-3-5-haiku",
]

TERMINAL_OK = {"completed"}
TERMINAL_FAIL = {"failed", "expired"}
CANCELLED_STATES = {"cancelling", "cancelled"}


def build_record(idx: int, endpoint: str, rng: random.Random) -> dict:
    body: dict
    model = rng.choice(MODEL_BANK)
    prompt = rng.choice(PROMPT_BANK)
    max_tokens = rng.choice([64, 128, 256])

    if endpoint == "/v1/chat/completions":
        body = {
            "model": model,
            "messages": [{"role": "user", "content": prompt}],
            "max_tokens": max_tokens,
        }
    elif endpoint == "/v1/completions":
        body = {"model": model, "prompt": prompt, "max_tokens": max_tokens}
    elif endpoint == "/v1/embeddings":
        body = {"model": "text-embedding-ada-002", "input": prompt}
    elif endpoint == "/v1/rerank":
        body = {
            "model": "reranker-v1",
            "query": prompt,
            "documents": rng.sample(PROMPT_BANK, 3),
        }
    else:
        raise ValueError(f"Unsupported endpoint for record builder: {endpoint}")

    return {
        "custom_id": f"req-{idx:04d}",
        "method": "POST",
        "url": endpoint,
        "body": body,
    }


def generate_records(num_records: int, endpoint: str, rng: random.Random) -> list[dict]:
    records = [build_record(i, endpoint, rng) for i in range(num_records)]
    rng.shuffle(records)
    return records


def to_jsonl_bytes(records: Iterable[dict]) -> bytes:
    return ("\n".join(json.dumps(r) for r in records) + "\n").encode("utf-8")


def step(label: str) -> None:
    print(f"\n── {label} ".ljust(72, "─"), flush=True)


def dump(label: str, payload: object) -> None:
    """Print a labelled JSON-ish view of an SDK object.

    Mirrors ``file_api_smoke.dump`` so failures show the full server
    response without scrolling. Pydantic models flow through
    ``model_dump``; everything else falls back to ``json.dumps`` with
    ``default=str`` to handle datetimes / enums.
    """
    if hasattr(payload, "model_dump"):
        body = payload.model_dump()  # type: ignore[union-attr]
    else:
        body = payload
    print(f"  {label}:")
    text = json.dumps(body, indent=2, ensure_ascii=False, default=str)
    for line in text.splitlines():
        print(f"    {line}")


def fail(msg: str) -> None:
    print(f"FAIL: {msg}", file=sys.stderr)
    sys.exit(1)


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.split("\n\n")[0],
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--base-url", default="http://localhost:8090/v1")
    p.add_argument("--api-key", default="dummy")
    p.add_argument(
        "--num-records",
        type=int,
        default=10,
        help="Number of JSONL input records. Default: %(default)s",
    )
    p.add_argument("--seed", type=int, default=None)
    p.add_argument(
        "--endpoint",
        default="/v1/chat/completions",
        choices=[
            "/v1/chat/completions",
            "/v1/completions",
            "/v1/embeddings",
            "/v1/rerank",
        ],
        help="Batch endpoint. Default: %(default)s",
    )
    p.add_argument(
        "--completion-window",
        default="24h",
        help=(
            "Pass-through completion window. AIBrix only accepts '24h'; "
            "use other values to verify enum rejection. Default: %(default)s"
        ),
    )
    p.add_argument(
        "--poll-interval",
        type=float,
        default=2.0,
        help="Seconds between status polls. Default: %(default)s",
    )
    p.add_argument(
        "--timeout",
        type=float,
        default=90.0,
        help="Total seconds to wait for a terminal state. Default: %(default)s",
    )
    p.add_argument(
        "--resource-type",
        default=None,
        help=(
            "Deployment type specified to run the job via "
            "extra_body.aibrix.planner_decisions.resource_details[].resource_type. "
            "Required if --disable-inference-engine and not --enable-k8s-job "
            "mode (no built-in fallback); ignored by standalone --dry-run."
        ),
    )
    p.add_argument(
        "--aibrix-template",
        default=None,
        help=(
            "AIBrix ModelDeploymentTemplate name to attach via "
            "extra_body.aibrix.model_template. Required in --enable-k8s-job "
            "mode (no built-in fallback); ignored by standalone --dry-run."
        ),
    )
    p.add_argument(
        "--aibrix-profile",
        default=None,
        help=(
            "AIBrix BatchProfile name to attach via "
            "extra_body.aibrix.profile. Falls back to the registry default "
            "if omitted. Has no effect in standalone mode."
        ),
    )
    p.add_argument(
        "--cancel",
        action="store_true",
        help=(
            "After creating the batch, immediately cancel and verify a "
            "cancelled/cancelling state instead of polling for completion."
        ),
    )
    p.add_argument(
        "--keep",
        action="store_true",
        help="Skip the final delete of input/output files.",
    )
    return p.parse_args()


def poll_until_terminal(
    client: OpenAI, batch_id: str, *, poll_interval: float, timeout: float
):
    deadline = time.monotonic() + timeout
    last_status = ""
    while time.monotonic() < deadline:
        batch = client.batches.retrieve(batch_id)
        if batch.status != last_status:
            counts = batch.request_counts
            counts_str = (
                f"  counts: total={counts.total} "
                f"completed={counts.completed} failed={counts.failed}"
                if counts is not None
                else ""
            )
            print(f"  status={batch.status}{counts_str}")
            last_status = batch.status
        if batch.status in TERMINAL_OK | TERMINAL_FAIL:
            return batch
        time.sleep(poll_interval)
    fail(
        f"timed out after {timeout:.0f}s waiting for terminal state "
        f"(last status: {last_status or 'unknown'})"
    )


def verify_output(output_bytes: bytes, expected_custom_ids: set[str]) -> None:
    lines = [line for line in output_bytes.decode("utf-8").splitlines() if line.strip()]
    if len(lines) != len(expected_custom_ids):
        fail(
            f"output line count {len(lines)} != input count "
            f"{len(expected_custom_ids)}"
        )
    seen: set[str] = set()
    for i, line in enumerate(lines, start=1):
        try:
            obj = json.loads(line)
        except json.JSONDecodeError as e:
            fail(f"line {i}: invalid JSON ({e})")
        for field in ("id", "custom_id", "response"):
            if field not in obj:
                fail(f"line {i}: missing top-level field '{field}'")
        cid = obj["custom_id"]
        if cid in seen:
            fail(f"line {i}: duplicate custom_id '{cid}'")
        if cid not in expected_custom_ids:
            fail(f"line {i}: unexpected custom_id '{cid}'")
        seen.add(cid)
    missing = expected_custom_ids - seen
    if missing:
        fail(f"output missing {len(missing)} custom_ids, e.g. {sorted(missing)[:3]}")


def main() -> None:
    args = parse_args()
    rng = random.Random(args.seed)
    client = OpenAI(base_url=args.base_url, api_key=args.api_key)

    step(
        f"Generate {args.num_records} shuffled records (endpoint={args.endpoint}, "
        f"seed={args.seed})"
    )
    records = generate_records(args.num_records, args.endpoint, rng)
    payload = to_jsonl_bytes(records)
    expected_custom_ids = {r["custom_id"] for r in records}
    print(
        f"  payload size: {len(payload)} bytes; first custom_id: {records[0]['custom_id']}"
    )

    step("Upload  POST /v1/files  purpose=batch")
    try:
        input_file = client.files.create(
            file=("batch-input.jsonl", io.BytesIO(payload), "application/jsonl"),
            purpose="batch",
        )
    except APIStatusError as e:
        fail(f"upload rejected: {e.status_code} {e.response.text}")
    dump("upload response", input_file)

    step(
        f"Create  POST /v1/batches  endpoint={args.endpoint} "
        f"completion_window={args.completion_window}"
        + (f"  template={args.aibrix_template}" if args.aibrix_template else "")
        + (f"  profile={args.aibrix_profile}" if args.aibrix_profile else "")
    )

    # Build extra_body.aibrix only if the user opted into K8s-mode
    # selectors. Standalone --dry-run servers ignore this block; K8s
    # servers reject batches without a model_template.
    create_kwargs: dict = {
        "input_file_id": input_file.id,
        "endpoint": args.endpoint,
        "completion_window": args.completion_window,
    }
    aibrix_block: dict = {}
    if args.resource_type:
        aibrix_block["planner_decision"] = {
            "resource_details": [{"resource_type": args.resource_type}]
        }
    if args.aibrix_template:
        aibrix_block["model_template"] = {"name": args.aibrix_template}
    if args.aibrix_profile:
        aibrix_block["profile"] = {"name": args.aibrix_profile}
    if aibrix_block:
        create_kwargs["extra_body"] = {"aibrix": aibrix_block}

    try:
        batch = client.batches.create(**create_kwargs)
    except APIStatusError as e:
        fail(f"batch creation rejected: {e.status_code} {e.response.text}")
    dump("create response", batch)

    if args.cancel:
        step(f"Cancel  POST /v1/batches/{batch.id}/cancel")
        try:
            cancelled = client.batches.cancel(batch.id)
        except APIStatusError as e:
            fail(f"cancel rejected: {e.status_code} {e.response.text}")
        dump("cancel response", cancelled)
        if cancelled.status not in CANCELLED_STATES:
            fail(f"expected status in {CANCELLED_STATES}, got '{cancelled.status}'")

        if not args.keep:
            step(f"Delete input file  DELETE /v1/files/{input_file.id}")
            client.files.delete(input_file.id)
            print("  deleted ✓")
        print("\nAll steps passed (cancel path).")
        return

    step(
        f"Poll until terminal  GET /v1/batches/{batch.id}  "
        f"(timeout={args.timeout:.0f}s, every {args.poll_interval:.1f}s)"
    )
    final = poll_until_terminal(
        client, batch.id, poll_interval=args.poll_interval, timeout=args.timeout
    )
    dump("terminal batch", final)
    if final.status in TERMINAL_FAIL:
        fail(f"batch reached terminal failure state '{final.status}'")
    if not final.output_file_id:
        fail("batch completed but output_file_id is missing")

    step(f"Download output  GET /v1/files/{final.output_file_id}/content")
    output_bytes = client.files.content(final.output_file_id).read()
    print(f"  {len(output_bytes)} bytes received")

    step(f"Verify output: {args.num_records} responses, custom_id round-trip")
    verify_output(output_bytes, expected_custom_ids)
    print("  every input custom_id present, no duplicates ✓")

    if args.keep:
        step(
            f"Skip cleanup (--keep)  input={input_file.id}  "
            f"output={final.output_file_id}"
        )
        print("\nAll steps passed.")
        return

    step("Cleanup  DELETE /v1/files/<input>  DELETE /v1/files/<output>")
    client.files.delete(input_file.id)
    client.files.delete(final.output_file_id)
    print("  both files deleted ✓")
    print("\nAll steps passed.")


if __name__ == "__main__":
    main()

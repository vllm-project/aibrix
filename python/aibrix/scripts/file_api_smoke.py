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

"""Smoke test for AIBrix's OpenAI-compatible Files API.

Drives the API through the official ``openai`` Python SDK so a passing
run is also a compatibility check: every wire field has to round-trip
through the SDK's pydantic models.

Steps run in order: generate input → upload → list → retrieve metadata →
download content → delete. Each step prints a concise status line so
failures are obvious without scrolling.

Usage:
    poetry run python scripts/file_api_smoke.py \\
        --base-url http://localhost:8090/v1 \\
        --num-records 32 \\
        --seed 42

Requires the metadata service to be running and reachable at --base-url.
"""

from __future__ import annotations

import argparse
import io
import json
import random
import sys
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


def generate_records(num_records: int, rng: random.Random) -> list[dict]:
    records: list[dict] = []
    for i in range(num_records):
        records.append(
            {
                "custom_id": f"req-{i:04d}",
                "method": "POST",
                "url": "/v1/chat/completions",
                "body": {
                    "model": rng.choice(MODEL_BANK),
                    "messages": [
                        {"role": "user", "content": rng.choice(PROMPT_BANK)},
                    ],
                    "max_tokens": rng.choice([64, 128, 256, 512]),
                },
            }
        )
    rng.shuffle(records)
    return records


def to_jsonl_bytes(records: Iterable[dict]) -> bytes:
    return ("\n".join(json.dumps(r) for r in records) + "\n").encode("utf-8")


def step(label: str) -> None:
    print(f"\n── {label} ".ljust(72, "─"), flush=True)


def dump(label: str, payload: object) -> None:
    """Print a labelled JSON-ish view of an SDK object.

    The smoke test exists to surface OpenAI-compat regressions, so we
    want every server response visible — failures otherwise hide the
    raw payload. SDK pydantic models go through ``model_dump``;
    everything else falls back to a plain ``json.dumps`` with a
    safe ``default``.
    """
    if hasattr(payload, "model_dump"):
        body = payload.model_dump()  # type: ignore[union-attr]
    else:
        body = payload
    print(f"  {label}:")
    text = json.dumps(body, indent=2, ensure_ascii=False, default=str)
    for line in text.splitlines():
        print(f"    {line}")


def fail(msg: str) -> "None":
    print(f"FAIL: {msg}", file=sys.stderr)
    sys.exit(1)


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument(
        "--base-url",
        default="http://localhost:8090/v1",
        help="Files API base URL (must end in /v1). Default: %(default)s",
    )
    p.add_argument(
        "--api-key",
        default="dummy",
        help="API key. Required by the SDK; AIBrix ignores it. Default: %(default)s",
    )
    p.add_argument(
        "--num-records",
        type=int,
        default=10,
        help="Number of JSONL records to generate. Default: %(default)s",
    )
    p.add_argument(
        "--seed",
        type=int,
        default=None,
        help="Random seed; omit for nondeterministic input.",
    )
    p.add_argument(
        "--purpose",
        default="batch",
        help=(
            "OpenAI files purpose. AIBrix only accepts 'batch' today; pass "
            "anything else to verify the route's enum rejection. "
            "Default: %(default)s"
        ),
    )
    p.add_argument(
        "--keep",
        action="store_true",
        help="Skip the final delete so the file remains for inspection.",
    )
    return p.parse_args()


def main() -> None:
    args = parse_args()
    rng = random.Random(args.seed)
    client = OpenAI(base_url=args.base_url, api_key=args.api_key)

    step(f"Generate {args.num_records} shuffled records (seed={args.seed})")
    records = generate_records(args.num_records, rng)
    payload = to_jsonl_bytes(records)
    print(
        f"  payload size: {len(payload)} bytes; "
        f"first custom_id: {records[0]['custom_id']}"
    )

    step(f"Upload  POST /v1/files  purpose={args.purpose}")
    try:
        uploaded = client.files.create(
            file=("smoke-input.jsonl", io.BytesIO(payload), "application/jsonl"),
            purpose=args.purpose,
        )
    except APIStatusError as e:
        fail(f"upload rejected: {e.status_code} {e.response.text}")
    dump("upload response", uploaded)

    step("List   GET  /v1/files")
    page = client.files.list()
    dump(
        f"list response (count={len(page.data)})",
        [f.model_dump() for f in page.data],
    )
    found = next((f for f in page.data if f.id == uploaded.id), None)
    if found is None:
        fail(f"uploaded file {uploaded.id} not in list response")
    print("  ours present ✓")

    step(f"Retrieve  GET  /v1/files/{uploaded.id}")
    metadata = client.files.retrieve(uploaded.id)
    dump("retrieve response", metadata)
    if metadata.bytes != uploaded.bytes:
        fail(
            f"bytes mismatch: upload reported {uploaded.bytes}, "
            f"retrieve reports {metadata.bytes}"
        )

    step(f"Download  GET  /v1/files/{uploaded.id}/content")
    body = client.files.content(uploaded.id).read()
    if body != payload:
        fail(
            f"content mismatch: uploaded {len(payload)} bytes, "
            f"downloaded {len(body)} bytes"
        )
    print(f"  {len(body)} bytes round-tripped byte-for-byte ✓")

    if args.keep:
        step(f"Skip delete (--keep)  file_id={uploaded.id}")
        return

    step(f"Delete  DELETE /v1/files/{uploaded.id}")
    deleted = client.files.delete(uploaded.id)
    dump("delete response", deleted)
    if not deleted.deleted:
        fail(f"delete returned deleted={deleted.deleted}")

    step("Verify post-delete 404  GET /v1/files/<id>")
    try:
        client.files.retrieve(uploaded.id)
    except APIStatusError as e:
        if e.status_code != 404:
            fail(f"expected 404 after delete, got {e.status_code}")
        print(f"  retrieve after delete -> {e.status_code} ✓")
    else:
        fail("retrieve after delete unexpectedly succeeded")

    print("\nAll steps passed.")


if __name__ == "__main__":
    main()

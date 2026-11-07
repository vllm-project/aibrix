# The `output` Extension

The OpenAI API returns a result only in the HTTP response body. Batch and
offline workloads usually need the result somewhere else: in an object-store
bucket, on a shared volume, or behind a pre-signed upload URL, so that
downstream jobs can pick it up.

`output` is an optional AIBrix field that lets a request **name where its
result should be stored**. The frontend persists the response payload and any
explicitly declared local image/video outputs. The HTTP response retains all
non-payload fields, empties `choices`/`data`, and adds `output`. This extension
is removed before forwarding the request to the engine.

![Proxy engine overview](https://github.com/user-attachments/assets/2c48db97-6fc5-46eb-af0c-9ba7aa06eae7)

*Where `output` fits: the proxy can resolve media inputs to local files before
inference. It persists response payloads and explicitly referenced local outputs
to the destination; uploaded source files are then removed (steps 5–8).*

This document describes the schema. It is defined in
[`schemas/output.py`](schemas/output.py) and used by the request and response
models in [`schemas/openai.py`](schemas/openai.py) and
[`schemas/generation.py`](schemas/generation.py).

---

## Request: `OutputRequest`

`output` is accepted on:

- `POST /v1/chat/completions`
- `POST /v1/completions`
- `POST /v1/embeddings`
- `POST /v1/images/generations`
- `POST /v1/videos`: wait for engine generation and output persistence,
  then return the completed response with `output`. No early creation receipt
  is returned for requests with `output`.

```json
{
  "model": "my-model",
  "messages": [{"role": "user", "content": "Summarise this document."}],
  "output": {"url": "tos://my-bucket/results/run-1"}
}
```

| Field | Required | Description |
|---|---|---|
| `url` | yes | Destination **prefix** for the result. |

`url` takes one of these forms:

| Form | Example | Meaning |
|---|---|---|
| Object storage | `tos://my-bucket/results`, `s3://my-bucket/results` | Bucket plus key prefix. Every object-storage backend AIBrix supports is accepted, using its name as the scheme. |
| Local directory | `/data/out`, `file:///data/out` | Absolute directory on the serving host. |
| HTTP `PUT` | `https://host/upload?signature=...` | The result is uploaded to this exact URL, for example a pre-signed URL. |

There is deliberately **no file-name field**. The name is derived from the
response id (for example `chatcmpl-123.json`), so concurrent requests writing
to the same prefix never collide.

Requests are rejected with a validation error when:

- the scheme is not one of the forms above;
- an object-storage URL has no bucket, or an HTTP URL has no host;
- a local path is relative or contains `..`;
- `output` contains any field other than `url` (for example `filename`);
- the request also sets `"stream": true`. `output` stores one complete
  response, so it cannot be combined with streaming.

---

## Response: `OutputResponse`

When a request carries `output`, the stored JSON contains only the complete
`choices` and/or `data` arrays that were present, e.g. `{"choices":[...]}`.
Text, token IDs, tool calls, logprobs, and base64 strings inside those arrays
are preserved; base64 is **not** decoded into media files. The request and
response metadata are not persisted in that JSON.

The HTTP response keeps **every non-payload field**, including unknown engine
extensions and explicit nulls, without a metadata whitelist. It sets the existing
`choices`/`data` fields to `[]` and adds `output`:

```json
{
  "id": "chatcmpl-123",
  "object": "chat.completion",
  "created": 1730000000,
  "model": "my-model",
  "usage": {"prompt_tokens": 12, "completion_tokens": 34, "total_tokens": 46},
  "choices": [],
  "output": {
    "url": "tos://my-bucket/results/run-1",
    "artifacts": ["chatcmpl-123.json"]
  }
}
```

Read the real result from the stored artifact. Existing payload array fields
remain present but empty; absent fields are not added.

| Field | Description |
|---|---|
| `url` | The destination prefix, exactly as requested. It is not the location of a file on its own. |
| `artifacts` | What was written, relative to `url`. Join each entry with `url` to get the full location, e.g. `tos://my-bucket/results/run-1/chatcmpl-123.json`. For an HTTP `PUT` target the entry is already the full URL. |

The `output` block is available on chat completion, text completion, embedding,
image-generation and successfully uploaded video responses.

---

## Configuring storage

Results are written through AIBrix's shared storage layer. Configure the backend
when starting the frontend with `--setenv KEY VALUE`. The bucket in `output.url`
must match the configured bucket.

TOS:

```bash
python -m aibrix.openai_frontend.main \
  --engine proxy \
  --proxy-engine-cmd 'python -m vllm.entrypoints.openai.api_server --model my-model' \
  --setenv STORAGE_TYPE tos \
  --setenv STORAGE_TOS_BUCKET my-bucket \
  --setenv STORAGE_TOS_ACCESS_KEY 'AK...' \
  --setenv STORAGE_TOS_SECRET_KEY 'SK...' \
  --setenv STORAGE_TOS_ENDPOINT tos-cn-beijing.volces.com \
  --setenv STORAGE_TOS_REGION cn-beijing
```

S3:

```bash
python -m aibrix.openai_frontend.main \
  --engine proxy \
  --proxy-engine-cmd 'python -m vllm.entrypoints.openai.api_server --model my-model' \
  --setenv STORAGE_TYPE s3 \
  --setenv STORAGE_AWS_BUCKET vectors \
  --setenv STORAGE_AWS_ACCESS_KEY_ID 'AK...' \
  --setenv STORAGE_AWS_SECRET_ACCESS_KEY 'SK...' \
  --setenv STORAGE_AWS_REGION us-east-1
```

Replace `my-model` and the example credentials with your deployment's values.
For an S3-compatible gateway, also add
`--setenv STORAGE_AWS_ENDPOINT_URL https://your-s3-endpoint` to the command.
The examples configure buckets matching the TOS and S3 requests below.

Local directories need no storage configuration; missing parent directories are
created. HTTP `PUT` targets need no storage configuration; any `2xx` counts as success.

---

## Examples

Chat completion to TOS:

```bash
curl -X POST http://localhost:8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "my-model",
    "messages": [{"role": "user", "content": "Hello"}],
    "output": {"url": "tos://my-bucket/jobs/2026-10-28"}
  }'
```

The result is written to `tos://my-bucket/jobs/2026-10-28/<chatcmpl-id>.json`.

Chat completion to a local directory:

```python
import httpx

resp = httpx.post(
    "http://localhost:8000/v1/chat/completions",
    json={
        "model": "my-model",
        "messages": [{"role": "user", "content": "Write a haiku about GPUs."}],
        "output": {"url": "/data/results"},
    },
    timeout=60,
)
resp.raise_for_status()
print(resp.json()["output"]["artifacts"])  # ['chatcmpl-....json']
```

Embeddings to S3:

```bash
curl -X POST http://localhost:8000/v1/embeddings \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "bge-large",
    "input": ["hello world", "goodbye world"],
    "output": {"url": "s3://vectors/jobs"}
  }'
```

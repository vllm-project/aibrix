# The `output` Extension

The OpenAI API returns a result only in the HTTP response body. Batch and
offline workloads usually need the result somewhere else: in an object-store
bucket, on a shared volume, or behind a pre-signed upload URL, so that
downstream jobs can pick it up.

`output` is an optional AIBrix field that lets a request **name where its
result should be stored**. The frontend writes the result there and returns
only a short receipt saying where it went. The engine is not involved: `output` is removed before
the request is forwarded, so engines that reject unknown fields keep working.

![Proxy engine overview](https://github.com/user-attachments/assets/2c48db97-6fc5-46eb-af0c-9ba7aa06eae7)

*Where `output` fits: the proxy engine resolves inputs to local files, the
engine works on local files only, and the result is uploaded from local disk
to the `output` destination (steps 5–8).*

This document describes the schema. It is defined in
[`schemas/output.py`](schemas/output.py) and used by the request and response
models in [`schemas/openai.py`](schemas/openai.py).

---

## Request: `OutputRequest`

`output` is accepted on:

- `POST /v1/chat/completions`
- `POST /v1/completions`
- `POST /v1/embeddings`

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

When a request carries `output`, the result is **not** returned in the HTTP
response. The complete engine response is written to the destination, and the
HTTP response keeps only its metadata (`id`, `object`, `created`, `model`,
`system_fingerprint`, `usage`) plus an `output` block that says where the
result went:

```json
{
  "id": "chatcmpl-123",
  "object": "chat.completion",
  "created": 1730000000,
  "model": "my-model",
  "usage": {"prompt_tokens": 12, "completion_tokens": 34, "total_tokens": 46},
  "output": {
    "url": "tos://my-bucket/results/run-1",
    "artifacts": ["chatcmpl-123.json"]
  }
}
```

For compatibility, the result fields are still present, but empty:
`choices` is `[]` on chat and text completions, and `data` is `[]` on
embeddings. Read the real result from the stored artifact.

| Field | Description |
|---|---|
| `url` | The destination prefix, exactly as requested. It is not the location of a file on its own. |
| `artifacts` | What was written, relative to `url`. Join each entry with `url` to get the full location, e.g. `tos://my-bucket/results/run-1/chatcmpl-123.json`. For an HTTP `PUT` target the entry is already the full URL. |

The `output` block is available on chat completion, text completion and
embedding responses.

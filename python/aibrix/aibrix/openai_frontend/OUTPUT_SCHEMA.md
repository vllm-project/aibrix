# The `output` Extension

The OpenAI API delivers inference results in the HTTP response body. Batch and
offline workloads often also need those results in durable storage: an
object-store bucket, a shared volume, or a destination accessible through a
pre-signed upload URL. This lets downstream jobs consume results without relying
on the client to save the response.

`output` is an optional AIBrix request field that **specifies where inference
results should be stored**. Add `"output": {"url": "..."}` to an eligible request.
The proxy removes this field before forwarding to the inner engine, persists
the schema-declared result payload and any explicitly declared local image/video
outputs, and returns a receipt identifying the written artifacts.

Persistence changes where the client reads the result, not which metadata it
receives. The HTTP response retains non-payload fields, including IDs, usage and
vendor metadata, while clearing persisted result fields and adding the `output`
receipt. For OpenAI responses, `choices`/`data` become empty arrays; for Responses,
the native `output` array is replaced by the receipt. The complete result is
available in the stored artifact rather than duplicated in the HTTP body.
Without the extension, the response keeps its native result shape.

![Proxy engine overview](https://github.com/user-attachments/assets/2c48db97-6fc5-46eb-af0c-9ba7aa06eae7)

*The proxy may resolve media inputs to local files before inference. After
inference, it persists the result payload and explicitly declared local outputs
to the requested destination, then returns the artifact receipt. Uploaded output
source files are cleaned up after use (steps 5–8).*

## Features

### Supported requests

- `POST /v1/chat/completions`, `/v1/completions`, `/v1/embeddings`
- `POST /v1/images/generations`, `/v1/videos`
- `POST /v1/responses`, `/v1/responses/compact`
- Declared engine-native JSON endpoints with a result schema; SGLang `/generate`,
  `/encode`, `/classify`, and `/v1/rerank` support both `POST` and `PUT`.

The inner engine must support the requested endpoint. Video requests with
`output` wait for generation and persistence instead of returning an early job
receipt.

### Destinations

- **Local:** `/data/out` or `file:///data/out` — an absolute directory; created
  if missing.
- **Object storage:** `tos://bucket/prefix`, `s3://bucket/prefix`, or another
  supported object-storage scheme — requires a matching configured backend and
  bucket.
- **HTTP:** `https://host/upload?signature=...` — one `PUT` to the exact URL;
  multiple output files are packaged as a ZIP. Any `2xx` is success.

`url` is required and is the only request field. Artifact names are generated
from the response ID, or the proxy request ID when none is available.

### Stored results and HTTP responses

- **OpenAI:** store the original `choices`/`data` arrays; clear them to `[]` in
  HTTP. Metadata stays in HTTP, not in the stored JSON.
- **Responses:** store `{"output": [...]}`; replace the native HTTP `output`
  array with the persistence receipt.
- **Native JSON:** store only schema-marked results, such as SGLang
  `text`/`output_ids`, embeddings and output logprobs, rerank results, scores,
  or Anthropic/Cohere message content. Preserve IDs, usage, counters and vendor
  metadata in HTTP. Clear result lists to `[]`, objects to `{}`, strings to
  `""`, and other scalars to `null`; explicit nulls stay null. Fields marked for
  retention, such as rerank indices and logprob dtypes, remain in both copies.
- **Native batches:** store the ordered list of per-item results; return
  `{"data": [<per-item metadata>], "output": <receipt>}`.
- **Local image/video outputs:** upload only paths explicitly declared by the
  response schema. Uploaded source files are cleaned up after use.

The receipt contains the requested `url` and an `artifacts` list. Join each
artifact with the local directory or object-storage prefix to read the result.
For HTTP destinations, each artifact is already the full upload URL.

## Examples

### Chat completion to a local directory

```bash
curl http://localhost:8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "my-model",
    "messages": [{"role": "user", "content": "Hello"}],
    "output": {"url": "/data/results"}
  }'
```

HTTP response (other metadata omitted):

```json
{
  "id": "chatcmpl-123",
  "choices": [],
  "output": {
    "url": "/data/results",
    "artifacts": ["chatcmpl-123.json"]
  }
}
```

`/data/results/chatcmpl-123.json` contains the original result:

```json
{
  "choices": [{
    "index": 0,
    "message": {"role": "assistant", "content": "Hello!"},
    "finish_reason": "stop"
  }]
}
```

### Responses to object storage

```bash
curl http://localhost:8000/v1/responses \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "my-model",
    "input": "Hello",
    "store": false,
    "output": {"url": "tos://my-bucket/results"}
  }'
```

HTTP `output` becomes `{"url":"tos://my-bucket/results","artifacts":["resp_123.json"]}`.
The stored file contains `{"output":[...]}`, not the receipt.

### Storage configuration

Local and HTTP destinations need no storage configuration. Configure object
storage with `--setenv KEY VALUE` when starting the proxy:

```bash
# TOS; replace the example model and credentials.
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

For S3, use `STORAGE_TYPE=s3`, `STORAGE_AWS_BUCKET`,
`STORAGE_AWS_ACCESS_KEY_ID`, `STORAGE_AWS_SECRET_ACCESS_KEY`, and
`STORAGE_AWS_REGION`. S3-compatible gateways also use
`STORAGE_AWS_ENDPOINT_URL`. For example, embeddings can target
`"output": {"url": "s3://vectors/results"}` when the configured bucket is `vectors`.

## Limitations

- No persistence for streaming, `background=true` Responses, GET-only metadata,
  multipart or WebSocket requests. Ordinary background Responses still forward;
  the proxy does not poll them for persistence.
- Responses persistence changes the HTTP `output` type from an array to a
  receipt object; it is an opt-in AIBrix extension, not the standard SDK shape.
- Native results require declared schema fields. Empty results and zero values
  are valid; missing or all-null results are errors. The full response envelope
  is not stored, arbitrary paths/URLs are not fetched, and result base64 strings
  are not decoded into media files.

## Schema references

[Output](schemas/output.py) · [OpenAI](schemas/openai.py) ·
[Responses](schemas/responses.py) · [Image/video](schemas/generation.py) ·
[Native common](schemas/engine_common.py) · [vLLM](schemas/vllm.py) ·
[SGLang](schemas/sglang.py)

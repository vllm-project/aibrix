# The `processors` Extension

`processors` is an optional AIBrix request field for processing media inputs
before inference and results before saving them. It sits beside `output` in the
request body. The proxy validates and removes it before forwarding the request
to the inner engine.

Each stage runs in array order. Omit `processors` when no processing is needed.
A pre-only request returns the usual inference response. Post processing requires
`output` and retains the existing [output receipt](OUTPUT_SCHEMA.md).

![Proxy engine overview](https://github.com/user-attachments/assets/2c48db97-6fc5-46eb-af0c-9ba7aa06eae7)

*Pre processing runs after input media is localized and before inference. Post
processing runs after inference and before results are saved.*

## Supported requests

- `POST /v1/chat/completions`, `/v1/completions`, `/v1/embeddings`
- `POST /v1/images/generations`
- `POST /v1/responses`, `/v1/responses/compact`

The inner engine must support the requested endpoint and provide the inputs or
outputs required by the selected processors. Engine-native endpoints and
asynchronous video generation do not support this extension.

## Request fields

`processors` contains two optional arrays:

- **`pre`** — process local input media before inference. Each input keeps one
  corresponding output, preserving input count and order. No `output` destination
  is required.
- **`post`** — process the result payload or declared local output files before
  saving them. Requires `output`. Response IDs, usage and other metadata are
  preserved in the HTTP response and are not processed.

Both arrays default to `[]` and accept at most **8 entries** each. At least one
must be non-empty; `{}` and `{"pre": [], "post": []}` are invalid.

Each entry has the following shape:

```json
{
  "type": "processor",
  "processor": {
    "name": "image.resize",
    "arguments": {"max_edge": 1024},
    "media_types": ["image"]
  }
}
```

- **`type`** — required; exactly `"processor"`.
- **`processor`** — required; the invocation object.
- **`processor.name`** — required; a processor enabled in the deployment and
  valid for the selected stage. Names are 1–64 characters and match
  `^[a-z][a-z0-9_.-]*$`.
- **`processor.arguments`** — optional JSON object, default `{}`. Allowed keys,
  value types and required arguments depend on the selected processor.
- **`processor.media_types`** — optional array containing `"image"`, `"audio"`
  or `"video"`. Selects the media types to process; unselected files are preserved.
  Omission selects all applicable media. Duplicate or unsupported values are
  rejected. This field is not valid for `json.validate`.

Unknown fields and unused arguments are rejected. Names, stages, arguments and
request conflicts are validated before inference. Errors identify the stage,
entry index, processor name and offending argument.

## Examples

### Resize images before chat inference

```json
{
  "model": "my-vision-model",
  "messages": [{"role": "user", "content": [
    {"type": "text", "text": "Describe this image."},
    {"type": "image_url", "image_url": {"url": "https://example.com/image.png"}}
  ]}],
  "processors": {
    "pre": [{"type": "processor", "processor": {
      "name": "image.resize", "arguments": {"max_edge": 1024}
    }}]
  }
}
```

Media localization must be enabled so the processor can access the image.
No `output` is required for pre-only processing.

### Validate a chat result before saving it

```json
{
  "model": "my-model",
  "messages": [{"role": "user", "content": "Return a short answer."}],
  "processors": {
    "post": [{"type": "processor", "processor": {
      "name": "json.validate",
      "arguments": {"schema": {
        "type": "object",
        "properties": {"choices": {"type": "array", "minItems": 1}},
        "required": ["choices"]
      }}
    }}]
  },
  "output": {"url": "/data/results"}
}
```

### Resize and archive generated images

```json
{
  "model": "my-image-model",
  "prompt": "A mountain landscape.",
  "processors": {
    "post": [
      {"type": "processor", "processor": {
        "name": "image.resize", "arguments": {"max_edge": 512}
      }},
      {"type": "processor", "processor": {
        "name": "files.compress", "arguments": {"format": "tar.gz"}
      }}
    ]
  },
  "output": {"url": "/data/results"}
}
```

The engine must return declared local image files. The JSON result and the
`tar.gz` archive are saved separately under `/data/results`.

## Requirements and errors

- Enable processing with `--proxy-enable-processors`. It defaults to off;
  requests containing `processors` are rejected with 400 while disabled.
- `--proxy-processor-allowlist` optionally restricts enabled processor names.
  For example: `files.compress,json.validate,image.resize`.
- Media processors require local files. Remote and inline inputs must be
  localized before pre processing. Processor arguments cannot supply filesystem
  paths, commands or output filenames.
- Requests must be non-streaming. `stream: true` and Responses `background: true`
  conflict with `processors` and return 400. Multipart and WebSocket requests
  are outside this JSON extension.
- Inference errors retain the engine's status. Processor failures are not
  automatically retried, and failed processing does not fall back to the
  original input or result.

## Schema references

[Processors](schemas/processors.py) · [OpenAI](schemas/openai.py) ·
[Responses](schemas/responses.py) · [Image generation](schemas/generation.py) ·
[Output](OUTPUT_SCHEMA.md)
